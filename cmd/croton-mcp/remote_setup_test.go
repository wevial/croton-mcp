//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

// Croton prompts on /dev/tty. Give each invocation its own controlling
// terminal on both supported platforms. Keep its output in the test process.
func setupTerminal(ctx context.Context, binary, input string, args ...string) *exec.Cmd {
	const driver = `
import errno, os, pty, signal, sys
pid, master = pty.fork()
if pid == 0:
    os.execv(sys.argv[1], sys.argv[1:])
signal.signal(signal.SIGTERM, lambda *_: os.kill(pid, signal.SIGTERM))
os.write(master, sys.stdin.buffer.read())
try:
    while data := os.read(master, 4096):
        sys.stdout.buffer.write(data)
except OSError as err:
    if err.errno != errno.EIO:
        raise
_, status = os.waitpid(pid, 0)
sys.exit(os.waitstatus_to_exitcode(status))
`
	cmd := exec.CommandContext(ctx, "python3", append([]string{"-I", "-c", driver, binary}, args...)...)
	cmd.Stdin = strings.NewReader(input)
	return cmd
}

func TestRemoteConnectorSetup(t *testing.T) {
	binary := buildCrotonBinary(t)
	bridge := fixtureBridge(t)
	funnel := newFunnelFixture(t, writeServerConfig(t, bridge))
	const lifetime = 5 * time.Second
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()

	serve := setupTerminal(ctx, binary, "yes\n", append(funnel.args, "--access-lifetime", lifetime.String())...)
	serve.Env = []string{"PATH=" + funnel.dir + ":/usr/bin:/bin"}
	if err := serve.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = serve.Process.Signal(syscall.SIGTERM)
		if err := serve.Wait(); err != nil {
			t.Error("synthetic server did not exit cleanly", err)
		}
	}()
	waitForMarker(t, filepath.Join(funnel.dir, "readback"))

	p := setupProxyClient(t, funnel.target)
	terminal := func(command, input string, args ...string) string {
		t.Helper()
		output, err := setupTerminal(ctx, binary, input, append([]string{command, "--state-dir", funnel.state}, args...)...).Output()
		if err != nil {
			t.Fatalf("owner command %s failed: %v", command, err)
		}
		return strings.TrimSpace(string(output))
	}
	path := func(endpoint string) string {
		t.Helper()
		if !strings.HasPrefix(endpoint, p.origin+"/") {
			t.Fatal("discovered endpoint outside the approved HTTPS origin")
		}
		return strings.TrimPrefix(endpoint, p.origin)
	}
	field := func(body []byte, pattern string) string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindSubmatch(body)
		if match == nil {
			t.Fatal("missing field in connector setup response")
		}
		return string(match[1])
	}

	headers, _ := p.request(t, "GET", "/mcp", "", "", http.StatusUnauthorized)
	metadataURL := field([]byte(headers.Get("WWW-Authenticate")), `^Bearer resource_metadata="([^"]+)"$`)
	_, body := p.request(t, "GET", path(metadataURL), "", "", http.StatusOK)
	var resource struct {
		Resource string
		Issuers  []string `json:"authorization_servers"`
	}
	if json.Unmarshal(body, &resource) != nil || resource.Resource != p.origin+"/mcp" || !slices.Equal(resource.Issuers, []string{p.origin}) {
		t.Fatal("invalid protected resource metadata")
	}

	_, body = p.request(t, "GET", path(resource.Issuers[0]+"/.well-known/oauth-authorization-server"), "", "", http.StatusOK)
	var metadata struct {
		Issuer    string
		Register  string `json:"registration_endpoint"`
		Authorize string `json:"authorization_endpoint"`
		Token     string `json:"token_endpoint"`
	}
	if json.Unmarshal(body, &metadata) != nil || metadata.Issuer != resource.Issuers[0] {
		t.Fatal("invalid authorization server metadata")
	}

	link, err := url.Parse(terminal("pair", ""))
	if err != nil || link.Scheme+"://"+link.Host != p.origin || link.Fragment == "" {
		t.Fatal("invalid terminal handoff")
	}
	p.request(t, "GET", link.Path, "", "", http.StatusOK)
	_, body = p.request(t, "POST", link.Path, clientForm, url.Values{"handoff": {link.Fragment}}.Encode(), http.StatusOK)
	owner := field(body, `data-owner="([^"]+)"`)

	_, body = p.request(t, "POST", path(metadata.Register), "application/json",
		`{"client_name":"setup.test","redirect_uris":["`+clientRedirect+`"],"grant_types":["authorization_code","refresh_token"]}`, http.StatusCreated)
	var client approvedClient
	if json.Unmarshal(body, &client) != nil || client.ID == "" {
		t.Fatal("client registration failed")
	}

	authorize := url.Values{
		"client_id": {client.ID}, "response_type": {"code"}, "redirect_uri": {clientRedirect},
		"code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(clientVerifier)},
		"resource": {resource.Resource}, "state": {"setup.test"},
	}
	_, body = p.request(t, "GET", path(metadata.Authorize)+"?"+authorize.Encode(), "", "", http.StatusOK)
	signin := url.Values{"owner": {owner}, "request": {field(body, `name="request" value="([^"]+)"`)}}.Encode()
	_, body = p.request(t, "POST", "/owner/signin", clientForm, signin, http.StatusOK)
	terminal("approve", field(body, `<strong>([^<]+)</strong>`)+"\n")

	headers, _ = p.request(t, "POST", "/owner/finish", clientForm, signin, http.StatusSeeOther)
	callback, err := url.Parse(headers.Get("Location"))
	if err != nil || callback.Scheme+"://"+callback.Host+callback.Path != clientRedirect ||
		callback.Query().Get("state") != authorize.Get("state") || callback.Query().Get("iss") != metadata.Issuer {
		t.Fatal("invalid client callback")
	}

	exchange := func(form url.Values) {
		t.Helper()
		_, body := p.request(t, "POST", path(metadata.Token), clientForm, form.Encode(), http.StatusOK)
		var token struct {
			Access  string `json:"access_token"`
			Refresh string `json:"refresh_token"`
			Seconds int    `json:"expires_in"`
		}
		if json.Unmarshal(body, &token) != nil || token.Access == "" || token.Refresh == "" || token.Seconds != int(lifetime.Seconds()) {
			t.Fatal("missing token pair or unexpected access lifetime")
		}
		client.Access, client.Refresh = token.Access, token.Refresh
	}

	exchange(url.Values{
		"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")}, "client_id": {client.ID},
		"redirect_uri": {clientRedirect}, "code_verifier": {clientVerifier}, "resource": {resource.Resource},
	})
	expired := time.Now().Add(lifetime)
	setupFolderRead(t, p, resource.Resource, client.Access, http.StatusOK)

	time.Sleep(time.Until(expired))
	setupFolderRead(t, p, resource.Resource, client.Access, http.StatusUnauthorized)
	exchange(client.refreshForm())
	setupFolderRead(t, p, resource.Resource, client.Access, http.StatusOK)

	listed := strings.Fields(terminal("clients", ""))
	if !slices.Equal(listed, []string{client.ID, `"setup.test"`}) {
		t.Fatal("owner terminal did not list the paired client")
	}

	terminal("unpair", "", listed[0])
	if terminal("clients", "") != "" {
		t.Fatal("client still listed after unpair")
	}
	if bridge.AssertReadOnlyCommands() != nil {
		t.Fatal("unexpected command at the synthetic Bridge")
	}
}

func setupProxyClient(t *testing.T, targetURL string) *httpProcess {
	t.Helper()
	const host = "croton.tail.test"

	// Model the approved host's TLS termination entirely on loopback. Preserve
	// the external Host and Origin, including for the browser's secure cookie.
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)

	transport := proxy.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != host+":443" {
			return nil, errors.New("unexpected connector destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)

	jar, _ := cookiejar.New(nil)
	return &httpProcess{origin: "https://" + host, client: &http.Client{
		Transport: transport, Jar: jar, Timeout: 5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}
}

// Use the stateless MCP wire format directly, independently of the server SDK.
func setupFolderRead(t *testing.T, p *httpProcess, endpoint, token string, status int) {
	t.Helper()
	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
		"name":"list_folders","arguments":{},"_meta":{
			"io.modelcontextprotocol/protocolVersion":"2026-07-28",
			"io.modelcontextprotocol/clientInfo":{"name":"setup.test","version":"1"},
			"io.modelcontextprotocol/clientCapabilities":{}
		}}}`
	req, err := http.NewRequestWithContext(t.Context(), "POST", endpoint, strings.NewReader(call))
	if err != nil {
		t.Fatal("invalid MCP endpoint")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("MCP-Protocol-Version", "2026-07-28")
	req.Header.Set("Mcp-Method", "tools/call")
	req.Header.Set("Mcp-Name", "list_folders")

	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal("MCP read failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		t.Fatalf("list_folders returned HTTP %d, want %d", resp.StatusCode, status)
	}
	if status == http.StatusUnauthorized {
		return
	}

	var rpc struct {
		ID     int
		Result struct {
			IsError bool
			Content []struct{ Type, Text string }
		}
	}
	media, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if media != "application/json" || json.NewDecoder(resp.Body).Decode(&rpc) != nil || rpc.ID != 1 ||
		rpc.Result.IsError || len(rpc.Result.Content) != 1 || rpc.Result.Content[0].Type != "text" {
		t.Fatal("list_folders did not return a JSON tool result")
	}
	var result struct{ Folders []struct{ Name string } }
	if json.Unmarshal([]byte(rpc.Result.Content[0].Text), &result) != nil || len(result.Folders) != 1 || result.Folders[0].Name != "INBOX" {
		t.Fatal("list_folders did not return the synthetic inbox")
	}
}

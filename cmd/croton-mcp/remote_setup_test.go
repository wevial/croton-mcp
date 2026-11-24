//go:build linux || darwin

package main

import (
	"bytes"
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

	"github.com/wevial/croton-mcp/internal/testkit"
	"golang.org/x/net/html"
	"golang.org/x/net/html/atom"
	"golang.org/x/oauth2"
)

// A private controlling terminal keeps each command's /dev/tty prompts and replies inside the test.
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
	// Resolve Node.js before the Funnel fixture adds its "node" data file to PATH.
	node, err := exec.LookPath("node")
	if err != nil {
		t.Fatal("the setup test requires Node.js on PATH")
	}
	browser := setupBrowser{node: node}

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
	browserURL := link.String()
	link.Fragment = ""
	headers, body = p.request(t, "GET", path(link.String()), "", "", http.StatusOK)
	page := setupDocument(t, body)
	pairURL, pair := setupPostForm(t, link.String(), page, "Pair this browser")
	pair = browser.runScripts(t, browserURL, page, headers, pair, false)

	headers, body = p.request(t, "POST", path(pairURL), clientForm, pair.Encode(), http.StatusOK)
	browser.runScripts(t, pairURL, setupDocument(t, body), headers, nil, false)

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
	authorizeURL := metadata.Authorize + "?" + authorize.Encode()
	headers, body = p.request(t, "GET", path(authorizeURL), "", "", http.StatusOK)
	page = setupDocument(t, body)
	signinURL, signin := setupPostForm(t, authorizeURL, page, "")
	signin = browser.runScripts(t, authorizeURL, page, headers, signin, true)
	_, body = p.request(t, "POST", path(signinURL), clientForm, signin.Encode(), http.StatusOK)
	terminal("approve", field(body, `<strong>([^<]+)</strong>`)+"\n")

	finishURL, continuation := setupPostForm(t, signinURL, setupDocument(t, body), "Continue after approval")
	headers, _ = p.request(t, "POST", path(finishURL), clientForm, continuation.Encode(), http.StatusSeeOther)
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
	beforeRead := len(bridge.Commands())
	setupFolderRead(t, p, resource.Resource, client.Access, http.StatusOK)
	if !slices.ContainsFunc(bridge.Commands()[beforeRead:], func(cmd testkit.Command) bool { return cmd.Name == "LIST" }) {
		t.Fatal("list_folders did not issue LIST to the synthetic Bridge")
	}

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

func setupDocument(t *testing.T, body []byte) *html.Node {
	t.Helper()
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		t.Fatal("invalid browser page")
	}

	return doc
}

type setupBrowser struct {
	node    string
	storage map[string]string
}

// Run the served inline scripts with the form controls and local storage of the synthetic browser.
func (b *setupBrowser) runScripts(t *testing.T, pageURL string, doc *html.Node, headers http.Header, fields url.Values, wantSubmit bool) url.Values {
	t.Helper()
	page := struct {
		URL, Owner string
		Scripts    []string
		Fields     url.Values
		Storage    map[string]string
	}{URL: pageURL, Fields: fields, Storage: b.storage}

	var scriptSources []string
	for directive := range strings.SplitSeq(headers.Get("Content-Security-Policy"), ";") {
		if parts := strings.Fields(directive); len(parts) > 0 && parts[0] == "script-src" {
			scriptSources = parts[1:]
			break
		}
	}

	for node := range doc.Descendants() {
		switch node.DataAtom {
		case atom.Body:
			for _, attr := range node.Attr {
				if attr.Key == "data-owner" {
					page.Owner = attr.Val
				}
			}
		case atom.Script:
			for _, attr := range node.Attr {
				if attr.Key == "nonce" && slices.Contains(scriptSources, "'nonce-"+attr.Val+"'") && node.FirstChild != nil {
					page.Scripts = append(page.Scripts, node.FirstChild.Data)
				}
			}
		}
	}

	const driver = `
const page = JSON.parse(require("node:fs").readFileSync(0, "utf8"));
const vm = require("node:vm");
const location = new URL(page.URL);
const store = new Map(Object.entries(page.Storage || {}));
const localStorage = {
    getItem: key => store.get(key) ?? null,
    setItem: (key, value) => store.set(key, String(value)),
};
const elements = Object.fromEntries(Object.entries(page.Fields || {}).map(
    ([name, [value]]) => [name, {value}]));
const values = () => Object.fromEntries(Object.entries(elements).map(
    ([name, control]) => [name, [control.value]]));
let submission;
const form = {...elements, elements, submit() { submission = values(); }};
const document = {forms: page.Fields ? [form] : [], body: {dataset: {owner: page.Owner}}};
const history = {replaceState(_, title, url) { location.href = new URL(url, location).href; }};
const context = vm.createContext({document, history, localStorage, location});
for (const script of page.Scripts || []) vm.runInContext(script, context, {timeout: 1000});
process.stdout.write(JSON.stringify({
    fields: submission ?? values(), submitted: !!submission, storage: Object.fromEntries(store)
}));
`
	input, err := json.Marshal(page)
	if err != nil {
		t.Fatal("could not encode synthetic page")
	}
	cmd := exec.CommandContext(t.Context(), b.node, "-e", driver)
	cmd.Stdin = bytes.NewReader(input)

	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("could not execute browser handoff scripts: %v", err)
	}
	var result struct {
		Fields    url.Values
		Submitted bool
		Storage   map[string]string
	}
	if json.Unmarshal(output, &result) != nil || result.Submitted != wantSubmit {
		t.Fatal("unexpected browser handoff form submission")
	}

	b.storage = result.Storage
	return result.Fields
}

func setupPostForm(t *testing.T, pageURL string, doc *html.Node, buttonLabel string) (string, url.Values) {
	t.Helper()
	page, err := url.Parse(pageURL)
	if err != nil {
		t.Fatal("invalid form page URL")
	}

	attributes := func(node *html.Node) map[string]string {
		attrs := make(map[string]string, len(node.Attr))
		for _, attr := range node.Attr {
			attrs[attr.Key] = attr.Val
		}
		return attrs
	}
	for form := range doc.Descendants() {
		if form.DataAtom != atom.Form {
			continue
		}
		attrs := attributes(form)
		method, target := attrs["method"], attrs["action"]
		foundButton := false
		fields := url.Values{}
		for control := range form.Descendants() {
			if control.DataAtom != atom.Input && control.DataAtom != atom.Button {
				continue
			}
			controlAttrs := attributes(control)
			if _, disabled := controlAttrs["disabled"]; disabled {
				continue
			}
			if owner, ok := controlAttrs["form"]; ok && (owner == "" || owner != attrs["id"]) {
				continue
			}

			if control.DataAtom == atom.Button {
				var label strings.Builder
				for node := range control.Descendants() {
					if node.Type == html.TextNode {
						label.WriteString(node.Data)
					}
				}
				if buttonLabel == "" || strings.Join(strings.Fields(label.String()), " ") != buttonLabel {
					continue
				}
				if kind := controlAttrs["type"]; kind != "" && !strings.EqualFold(kind, "submit") {
					continue
				}
				foundButton = true
				if override, ok := controlAttrs["formmethod"]; ok {
					method = override
				}
				if override, ok := controlAttrs["formaction"]; ok {
					target = override
				}
			}
			if name := controlAttrs["name"]; name != "" {
				fields.Add(name, controlAttrs["value"])
			}
		}
		if buttonLabel != "" && !foundButton {
			continue
		}

		action, err := page.Parse(target)
		if err != nil || !strings.EqualFold(method, "post") {
			t.Fatal("expected a form with a valid POST action")
		}
		return action.String(), fields
	}
	if buttonLabel != "" {
		t.Fatalf("missing enabled submit button %q", buttonLabel)
	}
	t.Fatal("missing OAuth form")
	return "", nil
}

func setupProxyClient(t *testing.T, targetURL string) *httpProcess {
	t.Helper()
	const host = "croton.tail.test"

	// Terminate TLS on loopback while preserving the public Host and Origin for the secure cookie.
	target, err := url.Parse(targetURL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)

	client := proxy.Client()
	transport := client.Transport.(*http.Transport)
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != host+":443" {
			return nil, errors.New("unexpected connector destination")
		}
		return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
	}
	t.Cleanup(transport.CloseIdleConnections)

	client.Jar, _ = cookiejar.New(nil)
	client.Timeout = 5 * time.Second
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

	return &httpProcess{origin: "https://" + host, client: client}
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

//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/owneroauth"
	"github.com/wevial/croton-mcp/internal/testkit"
	"golang.org/x/oauth2"
)

func fixtureBridge(t *testing.T) *testkit.Server {
	t.Helper()
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { server.Close() })
	return server
}

func editConfig(t *testing.T, path string, edit func(map[string]any)) {
	t.Helper()
	var cfg map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &cfg); err != nil {
		t.Fatal(err)
	}
	edit(cfg)
	b, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPTransportStartup(t *testing.T) {
	config := writeServerConfig(t, fixtureBridge(t))
	dir := filepath.Join(canonicalTempDir(t), "state")
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()

	for _, test := range []struct {
		args []string
		want string
	}{
		{[]string{"invalid.test"}, "invalid command"},
		{[]string{"serve", "--config", config}, "HTTP authorizer unavailable"},
		{[]string{"serve", "--authorizer", "other", "--config", config}, "HTTP authorizer unavailable"},
		{[]string{"serve", "--config", config, "--authorizer", "owner", "--bind", l.Addr().String()}, "HTTP listener unavailable"},
	} {
		if err := run(t.Context(), append(test.args, "--state-dir", dir), io.Discard); err == nil || err.Error() != test.want {
			t.Fatalf("startup error = %v, want %s", err, test.want)
		}
	}
	for _, bad := range []string{":8787", "0.0.0.0:8787", "[::]:8787", "192.0.2.1:8787", "localhost:8787"} {
		if err := run(t.Context(), []string{"serve", "--config", config, "--authorizer", "owner", "--bind", bad}, io.Discard); err == nil || err.Error() != "invalid loopback bind" {
			t.Fatal("accepted invalid bind")
		}
	}
	editConfig(t, config, func(cfg map[string]any) { cfg["mutations"] = map[string]bool{"enabled": true} })
	if err := run(t.Context(), []string{"serve", "--config", config, "--authorizer", "owner", "--state-dir", dir}, io.Discard); err == nil || err.Error() != "HTTP requires read-only configuration" {
		t.Fatal("mutation configuration")
	}
	if _, err := os.Lstat(filepath.Join(dir, "owner.sock")); !os.IsNotExist(err) {
		t.Fatal("IPC listener after startup failure")
	}
}

func testTLS(t *testing.T, dir string) *tls.Config {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err = os.WriteFile(filepath.Join(dir, "cert.pem"), certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "key.pem"), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
}

type httpProcess struct {
	cmd            *exec.Cmd
	done           chan struct{}
	err            error
	stdout, stderr bytes.Buffer
	dir, origin    string
	client         *http.Client
}

func startHTTP(t *testing.T, config string) *httpProcess {
	t.Helper()
	// Keep the Unix socket under Darwin's sockaddr_un path limit.
	dir, err := os.MkdirTemp("/tmp", "croton-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	dir, err = filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := l.Addr().String()
	l.Close()

	transport := &http.Transport{TLSClientConfig: testTLS(t, dir)}
	t.Cleanup(transport.CloseIdleConnections)
	jar, _ := cookiejar.New(nil)
	p := &httpProcess{dir: dir, origin: "https://" + address, done: make(chan struct{}), client: &http.Client{Transport: transport, Jar: jar, Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	p.cmd = exec.Command(executable, "serve", "--config", config, "--authorizer", "owner", "--bind", address, "--state-dir", dir, "--tls-cert", filepath.Join(dir, "cert.pem"), "--tls-key", filepath.Join(dir, "key.pem"))
	p.cmd.Env = append(os.Environ(), runMainEnv+"=1")
	p.cmd.Stdout, p.cmd.Stderr = &p.stdout, &p.stderr
	if err = p.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { p.err = p.cmd.Wait(); close(p.done) }()
	t.Cleanup(func() { _ = p.cmd.Process.Kill(); <-p.done })
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-p.done:
			t.Fatal("HTTP process failed to start", p.err)
		default:
		}
		if resp, err := p.client.Get(p.origin + "/.well-known/oauth-authorization-server"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return p
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("HTTP startup timed out")
	return nil
}

func (p *httpProcess) stop(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-p.done:
		if p.err != nil || p.stdout.Len() != 0 {
			t.Fatal("HTTP process exit or stdout", p.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("HTTP shutdown exceeded five seconds")
	}
}

func (p *httpProcess) request(t *testing.T, method, path, media, body string, status int) (http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, p.origin+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if media != "" {
		req.Header.Set("Content-Type", media)
	}
	req.Header.Set("Origin", p.origin)
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal("HTTP request failed")
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != status {
		t.Fatalf("%s returned %d, want %d", strings.Split(path, "?")[0], resp.StatusCode, status)
	}
	return resp.Header, b
}

type terminalIO struct {
	io.Reader
	io.Writer
}

func (p *httpProcess) pair(t *testing.T) string {
	t.Helper()
	var display bytes.Buffer
	if err := owneroauth.Terminal(t.Context(), p.dir, "pair", &display); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(strings.TrimSpace(display.String()))
	if err != nil || link.Fragment == "" {
		t.Fatal("missing owner handoff")
	}
	const form = "application/x-www-form-urlencoded"
	p.request(t, "GET", link.Path, "", "", 200)
	h, _ := p.request(t, "POST", link.Path, form, url.Values{"handoff": {link.Fragment}}.Encode(), 200)
	cookies := (&http.Response{Header: h}).Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite == http.SameSiteDefaultMode {
		t.Fatal("insecure owner cookie")
	}

	_, b := p.request(t, "POST", "/register", "application/json", `{"client_name":"Client\u001b\n.test","redirect_uris":["https://client.test/callback?tag=fixture"],"token_endpoint_auth_method":"none"}`, 201)
	var dcr struct {
		ID string `json:"client_id"`
	}
	if json.Unmarshal(b, &dcr) != nil || dcr.ID == "" {
		t.Fatal("registration response")
	}
	const verifier = "verifier.test-000000000000000000000000000000000000000000"
	params := url.Values{"client_id": {dcr.ID}, "redirect_uri": {"https://client.test/callback?tag=fixture"}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "resource": {p.origin + "/mcp"}}
	h, _ = p.request(t, "GET", "/authorize?"+params.Encode(), "", "", 303)
	_, b = p.request(t, "GET", h.Get("Location"), "", "", 200)
	match := regexp.MustCompile(`<strong>([A-Z0-9_-]+)</strong>`).FindSubmatch(b)
	if match == nil {
		t.Fatal("missing comparison code")
	}
	display.Reset()
	if err := owneroauth.Terminal(t.Context(), p.dir, "approve", terminalIO{strings.NewReader(string(match[1]) + "\n"), &display}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{params.Get("redirect_uri"), params.Get("resource"), params.Get("client_id"), params.Get("code_challenge"), `Client\x1b\n.test`, string(match[1])} {
		if !strings.Contains(display.String(), want) {
			t.Fatal("incomplete terminal review")
		}
	}
	h, _ = p.request(t, "POST", "/owner/finish", form, "", 303)
	redirect, err := url.Parse(h.Get("Location"))
	if err != nil || redirect.Host != "client.test" || redirect.Query().Get("iss") != p.origin {
		t.Fatal("wrong callback")
	}
	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {dcr.ID}, "redirect_uri": {params.Get("redirect_uri")}, "resource": {params.Get("resource")}, "code_verifier": {verifier}, "code": {redirect.Query().Get("code")}}
	_, b = p.request(t, "POST", "/token", form, exchange.Encode(), 200)
	var token struct {
		Access   string `json:"access_token"`
		Lifetime int    `json:"expires_in"`
	}
	if json.Unmarshal(b, &token) != nil || token.Access == "" || token.Lifetime != 600 {
		t.Fatal("access response")
	}
	return token.Access
}

func (p *httpProcess) session(t *testing.T, token, version string) *mcp.ClientSession {
	t.Helper()
	httpClient := &http.Client{Transport: &oauth2.Transport{Base: p.client.Transport, Source: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})}}
	client := mcp.NewClient(&mcp.Implementation{Name: "client.test", Version: "1"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: p.origin + "/mcp", HTTPClient: httpClient, DisableStandaloneSSE: true}, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal("MCP connection failed", err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestHTTPTransportPairedReads(t *testing.T) {
	server := fixtureBridge(t)
	p := startHTTP(t, writeServerConfig(t, server))
	token := p.pair(t)
	session := p.session(t, token, "2026-07-28")
	listed, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range listed.Tools {
		if !tool.Annotations.ReadOnlyHint {
			t.Fatal("write-capable catalog")
		}
		names = append(names, tool.Name)
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"get_message", "get_thread", "list_attachments", "list_folders", "search_mail", "select_digest_candidates"}) {
		t.Fatal("wrong HTTP catalog")
	}
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "INBOX") {
		t.Fatal("folder call failed", err)
	}
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatal(err)
	}
	p.stop(t)
	if strings.Contains(p.stderr.String(), token) || strings.Contains(p.stderr.String(), "INBOX") {
		t.Fatal("unsafe HTTP diagnostics")
	}
}

// A stalled synthetic credential helper holds the real adapter gate. Its
// marker lets the test place a second HTTP call behind a known active call.
func TestHTTPTransportBridgeQueue(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(fmt.Sprintf("shutdown=%t", shutdown), func(t *testing.T) {
			config := writeServerConfig(t, fixtureBridge(t))
			marker := filepath.Join(canonicalTempDir(t), "active")
			budget := 2 * time.Second
			if shutdown {
				budget = 30 * time.Second
			}
			editConfig(t, config, func(cfg map[string]any) {
				imap := cfg["imap"].(map[string]any)
				imap["commandTimeoutMs"] = budget.Milliseconds()
				imap["credentialCommand"] = []string{"/bin/sh", "-c", `echo started >> "$1"; exec /bin/sleep 45`, "fixture.test", marker}
			})
			p := startHTTP(t, config)
			version := "2025-11-25"
			if shutdown {
				version = "2026-07-28"
			}
			session := p.session(t, p.pair(t), version)
			results := make(chan bool, 2)
			call := func() {
				result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
				results <- err != nil || result.IsError
			}
			go call()
			until := time.Now().Add(3 * time.Second)
			for {
				if _, err := os.Stat(marker); err == nil {
					break
				}
				if time.Now().After(until) {
					t.Fatal("active request did not reach Bridge gate")
				}
				time.Sleep(5 * time.Millisecond)
			}
			go call()
			if shutdown {
				// Establish an idle keep-alive independently of the active calls.
				conn, err := tls.Dial("tcp", strings.TrimPrefix(p.origin, "https://"), p.client.Transport.(*http.Transport).TLSClientConfig)
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				req, err := http.NewRequest("GET", p.origin+"/.well-known/oauth-authorization-server", nil)
				if err != nil || req.Write(conn) != nil {
					t.Fatal("idle request")
				}
				reader := bufio.NewReader(conn)
				response, err := http.ReadResponse(reader, req)
				if err != nil {
					t.Fatal(err)
				}
				io.Copy(io.Discard, response.Body)
				response.Body.Close()
				p.stop(t)
				conn.SetReadDeadline(time.Now().Add(time.Second))
				if _, err := reader.ReadByte(); err != io.EOF {
					t.Fatalf("idle connection not closed: %v", err)
				}
			}
			// A fresh timeout after acquiring the gate would take two budgets.
			timer := time.NewTimer(3 * time.Second)
			defer timer.Stop()
			for range 2 {
				select {
				case failed := <-results:
					if !failed {
						t.Fatal("stalled request unexpectedly succeeded")
					}
				case <-timer.C:
					t.Fatal("queued work outlived its request budget")
				}
			}
			if !shutdown {
				p.stop(t)
			}
		})
	}
}

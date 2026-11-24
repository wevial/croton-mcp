//go:build linux || darwin

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
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
	"golang.org/x/oauth2"
)

const (
	clientForm     = "application/x-www-form-urlencoded"
	clientRedirect = "https://client.test/callback"
	clientVerifier = "verifier.test-000000000000000000000000000000000000000000"
)

type approvedClient struct {
	ID      string `json:"client_id"`
	Access  string `json:"access_token"`
	Refresh string `json:"refresh_token"`
	finish  url.Values
}

func approveClient(t *testing.T, p *httpProcess) approvedClient {
	t.Helper()

	field := func(body []byte, pattern string) string {
		t.Helper()
		match := regexp.MustCompile(pattern).FindSubmatch(body)
		if match == nil {
			t.Fatal("pairing page missing expected field")
		}
		return string(match[1])
	}

	handoff, err := adminCommand(t, p.dir, "pair")
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(strings.TrimSpace(handoff))
	if err != nil {
		t.Fatal("invalid pairing link")
	}
	_, body := p.request(t, "POST", link.Path, clientForm, url.Values{"handoff": {link.Fragment}}.Encode(), 200)
	owner := field(body, `data-owner="([^"]+)"`)

	_, body = p.request(t, "POST", "/register", "application/json",
		`{"client_name":"Client\u001b\n.test","redirect_uris":["https://client.test/callback"],"grant_types":["authorization_code","refresh_token"]}`, 201)
	var client approvedClient
	if json.Unmarshal(body, &client) != nil || client.ID == "" {
		t.Fatal("missing registered client ID")
	}

	query := url.Values{
		"client_id": {client.ID}, "redirect_uri": {clientRedirect}, "response_type": {"code"},
		"code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(clientVerifier)},
		"resource": {p.origin + "/mcp"},
	}
	_, body = p.request(t, "GET", "/authorize?"+query.Encode(), "", "", 200)
	client.finish = url.Values{"owner": {owner}, "request": {field(body, `name="request" value="([^"]+)"`)}}
	_, body = p.request(t, "POST", "/owner/signin", clientForm, client.finish.Encode(), 200)
	code := field(body, `<strong>([^<]+)</strong>`)

	if err := runOwnerCommand(t.Context(), "approve", []string{"--state-dir", p.dir}, terminalIO{strings.NewReader(code + "\n"), io.Discard}); err != nil {
		t.Fatal(err)
	}
	return client
}

func (client approvedClient) exchangeForm(t *testing.T, p *httpProcess) url.Values {
	t.Helper()

	h, _ := p.request(t, "POST", "/owner/finish", clientForm, client.finish.Encode(), 303)
	callback, err := url.Parse(h.Get("Location"))
	if err != nil || callback.Query().Get("code") == "" {
		t.Fatal("missing authorization code")
	}
	return url.Values{
		"grant_type": {"authorization_code"}, "code": {callback.Query().Get("code")},
		"client_id": {client.ID}, "redirect_uri": {clientRedirect}, "code_verifier": {clientVerifier}, "resource": {p.origin + "/mcp"},
	}
}

func (client *approvedClient) tokens(t *testing.T, p *httpProcess, form url.Values) {
	t.Helper()
	_, body := p.request(t, "POST", "/token", clientForm, form.Encode(), 200)
	client.Access, client.Refresh = "", ""
	if json.Unmarshal(body, client) != nil || client.Access == "" || client.Refresh == "" {
		t.Fatal("missing token pair")
	}
}

func (client approvedClient) refreshForm() url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {client.Refresh}}
}

func (client approvedClient) read(t *testing.T, p *httpProcess) {
	t.Helper()

	session := p.session(t, client.Access, "2026-07-28")
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
	if err != nil || result.IsError || !strings.Contains(result.Content[0].(*mcp.TextContent).Text, "INBOX") {
		t.Fatal("paired client could not read folders")
	}
}

func (client approvedClient) bearerDenied(t *testing.T, p *httpProcess) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), "POST", p.origin+"/mcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+client.Access)
	resp, err := p.client.Do(req)
	if err != nil {
		t.Fatal("bearer request failed")
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked bearer returned HTTP %d", resp.StatusCode)
	}
}

func (client approvedClient) denied(t *testing.T, p *httpProcess) {
	t.Helper()
	client.bearerDenied(t, p)
	_, body := p.request(t, "POST", "/token", clientForm, client.refreshForm().Encode(), 400)
	if strings.TrimSpace(string(body)) != `{"error":"invalid_grant"}` {
		t.Fatal("revoked refresh grant was not denied")
	}
}

func adminCommand(t *testing.T, dir, command string, args ...string) (string, error) {
	t.Helper()
	var tty bytes.Buffer
	err := runOwnerCommand(t.Context(), command, append([]string{"--state-dir", dir}, args...), &tty)
	return tty.String(), err
}

func checkClients(t *testing.T, dir string, clients ...approvedClient) {
	t.Helper()

	var rows []string
	for _, client := range clients {
		rows = append(rows, client.ID+"\t"+`"Client\x1b\n.test"`+"\n")
	}
	slices.Sort(rows)
	if got, err := adminCommand(t, dir, "clients"); err != nil || got != strings.Join(rows, "") {
		t.Fatal("listing did not contain exactly the expected IDs and escaped names", err)
	}
}

func restartClientsServer(t *testing.T, p *httpProcess) {
	t.Helper()

	ctx := t.Context()
	done := make(chan error, 1)
	go func() { done <- run(ctx, p.cmd.Args[1:], io.Discard, owneroauth.Defaults()) }()
	t.Cleanup(func() {
		if err := <-done; err != nil {
			t.Error(err)
		}
	})

	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		if resp, err := p.client.Get(p.origin + "/.well-known/oauth-authorization-server"); err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("server did not restart")
}

func TestOwnerOAuthClients(t *testing.T) {
	for _, mode := range []string{"online", "offline"} {
		t.Run(mode, func(t *testing.T) {
			p := startHTTP(t, writeServerConfig(t, fixtureBridge(t)), nil)
			first := approveClient(t, p)
			first.tokens(t, p, first.exchangeForm(t, p))
			second := approveClient(t, p)
			second.tokens(t, p, second.exchangeForm(t, p))

			checkClients(t, p.dir, first, second)
			if mode == "offline" {
				p.stop(t)
			}
			if got, err := adminCommand(t, p.dir, "unpair", strings.Repeat("A", 43)); err == nil || err.Error() != "unknown client ID" || got != "" {
				t.Fatal("unknown ID accepted or revealed state")
			}
			checkClients(t, p.dir, first, second)

			t.Run("unknown ID preserves both grants", func(t *testing.T) {
				if mode == "offline" {
					restartClientsServer(t, p)
				}

				for _, client := range []*approvedClient{&first, &second} {
					client.read(t, p)
					client.tokens(t, p, client.refreshForm())
				}
			})

			if _, err := adminCommand(t, p.dir, "unpair", first.ID); err != nil {
				t.Fatal(err)
			}
			checkClients(t, p.dir, second)
			if mode == "online" {
				first.denied(t, p)
				second.read(t, p)
				p.stop(t)
			}

			restartClientsServer(t, p)
			checkClients(t, p.dir, second)
			first.denied(t, p)
			second.read(t, p)
			second.tokens(t, p, second.refreshForm())
		})
	}
}

func TestOwnerOAuthClientsPending(t *testing.T) {
	p := startHTTP(t, writeServerConfig(t, fixtureBridge(t)), nil)
	for _, stage := range []string{"delivery", "code"} {
		client := approveClient(t, p)
		var form url.Values
		if stage == "code" {
			form = client.exchangeForm(t, p)
		}

		if _, err := adminCommand(t, p.dir, "unpair", client.ID); err != nil {
			t.Fatal(err)
		}
		if stage == "delivery" {
			p.request(t, "POST", "/owner/finish", clientForm, client.finish.Encode(), 403)
		} else {
			p.request(t, "POST", "/token", clientForm, form.Encode(), 400)
		}
	}
}

func TestOwnerOAuthClientsAccessOnly(t *testing.T) {
	p := startHTTP(t, writeServerConfig(t, fixtureBridge(t)), nil)
	client := approvedClient{Access: p.pair(t)}
	listing, err := adminCommand(t, p.dir, "clients")
	if err != nil {
		t.Fatal(err)
	}
	id, _, ok := strings.Cut(listing, "\t")
	if !ok {
		t.Fatal("access-only client missing from listing")
	}

	if err := runOwnerCommand(t.Context(), "unpair", []string{id, "--state-dir", p.dir}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	client.bearerDenied(t, p)
}

func TestOwnerOAuthClientsLocked(t *testing.T) {
	p := startHTTP(t, writeServerConfig(t, fixtureBridge(t)), nil)
	client := approveClient(t, p)
	client.tokens(t, p, client.exchangeForm(t, p))

	if err := os.Remove(filepath.Join(p.dir, "owner.sock")); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"clients"}, {"unpair", client.ID}} {
		start := time.Now()
		got, err := adminCommand(t, p.dir, args[0], args[1:]...)
		if err == nil || err.Error() != "owner state unavailable" || got != "" || time.Since(start) > 2*time.Second {
			t.Fatal("locked state did not fail closed within the open timeout")
		}
	}
	client.read(t, p)
}

func TestOwnerOAuthClientsWrongUID(t *testing.T) {
	const (
		helper        = "CROTON_TEST_FOREIGN_LISTENER"
		foreignUID    = 65534
		actualSocket  = "actual.sock"
		foreignSocket = "foreign.sock"
	)
	requireOK := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	if os.Getenv(helper) == "1" {
		// Drop privileges only in this short-lived test subprocess.
		requireOK(syscall.Setuid(foreignUID))

		// The shared path reaches the peer check outside the private state directory.
		server, err := net.DialTimeout("unix", actualSocket, time.Second)
		requireOK(err)
		server.SetDeadline(time.Now().Add(2 * time.Second))
		_, _ = io.WriteString(server, "clients\n")
		response, err := io.ReadAll(server)
		server.Close()
		if len(response) != 0 || (err != nil && !errors.Is(err, syscall.ECONNRESET)) {
			t.Fatal("owner server did not reject the foreign client")
		}

		listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: foreignSocket})
		requireOK(err)
		defer listener.Close()
		_, err = os.Stdout.Write([]byte{1})
		requireOK(err)
		listener.SetDeadline(time.Now().Add(5 * time.Second))
		conn, err := listener.AcceptUnix()
		requireOK(err)
		defer conn.Close()
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var b [1]byte
		if n, err := conn.Read(b[:]); n != 0 || err != io.EOF {
			t.Fatal("owner command sent data to a foreign peer")
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("creating a socket under a second UID requires root")
	}

	dir := httpStateDir(t)
	a, err := owneroauth.Open(dir, "https://127.0.0.1:8787", owneroauth.Defaults())
	requireOK(err)
	defer a.Close()

	stop, err := a.ListenOwner(t.Context())
	requireOK(err)
	defer stop()

	staging := httpStateDir(t)
	requireOK(os.Link(filepath.Join(dir, "owner.sock"), filepath.Join(staging, actualSocket)))
	requireOK(os.Chmod(filepath.Join(staging, actualSocket), 0777))
	requireOK(os.Chown(staging, foreignUID, -1))

	bin, err := os.Executable()
	requireOK(err)
	cmd := exec.CommandContext(t.Context(), bin, "-test.run=^TestOwnerOAuthClientsWrongUID$")
	cmd.Dir = staging
	cmd.Env = append(os.Environ(), helper+"=1")
	ready, err := cmd.StdoutPipe()
	requireOK(err)
	requireOK(cmd.Start())
	defer cmd.Process.Kill()
	var b [1]byte
	if _, err := io.ReadFull(ready, b[:]); err != nil || b[0] != 1 {
		t.Fatal("foreign listener unavailable")
	}
	stop()
	requireOK(a.Close())
	requireOK(os.Rename(filepath.Join(staging, foreignSocket), filepath.Join(dir, "owner.sock")))

	// The database is unlocked. A rejected peer must never trigger an offline retry.
	if got, err := adminCommand(t, dir, "clients"); err == nil || err.Error() != "pairing request denied" || got != "" {
		t.Fatal("clients did not return a redacted denial for the wrong-UID peer")
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal("foreign listener received command data")
	}
}

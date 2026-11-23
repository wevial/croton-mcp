//go:build linux || darwin

package owneroauth

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

const issuer = "https://127.0.0.1:8787"
const nativeRedirect = "http://127.0.0.1/callback?source=test"
const verifier = "synthetic.test-verifier-0123456789abcdef0123456789abcdef"

func privateDir(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(parent, "state")
}

func openTest(t *testing.T, dir string, settings Settings) *Authority {
	t.Helper()
	a, err := Open(dir, issuer, settings)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

func authorizationQuery(client, redirect string) url.Values {
	sum := sha256.Sum256([]byte(verifier))
	return url.Values{"client_id": {client}, "redirect_uri": {redirect}, "response_type": {"code"}, "resource": {issuer + "/mcp"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"}, "state": {"state.test"}}
}

func TestOwnerOAuthCodeWindow(t *testing.T) {
	settings := Defaults()
	now := time.Now()
	settings.Now = func() time.Time { return now }
	a := openTest(t, privateDir(t), settings)
	c := registration{Redirects: []string{nativeRedirect}}
	if _, err := a.register(c); err == nil {
		t.Fatal("closed registration")
	}
	if _, err := a.signin("", "", ""); err == nil {
		t.Fatal("closed sign-in")
	}
	link, _ := url.Parse(a.start())
	for range 8 {
		if _, err := a.register(c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := a.register(c); err == nil {
		t.Fatal("ninth registration")
	}
	now = now.Add(2 * time.Minute)
	if _, err := a.claim(link.Fragment); err == nil {
		t.Fatal("expired handoff")
	}
	now = now.Add(time.Minute)
	if _, err := a.register(c); err == nil {
		t.Fatal("expired window")
	}
	if _, err := a.signin("", "", ""); err == nil {
		t.Fatal("expired sign-in")
	}
	if len(a.window.clients) != 0 {
		t.Fatal("retained expired registrations")
	}

	for _, p := range []string{"/.well-known/oauth-authorization-server", ResourceMetadataPath} {
		w := httptest.NewRecorder()
		a.ServeHTTP(w, httptest.NewRequest("GET", issuer+p, nil))
		var discovery struct {
			Issuer         string
			DCR            string   `json:"registration_endpoint"`
			S256           []string `json:"code_challenge_methods_supported"`
			ResponseIssuer bool     `json:"authorization_response_iss_parameter_supported"`
			Resource       string
			Servers        []string `json:"authorization_servers"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &discovery); err != nil {
			t.Fatal(err)
		}
		if p == ResourceMetadataPath {
			if discovery.Resource != issuer+"/mcp" || len(discovery.Servers) != 1 || discovery.Servers[0] != issuer {
				t.Fatal("resource metadata")
			}
		} else if discovery.Issuer != issuer || discovery.DCR != issuer+"/register" || len(discovery.S256) != 1 || discovery.S256[0] != "S256" || !discovery.ResponseIssuer {
			t.Fatal("authorization metadata")
		}
	}
}

func TestOwnerOAuthCodeRedirectAndPKCE(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	for _, bad := range []string{"com.example:/cb", "http://example.test/cb", "http://localhost/cb", "https://example.test/cb#fragment", "https://user@example.test/cb", "file:///cb"} {
		a.start()
		if _, err := a.register(registration{Redirects: []string{bad}}); err == nil {
			t.Fatal("accepted unsafe redirect")
		}
	}
	for _, test := range []struct {
		registered, requested, app string
		ok                         bool
	}{
		{"https://example.test/cb", "https://example.test/cb", "web", true},
		{"https://example.test/cb", "https://example.test:8443/cb", "native", false},
		{nativeRedirect, "http://127.0.0.1:9090/callback?source=test", "native", true},
		{nativeRedirect, "http://127.0.0.1:9090/callback?source=test", "web", false},
		{nativeRedirect, "http://127.0.0.1:9090/other?source=test", "native", false},
		{nativeRedirect, "HTTP://127.0.0.1:9090/callback?source=test", "native", false},
		{nativeRedirect, "http://127.0.0.1:9090/callback?source=else", "native", false},
		{"http://[::1]/cb", "http://[::1]:9000/cb", "native", true},
	} {
		a.start()
		id, err := a.register(registration{Application: test.app, Redirects: []string{test.registered}})
		if err != nil {
			t.Fatal(err)
		}
		q := authorizationQuery(id, test.requested)
		for _, bad := range []string{"plain", "", "s256"} {
			q.Set("code_challenge_method", bad)
			if _, err := a.authorize(q); err == nil {
				t.Fatal("non-S256 accepted")
			}
		}
		q.Set("code_challenge_method", "S256")
		if _, err := a.authorize(q); (err == nil) != test.ok {
			t.Fatalf("redirect mismatch for %s to %s", test.registered, test.requested)
		}
	}
}

func approvedGrant(t *testing.T, a *Authority) (string, *authorization) {
	t.Helper()
	link, _ := url.Parse(a.start())
	owner, err := a.claim(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.claim(link.Fragment); err == nil {
		t.Fatal("replayed handoff")
	}
	id, err := a.register(registration{Name: "App\x1b[2J\n.test", Redirects: []string{nativeRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	q := authorizationQuery(id, "http://127.0.0.1:9876/callback?source=test")
	req, err := a.authorize(q)
	if err != nil {
		t.Fatal(err)
	}
	q.Set("code_challenge", strings.Repeat("A", 43))
	if _, err := a.authorize(q); err == nil {
		t.Fatal("replaced pending request")
	}

	if _, err := a.signin("", req.browser, req.requestID); err == nil {
		t.Fatal("foreign browser")
	}
	if _, err := a.signin(owner, "other-browser", req.requestID); err == nil {
		t.Fatal("relayed sign-in")
	}
	if err := a.approve(req.requestID, req.comparison); err == nil {
		t.Fatal("approved without sign-in")
	}
	details, err := a.signin(owner, req.browser, req.requestID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := a.register(registration{Redirects: []string{nativeRedirect}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.authorize(authorizationQuery(other, nativeRedirect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.signin(owner, second.browser, second.requestID); err == nil {
		t.Fatal("replaced terminal review with a second client")
	}
	if err := a.approve(second.requestID, details.Code); err == nil {
		t.Fatal("approved an unreviewed request")
	}
	if err := a.approve(req.requestID, "incorrect"); err == nil {
		t.Fatal("wrong comparison")
	}
	if err := a.approve(req.requestID, details.Code); err != nil {
		t.Fatal(err)
	}
	if _, err := a.register(registration{Redirects: []string{nativeRedirect}}); err == nil {
		t.Fatal("window left open")
	}
	if err := a.approve(req.requestID, details.Code); err == nil {
		t.Fatal("second approval")
	}
	if _, err := a.finish(owner, "other"); err == nil {
		t.Fatal("foreign delivery")
	}
	callback, err := a.finish(owner, req.browser)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(callback)
	if err != nil || u.Query().Get("iss") != issuer || u.Query().Get("state") != "state.test" || u.Query().Get("source") != "test" || u.Query().Get("code") == "" {
		t.Fatal("invalid callback")
	}
	if _, err := a.finish(owner, req.browser); err == nil {
		t.Fatal("replayed delivery")
	}
	return u.Query().Get("code"), req
}

func TestOwnerOAuthCodeRedemption(t *testing.T) {
	dir := privateDir(t)
	settings := Defaults()
	now := time.Now()
	settings.Now = func() time.Time { return now }
	a := openTest(t, dir, settings)
	code, req := approvedGrant(t, a)
	now = now.Add(60 * time.Second)
	if _, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
		t.Fatal("expired code")
	}

	code, req = approvedGrant(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTest(t, dir, settings)
	for _, bad := range [][]string{{"other", req.Redirect, req.Resource, verifier}, {req.Client, nativeRedirect, req.Resource, verifier}, {req.Client, req.Redirect, issuer, verifier}, {req.Client, req.Redirect, req.Resource, strings.Repeat("x", 43)}} {
		if _, err := a.redeem(code, bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Fatal("redeemed unbound request")
		}
	}
	now = now.Add(59 * time.Second)
	var wg sync.WaitGroup
	results := make(chan string, 12)
	for range cap(results) {
		wg.Go(func() {
			if token, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
				results <- token
			}
		})
	}
	wg.Wait()
	if len(results) != 1 {
		t.Fatalf("successful redemptions = %d", len(results))
	}
	token := <-results
	a.Close()
	a = openTest(t, dir, settings)
	now = now.Add(10*time.Minute - time.Second)
	if !a.ValidBearer(token) {
		t.Fatal("lost grant after restart")
	}
	if _, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
		t.Fatal("replayed after restart")
	}
	now = now.Add(time.Second)
	if a.ValidBearer(token) {
		t.Fatal("expired access")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "oauth.db"))
	if err != nil || bytes.Contains(raw, []byte(code)) || bytes.Contains(raw, []byte(token)) {
		t.Fatal("token storage")
	}
}

func TestOwnerOAuthCodeConfiguredLifetimes(t *testing.T) {
	settings := Defaults()
	settings.PairingWindow = 12 * time.Second
	settings.HandoffLifetime = 5 * time.Second
	settings.PendingLimit = 2
	settings.CodeLifetime = 3 * time.Second
	settings.AccessLifetime = 4 * time.Second
	now := time.Now()
	settings.Now = func() time.Time { return now }
	a := openTest(t, privateDir(t), settings)

	link, _ := url.Parse(a.start())
	now = now.Add(4 * time.Second)
	owner, err := a.claim(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	c := registration{Redirects: []string{nativeRedirect}}
	client, err := a.register(c)
	if err != nil {
		t.Fatal(err)
	}
	req, err := a.authorize(authorizationQuery(client, nativeRedirect))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.register(c); err != nil {
		t.Fatal(err)
	}
	if _, err := a.register(c); err == nil {
		t.Fatal("configured registration limit")
	}
	now = now.Add(8 * time.Second)
	if _, err := a.signin(owner, req.browser, req.requestID); err == nil {
		t.Fatal("expired sign-in with both browser bindings")
	}

	link, _ = url.Parse(a.start())
	now = now.Add(5 * time.Second)
	if _, err := a.claim(link.Fragment); err == nil {
		t.Fatal("configured handoff expiry")
	}
	if _, err := a.authorize(authorizationQuery(client, nativeRedirect)); err == nil {
		t.Fatal("unapproved client carried into next window")
	}

	code, req := approvedGrant(t, a)
	now = now.Add(3 * time.Second)
	if _, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
		t.Fatal("configured code expiry")
	}
	code, req = approvedGrant(t, a)
	token, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Second)
	if a.ValidBearer(token) {
		t.Fatal("configured access expiry")
	}
}

func TestOwnerOAuthCodeStateAndPeer(t *testing.T) {
	dir := privateDir(t)
	a := openTest(t, dir, Defaults())
	a.Close()
	for path, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "oauth.db"): 0600} {
		st, err := os.Lstat(path)
		if err != nil || st.Mode().Perm() != want {
			t.Fatal("unsafe creation mode")
		}
	}
	link := dir + "-link"
	if err := os.Symlink(dir, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, issuer, Defaults()); err == nil {
		t.Fatal("followed directory link")
	}
	db := filepath.Join(dir, "oauth.db")
	if err := os.Chmod(db, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, issuer, Defaults()); err == nil {
		t.Fatal("opened exposed db")
	}
	if err := os.Rename(db, db+".test"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(db+".test", db); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, issuer, Defaults()); err == nil {
		t.Fatal("followed db link")
	}

	// The actual kernel predicate used at both IPC endpoints rejects a different uid.
	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fds[0])
	defer unix.Close(fds[1])
	if !ownerPeer(fds[0], os.Geteuid()) || ownerPeer(fds[0], os.Geteuid()+1) {
		t.Fatal("peer UID check")
	}
}

//go:build linux || darwin

package owneroauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	bolt "go.etcd.io/bbolt"
	"golang.org/x/oauth2"
	"golang.org/x/sys/unix"
)

const issuer = "https://127.0.0.1:8787"
const nativeRedirect = "http://127.0.0.1/callback?source=test"
const verifier = "synthetic.test-verifier-0123456789abcdef0123456789abcdef"

func (a *Authority) ValidBearer(token string) bool {
	_, ok := a.BearerClient(token)
	return ok
}

func privateDir(t *testing.T) string {
	t.Helper()

	// The state needs trusted ancestors and a short Unix socket path.
	root, err := filepath.EvalSymlinks("/tmp")
	if err != nil {
		t.Fatal(err)
	}

	parent, err := os.MkdirTemp(root, "croton-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(parent); err != nil {
			t.Error(err)
		}
	})

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

func reopenTest(t *testing.T, a *Authority, dir string, settings Settings) *Authority {
	t.Helper()
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	return openTest(t, dir, settings)
}

func startWindow(t *testing.T, a *Authority) *url.URL {
	t.Helper()

	handoff, err := a.start()
	if err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(handoff)
	if err != nil {
		t.Fatal(err)
	}

	return link
}

func registerClient(t *testing.T, a *Authority, c registration) string {
	t.Helper()

	id, err := a.register(c)
	if err != nil {
		t.Fatal(err)
	}

	return id
}

func authorizationQuery(client, redirect string) url.Values {
	return url.Values{"client_id": {client}, "redirect_uri": {redirect}, "response_type": {"code"}, "resource": {issuer + "/mcp"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(verifier)}, "code_challenge_method": {"S256"}, "state": {"state.test"}}
}

func codeForm(code string, req *authorization) url.Values {
	return url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"client_id":     {req.Client},
		"redirect_uri":  {req.Redirect},
		"resource":      {req.Resource},
		"code_verifier": {verifier},
	}
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
	link := startWindow(t, a)
	for range 8 {
		registerClient(t, a, c)
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

func TestOwnerOAuthCodeRegistrationMetadata(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	startWindow(t, a)

	const prefix = `{"redirect_uris":["https://client.test/callback"],`
	for _, tc := range []struct {
		name, metadata string
		status         int
	}{
		{"optional metadata", `"client_uri":"https://client.test","software_id":"app.test","software_version":"1","extension":{"tags":["test"]}`, http.StatusCreated},
		{"null in extension", `"example_extension":{"label":null}`, http.StatusCreated},
		{"duplicate in extension", `"extension":{"tag":1,"tag":2}`, http.StatusBadRequest},
		{"case alias", `"client_name":"one","CLIENT_NAME":"two"`, http.StatusBadRequest},
		{"wrong known type", `"client_name":42`, http.StatusBadRequest},
		{"null client name", `"client_name":null`, http.StatusBadRequest},
		{"null grant types", `"grant_types":null`, http.StatusBadRequest},
		{"oversize extension", `"extension":"` + strings.Repeat("x", 64<<10) + `"`, http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, issuer+"/register", strings.NewReader(prefix+tc.metadata+"}"))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			a.ServeHTTP(w, req)

			if w.Code != tc.status {
				t.Fatalf("registration status = %d, want %d", w.Code, tc.status)
			}
		})
	}
}

func TestOwnerOAuthCodeRedirectAndPKCE(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	for _, bad := range []string{"com.example:/cb", "http://example.test/cb", "http://localhost/cb", "https://example.test/cb#fragment", "https://user@example.test/cb", "file:///cb", "https://example.test/cb?source=%ZZ"} {
		startWindow(t, a)
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
		startWindow(t, a)
		id := registerClient(t, a, registration{Application: test.app, Redirects: []string{test.registered}})
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

func approveRequest(t *testing.T, a *Authority, grants ...string) (string, *authorization) {
	t.Helper()
	link := startWindow(t, a)
	owner, err := a.claim(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.claim(link.Fragment); err == nil {
		t.Fatal("replayed handoff")
	}
	id := registerClient(t, a, registration{Name: "App\x1b[2J\n.test", Redirects: []string{nativeRedirect}, Grants: grants})
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
	other := registerClient(t, a, registration{Redirects: []string{nativeRedirect}})
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

	return owner, req
}

func approvedGrant(t *testing.T, a *Authority, grants ...string) (string, *authorization) {
	t.Helper()

	owner, req := approveRequest(t, a, grants...)
	return collectGrant(t, a, owner, req), req
}

func collectGrant(t *testing.T, a *Authority, owner string, req *authorization) string {
	t.Helper()
	if _, err := a.start(); err != errDenied {
		t.Fatal("opened a window while callback awaited delivery")
	}
	if _, err := a.finish("", req.browser); err == nil {
		t.Fatal("delivered callback with only the request cookie")
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
	return u.Query().Get("code")
}

func TestOwnerOAuthCodeRedemption(t *testing.T) {
	dir := privateDir(t)
	settings := Defaults()
	now := time.Now()
	settings.Now = func() time.Time { return now }
	a := openTest(t, dir, settings)
	owner, req := approveRequest(t, a)
	now = now.Add(59 * time.Second)
	code := collectGrant(t, a, owner, req)
	now = now.Add(time.Second)
	if _, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
		t.Fatal("expired code")
	}

	owner, req = approveRequest(t, a)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTest(t, dir, settings)
	now = now.Add(59 * time.Second)
	code = collectGrant(t, a, owner, req)
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTest(t, dir, settings)
	for _, bad := range [][]string{{"other", req.Redirect, req.Resource, verifier}, {req.Client, nativeRedirect, req.Resource, verifier}, {req.Client, req.Redirect, issuer, verifier}, {req.Client, req.Redirect, req.Resource, strings.Repeat("x", 43)}} {
		if _, err := a.redeem(code, bad[0], bad[1], bad[2], bad[3]); err == nil {
			t.Fatal("redeemed unbound request")
		}
	}
	var wg sync.WaitGroup
	results := make(chan string, 12)
	for range cap(results) {
		wg.Go(func() {
			if token, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err == nil {
				results <- token.Access
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
	if _, err := a.redeem(code, req.Client, req.Redirect, req.Resource, verifier); err != errDenied {
		t.Fatal("replayed after restart")
	}
	now = now.Add(10*time.Minute - time.Second)
	if client, ok := a.BearerClient(token); !ok || client != req.Client {
		t.Fatal("lost client grant after restart")
	}
	now = now.Add(time.Second)
	if a.ValidBearer(token) {
		t.Fatal("expired access")
	}
	raw, err := os.ReadFile(filepath.Join(dir, "oauth.db"))
	if err != nil || bytes.Contains(raw, []byte(code)) || bytes.Contains(raw, []byte(token)) || bytes.Contains(raw, []byte(owner)) || bytes.Contains(raw, []byte(req.browser)) {
		t.Fatal("token storage")
	}
}

func TestOwnerOAuthCodeBrowserBinding(t *testing.T) {
	dir := privateDir(t)
	a := openTest(t, dir, Defaults())
	stopOwner, err := a.ListenOwner(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer stopOwner()

	jar, _ := cookiejar.New(nil)
	// A cookie jar exposes matching cookies at this port as well.
	sibling, _ := url.Parse("https://127.0.0.1:9999/owner/")
	request := func(method, path string, form url.Values, status int) *httptest.ResponseRecorder {
		t.Helper()
		r := httptest.NewRequest(method, issuer+path, strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", issuer)
		for _, c := range jar.Cookies(sibling) {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		if w.Code != status {
			t.Fatalf("%s %s returned %d, want %d", method, r.URL.Path, w.Code, status)
		}

		jar.SetCookies(r.URL, w.Result().Cookies())
		return w
	}

	link := startWindow(t, a)
	paired := request("POST", link.Path, url.Values{"handoff": {link.Fragment}}, 200)
	proof := regexp.MustCompile(`data-owner="([A-Za-z0-9_-]+)"`).FindStringSubmatch(paired.Body.String())
	if proof == nil {
		t.Fatal("missing origin-scoped proof")
	}

	// Exercise terminal review at the redirect and client name limits. Each
	// ampersand expands to six bytes in the JSON reply on the owner socket.
	redirect := nativeRedirect + strings.Repeat("&", 2048-len(nativeRedirect))
	id := registerClient(t, a, registration{Name: strings.Repeat("&", 256), Redirects: []string{redirect}})
	request("GET", "/authorize?"+authorizationQuery(id, redirect).Encode(), nil, 200)
	shared := jar.Cookies(sibling)
	if len(shared) == 0 {
		t.Fatal("fixture has no cross-port cookie")
	}
	for _, c := range shared {
		if strings.Contains(c.Value, proof[1]) {
			t.Fatal("owner proof exposed in a cross-port cookie")
		}
	}

	req := a.window.requests[id]
	form := url.Values{"request": {req.requestID}}
	request("POST", "/owner/signin", form, 403)
	if _, err := a.inspect(); err == nil {
		t.Fatal("cookie-only sign-in reached terminal review")
	}
	form.Set("owner", proof[1])
	page := request("POST", "/owner/signin", form, 200)
	if !strings.Contains(page.Body.String(), `name="request" value="`+req.requestID+`"`) {
		t.Fatal("Continue form lost the first request")
	}
	comparison := regexp.MustCompile(`<strong>([A-Z0-9_-]+)</strong>`).FindStringSubmatch(page.Body.String())
	if comparison == nil {
		t.Fatal("missing comparison code for the first request")
	}

	const otherRedirect = "https://other.test/callback"
	other := registerClient(t, a, registration{Redirects: []string{otherRedirect}})
	request("GET", "/authorize?"+authorizationQuery(other, otherRedirect).Encode(), nil, 200)
	otherForm := url.Values{"owner": {proof[1]}, "request": {a.window.requests[other].requestID}}
	request("POST", "/owner/signin", otherForm, 403)

	tty := bytes.NewBufferString(comparison[1] + "\n")
	if err := Terminal(t.Context(), dir, "approve", tty); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(tty.String(), "Full redirect: "+strconv.QuoteToASCII(redirect)+"\n") {
		t.Fatal("terminal review omitted or truncated the redirect")
	}

	request("POST", "/owner/finish", nil, 403)
	request("POST", "/owner/finish", otherForm, 403)
	response := request("POST", "/owner/finish", form, 303)
	callback, err := url.Parse(response.Header().Get("Location"))
	if err != nil || callback.Host != "127.0.0.1" || callback.Path != "/callback" {
		t.Fatal("wrong client callback")
	}
	if _, err := a.redeem(callback.Query().Get("code"), id, redirect, issuer+"/mcp", verifier); err != nil {
		t.Fatal("first client could not redeem its code")
	}
}

func TestOwnerOAuthCodeLateApprovalCookie(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		settings := Defaults()
		settings.PairingWindow = 12900 * time.Millisecond
		settings.CodeLifetime = 3900 * time.Millisecond
		a := openTest(t, privateDir(t), settings)
		link := startWindow(t, a)
		owner, err := a.claim(link.Fragment)
		if err != nil {
			t.Fatal(err)
		}

		id := registerClient(t, a, registration{Redirects: []string{nativeRedirect}})
		time.Sleep(100 * time.Millisecond)
		authorize := httptest.NewRequest("GET", issuer+"/authorize?"+authorizationQuery(id, nativeRedirect).Encode(), nil)
		w := httptest.NewRecorder()
		a.ServeHTTP(w, authorize)
		if w.Code != http.StatusOK {
			t.Fatalf("authorize returned %d", w.Code)
		}

		jar, _ := cookiejar.New(nil)
		jar.SetCookies(authorize.URL, w.Result().Cookies())
		req := a.window.requests[id]
		details, err := a.signin(owner, req.browser, req.requestID)
		if err != nil {
			t.Fatal(err)
		}

		// Approve at 12.8s, then continue at 16.3s, before delivery expires at 16.7s.
		time.Sleep(12700 * time.Millisecond)
		if err := a.approve(req.requestID, details.Code); err != nil {
			t.Fatal(err)
		}
		time.Sleep(3500 * time.Millisecond)

		form := url.Values{"owner": {owner}, "request": {req.requestID}}
		finish := httptest.NewRequest("POST", issuer+"/owner/finish", strings.NewReader(form.Encode()))
		finish.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		finish.Header.Set("Origin", issuer)
		for _, c := range jar.Cookies(finish.URL) {
			finish.AddCookie(c)
		}
		w = httptest.NewRecorder()
		a.ServeHTTP(w, finish)
		if w.Code != http.StatusSeeOther {
			t.Fatalf("Continue returned %d while delivery was still valid", w.Code)
		}
	})
}

func TestOwnerOAuthCodeTokenStorageFailure(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	code, req := approvedGrant(t, a)
	form := codeForm(code, req)

	// Make the access write fail after redeem has deleted the code in the transaction.
	if err := a.db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("access")) }); err != nil {
		t.Fatal(err)
	}
	if w := PostToken(a, form); w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"error":"server_error"}` {
		t.Fatal("token storage failure did not return a sanitized server error")
	}

	if err := a.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucket([]byte("access"))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if w := PostToken(a, form); w.Code != http.StatusOK {
		t.Fatal("could not retry rolled-back exchange")
	}
	if w := PostToken(a, form); w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != `{"error":"invalid_grant"}` {
		t.Fatal("used code did not return invalid_grant")
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

	link := startWindow(t, a)
	now = now.Add(4 * time.Second)
	owner, err := a.claim(link.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	c := registration{Redirects: []string{nativeRedirect}}
	client := registerClient(t, a, c)
	req, err := a.authorize(authorizationQuery(client, nativeRedirect))
	if err != nil {
		t.Fatal(err)
	}
	registerClient(t, a, c)
	if _, err := a.register(c); err == nil {
		t.Fatal("configured registration limit")
	}
	now = now.Add(8 * time.Second)
	if _, err := a.signin(owner, req.browser, req.requestID); err == nil {
		t.Fatal("expired sign-in with both browser bindings")
	}

	link = startWindow(t, a)
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
	if a.ValidBearer(token.Access) {
		t.Fatal("configured access expiry")
	}
}

func TestOwnerOAuthCodeDeliveryExpiry(t *testing.T) {
	settings := Defaults()
	now := time.Now()
	settings.Now = func() time.Time { return now }
	a := openTest(t, privateDir(t), settings)
	owner, req := approveRequest(t, a)

	now = now.Add(settings.CodeLifetime)
	startWindow(t, a)
	if _, err := a.finish(owner, req.browser); err != errDenied {
		t.Fatal("delivered expired callback")
	}
}

func TestOwnerOAuthCodeLinuxDefaultDir(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux state directory convention")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, xdg := range []string{"", "state", filepath.Join(home, "absolute")} {
		t.Setenv("XDG_STATE_HOME", xdg)
		want := filepath.Join(home, ".local", "state", "croton-mcp")
		if filepath.IsAbs(xdg) {
			want = filepath.Join(xdg, "croton-mcp")
		}

		if got, err := DefaultDir(); err != nil || got != want {
			t.Fatalf("XDG_STATE_HOME=%q: got %q, %v; want %q", xdg, got, err, want)
		}
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

	if err := os.Chmod(db, 0600); err != nil {
		t.Fatal(err)
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

//go:build linux || darwin

package owneroauth

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

// PostToken sends a form through the supplied token endpoint handler.
func PostToken(handler http.Handler, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, issuer+"/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	return w
}

// IssuedPair checks the token response and returns the decoded pair.
func IssuedPair(t *testing.T, w *httptest.ResponseRecorder) tokenPair {
	t.Helper()

	var p tokenPair
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Type != "Bearer" || p.ExpiresIn != 600 || p.Access == "" || p.Refresh == "" || p.Access == p.Refresh {
		t.Fatalf("incomplete token pair (HTTP %d)", w.Code)
	}
	return p
}

func approvedPair(t *testing.T, a *Authority) (string, tokenPair) {
	t.Helper()
	code, r := approvedGrant(t, a, "authorization_code", "refresh_token")
	return r.Client, IssuedPair(t, PostToken(a, codeForm(code, r)))
}

// PairedRefreshFixture exchanges an approved code at the token endpoint with the pairing window closed.
func PairedRefreshFixture(t *testing.T, settings Settings) (*Authority, string, tokenPair) {
	t.Helper()
	a := openTest(t, privateDir(t), settings)
	client, pair := approvedPair(t, a)
	return a, client, pair
}

func refreshForm(client, token string) url.Values {
	return url.Values{"grant_type": {"refresh_token"}, "client_id": {client}, "refresh_token": {token}, "resource": {issuer + "/mcp"}}
}

func requireGrantDenial(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusBadRequest || strings.TrimSpace(w.Body.String()) != `{"error":"invalid_grant"}` {
		t.Fatalf("expected sanitized invalid_grant (HTTP %d)", w.Code)
	}
}

func TestOwnerOAuthRefreshRegistration(t *testing.T) {
	for _, scheme := range []string{"http", "https"} {
		t.Run(scheme, func(t *testing.T) {
			origin := scheme + "://127.0.0.1:8787"
			a, err := Open(privateDir(t), origin, Defaults())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { a.Close() })

			w := httptest.NewRecorder()
			a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, origin+"/.well-known/oauth-authorization-server", nil))
			var metadata struct {
				Grants []string `json:"grant_types_supported"`
			}
			supported := []string{"authorization_code"}
			if scheme == "https" {
				supported = append(supported, "refresh_token")
			}
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &metadata) != nil || !slices.Equal(metadata.Grants, supported) {
				t.Fatal("discovery returned incorrect grant types")
			}

			startWindow(t, a)
			for _, grants := range [][]string{nil, {"authorization_code"}, {"refresh_token", "authorization_code"}} {
				payload := map[string]any{"redirect_uris": []string{nativeRedirect}}
				want := []string{"authorization_code"}
				if grants != nil {
					payload["grant_types"] = grants
					want = grants
				}
				data, err := json.Marshal(payload)
				if err != nil {
					t.Fatal(err)
				}
				r := httptest.NewRequest(http.MethodPost, origin+"/register", bytes.NewReader(data))
				r.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				a.ServeHTTP(w, r)

				if scheme == "http" && slices.Contains(grants, "refresh_token") {
					if w.Code != http.StatusBadRequest {
						t.Fatal("HTTP issuer accepted refresh registration")
					}
					continue
				}

				var registered struct {
					Client string   `json:"client_id"`
					Grants []string `json:"grant_types"`
				}
				if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &registered) != nil || !slices.Equal(registered.Grants, want) {
					t.Fatalf("unexpected registration for grant types %v", grants)
				}
				query := authorizationQuery(registered.Client, nativeRedirect)
				query.Set("resource", origin+"/mcp")
				req, err := a.authorize(query)
				if err != nil || req.Refresh != slices.Contains(grants, "refresh_token") {
					t.Fatal("grant did not inherit registered refresh permission")
				}
			}
		})
	}
}

func TestOwnerOAuthRefreshHTTP(t *testing.T) {
	const origin = "http://127.0.0.1:8787"
	a, err := Open(privateDir(t), origin, Defaults())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })

	// Earlier versions could persist both refresh-enabled codes and families for HTTP.
	const code, client, token = "code.test", "client.test", "refresh.test"
	query := authorizationQuery(client, nativeRedirect)
	g := grant{
		Client: client, Redirect: nativeRedirect, Resource: origin + "/mcp",
		Challenge: query.Get("code_challenge"), Refresh: true, Expires: time.Now().Add(time.Minute),
	}
	err = a.db.Update(func(tx *bolt.Tx) error {
		if err := put(tx, "codes", code, g); err != nil {
			return err
		}
		family, err := tx.Bucket([]byte("families")).CreateBucket(digest("previous.test"))
		if err != nil {
			return err
		}
		return putRecord(family, []byte("current"), refreshFamily{
			Resource: g.Resource, Access: digest("access.test"), Refresh: digest(token), Expires: g.Expires,
		})
	})
	if err != nil {
		t.Fatal(err)
	}

	post := func(form url.Values) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, origin+"/token", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)
		return w
	}
	w := post(codeForm(code, &authorization{grant: g}))
	var issued tokenPair
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &issued) != nil || !a.ValidBearer(issued.Access) || issued.Refresh != "" {
		t.Fatal("HTTP code exchange must issue only a usable bearer")
	}

	form := refreshForm("previous.test", token)
	form.Set("resource", g.Resource)
	requireGrantDenial(t, post(form))
}

func TestOwnerOAuthRefreshRequiresTLS(t *testing.T) {
	a := openTest(t, privateDir(t), Defaults())
	code, req := approvedGrant(t, a, "authorization_code", "refresh_token")
	form := codeForm(code, req)

	r := httptest.NewRequest(http.MethodPost, "/token", strings.NewReader(form.Encode()))
	r.Host = "127.0.0.1:8787"
	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)

	requireGrantDenial(t, w)
	IssuedPair(t, PostToken(a, form))
}

func TestOwnerOAuthRefreshReuse(t *testing.T) {
	t.Setenv("CROTON_REFRESH_IDLE_TIMEOUT", "720h")

	for _, scenario := range []string{"simultaneous", "after restart"} {
		t.Run(scenario, func(t *testing.T) {
			settings := Defaults()
			now := time.Unix(1700000000, 0)
			settings.Now = func() time.Time { return now }
			dir := privateDir(t)
			a := openTest(t, dir, settings)

			client, first := approvedPair(t, a)
			form := refreshForm(client, first.Refresh)
			var current tokenPair
			if scenario == "after restart" {
				now = now.Add(29 * 24 * time.Hour)
				second := IssuedPair(t, PostToken(a, form))
				now = now.Add(2 * 24 * time.Hour)
				current = IssuedPair(t, PostToken(a, refreshForm(client, second.Refresh)))
				a = reopenTest(t, a, dir, settings)
				if !a.ValidBearer(current.Access) {
					t.Fatal("lost current access before replay")
				}
			}
			// Approval also exercises cleanup beyond the first token's idle deadline.
			otherClient, other := approvedPair(t, a)

			if scenario == "simultaneous" {
				start := make(chan struct{})
				results := make(chan *httptest.ResponseRecorder, 2)
				for range 2 {
					go func() {
						<-start
						results <- PostToken(a, form)
					}()
				}
				close(start)
				winner, loser := <-results, <-results
				if winner.Code != http.StatusOK {
					winner, loser = loser, winner
				}
				current = IssuedPair(t, winner)
				requireGrantDenial(t, loser)
			} else {
				requireGrantDenial(t, PostToken(a, form))
			}

			a = reopenTest(t, a, dir, settings)
			if a.ValidBearer(first.Access) || a.ValidBearer(current.Access) {
				t.Fatal("access survived family revocation")
			}
			requireGrantDenial(t, PostToken(a, refreshForm(client, current.Refresh)))
			if !a.ValidBearer(other.Access) {
				t.Fatal("replay revoked a different grant")
			}
			IssuedPair(t, PostToken(a, refreshForm(otherClient, other.Refresh)))

			if err := a.db.View(func(tx *bolt.Tx) error {
				if tx.Bucket([]byte("families")).Bucket(digest(client)) != nil {
					t.Fatal("retained revoked family's history")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOwnerOAuthRefreshTransaction(t *testing.T) {
	dir := privateDir(t)
	a := openTest(t, dir, Defaults())
	client, first := approvedPair(t, a)

	// Pin old pages so the size limit eventually fails a commit with both tokens staged.
	pin, err := a.db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	a.db.MaxSize = 1
	current := first
	var w *httptest.ResponseRecorder
	for range 100 {
		w = PostToken(a, refreshForm(client, current.Refresh))
		if w.Code != http.StatusOK {
			break
		}
		current = IssuedPair(t, w)
	}
	if err := pin.Rollback(); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"error":"server_error"}` {
		t.Fatalf("failed commit response (HTTP %d)", w.Code)
	}
	a = reopenTest(t, a, dir, Defaults())
	if !a.ValidBearer(current.Access) {
		t.Fatal("failed exchange consumed original access")
	}
	if err := a.db.View(func(tx *bolt.Tx) error {
		if tx.Bucket([]byte("access")).Stats().KeyN != 1 {
			t.Fatal("orphaned access after rollback")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	replacement := IssuedPair(t, PostToken(a, refreshForm(client, current.Refresh)))
	raw, err := os.ReadFile(filepath.Join(dir, "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first.Access, first.Refresh, current.Access, current.Refresh, replacement.Access, replacement.Refresh} {
		if bytes.Contains(raw, []byte(token)) {
			t.Fatal("database contained a plaintext token")
		}
	}
	requireGrantDenial(t, PostToken(a, refreshForm(client, first.Refresh)))
	if a.ValidBearer(replacement.Access) {
		t.Fatal("reuse marker failed to revoke retried exchange")
	}
}

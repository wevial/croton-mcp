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

func postToken(a *Authority, form url.Values) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, issuer+"/token", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	return w
}

func issuedPair(t *testing.T, w *httptest.ResponseRecorder) tokenPair {
	t.Helper()

	var p tokenPair
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.Type != "Bearer" || p.ExpiresIn != 600 || p.Access == "" || p.Refresh == "" || p.Access == p.Refresh {
		t.Fatalf("incomplete token pair (HTTP %d)", w.Code)
	}
	return p
}

func approvedPair(t *testing.T, a *Authority) (string, tokenPair) {
	t.Helper()
	code, r := approvedGrant(t, a)
	w := postToken(a, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {r.Client}, "code": {code},
		"redirect_uri": {r.Redirect}, "code_verifier": {verifier}, "resource": {r.Resource},
	})
	return r.Client, issuedPair(t, w)
}

// PairedRefreshFixture gives the transport-facing test a closed pairing window
// and a pair obtained at the real token endpoint.
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
	a := openTest(t, privateDir(t), Defaults())
	w := httptest.NewRecorder()
	a.ServeHTTP(w, httptest.NewRequest(http.MethodGet, issuer+"/.well-known/oauth-authorization-server", nil))
	var metadata struct {
		Grants []string `json:"grant_types_supported"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &metadata) != nil || !slices.Contains(metadata.Grants, "refresh_token") {
		t.Fatal("discovery omitted refresh grant")
	}

	startWindow(t, a)
	for _, grants := range [][]string{{"authorization_code"}, {"refresh_token", "authorization_code"}} {
		data, err := json.Marshal(map[string]any{"redirect_uris": []string{nativeRedirect}, "grant_types": grants})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, issuer+"/register", bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		a.ServeHTTP(w, r)

		var registered struct {
			Client string   `json:"client_id"`
			Grants []string `json:"grant_types"`
		}
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &registered) != nil || !slices.Equal(registered.Grants, grants) {
			t.Fatal("registration did not honor the client's grant types")
		}
		req, err := a.authorize(authorizationQuery(registered.Client, nativeRedirect))
		if err != nil || req.Refresh != slices.Contains(grants, "refresh_token") {
			t.Fatal("grant did not inherit registered refresh permission")
		}
	}
}

func TestOwnerOAuthRefreshReuse(t *testing.T) {
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
				second := issuedPair(t, postToken(a, form))
				now = now.Add(2 * 24 * time.Hour)
				current = issuedPair(t, postToken(a, refreshForm(client, second.Refresh)))
				if err := a.Close(); err != nil {
					t.Fatal(err)
				}
				a = openTest(t, dir, settings)
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
						results <- postToken(a, form)
					}()
				}
				close(start)
				winner, loser := <-results, <-results
				if winner.Code != http.StatusOK {
					winner, loser = loser, winner
				}
				current = issuedPair(t, winner)
				requireGrantDenial(t, loser)
			} else {
				requireGrantDenial(t, postToken(a, form))
			}

			if err := a.Close(); err != nil {
				t.Fatal(err)
			}
			a = openTest(t, dir, settings)
			if a.ValidBearer(first.Access) || a.ValidBearer(current.Access) {
				t.Fatal("access survived family revocation")
			}
			requireGrantDenial(t, postToken(a, refreshForm(client, current.Refresh)))
			if !a.ValidBearer(other.Access) {
				t.Fatal("replay revoked a different grant")
			}
			issuedPair(t, postToken(a, refreshForm(otherClient, other.Refresh)))

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
	form := refreshForm(client, first.Refresh)

	// Keep old pages pinned so each commit eventually needs a new page.
	// The size limit interrupts commit after the handler staged both tokens.
	pin, err := a.db.Begin(false)
	if err != nil {
		t.Fatal(err)
	}
	a.db.MaxSize = 1
	current := first
	var w *httptest.ResponseRecorder
	for range 100 {
		w = postToken(a, refreshForm(client, current.Refresh))
		if w.Code != http.StatusOK {
			break
		}
		current = issuedPair(t, w)
	}
	if err := pin.Rollback(); err != nil {
		t.Fatal(err)
	}
	if w.Code != http.StatusInternalServerError || strings.TrimSpace(w.Body.String()) != `{"error":"server_error"}` {
		t.Fatalf("failed commit response (HTTP %d)", w.Code)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	a = openTest(t, dir, Defaults())
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

	replacement := issuedPair(t, postToken(a, refreshForm(client, current.Refresh)))
	raw, err := os.ReadFile(filepath.Join(dir, "oauth.db"))
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{first.Access, first.Refresh, current.Access, current.Refresh, replacement.Access, replacement.Refresh} {
		if bytes.Contains(raw, []byte(token)) {
			t.Fatal("database contained a plaintext token")
		}
	}
	requireGrantDenial(t, postToken(a, form))
	if a.ValidBearer(replacement.Access) {
		t.Fatal("reuse marker failed to revoke retried exchange")
	}
}

package owneroauth

import (
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	bolt "go.etcd.io/bbolt"
)

type registration struct {
	Name        string
	Redirects   []string
	Application string
	Auth        string
	Grants      []string
	Responses   []string
}

func (a *Authority) supportedGrantTypes() oauth2.Arguments {
	if strings.HasPrefix(a.issuer, "https://") {
		return oauth2.Arguments{"authorization_code", "refresh_token"}
	}

	return oauth2.Arguments{"authorization_code"}
}

func (c registration) grantTypes() oauth2.Arguments {
	if c.Grants == nil {
		return oauth2.Arguments{"authorization_code"}
	}

	return c.Grants
}

type authorization struct {
	grant
	state, requestID, browser, comparison string
}

type window struct {
	expires, handoffExpires time.Time
	handoff, owner          string
	clients                 map[string]registration
	requests                map[string]*authorization
	review                  *authorization
}

// Only the approving browser may collect the redirect after the window closes.
type delivery struct {
	grant
	State                  string
	OwnerHash, BrowserHash []byte
}

func matches(a, b string) bool {
	return a != "" && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

// The IPC server is the sole caller of the window-opening and approval operations.
func (a *Authority) start() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.settings.Now()
	err := a.db.View(func(tx *bolt.Tx) error {
		data := tx.Bucket([]byte("deliveries")).Get(digest(a.issuer))
		if data == nil {
			return nil
		}

		var d delivery
		if json.Unmarshal(data, &d) != nil {
			return errState
		}
		if now.Before(d.Expires) {
			return errDenied
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	a.window = window{
		expires:        now.Add(a.settings.PairingWindow),
		handoffExpires: now.Add(a.settings.HandoffLifetime),
		handoff:        secret(),
		clients:        make(map[string]registration),
		requests:       make(map[string]*authorization),
	}

	return a.issuer + "/owner/pair#" + a.window.handoff, nil
}

// Caller holds mu for the entire transition, including any grant transaction.
func (a *Authority) openWindow() bool {
	if !a.settings.Now().Before(a.window.expires) {
		a.window = window{}
		return false
	}
	return true
}

func (a *Authority) claim(handoff string) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() || !a.settings.Now().Before(a.window.handoffExpires) || !matches(a.window.handoff, handoff) {
		return "", errDenied
	}

	a.window.handoff = ""
	a.window.owner = secret()
	return a.window.owner, nil
}

func validRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || len(raw) > 2048 || strings.ContainsAny(raw, "#\\") || !oauth2.IsValidRedirectURI(u) {
		return false
	}
	if port := u.Port(); port != "" {
		if n, err := strconv.ParseUint(port, 10, 16); err != nil || n == 0 {
			return false
		}
	}
	// These names belong to the authorization response.
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil || query.Has("code") || query.Has("iss") || query.Has("state") {
		return false
	}
	return u.Scheme == "https" || (u.Scheme == "http" && net.ParseIP(u.Hostname()).IsLoopback())
}

func (a *Authority) register(c registration) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() || len(a.window.clients) >= a.settings.PendingLimit {
		return "", errDenied
	}
	if len(c.Redirects) == 0 || len(c.Redirects) > 8 || len(c.Name) > 256 || (c.Auth != "" && c.Auth != "none") || (c.Application != "" && c.Application != "native" && c.Application != "web") {
		return "", errInvalid
	}
	grants := c.grantTypes()
	if !grants.ExactOne("authorization_code") && !grants.Matches(a.supportedGrantTypes()...) {
		return "", errInvalid
	}
	if c.Responses != nil && !slices.Equal(c.Responses, []string{"code"}) {
		return "", errInvalid
	}
	for _, raw := range c.Redirects {
		if !validRedirect(raw) {
			return "", errInvalid
		}
	}

	id := secret()
	a.window.clients[id] = c
	return id, nil
}

func (client registration) matchesRedirect(raw string) bool {
	if slices.Contains(client.Redirects, raw) {
		return true
	}
	if client.Application == "web" {
		return false
	}

	// Authelia validates native loopback equivalence. Preserve the literal
	// spelling outside the authority too (net/url normalizes e.g. the scheme).
	for _, registered := range client.Redirects {
		native := &oauth2.DefaultClient{Public: true, RedirectURIs: []string{registered}}
		target, err := oauth2.MatchRedirectURIWithClientRedirectURIs(raw, native)
		if err != nil {
			continue
		}

		original, _ := url.Parse(registered)
		if strings.Replace(raw, "://"+target.Host, "://"+original.Host, 1) == registered {
			return true
		}
	}
	return false
}

func (a *Authority) authorize(q url.Values) (*authorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() {
		return nil, errDenied
	}
	clientID, raw := q.Get("client_id"), q.Get("redirect_uri")
	client, ok := a.window.clients[clientID]
	if !ok || a.window.requests[clientID] != nil {
		return nil, errDenied
	}

	challenge := q.Get("code_challenge")
	decoded, err := base64.RawURLEncoding.Strict().DecodeString(challenge)
	if err != nil || len(challenge) != 43 || len(decoded) != 32 || q.Get("code_challenge_method") != "S256" || q.Get("response_type") != "code" || q.Get("resource") != a.issuer+"/mcp" {
		return nil, errInvalid
	}
	if !validRedirect(raw) || !client.matchesRedirect(raw) {
		return nil, errInvalid
	}

	request := &authorization{
		grant: grant{
			Client: clientID, Redirect: raw, Challenge: challenge, Resource: q.Get("resource"),
			Refresh: client.grantTypes().Has("refresh_token"),
		},
		state:      q.Get("state"),
		requestID:  secret(),
		browser:    secret(),
		comparison: strings.ToUpper(secret()[:10]),
	}
	a.window.requests[clientID] = request
	return request, nil
}

func (a *Authority) signin(owner, browser, id string) (review, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() || !matches(a.window.owner, owner) {
		return review{}, errDenied
	}
	for _, req := range a.window.requests {
		if matches(req.requestID, id) && matches(req.browser, browser) && (a.window.review == nil || a.window.review == req) {
			a.window.review = req
			return a.window.details(), nil
		}
	}
	return review{}, errDenied
}

type review struct {
	RequestID, Name, Client, Redirect, Resource, Challenge, Code string
}

func (a *Authority) inspect() (review, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() || a.window.review == nil {
		return review{}, errDenied
	}
	return a.window.details(), nil
}

func (w *window) details() review {
	r := w.review
	return review{
		RequestID: r.requestID, Name: w.clients[r.Client].Name, Client: r.Client,
		Redirect: r.Redirect, Resource: r.Resource, Challenge: r.Challenge, Code: r.comparison,
	}
}

func (a *Authority) approve(id, comparison string) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.openWindow() || a.window.review == nil || !matches(a.window.review.requestID, id) || !matches(a.window.review.comparison, comparison) {
		return errDenied
	}

	r := a.window.review
	now := a.settings.Now()
	d := delivery{
		grant: r.grant, State: r.state,
		OwnerHash: digest(a.window.owner), BrowserHash: digest(r.browser),
	}
	d.Expires = now.Add(a.settings.CodeLifetime)

	err := a.db.Update(func(tx *bolt.Tx) error {
		if err := prune(tx, now); err != nil {
			return err
		}

		clients, err := tx.CreateBucketIfNotExists([]byte("clients"))
		if err != nil {
			return err
		}
		if err := clients.Put([]byte(r.Client), []byte(a.window.clients[r.Client].Name)); err != nil {
			return err
		}

		return put(tx, "deliveries", a.issuer, d)
	})
	if err != nil {
		return errState
	}

	a.window = window{}
	return nil
}

func (a *Authority) finish(owner, browser string) (string, error) {
	if owner == "" || browser == "" {
		return "", errDenied
	}

	var d delivery
	var code string
	err := a.db.Update(func(tx *bolt.Tx) error {
		pending := tx.Bucket([]byte("deliveries"))
		key := digest(a.issuer)
		data := pending.Get(key)
		if data == nil {
			return errDenied
		}
		if json.Unmarshal(data, &d) != nil {
			return errState
		}
		if !a.settings.Now().Before(d.Expires) ||
			subtle.ConstantTimeCompare(d.OwnerHash, digest(owner)) != 1 ||
			subtle.ConstantTimeCompare(d.BrowserHash, digest(browser)) != 1 {
			return errDenied
		}

		// The issued code inherits the deadline set by terminal approval.
		code = secret()
		if err := put(tx, "codes", code, d.grant); err != nil {
			return err
		}
		return pending.Delete(key)
	})
	if errors.Is(err, errDenied) {
		return "", errDenied
	}
	if err != nil {
		return "", errState
	}

	target, _ := url.Parse(d.Redirect)
	query := url.Values{"code": {code}, "iss": {a.issuer}}
	if d.State != "" {
		query.Set("state", d.State)
	}
	if target.RawQuery != "" {
		target.RawQuery += "&"
	}
	target.RawQuery += query.Encode()
	return target.String(), nil
}

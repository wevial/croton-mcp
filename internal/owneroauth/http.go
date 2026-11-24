package owneroauth

import (
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"authelia.com/provider/oauth2"
	"github.com/wevial/croton-mcp/internal/stdioframe"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

const (
	ResourceMetadataPath = "/.well-known/oauth-protected-resource/mcp"
	requestCookiePrefix  = "croton-request-"
)

var errInvalid = errors.New("invalid request")

var signinPage = template.Must(template.New("signin").Funcs(template.FuncMap{"quote": strconv.QuoteToASCII}).Parse(`<!doctype html>
<title>Approve Croton client</title>
<p>Self-reported client name <code>{{quote .Name}}</code></p>
<p>Redirect URI <code>{{quote .Redirect}}</code></p>
<p>Run <code>croton-mcp approve</code> in your terminal. Check the client and redirect, then enter <strong>{{.Code}}</strong>.</p>
<form method="post" action="/owner/finish"><input type="hidden" name="request" value="{{.RequestID}}"><input type="hidden" name="owner" value="{{.Owner}}"><button>Continue after approval</button></form>`))

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func cookie(r *http.Request, name string) string {
	cookies := r.CookiesNamed(name)
	if len(cookies) != 1 {
		return ""
	}
	return cookies[0].Value
}

func (a *Authority) setRequestCookie(w http.ResponseWriter, req *authorization) {
	// Both durations are positive. Their sum fits in uint64 even when it
	// exceeds time.Duration, and Max-Age must round up to a whole second.
	lifetime := uint64(a.settings.PairingWindow) + uint64(a.settings.CodeLifetime)
	seconds := 1 + (lifetime-1)/uint64(time.Second)

	http.SetCookie(w, &http.Cookie{
		Name: requestCookiePrefix + req.requestID, Value: req.browser, Path: "/owner/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(seconds),
	})
}

// The nonce allows only the script that transfers the browser proof at this origin.
func browserPage(w http.ResponseWriter) string {
	nonce := secret()
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	return nonce
}

func parseValues(raw string) (url.Values, error) {
	q, err := url.ParseQuery(raw)
	if err != nil {
		return nil, errInvalid
	}
	for _, values := range q {
		if len(values) != 1 {
			return nil, errInvalid
		}
	}
	return q, nil
}

func readBody(w http.ResponseWriter, r *http.Request, contentType string) ([]byte, error) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != contentType || r.URL.RawQuery != "" {
		return nil, errInvalid
	}

	return io.ReadAll(http.MaxBytesReader(w, r.Body, stdioframe.MaxFrameBytes))
}

func readForm(w http.ResponseWriter, r *http.Request) (url.Values, error) {
	body, err := readBody(w, r, "application/x-www-form-urlencoded")
	if err != nil {
		return nil, err
	}

	return parseValues(string(body))
}

func readRegistration(w http.ResponseWriter, r *http.Request) (registration, error) {
	body, err := readBody(w, r, "application/json")
	if err != nil {
		return registration{}, err
	}

	var metadata map[string]json.RawMessage
	if !strictjson.DecodeValue(body, stdioframe.MaxFrameBytes, &metadata) {
		return registration{}, errInvalid
	}

	var c registration
	for key, value := range metadata {
		var field any
		switch {
		case strings.EqualFold(key, "client_name"):
			field = &c.Name
		case strings.EqualFold(key, "redirect_uris"):
			field = &c.Redirects
		case strings.EqualFold(key, "application_type"):
			field = &c.Application
		case strings.EqualFold(key, "token_endpoint_auth_method"):
			field = &c.Auth
		case strings.EqualFold(key, "grant_types"):
			field = &c.Grants
		case strings.EqualFold(key, "response_types"):
			field = &c.Responses
		default:
			continue
		}

		if string(value) == "null" || json.Unmarshal(value, field) != nil {
			return registration{}, errInvalid
		}
	}

	return c, nil
}

func (a *Authority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")

	err := a.route(w, r)
	var limit *http.MaxBytesError
	switch {
	case err == nil:
		return
	case errors.Is(err, errState):
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "server_error"})
	case errors.Is(err, oauth2.ErrInvalidScope):
		writeJSON(w, 400, map[string]string{"error": "invalid_scope"})
	case errors.Is(err, errDenied) && r.URL.Path == "/token":
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
	case errors.Is(err, errDenied):
		writeJSON(w, 403, map[string]string{"error": "access_denied"})
	case errors.As(err, &limit):
		w.WriteHeader(413)
	default:
		writeJSON(w, 400, map[string]string{"error": "invalid_request"})
	}
}

func (a *Authority) route(w http.ResponseWriter, r *http.Request) error {
	switch r.Method + " " + r.URL.EscapedPath() {
	case "GET " + ResourceMetadataPath:
		writeJSON(w, 200, map[string]any{"resource": a.issuer + "/mcp", "authorization_servers": []string{a.issuer}, "bearer_methods_supported": []string{"header"}})
	case "GET /.well-known/oauth-authorization-server":
		writeJSON(w, http.StatusOK, oauth2.OAuth2AuthorizationServerMetadata{
			Issuer:                            a.issuer,
			AuthorizationEndpoint:             a.issuer + "/authorize",
			RegistrationEndpoint:              a.issuer + "/register",
			TokenEndpoint:                     a.issuer + "/token",
			ResponseTypesSupported:            []string{"code"},
			GrantTypesSupported:               a.supportedGrantTypes(),
			TokenEndpointAuthMethodsSupported: []string{"none"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			RFC9207AuthorizationServerIssuerIdentificationMetadata: &oauth2.RFC9207AuthorizationServerIssuerIdentificationMetadata{
				AuthorizationResponseISSParameterSupported: true,
			},
		})
	case "POST /register":
		c, err := readRegistration(w, r)
		if err != nil {
			return err
		}

		id, err := a.register(c)
		if err != nil {
			return err
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id": id, "client_name": c.Name, "redirect_uris": c.Redirects,
			"token_endpoint_auth_method": "none",
			"grant_types":                c.grantTypes(), "response_types": []string{"code"},
		})
	case "GET /authorize":
		q, err := parseValues(r.URL.RawQuery)
		if err != nil {
			return err
		}
		req, err := a.authorize(q)
		if err != nil {
			return err
		}

		a.setRequestCookie(w, req)
		nonce := browserPage(w)
		fmt.Fprintf(w, `<!doctype html><title>Sign in to Croton</title>
<form method="post" action="/owner/signin"><input type="hidden" name="request" value="%s"><input type="hidden" name="owner"></form>
<script nonce="%s">const form=document.forms[0];form.elements.owner.value=localStorage.getItem("croton-owner") || "";form.submit()</script>`, req.requestID, nonce)
	case "GET /owner/pair":
		a.mu.Lock()
		open := a.openWindow() && a.window.handoff != "" && a.settings.Now().Before(a.window.handoffExpires)
		a.mu.Unlock()
		if !open {
			return errDenied
		}

		nonce := browserPage(w)
		fmt.Fprintf(w, `<!doctype html><title>Pair Croton</title><form method="post"><input name="handoff" type="hidden"><button>Pair this browser</button></form><script nonce="%s">document.forms[0].handoff.value=location.hash.slice(1);history.replaceState(null,"",location.pathname)</script>`, nonce)
	case "POST /owner/pair", "POST /owner/signin", "POST /owner/finish":
		if r.Header.Get("Origin") != a.issuer {
			return errDenied
		}
		form, err := readForm(w, r)
		if err != nil {
			return err
		}
		switch r.URL.Path {
		case "/owner/pair":
			owner, err := a.claim(form.Get("handoff"))
			if err != nil {
				return err
			}

			nonce := browserPage(w)
			fmt.Fprintf(w, `<!doctype html><title>Browser paired</title><body data-owner="%s">
<p>Start authorization in your MCP client using this browser.</p>
<script nonce="%s">localStorage.setItem("croton-owner", document.body.dataset.owner)</script>`, owner, nonce)
		case "/owner/signin":
			owner := form.Get("owner")
			id := form.Get("request")
			details, err := a.signin(owner, cookie(r, requestCookiePrefix+id), id)
			if err != nil {
				return err
			}

			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_ = signinPage.Execute(w, struct {
				review
				Owner string
			}{details, owner})
		case "/owner/finish":
			location, err := a.finish(form.Get("owner"), cookie(r, requestCookiePrefix+form.Get("request")))
			if err != nil {
				return err
			}

			w.Header().Set("Location", location)
			w.WriteHeader(http.StatusSeeOther)
		}
	case "POST /token":
		if r.TLS == nil && !a.settings.BehindHTTPSProxy && strings.HasPrefix(a.issuer, "https://") {
			return errDenied
		}

		form, err := readForm(w, r)
		if err != nil {
			return err
		}
		if r.Header.Get("Authorization") != "" {
			return errInvalid
		}

		var pair tokenPair
		switch form.Get("grant_type") {
		case "authorization_code":
			pair, err = a.redeem(form.Get("code"), form.Get("client_id"), form.Get("redirect_uri"), form.Get("resource"), form.Get("code_verifier"))
		case "refresh_token":
			pair, err = a.refresh(r.Context(), form)
		default:
			return errInvalid
		}
		if err != nil {
			return err
		}
		pair.Type = "Bearer"
		pair.ExpiresIn = int64(a.settings.AccessLifetime / time.Second)
		writeJSON(w, 200, pair)
	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return nil
}

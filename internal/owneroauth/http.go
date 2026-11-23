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
	"time"

	"authelia.com/provider/oauth2"
	"github.com/wevial/croton-mcp/internal/stdioframe"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

const (
	ResourceMetadataPath = "/.well-known/oauth-protected-resource/mcp"
	ownerCookie          = "croton-owner"
	requestCookie        = "croton-request"
)

var errInvalid = errors.New("invalid request")

var signinPage = template.Must(template.New("signin").Funcs(template.FuncMap{"quote": strconv.QuoteToASCII}).Parse(`<!doctype html>
<title>Approve Croton client</title>
<p>Self-reported client name <code>{{quote .Name}}</code></p>
<p>Redirect URI <code>{{quote .Redirect}}</code></p>
<p>Run <code>croton-mcp approve</code> in your terminal. Check the client and redirect, then enter <strong>{{.Code}}</strong>.</p>
<form method="post" action="/owner/finish"><button>Continue after approval</button></form>`))

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

func (a *Authority) setCookie(w http.ResponseWriter, name, value string) {
	http.SetCookie(w, &http.Cookie{
		Name: name, Value: value, Path: "/owner/",
		Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
		MaxAge: int(a.settings.PairingWindow/time.Second) + int(a.settings.CodeLifetime/time.Second) + 1,
	})
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

func (a *Authority) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'; base-uri 'none'")

	err := a.route(w, r)
	switch {
	case err == nil:
		return
	case errors.Is(err, errDenied) && r.URL.Path == "/token":
		writeJSON(w, 400, map[string]string{"error": "invalid_grant"})
	case errors.Is(err, errDenied):
		writeJSON(w, 403, map[string]string{"error": "access_denied"})
	default:
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			w.WriteHeader(413)
			return
		}
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
			GrantTypesSupported:               []string{"authorization_code"},
			TokenEndpointAuthMethodsSupported: []string{"none"},
			CodeChallengeMethodsSupported:     []string{"S256"},
			RFC9207AuthorizationServerIssuerIdentificationMetadata: &oauth2.RFC9207AuthorizationServerIssuerIdentificationMetadata{
				AuthorizationResponseISSParameterSupported: true,
			},
		})
	case "POST /register":
		body, err := readBody(w, r, "application/json")
		if err != nil {
			return err
		}
		var c registration
		if !strictjson.DecodeObject(body, stdioframe.MaxFrameBytes, &c) {
			return errInvalid
		}
		id, err := a.register(c)
		if err != nil {
			return err
		}

		writeJSON(w, http.StatusCreated, map[string]any{
			"client_id": id, "client_name": c.Name, "redirect_uris": c.Redirects,
			"token_endpoint_auth_method": "none",
			"grant_types":                []string{"authorization_code"}, "response_types": []string{"code"},
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

		a.setCookie(w, requestCookie, req.browser)
		w.Header().Set("Location", "/owner/signin?request="+req.requestID)
		w.WriteHeader(http.StatusSeeOther)
	case "GET /owner/pair":
		a.mu.Lock()
		open := a.openWindow() && a.window.handoff != "" && a.settings.Now().Before(a.window.handoffExpires)
		a.mu.Unlock()
		if !open {
			return errDenied
		}

		nonce := secret()
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'nonce-"+nonce+"'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>Pair Croton</title><form method="post"><input name="handoff" type="hidden"><button>Pair this browser</button></form><script nonce="%s">document.forms[0].handoff.value=location.hash.slice(1);history.replaceState(null,"",location.pathname)</script>`, nonce)
	case "POST /owner/pair", "POST /owner/finish":
		if r.Header.Get("Origin") != a.issuer {
			return errDenied
		}
		form, err := readForm(w, r)
		if err != nil {
			return err
		}
		if r.URL.Path == "/owner/pair" {
			owner, err := a.claim(form.Get("handoff"))
			if err != nil {
				return err
			}

			a.setCookie(w, ownerCookie, owner)
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			fmt.Fprintln(w, "Start authorization in your MCP client using this browser.")
			return nil
		}
		location, err := a.finish(cookie(r, ownerCookie), cookie(r, requestCookie))
		if err != nil {
			return err
		}
		w.Header().Set("Location", location)
		w.WriteHeader(http.StatusSeeOther)
	case "GET /owner/signin":
		q, err := parseValues(r.URL.RawQuery)
		if err != nil {
			return err
		}
		details, err := a.signin(cookie(r, ownerCookie), cookie(r, requestCookie), q.Get("request"))
		if err != nil {
			return err
		}

		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = signinPage.Execute(w, details)
	case "POST /token":
		form, err := readForm(w, r)
		if err != nil {
			return err
		}
		if form.Get("grant_type") != "authorization_code" || r.Header.Get("Authorization") != "" {
			return errInvalid
		}
		token, err := a.redeem(form.Get("code"), form.Get("client_id"), form.Get("redirect_uri"), form.Get("resource"), form.Get("code_verifier"))
		if err != nil {
			return err
		}
		writeJSON(w, 200, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": int64(a.settings.AccessLifetime / time.Second)})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
	return nil
}

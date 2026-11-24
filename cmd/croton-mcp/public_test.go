//go:build linux || darwin

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/mcpserver"
	"github.com/wevial/croton-mcp/internal/owneroauth"
	"golang.org/x/oauth2"
)

func TestPublicServeInvalidOrigin(t *testing.T) {
	for _, raw := range []string{
		"", "mail.test", "http://mail.test", "https:///missing", "https://:443",
		"https://mail.test:444", "https://mail.test:", "https://mail.test/mcp",
		"https://mail.test/%2F", "https://mail.test?peer=test", "https://mail.test?",
		"https://mail.test/#fragment", "https://mail.test#", "https://owner@mail.test",
		"https://@mail.test", "https://[mail.test]", "https://::1", "https://mail..test",
		"https://192.0.2.009", "https://192.0.2.0400",
		"https://mail%2Ftest", "https://mail%3A443", "https://owner%40mail.test",
		"https://mail.test:%34%34%33", "https://mail.test/%2E", "https://mail%6",
		"https://[2001:d%628::1]", "https://mail.\u00ad.test", "https://mail.test..\u00ad",
	} {
		t.Run(raw, func(t *testing.T) {
			args := []string{"serve", "--public-url", raw}
			if err := run(t.Context(), args, io.Discard, owneroauth.Defaults()); err == nil || err.Error() != "invalid arguments" {
				t.Fatal("invalid origin did not fail flag parsing")
			}
		})
	}
}

func TestPublicServeOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"https://mail.test":                  "https://mail.test",
		"https://%6Dail%2dproxy%2Etest:443/": "https://mail-proxy.test",
		"https://mail.test.\u00ad":           "https://mail.test.",
		"https://MAIL.test.:443":             "https://mail.test.",
		"https://mail.test。:443":             "https://mail.test.",
		"https://192.0.2.1.:443":             "https://192.0.2.1",
		"https://192.0.2.010.:443":           "https://192.0.2.8",
		"https://0xc0.0x201":                 "https://192.0.2.1",
		"https://[2001:0DB8:0:0::1]:443/":    "https://[2001:db8::1]",
		"https://[::ffff:192.0.2.1]/":        "https://[::ffff:c000:201]",
		"https://münchen.test:0443":          "https://xn--mnchen-3ya.test",
	} {
		if got, err := parsePublicOrigin(raw); err != nil || got != want {
			t.Errorf("parsePublicOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestPublicServeProxy(t *testing.T) {
	const origin = "https://192.0.2.1"
	config, marker := stalledBridgeConfig(t, 30*time.Second)
	dir := httpStateDir(t)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := probe.Addr().String()
	probe.Close()
	target := &url.URL{Scheme: "http", Host: address}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	t.Cleanup(proxy.Close)
	transport := proxy.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.ServerName = "127.0.0.1"
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, proxy.Listener.Addr().String())
	}

	args := []string{"serve", "--config", config, "--bind", address, "--state-dir", dir, "--public-url", "https://192.0.2.001:443"}
	ctx, cancelStartup := context.WithTimeout(t.Context(), time.Second)
	defer cancelStartup()
	if err := run(ctx, args, io.Discard, owneroauth.Defaults()); err == nil || err.Error() != "HTTP authorizer unavailable" {
		t.Fatal("public listener started without the owner authorizer")
	}
	args = append(args, "--authorizer", "owner")
	p := launchHTTP(t, dir, origin, args, transport, nil, "CROTON_TEST_BRIDGE_GATE="+marker)
	p.checkDiscovery(t)
	_, body := p.request(t, "GET", "/.well-known/oauth-authorization-server", "", "", 200)
	var metadata struct{ Issuer string }
	if json.Unmarshal(body, &metadata) != nil || metadata.Issuer != origin {
		t.Fatal("incorrect external OAuth issuer")
	}

	for _, tc := range []struct{ host, origin string }{
		{address, origin}, {"192.0.2.1", "https://foreign.test"},
	} {
		r, err := http.NewRequestWithContext(t.Context(), "GET", origin+owneroauth.ResourceMetadataPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Host", "192.0.2.1")
		resp, err := p.client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Fatal("accepted foreign Host or Origin")
		}
	}

	token := p.pair(t)
	secondToken := p.pair(t)
	session := p.session(t, token, "2026-07-28")
	callCtx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var calls sync.WaitGroup
	for _, path := range []string{marker, marker + ".queued"} {
		calls.Go(func() {
			_, _ = session.CallTool(callCtx, &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		})
		waitForMarker(t, path)
	}

	const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
		"name":"list_folders","arguments":{},"_meta":{
			"io.modelcontextprotocol/protocolVersion":"2026-07-28",
			"io.modelcontextprotocol/clientInfo":{"name":"body.test","version":"1"},
			"io.modelcontextprotocol/clientCapabilities":{}
		}}}`
	r, err := http.NewRequestWithContext(t.Context(), "POST", origin+"/mcp?tag=query.test", strings.NewReader(call))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+secondToken)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", "2026-07-28")
	r.Header.Set("Mcp-Method", "tools/call")
	r.Header.Set("Mcp-Name", "list_folders")

	resp, err := p.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("second client's MCP call returned HTTP %d, want 429", resp.StatusCode)
	}

	cancel()
	calls.Wait()
	p.stop(t)
	for _, sensitive := range []string{token, secondToken, "query.test", "body.test"} {
		if strings.Contains(p.stderr.String(), sensitive) {
			t.Fatal("request content in diagnostics")
		}
	}
}

func TestPublicServeUnboundBearer(t *testing.T) {
	const token = "legacy-bearer.test"
	authorize := func(bearer string) (string, bool) { return "", bearer == token }
	const initialize = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"client.test","version":"1"}
	}}`

	for _, tc := range []struct {
		name, origin string
		public       bool
		want         int
	}{
		{"public", "https://proxy.test", true, http.StatusUnauthorized},
		{"local", "http://127.0.0.1:8787", false, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := mcpserver.NewHTTP(tc.origin, tc.public, nil, nil, time.Second, authorize, http.NotFoundHandler())
			if err != nil {
				t.Fatal(err)
			}

			req := httptest.NewRequest(http.MethodPost, tc.origin+"/mcp", strings.NewReader(initialize))
			req.Header.Set("Authorization", "Bearer "+token)
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			w := httptest.NewRecorder()

			handler.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("MCP initialize returned HTTP %d, want %d", w.Code, tc.want)
			}
		})
	}
}

func TestPublicServeRateLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const origin = "https://proxy.test"
		dir := httpStateDir(t)
		oauth, err := owneroauth.Open(dir, origin, owneroauth.Defaults())
		if err != nil {
			t.Fatal(err)
		}
		defer oauth.Close()

		stop, err := oauth.ListenOwner(t.Context())
		if err != nil {
			t.Fatal(err)
		}

		_, err = adminCommand(t, dir, "pair")
		stop()
		if err != nil {
			t.Fatal(err)
		}

		clients := map[string]string{"bearer-one": "one", "rotated-one": "one", "bearer-two": "two"}
		handler, err := mcpserver.NewHTTP(origin, true, nil, nil, time.Second, func(token string) (string, bool) {
			if id, ok := clients[token]; ok {
				return id, true
			}
			return token, false
		}, oauth)
		if err != nil {
			t.Fatal(err)
		}

		sequence := 0
		request := func(method, path, token, forwardedUser string, want int) {
			t.Helper()
			sequence++
			peer := fmt.Sprintf("192.0.2.%d", sequence)
			r := httptest.NewRequest(method, origin+path, nil)
			r.RemoteAddr = fmt.Sprintf("127.0.0.%d:%d", sequence, 10000+sequence)
			r.Header.Set("Forwarded", "for="+peer+";proto=https")
			r.Header.Set("X-Forwarded-For", peer)
			if forwardedUser == "" {
				forwardedUser = peer
			}
			r.Header.Set("X-Forwarded-User", forwardedUser)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("request %d (%s %s) returned %d, want %d", sequence, method, r.URL.Path, w.Code, want)
			}
		}

		reg := httptest.NewRequest("POST", origin+"/register", strings.NewReader(`{"redirect_uris":["https://client.test/callback"]}`))
		reg.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, reg)

		var client approvedClient
		if w.Code != http.StatusCreated || json.Unmarshal(w.Body.Bytes(), &client) != nil || client.ID == "" {
			t.Fatal("expected a registered client ID")
		}

		query := url.Values{
			"client_id": {client.ID}, "redirect_uri": {clientRedirect}, "response_type": {"code"},
			"code_challenge_method": {"S256"}, "code_challenge": {oauth2.S256ChallengeFromVerifier(clientVerifier)},
			"resource": {origin + "/mcp"},
		}
		authorize := "/authorize?" + query.Encode()

		for n := range 14 {
			request("GET", fmt.Sprintf("/authorize?client_id=client%d.test", n), "", "", 403)
			request("POST", "/mcp", fmt.Sprintf("invalid%d.test", n), "", 401)
		}
		request("POST", "/mcp", "", "", 401)
		request("GET", authorize, "", "one", 429)
		request("POST", "/register", "", "", 429)
		request("POST", "/mcp", "", "one", 429)

		// HTTP 400 for the empty MCP frame means the limiter admitted the request.
		for range 60 {
			request("POST", "/mcp", "bearer-one", "", 400)
		}
		time.Sleep(30 * time.Second)
		for range 60 {
			request("GET", "/authorize", "rotated-one", "", 403)
		}
		request("POST", "/mcp", "rotated-one", "two", 429)
		request("POST", "/mcp", "bearer-two", "one", 400)

		time.Sleep(30 * time.Second)
		request("GET", authorize, "", "one", 200)
		request("POST", "/mcp", "", "two", 401)
		for range 60 {
			request("POST", "/mcp", "rotated-one", "", 400)
		}
		request("POST", "/mcp", "rotated-one", "", 429)
	})
}

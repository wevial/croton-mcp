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
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/mcpserver"
	"github.com/wevial/croton-mcp/internal/owneroauth"
)

func TestPublicServeInvalidOrigin(t *testing.T) {
	config := writeServerConfig(t, fixtureBridge(t))
	dir := httpStateDir(t)
	for _, raw := range []string{
		"", "mail.test", "http://mail.test", "https:///missing", "https://:443",
		"https://mail.test:444", "https://mail.test:", "https://mail.test/mcp",
		"https://mail.test/%2F", "https://mail.test?peer=test", "https://mail.test?",
		"https://mail.test/#fragment", "https://mail.test#", "https://owner@mail.test",
		"https://@mail.test", "https://[mail.test]", "https://::1", "https://mail..test",
	} {
		t.Run(raw, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			args := []string{"serve", "--config", config, "--state-dir", dir, "--authorizer", "owner", "--public-url", raw}
			if err := run(ctx, args, io.Discard, owneroauth.Defaults()); err == nil || err.Error() != "invalid arguments" {
				t.Fatal("invalid origin did not fail flag parsing")
			}
		})
	}
}

func TestPublicServeOrigin(t *testing.T) {
	for raw, want := range map[string]string{
		"https://mail.test":               "https://mail.test",
		"https://MAIL.test.:443":          "https://mail.test.",
		"https://[2001:0DB8:0:0::1]:443/": "https://[2001:db8::1]",
		"https://[::ffff:192.0.2.1]/":     "https://[::ffff:c000:201]",
		"https://münchen.test:0443":       "https://xn--mnchen-3ya.test",
	} {
		if got, err := parsePublicOrigin(raw); err != nil || got != want {
			t.Errorf("parsePublicOrigin(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
}

func TestPublicServeProxy(t *testing.T) {
	const origin = "https://mail.test"
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

	args := []string{"serve", "--config", config, "--bind", address, "--state-dir", dir, "--public-url", "https://Mail.Test:443/"}
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
		{address, origin}, {"mail.test", "https://foreign.test"},
	} {
		r, err := http.NewRequestWithContext(t.Context(), "GET", origin+owneroauth.ResourceMetadataPath, nil)
		if err != nil {
			t.Fatal(err)
		}
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		r.Header.Set("X-Forwarded-Host", "mail.test")
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
	session := p.session(t, token, "2026-07-28")
	calls, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan struct{}, 2)
	call := func() {
		_, _ = session.CallTool(calls, &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		done <- struct{}{}
	}

	go call()
	waitForMarker(t, marker)
	go call()
	waitForMarker(t, marker+".queued")

	r, err := http.NewRequestWithContext(t.Context(), "POST", origin+"/mcp?tag=query.test", strings.NewReader(`{"tag":"body.test"}`))
	if err != nil {
		t.Fatal(err)
	}
	r.Header.Set("Authorization", "Bearer "+token)
	resp, err := p.client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("third MCP request returned %d", resp.StatusCode)
	}

	cancel()
	for range 2 {
		<-done
	}
	p.stop(t)
	for _, sensitive := range []string{token, "query.test", "body.test"} {
		if strings.Contains(p.stderr.String(), sensitive) {
			t.Fatal("request content in diagnostics")
		}
	}
}

func TestPublicServeRateLimits(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const origin = "https://proxy.test"
		clients := map[string]string{"bearer-one": "one", "rotated-one": "one", "bearer-two": "two"}
		handler, err := mcpserver.NewHTTP(origin, true, nil, nil, time.Second, func(token string) (string, bool) {
			if id, ok := clients[token]; ok {
				return id, true
			}
			return token, false
		}, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }))
		if err != nil {
			t.Fatal(err)
		}

		sequence := 0
		request := func(path, token string, want int) {
			t.Helper()
			sequence++
			peer := fmt.Sprintf("192.0.2.%d", sequence)
			r := httptest.NewRequest("POST", origin+path, nil)
			r.Header.Set("Forwarded", "for="+peer+";proto=https")
			r.Header.Set("X-Forwarded-For", peer)
			r.Header.Set("X-Forwarded-User", peer)
			if token != "" {
				r.Header.Set("Authorization", "Bearer "+token)
			}

			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			if w.Code != want {
				t.Fatalf("request %d returned %d, want %d", sequence, w.Code, want)
			}
		}

		for n := range 15 {
			request(fmt.Sprintf("/authorize?client_id=client%d.test", n), "", 204)
			request("/mcp", fmt.Sprintf("invalid%d.test", n), 401)
		}
		request("/register", "", 429)
		request("/mcp", "", 429)

		// The empty MCP frame is rejected only after admission. Rotation and
		// OAuth routes share the same verified client's rolling budget.
		for range 60 {
			request("/mcp", "bearer-one", 400)
		}
		time.Sleep(30 * time.Second)
		for range 60 {
			request("/authorize", "rotated-one", 204)
		}
		request("/mcp", "rotated-one", 429)
		request("/mcp", "bearer-two", 400)

		time.Sleep(30 * time.Second)
		request("/mcp", "", 401)
		for range 60 {
			request("/mcp", "rotated-one", 400)
		}
		request("/mcp", "rotated-one", 429)
	})
}

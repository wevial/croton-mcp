//go:build linux || darwin

package owneroauth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/mcpserver"
	"github.com/wevial/croton-mcp/internal/owneroauth"
)

type folders struct{ mcpserver.Mail }

func (folders) ListFolders(context.Context) ([]bridge.Folder, error) {
	return []bridge.Folder{{Name: "refresh.test"}}, nil
}

func TestOwnerOAuthRefreshFlow(t *testing.T) {
	const origin = "https://127.0.0.1:8787"
	settings := owneroauth.Defaults()
	if settings.RefreshIdleTimeout != 30*24*time.Hour {
		t.Fatal("unexpected default idle timeout")
	}
	t.Setenv("CROTON_REFRESH_IDLE_TIMEOUT", "24h")
	now := time.Unix(1700000000, 0)
	settings.Now = func() time.Time { return now }
	a, client, pair := owneroauth.PairedRefreshFixture(t, settings)
	handler, err := mcpserver.NewHTTP(origin, false, folders{}, nil, time.Second, a.BearerClient, a)
	if err != nil {
		t.Fatal(err)
	}
	read := func(path string, status int) {
		t.Helper()
		const call = `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{
            "name":"list_folders","arguments":{},"_meta":{
                "io.modelcontextprotocol/protocolVersion":"2026-07-28",
                "io.modelcontextprotocol/clientInfo":{"name":"client.test","version":"1"},
                "io.modelcontextprotocol/clientCapabilities":{}
            }}}`
		req := httptest.NewRequest(http.MethodPost, origin+path, strings.NewReader(call))
		req.Header.Set("Authorization", "Bearer "+pair.Access)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		req.Header.Set("MCP-Protocol-Version", "2026-07-28")
		req.Header.Set("Mcp-Method", "tools/call")
		req.Header.Set("Mcp-Name", "list_folders")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, req)
		if w.Code != status || (status == http.StatusOK && !strings.Contains(w.Body.String(), "refresh.test")) {
			t.Fatalf("folder read at %s returned HTTP %d", path, w.Code)
		}
	}
	refresh := func(id, resource, scope string, status int) {
		t.Helper()
		f := url.Values{"grant_type": {"refresh_token"}, "client_id": {id}, "refresh_token": {pair.Refresh}}
		if resource != "" {
			f.Set("resource", resource)
		}
		if scope != "" {
			f.Set("scope", scope)
		}
		w := owneroauth.PostToken(handler, f)
		if status == http.StatusOK {
			old := pair
			pair = owneroauth.IssuedPair(t, w)
			if pair.Access == old.Access || pair.Refresh == old.Refresh {
				t.Fatal("incomplete rotation")
			}
		} else if w.Code != status {
			t.Fatalf("refresh returned HTTP %d, want %d", w.Code, status)
		}
	}

	now = now.Add(10 * time.Minute)
	read("/mcp", http.StatusUnauthorized)
	refresh("other.test", origin+"/mcp", "", http.StatusBadRequest)
	refresh(client, origin+"/mcp/", "", http.StatusBadRequest)
	refresh(client, origin+"/mcp", "write", http.StatusBadRequest)
	refresh(client, origin+"/mcp", "", http.StatusOK)
	read("/mcp", http.StatusOK)
	read("/mcp/", http.StatusNotFound)

	// Cross the default thirty-day lifetime while staying inside each
	// configured idle window. An omitted resource inherits the approved URL.
	for range 32 {
		now = now.Add(23*time.Hour + 59*time.Minute)
		refresh(client, "", "", http.StatusOK)
	}
	read("/mcp", http.StatusOK)
	now = now.Add(24 * time.Hour)
	refresh(client, origin+"/mcp", "", http.StatusBadRequest)
}

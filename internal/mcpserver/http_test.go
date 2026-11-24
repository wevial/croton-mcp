package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/stdioframe"
)

func TestHTTPTransportAdmission(t *testing.T) {
	const origin = "http://127.0.0.1:8787"
	now := time.Unix(100, 0)
	tokens := map[string]time.Time{"valid": now.Add(time.Minute), "expired": now}
	var checks int
	handler, err := NewHTTP(origin, false, nil, nil, time.Second, func(token string) (string, bool) {
		checks++
		return "", now.Before(tokens[token])
	}, http.NotFoundHandler())
	if err != nil {
		t.Fatal(err)
	}

	for _, method := range []string{"initialize", "tools/list", "tools/call"} {
		for _, credential := range []string{"", "Bearer private.test", "Bearer expired"} {
			req := httptest.NewRequest("POST", origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"`+method+`"}`))
			req.Header.Set("Authorization", credential)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)
			if w.Code != 401 || w.Header().Get("WWW-Authenticate") != `Bearer resource_metadata="http://127.0.0.1:8787/.well-known/oauth-protected-resource/mcp"` || w.Body.Len() != 0 {
				t.Fatal("incorrect bearer rejection for", method)
			}
		}
	}
	for _, test := range []struct {
		name, path, host, body string
		origins                []string
		status                 int
	}{
		{"host", "/mcp", "localhost:8787", "{}", nil, 403},
		{"foreign origin", "/mcp", "", "{}", []string{"http://127.0.0.1:8000"}, 403},
		{"duplicate origin", "/mcp", "", "{}", []string{origin, origin}, 403},
		{"wrong path", "/mcp/", "", "{}", nil, 404},
		{"encoded path", "/%6dcp", "", "{}", nil, 404},
		{"chunked oversize", "/mcp", "", strings.Repeat(" ", stdioframe.MaxFrameBytes-1) + "{}", nil, 413},
		{"duplicate", "/mcp", "", `{"params":{"x":1,"x":2}}`, nil, 400},
		{"batch", "/mcp", "", `[{}]`, nil, 400},
		{"trailing", "/mcp", "", "{} {}", nil, 400},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", origin+test.path, strings.NewReader(test.body))
			req.ContentLength = -1
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json, text/event-stream")
			if test.host != "" {
				req.Host = test.host
			}
			req.Header["Origin"] = test.origins
			req.Header.Set("Authorization", "Bearer valid")
			w := httptest.NewRecorder()
			before := checks
			handler.ServeHTTP(w, req)
			if w.Code != test.status {
				t.Fatalf("status = %d, want %d", w.Code, test.status)
			}
			if (test.status == 403 || test.status == 404) && checks != before {
				t.Fatal("credential lookup before routing and origin validation")
			}
		})
	}
}

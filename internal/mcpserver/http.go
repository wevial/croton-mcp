package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/owneroauth"
	"github.com/wevial/croton-mcp/internal/stdioframe"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

type httpContextKey struct{}

// NewHTTP mounts a read-only SDK server and the owner's OAuth endpoints on
// the exact configured origin. All security checks precede SDK dispatch.
func NewHTTP(origin string, public bool, mail Mail, audit *Auditor, timeout time.Duration, authorize func(string) (string, bool), oauth http.Handler) (http.Handler, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || authorize == nil || oauth == nil || timeout <= 0 {
		return nil, errors.New("HTTP authorizer unavailable")
	}

	var limits *publicAdmission
	if public {
		limits = &publicAdmission{recent: make(map[string][]time.Time), inFlight: make(chan struct{}, 2)}
	}

	server := New(Options{Mail: mail, Audit: audit})
	// The SDK deliberately detaches legacy sessions from the POST context.
	// Restore its deadline and shutdown signal for Bridge operations on every version.
	server.sdk.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			parent, ok := ctx.Value(httpContextKey{}).(context.Context)
			if !ok {
				return nil, errServerUnavailable
			}
			deadline, _ := parent.Deadline()
			call, cancel := context.WithDeadline(ctx, deadline)
			defer cancel()
			stop := context.AfterFunc(parent, cancel)
			defer stop()
			if parent.Err() != nil {
				cancel()
			}
			return next(call, method, req)
		}
	})
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server.sdk }, &mcp.StreamableHTTPOptions{
		// Public Host and Origin are pinned before SDK dispatch.
		DisableLocalhostProtection:   public,
		Stateless:                    true,
		JSONResponse:                 true,
		MaxRequestBodyBytes:          stdioframe.MaxFrameBytes,
		PropagateRequestCancellation: true,
	})

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if r.Host != u.Host || len(origins) > 1 || (len(origins) == 1 && origins[0] != origin) {
			w.WriteHeader(http.StatusForbidden)
			return
		}

		mcpRequest := r.URL.EscapedPath() == "/mcp"
		var client string
		var valid bool
		if mcpRequest || public {
			scheme, token, _ := strings.Cut(r.Header.Get("Authorization"), " ")
			if len(r.Header.Values("Authorization")) == 1 && strings.EqualFold(scheme, "Bearer") && token != "" {
				client, valid = authorize(token)
			}
		}

		if !valid {
			client = ""
		}

		w.Header().Set("Cache-Control", "no-store")
		if public && !limits.allow(client) {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		if !mcpRequest {
			oauth.ServeHTTP(w, r)
			return
		}
		if !valid || (public && client == "") {
			w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+origin+owneroauth.ResourceMetadataPath+`"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}

		if public {
			select {
			case limits.inFlight <- struct{}{}:
				defer func() { <-limits.inFlight }()
			default:
				w.WriteHeader(http.StatusTooManyRequests)
				return
			}
		}

		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		if r.Method == http.MethodPost || r.ContentLength != 0 {
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, stdioframe.MaxFrameBytes))
			var limit *http.MaxBytesError
			if errors.As(err, &limit) {
				w.WriteHeader(http.StatusRequestEntityTooLarge)
				return
			}
			if err != nil || (r.Method == http.MethodPost && !strictjson.ValidateObject(body, stdioframe.MaxFrameBytes)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}

		sdk.ServeHTTP(w, r.WithContext(context.WithValue(ctx, httpContextKey{}, ctx)))
	}), nil
}

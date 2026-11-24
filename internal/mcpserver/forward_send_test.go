package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/forwardpolicy"
	"github.com/wevial/croton-mcp/internal/stdioframe"
)

type smtpPeer struct {
	config                                   bridge.ForwardConfig
	connections, auth, clearAuth, rcpt, data atomic.Int32
	deliveries                               chan smtpDelivery
}

type smtpDelivery struct {
	from, to, message string
}

// macOS can put temporary directories under the /var symlink. Give quota
// setup the physical path, as an operator must.
func newQuotaTestDir(t *testing.T) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "state")
	if err := forwardpolicy.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	return dir
}

func newSMTPPeer(t *testing.T, mode, fault string) *smtpPeer {
	t.Helper()
	cert := httptest.NewTLSServer(nil)
	policy := cert.TLS.Clone()
	pin := fmt.Sprintf("%x", sha256.Sum256(cert.Certificate().RawSubjectPublicKeyInfo))
	cert.Close()

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	dir := newQuotaTestDir(t)
	peer := &smtpPeer{deliveries: make(chan smtpDelivery, 10), config: bridge.ForwardConfig{
		Enabled: true, SMTPPort: listener.Addr().(*net.TCPAddr).Port, SMTPTLSMode: mode,
		SMTPSPKISHA256: pin, StateDir: dir,
	}}
	if fault == "pin" {
		peer.config.SMTPSPKISHA256 = strings.Repeat("0", 64)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			peer.connections.Add(1)
			peer.serve(conn, policy, mode, fault)
			conn.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return peer
}

func (p *smtpPeer) serve(conn net.Conn, policy *tls.Config, mode, fault string) {
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	secure := mode == bridge.TLSModeImplicit
	if secure {
		conn = tls.Server(conn, policy)
	}
	if _, err := io.WriteString(conn, "220 bridge.test ready\r\n"); err != nil {
		return
	}

	reader := bufio.NewReader(conn)
	var envelope smtpDelivery
	authenticated := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		switch {
		case line == "EHLO localhost\r\n":
			reply := "250-bridge.test\r\n250 AUTH PLAIN\r\n"
			if !secure && fault != "no upgrade" {
				reply = "250-bridge.test\r\n250-AUTH PLAIN\r\n250 STARTTLS\r\n"
			}
			io.WriteString(conn, reply)
		case !secure && line == "STARTTLS\r\n":
			if fault == "upgrade refused" {
				io.WriteString(conn, "454 unavailable\r\n")
				continue
			}
			io.WriteString(conn, "220 begin TLS\r\n")
			if fault == "handshake" {
				io.WriteString(conn, "invalid TLS record\r\n")
				continue
			}
			conn = tls.Server(conn, policy)
			secure = true
			reader.Reset(conn)
		case strings.HasPrefix(line, "AUTH "):
			if !secure {
				p.clearAuth.Add(1)
				return
			}
			want := base64.StdEncoding.EncodeToString([]byte("\x00" + forwardLogin + "\x00synthetic"))
			if line != "AUTH PLAIN "+want+"\r\n" {
				io.WriteString(conn, "535 refused\r\n")
				return
			}
			authenticated = true
			p.auth.Add(1)
			io.WriteString(conn, "235 authenticated\r\n")
		case authenticated && envelope.from == "" && strings.HasPrefix(line, "MAIL FROM:"):
			envelope.from = line
			io.WriteString(conn, "250 sender ok\r\n")
		case envelope.from != "" && envelope.to == "" && strings.HasPrefix(line, "RCPT TO:"):
			envelope.to = line
			p.rcpt.Add(1)
			if fault == "rcpt refused" {
				io.WriteString(conn, "550 recipient refused\r\n")
			} else {
				io.WriteString(conn, "250 recipient ok\r\n")
			}
		case envelope.to != "" && line == "DATA\r\n":
			p.data.Add(1)
			io.WriteString(conn, "354 send message\r\n")
			var body strings.Builder
			for {
				line, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				if line == ".\r\n" {
					break
				}
				body.WriteString(strings.TrimPrefix(line, "."))
			}
			envelope.message = body.String()
			p.deliveries <- envelope
			switch fault {
			case "lost final reply":
				return
			case "final refusal":
				io.WriteString(conn, "554 refused\r\n")
			default:
				io.WriteString(conn, "250 accepted\r\n")
			}
			return
		default:
			return
		}
	}
}

func forwardRequest(t *testing.T, f *forwardFixture, r forwardResult) *mcp.CallToolRequest {
	t.Helper()
	args, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	return &mcp.CallToolRequest{Session: f.store.current.session, Params: &mcp.CallToolParamsRaw{
		Name: "send_forward", Arguments: args,
		Meta: mcp.Meta{mcp.MetaKeyClientCapabilities: map[string]any{"elicitation": map[string]any{"form": map[string]any{}}}},
	}}
}

func confirmation(t *testing.T, f *forwardFixture, cfg bridge.ForwardConfig, r forwardResult) *mcp.CallToolRequest {
	t.Helper()
	req := forwardRequest(t, f, r)
	result := f.store.send(t.Context(), cfg, req, nil)
	if result == nil || result.IsError || result.RequestState == "" || len(result.InputRequests) != 1 {
		t.Fatal("expected one form before authentication")
	}
	form, ok := result.InputRequests["confirmation"].(*mcp.ElicitParams)
	preview, _ := json.Marshal(r.Preview)
	if !ok || form.Mode != "form" || !strings.Contains(form.Message, string(preview)) {
		t.Fatal("missing complete preview")
	}
	schema, _ := json.Marshal(form.RequestedSchema)
	if string(schema) != `{"additionalProperties":false,"properties":{"confirm":{"type":"boolean"}},"required":["confirm"],"type":"object"}` {
		t.Fatal("expected one required boolean without a default")
	}
	req.Params.RequestState = result.RequestState
	req.Params.InputResponses = mcp.InputResponseMap{"confirmation": &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}}
	return req
}

func requirePreflightRefusal(t *testing.T, f *forwardFixture, cfg bridge.ForwardConfig, req *mcp.CallToolRequest) {
	t.Helper()
	result := f.store.send(t.Context(), cfg, req, nil)
	if result == nil || !result.IsError {
		t.Fatal("expected a refusal before SMTP")
	}
}

func TestForwardSend(t *testing.T) {
	// Reopen the parent's fully reserved account in a fresh process.
	if len(os.Args) > 2 && os.Args[len(os.Args)-2] == "--check-forward-quota" {
		if err := forwardpolicy.Reserve(os.Args[len(os.Args)-1], forwardLogin); err != forwardpolicy.ErrLimit {
			t.Fatal("fresh process lost reservations")
		}
		return
	}
	plainFixture := func(t *testing.T) *forwardFixture {
		t.Helper()
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure("Hello"), "1", "Hello")
		return f
	}

	t.Run("Binding", func(t *testing.T) {
		f := plainFixture(t)
		r := f.prepare(t, false)
		p := newSMTPPeer(t, "", "")
		off := p.config
		off.Enabled = false
		for _, cfg := range []bridge.ForwardConfig{{}, off} {
			requirePreflightRefusal(t, f, cfg, forwardRequest(t, f, r))
		}

		req := confirmation(t, f, p.config, r)
		original := req.Params.Arguments
		for _, invalid := range []string{
			strings.Replace(string(original), forwardTo, "trap@sender.test", 1),
			strings.Replace(string(original), `"files":0,`, "", 1),
			strings.Replace(string(original), "My note", "My Note", 1),
			strings.Replace(string(original), r.Handle, r.Handle+"x", 1),
		} {
			req.Params.Arguments = json.RawMessage(invalid)
			requirePreflightRefusal(t, f, p.config, req)
		}
		req.Params.Arguments = original
		session := req.Session
		req.Session = new(mcp.ServerSession)
		requirePreflightRefusal(t, f, p.config, req)
		req.Session = session

		f.prepare(t, false)
		requirePreflightRefusal(t, f, p.config, req)
		r = f.prepare(t, false)
		req = confirmation(t, f, p.config, r)
		f.store.mu.Lock()
		f.store.current.expires = time.Now()
		f.store.mu.Unlock()
		requirePreflightRefusal(t, f, p.config, req)

		r = f.prepare(t, false)
		req = confirmation(t, f, p.config, r)
		f.login(t, "other@bridge.test")
		requirePreflightRefusal(t, f, p.config, req)
		f.login(t, forwardLogin)
		if p.connections.Load() != 0 {
			t.Fatal("invalid handle opened SMTP")
		}

		req = confirmation(t, f, p.config, r)
		var successes atomic.Int32
		var group sync.WaitGroup
		for range 8 {
			group.Go(func() {
				result := f.store.send(t.Context(), p.config, req, nil)
				if !result.IsError {
					requireSendOutcome(t, result, "accepted")
					successes.Add(1)
				}
			})
		}
		group.Wait()
		requirePreflightRefusal(t, f, p.config, req)
		if successes.Load() != 1 || p.data.Load() != 1 || p.connections.Load() != 1 || p.auth.Load() != 1 {
			t.Fatal("concurrent approval did not spend exactly one handle")
		}

		if _, err := f.client.CallTool(t.Context(), &mcp.CallToolParams{Name: "send_forward", Arguments: r}); err == nil {
			t.Fatal("default stdio exposed submission")
		}
		const origin = "http://127.0.0.1:8787"
		handler, err := NewHTTP(origin, false, f.store.adapter, nil, time.Second, func(string) (string, bool) { return "synthetic", true }, http.NotFoundHandler())
		if err != nil {
			t.Fatal(err)
		}
		httpReq := httptest.NewRequest(http.MethodPost, origin+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"send_forward","arguments":`+string(original)+`}}`))
		httpReq.Header.Set("Authorization", "Bearer synthetic")
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Accept", "application/json, text/event-stream")
		httpReq.Header.Set("MCP-Protocol-Version", "2025-11-25")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, httpReq)
		var rpc struct{ Error *struct{ Message string } }
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rpc) != nil || rpc.Error == nil || !strings.Contains(rpc.Error.Message, "send_forward") {
			t.Fatal("expected HTTP tools/call to refuse the unknown tool", w.Code)
		}
	})

	t.Run("Confirmation", func(t *testing.T) {
		f := plainFixture(t)
		r := f.prepare(t, false)
		p := newSMTPPeer(t, "", "")
		req := forwardRequest(t, f, r)
		req.Params.Meta = nil
		requirePreflightRefusal(t, f, p.config, req)

		for _, res := range []*mcp.ElicitResult{
			{Action: "decline", Content: map[string]any{"confirm": true}},
			{Action: "cancel", Content: map[string]any{"confirm": true}},
			{Action: "accept"},
			{Action: "accept", Content: map[string]any{"confirm": false}},
			{Action: "accept", Content: map[string]any{"confirm": "true"}},
			{Action: "accept", Content: map[string]any{"confirm": true, "to": forwardTo}},
		} {
			req = confirmation(t, f, p.config, r)
			req.Params.InputResponses["confirmation"] = res
			requirePreflightRefusal(t, f, p.config, req)
			req.Params.InputResponses["confirmation"] = &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}
			requirePreflightRefusal(t, f, p.config, req)
		}
		if p.connections.Load() != 0 {
			t.Fatal("unconfirmed request opened SMTP")
		}
		for range 10 {
			if err := forwardpolicy.Reserve(p.config.StateDir, forwardLogin); err != nil {
				t.Fatal("declined form consumed quota", err)
			}
		}
	})

	t.Run("Policy", func(t *testing.T) {
		f := plainFixture(t)
		p := newSMTPPeer(t, "", "lost final reply")
		// Even an uncertain post-DATA result consumes one durable slot.
		r := f.prepare(t, false)
		requireSendOutcome(t, f.store.send(t.Context(), p.config, confirmation(t, f, p.config, r), nil), "unknown")
		var group sync.WaitGroup
		var reserved atomic.Int32
		for range 12 {
			group.Go(func() {
				err := forwardpolicy.Reserve(p.config.StateDir, forwardLogin)
				if err == nil {
					reserved.Add(1)
				} else if err != forwardpolicy.ErrLimit {
					t.Error(err)
				}
			})
		}
		group.Wait()
		if reserved.Load() != 9 {
			t.Fatal("concurrent reservations exceeded or fell short of the cap")
		}
		r = f.prepare(t, false)
		requireToolError(t, f.store.send(t.Context(), p.config, confirmation(t, f, p.config, r), nil), "quota_reached")
		if p.connections.Load() != 1 {
			t.Fatal("quota refusal opened SMTP")
		}
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		if err := exec.CommandContext(t.Context(), exe, "-test.run=^TestForwardSend$", "--", "--check-forward-quota", p.config.StateDir).Run(); err != nil {
			t.Fatal("failed quota restart check", err)
		}
		if err := forwardpolicy.Reserve(p.config.StateDir, "second@bridge.test"); err != nil {
			t.Fatal(err)
		}
		t.Run("window", func(t *testing.T) {
			dir := newQuotaTestDir(t)
			synctest.Test(t, func(t *testing.T) {
				for range 10 {
					if err := forwardpolicy.Reserve(dir, forwardLogin); err != nil {
						t.Fatal(err)
					}
				}

				time.Sleep(24*time.Hour - time.Nanosecond)
				if err := forwardpolicy.Reserve(dir, forwardLogin); err != forwardpolicy.ErrLimit {
					t.Fatal("released a slot early", err)
				}
				time.Sleep(time.Nanosecond)
				if err := forwardpolicy.Reserve(dir, forwardLogin); err != nil {
					t.Fatal("expired slot unavailable", err)
				}
			})
		})
		for _, kind := range []string{"missing", "empty", "damaged", "loose permissions", "symlink"} {
			t.Run(kind, func(t *testing.T) {
				dir := newQuotaTestDir(t)
				path := filepath.Join(dir, "quota.db")
				var err error
				switch kind {
				case "missing":
					err = os.Remove(path)
				case "empty":
					err = os.Truncate(path, 0)
				case "damaged":
					err = os.WriteFile(path, []byte("broken"), 0600)
				case "loose permissions":
					err = os.Chmod(path, 0644)
				case "symlink":
					if err = os.Rename(path, path+".old"); err == nil {
						err = os.Symlink(path+".old", path)
					}
				}
				if err != nil {
					t.Fatal(err)
				}
				if forwardpolicy.Reserve(dir, forwardLogin) != forwardpolicy.ErrUnavailable || forwardpolicy.Initialize(dir) != forwardpolicy.ErrUnavailable {
					t.Fatal("unsafe state accepted or reinitialized")
				}
			})
		}
	})

	t.Run("TLS", func(t *testing.T) {
		f := plainFixture(t)
		for _, tc := range []struct{ mode, fault string }{
			{"", ""}, {"implicit", ""}, {"", "no upgrade"}, {"", "upgrade refused"}, {"", "handshake"}, {"", "pin"}, {"implicit", "pin"},
		} {
			t.Run(tc.mode+"/"+tc.fault, func(t *testing.T) {
				p := newSMTPPeer(t, tc.mode, tc.fault)
				r := f.prepare(t, false)
				req := confirmation(t, f, p.config, r)
				result := f.store.send(t.Context(), p.config, req, nil)
				if tc.fault == "" {
					requireSendOutcome(t, result, "accepted")
					if p.auth.Load() != 1 {
						t.Fatal("missing authenticated TLS session")
					}
				} else {
					requireSendOutcome(t, result, "not_submitted")
					if p.auth.Load() != 0 {
						t.Fatal("sent AUTH after failed TLS negotiation")
					}
				}
				if p.connections.Load() != 1 || p.clearAuth.Load() != 0 {
					t.Fatal("unexpected connection count or plaintext AUTH")
				}
			})
		}
	})

	t.Run("Wire", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.setWrappedText(t, strings.Repeat("long é", 800)+"\r\n.\r\n..last")
		for _, tc := range []struct{ version, fault, want string }{
			{"2026-07-28", "", "accepted"},
			{"2025-11-25", "", "accepted"},
			{"2026-07-28", "rcpt refused", "not_submitted"},
			{"2026-07-28", "lost final reply", "unknown"},
			{"2026-07-28", "final refusal", "unknown"},
		} {
			t.Run(tc.version+"/"+tc.want+"/"+tc.fault, func(t *testing.T) {
				p := newSMTPPeer(t, "", tc.fault)
				store := NewForwardPreviews(f.store.adapter)
				store.Submission = p.config
				var audit bytes.Buffer
				server := New(Options{Mail: store.adapter, Forward: store, Audit: NewAuditor(&audit)})
				local, remote := net.Pipe()
				ss, err := server.sdk.Connect(t.Context(), stdioframe.NewTransport(local, local), nil)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { ss.Close() })

				var forms atomic.Int32
				client := mcp.NewClient(&mcp.Implementation{Name: "synthetic", Version: "1"}, &mcp.ClientOptions{
					ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
						forms.Add(1)
						return &mcp.ElicitResult{Action: "accept", Content: map[string]any{"confirm": true}}, nil
					},
				})
				cs, err := client.Connect(t.Context(), &mcp.IOTransport{Reader: remote, Writer: remote}, &mcp.ClientSessionOptions{ProtocolVersion: tc.version})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { cs.Close() })

				var r forwardResult
				callJSON(t, cs, forwardTool, f.arguments(false), &r)
				stored, ok := store.lookup(r, time.Now())
				if !ok {
					t.Fatal("missing stored MIME")
				}
				requireSendOutcome(t, callTool(t, cs, "send_forward", r), tc.want)
				requireToolError(t, callTool(t, cs, "send_forward", r), errUnavailable)
				if forms.Load() != 1 || p.connections.Load() != 1 || p.rcpt.Load() != 1 {
					t.Fatal("expected one form and one SMTP recipient across both calls")
				}
				if tc.fault == "rcpt refused" {
					if p.data.Load() != 0 {
						t.Fatal("sent DATA after RCPT refusal")
					}
					return
				}
				if p.data.Load() != 1 {
					t.Fatal("expected one DATA transaction")
				}
				got := <-p.deliveries
				if got.from != "MAIL FROM:<"+forwardLogin+">\r\n" || got.to != "RCPT TO:<"+forwardTo+">\r\n" || got.message != stored {
					t.Fatal("SMTP envelope or MIME differs from the stored forward")
				}
				msg, err := mail.ReadMessage(strings.NewReader(got.message))
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
				if err != nil || string(body) != r.Preview.Body {
					t.Fatal("SMTP altered the approved body")
				}
				event := auditEvent{Event: "tool_call", Tool: "send_forward", Outcome: tc.want}
				encoded, _ := json.Marshal(event)
				if !strings.Contains(audit.String(), string(encoded)) || strings.ContainsAny(audit.String(), "@<>") || strings.Contains(audit.String(), r.Handle) {
					t.Fatal("missing content-free submission audit")
				}
			})
		}
	})
}

func requireSendOutcome(t *testing.T, result *mcp.CallToolResult, want string) {
	t.Helper()
	var got struct{ Outcome string }
	decodeResult(t, result, &got)
	if got.Outcome != want {
		t.Errorf("outcome = %q, want %q", got.Outcome, want)
	}
}

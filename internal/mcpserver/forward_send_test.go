package mcpserver

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
)

type smtpPeer struct {
	config                       bridge.ForwardConfig
	connections, auth, clearAuth atomic.Int32
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
	dir := filepath.Join(t.TempDir(), "state")
	if err := forwardpolicy.Initialize(dir); err != nil {
		t.Fatal(err)
	}
	peer := &smtpPeer{config: bridge.ForwardConfig{
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
			p.auth.Add(1)
			io.WriteString(conn, "235 authenticated\r\n")
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
	conn, msg, result := f.store.openApproved(t.Context(), cfg, req)
	if conn != nil || msg != nil || result == nil || result.IsError || result.RequestState == "" || len(result.InputRequests) != 1 {
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
	conn, msg, result := f.store.openApproved(t.Context(), cfg, req)
	if conn != nil || msg != nil || result == nil || !result.IsError {
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
	t.Run("Binding", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure("Hello"), "1", "Hello")
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
				conn, msg, _ := f.store.openApproved(t.Context(), p.config, req)
				if conn != nil {
					defer conn.Close()
					if msg == nil || msg.forwardResult != r {
						t.Error("stored message identity changed")
					}
					successes.Add(1)
				}
			})
		}
		group.Wait()
		requirePreflightRefusal(t, f, p.config, req)
		if successes.Load() != 1 || p.connections.Load() != 1 || p.auth.Load() != 1 {
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
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure("Hello"), "1", "Hello")
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
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure("Hello"), "1", "Hello")
		p := newSMTPPeer(t, "", "upgrade refused")
		// A committed slot stays spent even when the transport fails.
		r := f.prepare(t, false)
		requirePreflightRefusal(t, f, p.config, confirmation(t, f, p.config, r))
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
		requirePreflightRefusal(t, f, p.config, confirmation(t, f, p.config, r))
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
			dir := filepath.Join(t.TempDir(), "state")
			if err := forwardpolicy.Initialize(dir); err != nil {
				t.Fatal(err)
			}
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
				dir := filepath.Join(t.TempDir(), "quota")
				if err := forwardpolicy.Initialize(dir); err != nil {
					t.Fatal(err)
				}
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
		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure("Hello"), "1", "Hello")
		for _, tc := range []struct{ mode, fault string }{
			{"", ""}, {"implicit", ""}, {"", "no upgrade"}, {"", "upgrade refused"}, {"", "handshake"}, {"", "pin"}, {"implicit", "pin"},
		} {
			t.Run(tc.mode+"/"+tc.fault, func(t *testing.T) {
				p := newSMTPPeer(t, tc.mode, tc.fault)
				r := f.prepare(t, false)
				req := confirmation(t, f, p.config, r)
				conn, _, result := f.store.openApproved(t.Context(), p.config, req)
				if tc.fault == "" {
					if conn == nil || result != nil || p.auth.Load() != 1 {
						t.Fatal("missing authenticated TLS session")
					}
					conn.Close()
				} else if conn != nil || result == nil || !result.IsError || p.auth.Load() != 0 {
					t.Fatal("accepted failed TLS negotiation")
				}
				if p.connections.Load() != 1 || p.clearAuth.Load() != 0 {
					t.Fatal("unexpected connection count or plaintext AUTH")
				}
			})
		}
	})
}

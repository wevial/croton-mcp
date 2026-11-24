package mcpserver

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	"net/mail"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/stdioframe"
)

const (
	forwardTool   = "prepare_forward"
	utf8Params    = `("charset" "utf-8")`
	forwardLogin  = "owner@bridge.test"
	forwardTo     = "reviewer@recipient.test"
	forwardFrom   = "Sender, Original <source@sender.test>"
	declaredDate  = "Thu, 01 Jan 2026 03:04:05 +0530"
	forwardHeader = "From: =?UTF-8?Q?Sender=2C_Original?= <source@sender.test>\r\nDate: " + declaredDate + "\r\nSubject: Original  subject\r\nSubject: Later subject\r\nTo: trap@sender.test\r\nReply-To: trap@sender.test\r\nBcc: trap@sender.test\r\nMessage-ID: <original@sender.test>\r\n\r\n"
)

func textStructure(subtype, encoding, parameters string, size int) string {
	return fmt.Sprintf(`("text" %q %s NIL NIL %q %d 1 NIL NIL NIL NIL)`, subtype, parameters, encoding, size)
}

func plainStructure(body string) string {
	return textStructure("plain", "8bit", utf8Params, len(body))
}

func multiStructure(kind string, children ...string) string {
	return "(" + strings.Join(children, " ") + " \"" + kind + "\" NIL NIL NIL NIL)"
}

// Only a matching numbered PEEK can return the configured body literal.
type forwardFixture struct {
	mu                               sync.Mutex
	conn                             net.Conn
	generation                       int
	header, body, section, structure string
	commands                         []string
	credentials                      string
	store                            *ForwardPreviews
	client                           *mcp.ClientSession
	id                               string
}

func newForwardFixture(t *testing.T, bounds bridge.BoundsPatch) *forwardFixture {
	t.Helper()

	cert := httptest.NewTLSServer(nil)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", cert.TLS.Clone())
	pin := sha256.Sum256(cert.Certificate().RawSubjectPublicKeyInfo)
	cert.Close()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	file := filepath.Join(t.TempDir(), "synthetic.json")
	f := &forwardFixture{generation: 42, header: forwardHeader, body: "Hello", credentials: file}
	f.login(t, forwardLogin)
	cat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatal(err)
	}
	config := bridge.Config{Bounds: bounds, IMAP: bridge.IMAPConfig{
		Port: listener.Addr().(*net.TCPAddr).Port, TLSMode: bridge.TLSModeImplicit,
		TLS: bridge.TLSConfig{SPKISHA256: fmt.Sprintf("%x", pin)}, CredentialCommand: []string{cat, file},
	}}
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conn = conn
			f.mu.Unlock()
			go f.serve(conn)
		}
	}()

	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = adapter.Close() })
	f.store = NewForwardPreviews(adapter)
	f.client = connectTestClient(t, Options{Mail: adapter, Forward: f.store})
	var found wireSearch
	callJSON(t, f.client, "search_mail", map[string]any{"mailbox": "INBOX"}, &found)
	if len(found.Results) != 1 || found.Results[0].ReadState != "unread" {
		t.Fatal("expected one unread synthetic message")
	}
	f.id = found.Results[0].ID
	return f
}

func (f *forwardFixture) login(t *testing.T, address string) {
	t.Helper()
	data, err := json.Marshal(map[string]string{"username": address, "password": "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.credentials, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *forwardFixture) setHeader(header string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.header = header
}

func (f *forwardFixture) set(structure, section, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.structure, f.section, f.body = structure, section, body
	f.commands = nil
}

var forwardPeek = regexp.MustCompile(`^UID FETCH 1 \(UID BODY\.PEEK\[([.0-9]+)\]<0\.([0-9]+)>\)$`)

func (f *forwardFixture) serve(conn net.Conn) {
	defer conn.Close()
	fmt.Fprint(conn, "* OK fixture ready\r\n")
	reader := bufio.NewReader(conn)
	authenticated := false
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		tag, command, _ := strings.Cut(strings.TrimSpace(line), " ")
		var response string
		switch {
		case command == "CAPABILITY":
			response = "* CAPABILITY IMAP4rev1\r\n"
		case strings.HasPrefix(command, "LOGIN "):
			authenticated = true
		case command == "LOGOUT":
			fmt.Fprintf(conn, "* BYE closing\r\n%s OK done\r\n", tag)
			return
		case authenticated:
			response = f.reply(command)
			if response == "BAD" {
				fmt.Fprintf(conn, "%s BAD unexpected command\r\n", tag)
				continue
			}
		default:
			return
		}
		if _, err := fmt.Fprintf(conn, "%s%s OK done\r\n", response, tag); err != nil {
			return
		}
	}
}

func (f *forwardFixture) reply(command string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, command)
	switch {
	case command == "NOOP":
		return ""
	case strings.HasPrefix(command, "EXAMINE "):
		return fmt.Sprintf("* 1 EXISTS\r\n* OK [UIDVALIDITY %d] generation\r\n* OK [UIDNEXT 2] next\r\n", f.generation)
	case strings.HasPrefix(command, "UID SEARCH "):
		return "* SEARCH 1\r\n"
	case command == "UID FETCH 1 (UID BODYSTRUCTURE)":
		return "* 1 FETCH (UID 1 BODYSTRUCTURE " + f.structure + ")\r\n"
	case command == "UID FETCH 1 (UID FLAGS RFC822.SIZE BODY.PEEK[HEADER])",
		command == "UID FETCH 1 (UID RFC822.SIZE FLAGS BODY.PEEK[HEADER])":
		return fmt.Sprintf("* 1 FETCH (UID 1 FLAGS () RFC822.SIZE %d BODY[HEADER] {%d}\r\n%s)\r\n", len(f.header)+len(f.body), len(f.header), f.header)
	}

	match := forwardPeek.FindStringSubmatch(command)
	if match == nil || match[1] != f.section {
		return "BAD"
	}
	n, _ := strconv.Atoi(match[2])
	content := f.body[:min(n, len(f.body))]
	return fmt.Sprintf("* 1 FETCH (UID 1 BODY[%s]<0> {%d}\r\n%s)\r\n", match[1], len(content), content)
}

func (f *forwardFixture) arguments(consent bool) map[string]any {
	return map[string]any{"messageId": f.id, "to": forwardTo, "text": "My note", "text_only": consent}
}

func (f *forwardFixture) prepare(t *testing.T, consent bool) forwardResult {
	t.Helper()
	var result forwardResult
	callJSON(t, f.client, forwardTool, f.arguments(consent), &result)
	if result.Handle == "" {
		t.Fatal("missing handle")
	}
	return result
}

func (f *forwardFixture) refuse(t *testing.T, args map[string]any) {
	t.Helper()
	result := callTool(t, f.client, forwardTool, args)
	if !result.IsError || strings.Contains(resultText(t, result), "handle") {
		t.Fatal("expected a tool error without a handle")
	}
}

func TestForwardPreview(t *testing.T) {
	const file = `("application" "pdf" NIL NIL NIL "base64" 7000 NIL ("attachment" ("filename" "file.pdf")) NIL NIL)`
	const nested = `("message" "rfc822" NIL NIL NIL "7bit" 423 (NIL NIL NIL NIL NIL NIL NIL NIL NIL NIL) ("text" "plain" NIL NIL NIL "7bit" 10 1) 5 NIL NIL NIL NIL)`

	t.Run("Wire", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		// Search metadata caps From at 512 bytes. The bounded header
		// projection keeps this entire display name, including its comma.
		namePrefix := strings.Repeat("Archive contact ", 33)
		f.setHeader(strings.Replace(forwardHeader, "From: ", "From: "+namePrefix, 1))

		const raw = "one=0A=c3=a9\r\nlast\r\n\r\n"
		plain := textStructure("plain", "quoted-printable", utf8Params, len(raw))
		alt := multiStructure("alternative", plain, textStructure("html", "7bit", "NIL", 91))
		for _, tc := range []struct {
			name, structure, section string
			omissions                bridge.ForwardOmissions
		}{
			{"plain", plain, "1", bridge.ForwardOmissions{}},
			{"alternative", alt, "1", bridge.ForwardOmissions{HTMLAlternatives: 1}},
			{"attached message", multiStructure("mixed", alt, file, nested), "1.1", bridge.ForwardOmissions{HTMLAlternatives: 1, Files: 1, AttachedMessages: 1}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(tc.structure, tc.section, raw)
				result := f.prepare(t, tc.omissions.Files != 0)
				want := forwardPreview{From: forwardLogin, To: forwardTo, Subject: "Fwd: Original subject", Omissions: tc.omissions,
					Body: "My note\r\n\r\n---------- Forwarded message ----------\r\nFrom: " + namePrefix + forwardFrom + "\r\nDate: " + declaredDate + "\r\nSubject: Original  subject\r\n\r\none\r\né\r\nlast\r\n\r\n"}
				if result.Preview != want {
					t.Fatalf("preview = %+v", result.Preview)
				}
				f.mu.Lock()
				defer f.mu.Unlock()
				if len(f.commands) != 5 || !strings.HasPrefix(f.commands[0], "EXAMINE ") ||
					!strings.HasPrefix(f.commands[1], "UID SEARCH ") || !strings.Contains(f.commands[2], "BODYSTRUCTURE") ||
					!strings.Contains(f.commands[3], "BODY.PEEK[HEADER]") || !strings.Contains(f.commands[4], "BODY.PEEK["+tc.section+"]") {
					t.Fatalf("unexpected transcript %q", f.commands)
				}
			})
		}
	})

	t.Run("Refuse", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		plain := plainStructure("ok")
		for _, tc := range []struct {
			name, structure, raw string
			consent              bool
		}{
			{"HTML only", textStructure("html", "8bit", utf8Params, 2), "ok", true},
			{"ambiguous", multiStructure("mixed", plain, plain), "ok", true},
			{"signed", multiStructure("signed", plain, file), "ok", true},
			{"flowed", textStructure("plain", "7bit", `("format" "flowed")`, 2), "ok", true},
			{"primary nested", multiStructure("mixed", nested, plain), "ok", true},
			{"attachment", multiStructure("mixed", plain, file), "ok", false},
			{"attached message", multiStructure("mixed", plain, nested), "ok", false},
			{"short", plain, "o", false}, {"underdeclared", plain, "okay", false},
			{"oversize", textStructure("plain", "8bit", utf8Params, 262145), "ok", false},
			{"bad base64", textStructure("plain", "base64", utf8Params, 4), "x!==", false},
			{"bad QP", textStructure("plain", "quoted-printable", utf8Params, 3), "=xz", false},
			{"damaged name parameter", textStructure("plain", "7bit", `("name*" "unknown''file.txt")`, 2), "ok", true},
			{"damaged charset parameter", textStructure("plain", "8bit", `("charset*" "unknown''utf-8")`, 2), "ok", false},
			{"ambiguous format", textStructure("plain", "8bit", `("format" "flowed" "format*" "us-ascii''fixed")`, 2), "ok", false},
			{"lossy", plainStructure("\xff"), "\xff", false},
			{"bad charset", textStructure("plain", "8bit", `("charset" "bogus")`, 2), "ok", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(tc.structure, "1", tc.raw)
				f.refuse(t, f.arguments(tc.consent))
			})
		}
		f.set(plain, "1", "ok")
		for _, change := range []struct{ old, replacement string }{
			{"Original  subject", "=?UTF-8?Q?=ZZ?="},
			{"Original  subject", "=?UTF-8?B?/w==?="},
			{"From: ", "From: other@sender.test\r\nFrom: "},
			{"Date: ", "Date: invalid\r\nDate: "},
			{declaredDate, declaredDate + " (bad \xff)"},
		} {
			f.setHeader(strings.Replace(forwardHeader, change.old, change.replacement, 1))
			f.refuse(t, f.arguments(false))
		}
		f.mu.Lock()
		f.header = forwardHeader
		f.commands = nil
		f.mu.Unlock()

		for _, to := range []string{"", "A <a@b.test>", "a@b.test,c@d.test", "a@b.test\r\nBcc: trap@sender.test", "a@b.test (comment)",
			"recipient@bad/path.test", "recipient@-bad.test", "recipient@" + strings.Repeat("x", 64) + ".test"} {
			args := f.arguments(false)
			args["to"] = to
			f.refuse(t, args)
		}
		f.mu.Lock()
		if len(f.commands) != 0 {
			t.Error("invalid recipient reached Bridge")
		}
		f.generation++
		f.mu.Unlock()
		requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errStaleID)

		t.Run("underdeclared at section limit", func(t *testing.T) {
			f := newForwardFixture(t, bridge.BoundsPatch{MaxBodyBytes: bridge.Int(4)})
			f.set(plainStructure("four"), "1", "four!")
			f.refuse(t, f.arguments(false))
		})
	})

	t.Run("Handle", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		list, err := f.client.ListTools(t.Context(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Tools) != 7 {
			t.Fatalf("unexpected catalog size %d", len(list.Tools))
		}
		for _, tool := range list.Tools {
			if !tool.Annotations.ReadOnlyHint || strings.Contains(tool.Name, "send") {
				t.Fatal("unexpected write tool")
			}
		}

		// Exercise subject folding and QP soft breaks alongside hard breaks.
		f.setHeader(strings.Replace(forwardHeader, "Original  subject", strings.Repeat("Résumé", 180), 1))
		sourceText := strings.Repeat("é=", 600) + "\r\n\r\n"
		f.set(plainStructure(sourceText), "1", sourceText)
		first := f.prepare(t, false)
		wire, ok := f.store.lookup(first, time.Now())
		if !ok {
			t.Fatal("missing prepared MIME")
		}
		message, err := mail.ReadMessage(strings.NewReader(wire))
		if err != nil {
			t.Fatal(err)
		}
		subject, err := new(mime.WordDecoder).DecodeHeader(message.Header.Get("Subject"))
		body, readErr := io.ReadAll(quotedprintable.NewReader(message.Body))
		if err != nil || readErr != nil || subject != first.Preview.Subject || string(body) != first.Preview.Body ||
			message.Header.Get("To") != forwardTo || message.Header.Get("From") != forwardLogin ||
			message.Header.Get("Content-Transfer-Encoding") != "quoted-printable" ||
			message.Header.Get("Message-ID") == "" || message.Header.Get("Message-ID") == "<original@sender.test>" ||
			message.Header.Get("Reply-To") != "" || message.Header.Get("Cc") != "" || message.Header.Get("Bcc") != "" {
			t.Fatal("stored message differs from the approved envelope or preview")
		}
		for line := range strings.SplitSeq(wire, "\r\n") {
			if len(line) > 998 {
				t.Fatal("overlong serialized line")
			}
		}
		tampered := first
		tampered.Preview.To = "other@recipient.test"
		if _, ok := f.store.lookup(tampered, time.Now()); ok {
			t.Fatal("recipient change accepted")
		}
		second := f.prepare(t, false)
		if _, ok := f.store.lookup(first, time.Now()); ok || first.Handle == second.Handle {
			t.Fatal("superseded handle accepted")
		}
		secondWire, ok := f.store.lookup(second, time.Now())
		secondMessage, err := mail.ReadMessage(strings.NewReader(secondWire))
		if !ok || err != nil || secondMessage.Header.Get("Message-ID") == message.Header.Get("Message-ID") {
			t.Fatal("expected freshly serialized Message-ID")
		}
		if _, ok := NewForwardPreviews(f.store.adapter).lookup(second, time.Now()); ok {
			t.Fatal("handle survived restart")
		}
		if _, ok := f.store.lookup(second, time.Now().Add(2*time.Minute)); ok {
			t.Fatal("expired handle accepted")
		}

		for _, login := range []string{forwardLogin, "second@bridge.test"} {
			last := f.prepare(t, false)
			f.login(t, login)
			f.mu.Lock()
			conn := f.conn
			f.mu.Unlock()
			_ = conn.Close()
			if _, err := f.store.adapter.GetMessageMetadata(t.Context(), f.id); err != nil {
				t.Fatal(err)
			}
			if _, ok := f.store.lookup(last, time.Now()); ok {
				t.Fatal("handle survived reconnect")
			}
			if next := f.prepare(t, false); next.Preview.From != login {
				t.Fatal("preview reused previous login")
			}
		}
	})

	t.Run("Frame", func(t *testing.T) {
		frameSize := func(r forwardResult) int {
			data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "send_forward", "arguments": r}})
			if err != nil {
				t.Fatal(err)
			}
			return len(data) + 1
		}

		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure(""), "1", "")
		room := stdioframe.MaxFrameBytes - frameSize(f.prepare(t, false))
		for _, unit := range []string{"p", "\"\\<&"} {
			encoded, _ := json.Marshal(unit)
			cost := len(encoded) - 2
			content := strings.Repeat(unit, room/cost) + strings.Repeat("x", room%cost)
			f.set(plainStructure(content), "1", content)
			exact := f.prepare(t, false)
			if frameSize(exact) != stdioframe.MaxFrameBytes || !strings.HasSuffix(exact.Preview.Body, content) {
				t.Fatal("exact frame preview was clipped")
			}
			content += "x"
			f.set(plainStructure(content), "1", content)
			requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errBoundsExceeded)
			if _, ok := f.store.lookup(exact, time.Now()); !ok {
				t.Fatal("failed preview displaced prior approval")
			}
		}
	})
}

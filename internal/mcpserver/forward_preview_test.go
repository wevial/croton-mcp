package mcpserver

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"encoding/base64"
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
	"slices"
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
	plainMIME     = "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n"
	qpMIME        = "Content-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n"
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
	mu                      sync.Mutex
	conn                    net.Conn
	generation              int
	header, body, structure string
	sections                []string
	mimeFields              map[string]string
	commands                []string
	credentials             string
	store                   *ForwardPreviews
	client                  *mcp.ClientSession
	id                      string
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

func (f *forwardFixture) set(structure, section, body string, otherSections ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.structure, f.body = structure, body
	f.sections = append([]string{section}, otherSections...)
	f.mimeFields = map[string]string{"HEADER": plainMIME}
	f.commands = nil
}

func (f *forwardFixture) setWrappedText(t *testing.T, body string) {
	t.Helper()

	var raw strings.Builder
	writer := quotedprintable.NewWriter(&raw)
	if _, err := io.WriteString(writer, body); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	f.set(textStructure("plain", "quoted-printable", utf8Params, raw.Len()), "1", raw.String())
	f.setMIME(map[string]string{"HEADER": qpMIME})
}

func (f *forwardFixture) setMIME(fields map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.mimeFields = fields
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

	if section, ok := strings.CutPrefix(command, "UID FETCH 1 (UID BODY.PEEK["); ok {
		if section, ok = strings.CutSuffix(section, "])"); ok {
			fields, exists := f.mimeFields[section]
			if !exists {
				return "BAD"
			}
			header := fields + "\r\n"
			if section == "HEADER" {
				header = fields + f.header
			}
			return fmt.Sprintf("* 1 FETCH (UID 1 BODY[%s] {%d}\r\n%s)\r\n", section, len(header), header)
		}
	}

	match := forwardPeek.FindStringSubmatch(command)
	if match == nil || !slices.Contains(f.sections, match[1]) {
		return "BAD"
	}
	n, _ := strconv.Atoi(match[2])
	content := f.body[:min(n, len(f.body))]
	return fmt.Sprintf("* 1 FETCH (UID 1 BODY[%s]<0> {%d}\r\n%s)\r\n", match[1], len(content), content)
}

func (f *forwardFixture) requireTranscript(t *testing.T, fetches ...string) {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()

	want := append([]string{"EXAMINE INBOX", "UID SEARCH UID 1", "UID FETCH 1 (UID BODYSTRUCTURE)"}, fetches...)
	if !slices.Equal(f.commands, want) {
		t.Fatalf("Bridge commands = %q, want %q", f.commands, want)
	}
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
	const imagePart = `("image" "png" NIL "<logo@sender.test>" NIL "base64" 420 NIL ("inline" NIL) NIL NIL)`

	t.Run("Wire", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		// Search metadata caps From at 512 bytes. The bounded header
		// projection keeps this entire display name, including its comma.
		namePrefix := "Archive\tcontact " + strings.Repeat("Archive contact ", 32)
		header := strings.Replace(forwardHeader, "From: ", "From: "+namePrefix, 1)

		const raw = "one=0A=C3=A9\r\nlast\r\n\r\n"
		const altMIME = "Content-Type: multipart/alternative; boundary=alt\r\n"
		const htmlMIME = "Content-Type: text/html\r\n"
		const tabDate = "Thu, 01 Jan 2026 03:04:05\t+0530"
		const literalSubject = "Is =?UTF-8?Q? an encoded-word prefix?"
		plain := textStructure("plain", "quoted-printable", utf8Params, len(raw))
		html := textStructure("html", "7bit", "NIL", 91)
		alt := multiStructure("alternative", plain, html)
		related := fmt.Sprintf(`(%s %s "related" ("type" "text/html" "boundary" "rel") NIL NIL NIL)`, html, imagePart)
		const namedHTML = `("text" "html" NIL "<html@sender.test>" NIL "7bit" 91 1 NIL NIL NIL NIL)`
		namedRelated := fmt.Sprintf(`(%s %s "related" ("type" "text/html" "boundary" "rel" "start" "<html@sender.test>") NIL NIL NIL)`, imagePart, namedHTML)
		for _, tc := range []struct {
			name, structure, section  string
			rawSubject, subject, date string
			omissions                 bridge.ForwardOmissions
			fields                    map[string]string
			sections                  []string
		}{
			{
				"plain", plain, "1", "What does x=? mean", "What does x=? mean", tabDate, bridge.ForwardOmissions{},
				map[string]string{"HEADER": qpMIME}, []string{"HEADER"},
			},
			{
				"literal Subject prefix", plain, "1", literalSubject, literalSubject, declaredDate, bridge.ForwardOmissions{},
				map[string]string{"HEADER": qpMIME}, []string{"HEADER"},
			},
			{
				"alternative", alt, "1", "=?UTF-8?Q?What_does_x=3D=3F__mean?=", "What does x=?  mean", declaredDate, bridge.ForwardOmissions{HTMLAlternatives: 1},
				map[string]string{"HEADER": altMIME, "1.MIME": qpMIME, "2.MIME": htmlMIME},
				[]string{"HEADER", "1.MIME", "2.MIME"},
			},
			{
				"related HTML alternative", multiStructure("alternative", plain, related), "1",
				"Original  subject", "Original  subject", declaredDate, bridge.ForwardOmissions{HTMLAlternatives: 1, Files: 1},
				map[string]string{"HEADER": altMIME, "1.MIME": qpMIME, "2.MIME": "Content-Type: multipart/related; boundary=rel; type=\"text/html\"\r\n", "2.1.MIME": htmlMIME},
				[]string{"HEADER", "1.MIME", "2.MIME", "2.1.MIME"},
			},
			{
				"related HTML by Content-ID", multiStructure("alternative", plain, namedRelated), "1",
				"Original  subject", "Original  subject", declaredDate, bridge.ForwardOmissions{HTMLAlternatives: 1, Files: 1},
				map[string]string{"HEADER": altMIME, "1.MIME": qpMIME, "2.MIME": "Content-Type: multipart/related; boundary=rel; type=\"text/html\"; start=\"<html@sender.test>\"\r\n",
					"2.2.MIME": htmlMIME + "Content-ID: <html@sender.test>\r\n"},
				[]string{"HEADER", "1.MIME", "2.MIME", "2.2.MIME"},
			},
			{
				"attached message", multiStructure("mixed", alt, file, nested), "1.1",
				"=?UTF-8?B?77+9?=", "�", declaredDate,
				bridge.ForwardOmissions{HTMLAlternatives: 1, Files: 1, AttachedMessages: 1},
				map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=mixed\r\n",
					"1.MIME": altMIME, "1.1.MIME": qpMIME, "1.2.MIME": htmlMIME},
				[]string{"HEADER", "1.MIME", "1.1.MIME", "1.2.MIME"},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.setHeader(strings.NewReplacer("Original  subject", tc.rawSubject, declaredDate, tc.date).Replace(header))
				f.set(tc.structure, tc.section, raw)
				f.setMIME(tc.fields)
				result := f.prepare(t, tc.omissions.Files != 0)
				want := forwardPreview{From: forwardLogin, To: forwardTo, Subject: "Fwd: " + strings.ReplaceAll(tc.subject, "  ", " "), Omissions: tc.omissions,
					Body: "My note\r\n\r\n---------- Forwarded message ----------\r\nFrom: " + namePrefix + forwardFrom + "\r\nDate: " + tc.date + "\r\nSubject: " + tc.subject + "\r\n\r\none\r\né\r\nlast\r\n\r\n"}
				if result.Preview != want {
					t.Fatalf("preview = %+v", result.Preview)
				}
				var fetches []string
				for _, section := range tc.sections {
					fetches = append(fetches, "UID FETCH 1 (UID BODY.PEEK["+section+"])")
				}
				fetches = append(fetches, fmt.Sprintf("UID FETCH 1 (UID BODY.PEEK[%s]<0.%d>)", tc.section, len(raw)+1))
				f.requireTranscript(t, fetches...)
			})
		}
	})

	t.Run("Wire/lossless text", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		for _, tc := range []struct{ charset, encoding, raw, want string }{
			{"ISO-2022-JP", "7bit", "\x1b(J\\~\x1b(B", "¥‾"},
			{"HZ-GB-2312", "base64", base64.StdEncoding.EncodeToString([]byte("ab~\ncd")), "abcd"},
			{"UTF-16", "binary", "\xff\xfe\xfd\xff", "�"},
			{"GB18030", "8bit", "\x80\x84\x31\xa4\x37", "€�"},
			{"ISO-2022-JP", "7bit", "\x1b$@$\"\x1b(B", "あ"},
			{"us-ascii", "base64", strings.Repeat("YWJj", 19) + "\r\neA==", strings.Repeat("abc", 19) + "x"},
			{"windows-1252", "quoted-printable", "Price =8012", "Price €12"},
			{"utf-8", "8bit", "literal U+FFFD: �", "literal U+FFFD: �"},
			{"utf-8", "8bit", strings.Repeat("é", 499) + "\r\nx", strings.Repeat("é", 499) + "\r\nx"},
			{"us-ascii", "quoted-printable", strings.Repeat("a", 75) + "=\r\nb", strings.Repeat("a", 75) + "b"},
		} {
			f.set(textStructure("plain", tc.encoding, fmt.Sprintf(`("charset" %q)`, tc.charset), len(tc.raw)), "1", tc.raw)
			f.setMIME(map[string]string{"HEADER": fmt.Sprintf("Content-Type: text/plain; charset=%s\r\nContent-Transfer-Encoding: %s\r\n", tc.charset, tc.encoding)})
			if !strings.HasSuffix(f.prepare(t, false).Preview.Body, "\r\n\r\n"+tc.want) {
				t.Fatal("decoded text changed")
			}
		}
	})

	t.Run("Refuse", func(t *testing.T) {
		f := newForwardFixture(t, bridge.BoundsPatch{})
		plain := plainStructure("ok")
		for _, tc := range []struct {
			name, structure, raw string
			consent              bool
		}{
			{"short", plain, "o", false}, {"underdeclared", plain, "okay", false},
			{"damaged name parameter", textStructure("plain", "7bit", `("name*" "unknown''file.txt")`, 2), "ok", true},
			{"damaged charset parameter", textStructure("plain", "8bit", `("charset*" "unknown''utf-8")`, 2), "ok", false},
			{"ambiguous format", textStructure("plain", "8bit", `("format" "flowed" "format*" "us-ascii''fixed")`, 2), "ok", false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(tc.structure, "1", tc.raw)
				f.refuse(t, f.arguments(tc.consent))
			})
		}
		const signature = `("application" "pgp-signature" NIL NIL NIL "7bit" 2 NIL ("attachment" NIL) NIL NIL)`
		html := textStructure("html", "7bit", "NIL", 2)
		for _, tc := range []struct {
			name, structure string
			fields          map[string]string
			otherSections   []string
		}{
			{"HTML only", textStructure("html", "8bit", utf8Params, 2),
				map[string]string{"HEADER": "Content-Type: text/html; charset=utf-8\r\nContent-Transfer-Encoding: 8bit\r\n"}, nil},
			{"missing related type in BODYSTRUCTURE", multiStructure("alternative", plain, fmt.Sprintf(`(%s "related" ("boundary" "r") NIL NIL NIL)`, html)),
				map[string]string{"HEADER": "Content-Type: multipart/alternative; boundary=a\r\n", "1.MIME": plainMIME,
					"2.MIME": "Content-Type: multipart/related; boundary=r; type=\"text/html\"\r\n", "2.1.MIME": "Content-Type: text/html\r\n"}, nil},
			{"related root is an image", multiStructure("alternative", plain, fmt.Sprintf(`(%s %s "related" ("type" "image/png" "boundary" "r") NIL NIL NIL)`, imagePart, textStructure("html", "7bit", "NIL", 2))),
				map[string]string{"HEADER": "Content-Type: multipart/alternative; boundary=a\r\n", "1.MIME": plainMIME,
					"2.MIME": "Content-Type: multipart/related; boundary=r; type=\"image/png\"\r\n", "2.2.MIME": "Content-Type: text/html\r\n"}, nil},
			{"ambiguous", fmt.Sprintf(`(%s %s "mixed" ("boundary" "m") NIL NIL NIL)`, plain, plain),
				map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=m\r\n", "1.MIME": plainMIME, "2.MIME": plainMIME}, []string{"2"}},
			{"signed", fmt.Sprintf(`(%s %s "signed" ("boundary" "s" "protocol" "application/pgp-signature" "micalg" "pgp-sha256") NIL NIL NIL)`, plain, signature),
				map[string]string{"HEADER": "Content-Type: multipart/signed; protocol=\"application/pgp-signature\"; micalg=pgp-sha256; boundary=s\r\n",
					"1.MIME": plainMIME, "2.MIME": "Content-Type: application/pgp-signature\r\nContent-Disposition: attachment\r\n"}, []string{"2"}},
			{"flowed", textStructure("plain", "7bit", `("format" "flowed")`, 2),
				map[string]string{"HEADER": "Content-Type: text/plain; format=flowed\r\n"}, nil},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(tc.structure, "1", "ok", tc.otherSections...)
				f.setMIME(tc.fields)

				requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(true)), errUnsupported)
				f.requireTranscript(t)
			})
		}
		for _, tc := range []struct {
			name, omitted, mimeFields string
			want                      bridge.ForwardOmissions
		}{
			{"attachment", file, "Content-Type: application/pdf\r\nContent-Disposition: attachment; filename=file.pdf\r\nContent-Transfer-Encoding: base64\r\n", bridge.ForwardOmissions{Files: 1}},
			{"attached message", nested, "Content-Type: message/rfc822\r\n", bridge.ForwardOmissions{AttachedMessages: 1}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(fmt.Sprintf(`(%s %s "mixed" ("boundary" "mixed") NIL NIL NIL)`, plain, tc.omitted), "1", "ok")
				f.setMIME(map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=mixed\r\n", "1.MIME": plainMIME, "2.MIME": tc.mimeFields})

				requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errUnsupported)
				if got := f.prepare(t, true).Preview.Omissions; got != tc.want {
					t.Fatalf("omissions = %+v, want %+v", got, tc.want)
				}
			})
		}

		t.Run("primary nested", func(t *testing.T) {
			f.set(multiStructure("mixed", nested, plain), "2", "ok")
			f.setMIME(map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=test\r\n", "2.MIME": plainMIME})
			requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(true)), errUnsupported)
			f.mu.Lock()
			defer f.mu.Unlock()
			for _, command := range f.commands {
				if strings.Contains(command, "BODY.PEEK[") && !strings.Contains(command, "[HEADER]") {
					t.Fatal("primary nested selection requested a numbered PEEK")
				}
			}
		})

		for _, tc := range []struct{ name, charset, encoding, raw string }{
			{"7bit NUL", "UTF-16LE", "7bit", "A\x00B\x00"},
			{"8bit NUL", "UTF-16LE", "8bit", "A\x00B\x00"},
			{"7bit bare LF", "utf-8", "7bit", "a\nb"},
			{"8bit bare CR", "UTF-16LE", "8bit", "\rA"},
			{"overlong 8bit line", "utf-8", "8bit", strings.Repeat("é", 499) + "x"},
			{"lowercase QP", "utf-8", "quoted-printable", "caf=c3=a9"},
			{"overlong base64 line", "us-ascii", "base64", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("a", 58)))},
			{"spurious HZ shift", "HZ-GB-2312", "7bit", "a~}b"},
			{"truncated ISO-2022-JP", "ISO-2022-JP", "7bit", "\x1b$@$"},
			{"unterminated ISO-2022-JP shift", "ISO-2022-JP", "7bit", "\x1b$@$\""},
			{"bad base64", "utf-8", "base64", "x!=="},
			{"bad QP", "utf-8", "quoted-printable", "=xz"},
			{"overlong QP line", "utf-8", "quoted-printable", strings.Repeat("a", 77) + "\r\n"},
			{"bad GB18030 digit", "GB18030", "8bit", "\x81:\x810"},
			{"lossy UTF-16", "UTF-16", "binary", "\xff\xfe\xfd\xff\x00\xdc"},
			{"lossy UTF-8", "utf-8", "8bit", "\xff"},
			{"lossy Windows-1252", "windows-1252", "8bit", "\x81"},
			{"bad charset", "bogus", "8bit", "ok"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(textStructure("plain", tc.encoding, fmt.Sprintf(`("charset" %q)`, tc.charset), len(tc.raw)), "1", tc.raw)
				f.setMIME(map[string]string{"HEADER": fmt.Sprintf("Content-Type: text/plain; charset=%s\r\nContent-Transfer-Encoding: %s\r\n", tc.charset, tc.encoding)})
				requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errUnsupported)
			})
		}

		const hiddenDisposition = "Content-Disposition: attachment; filename=a.txt; filename=b.txt\r\n"
		const conflictingFormat = "Content-Type: text/plain; charset=utf-8; format=flowed;\r\n format*=us-ascii''fixed\r\nContent-Transfer-Encoding: 8bit\r\n"
		fixed := textStructure("plain", "8bit", `("charset" "utf-8" "format" "fixed")`, 2)
		for _, tc := range []struct {
			name, structure, section string
			consent                  bool
			fields                   map[string]string
		}{
			{"encoded multipart", multiStructure("alternative", plain), "1", false,
				map[string]string{"HEADER": "Content-Type: multipart/alternative; boundary=a\r\nContent-Transfer-Encoding: quoted-printable\r\n", "1.MIME": plainMIME}},
			{"missing related type in MIME header", multiStructure("alternative", plain, fmt.Sprintf(`(%s "related" ("boundary" "r" "type" "text/html") NIL NIL NIL)`, html)), "1", false,
				map[string]string{"HEADER": "Content-Type: multipart/alternative; boundary=a\r\n", "1.MIME": plainMIME,
					"2.MIME": "Content-Type: multipart/related; boundary=r\r\n", "2.1.MIME": "Content-Type: text/html\r\n"}},
			{"hidden root attachment", plain, "1", false, map[string]string{"HEADER": plainMIME + hiddenDisposition}},
			{"normalized format", fixed, "1", false, map[string]string{"HEADER": conflictingFormat}},
			{"malformed disposition field", plain, "1", false, map[string]string{"HEADER": plainMIME + "Content-Disposition : attachment\r\n"}},
			{
				"ambiguous multipart boundary", multiStructure("mixed", plain), "1", false,
				map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=a; boundary*=us-ascii''b\r\n", "1.MIME": plainMIME},
			},
			{
				"hidden leaf attachment", multiStructure("mixed", plain, file), "1", true,
				map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=m\r\n", "1.MIME": plainMIME + hiddenDisposition},
			},
			{
				"hidden container attachment", multiStructure("mixed", multiStructure("alternative", plain)), "1.1", true,
				map[string]string{"HEADER": "Content-Type: multipart/mixed; boundary=m\r\n",
					"1.MIME": "Content-Type: multipart/alternative; boundary=a\r\n" + hiddenDisposition, "1.1.MIME": plainMIME},
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f.set(tc.structure, tc.section, "ok")
				f.setMIME(tc.fields)
				requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(tc.consent)), errUnsupported)
				f.mu.Lock()
				defer f.mu.Unlock()
				if slices.ContainsFunc(f.commands, forwardPeek.MatchString) {
					t.Fatal("fetched body bytes before validating MIME declarations")
				}
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
		f.setHeader(forwardHeader)
		for _, to := range []string{"reviewer@[192.0.2.1]", "reviewer@[IPv6:2001:db8::1]", "reviewer@[IPv6:::ffff:192.0.2.1]"} {
			args := f.arguments(false)
			args["to"] = to
			var result forwardResult
			callJSON(t, f.client, forwardTool, args, &result)
			if result.Handle == "" || result.Preview.To != to {
				t.Fatal("valid envelope literal changed")
			}
		}

		f.mu.Lock()
		f.commands = nil
		f.mu.Unlock()

		for _, malformed := range []string{`\ud800`, "\xff"} {
			args := json.RawMessage(fmt.Sprintf(`{"messageId":%q,"to":%q,"text":"%s"}`, f.id, forwardTo, malformed))
			if _, code := runPrepareForward(t.Context(), Options{Forward: f.store}, args); code != errInvalidArgument {
				t.Fatalf("malformed authored text returned %q", code)
			}
		}

		for _, to := range []string{"", "A <a@b.test>", "a@b.test,c@d.test", "a@b.test\r\nBcc: trap@sender.test", "a@b.test (comment)",
			"reviewer@[IPv6:192.0.2.1]", "reviewer@[::ffff:192.0.2.1]", "recipient@bad/path.test", "recipient@-bad.test", "recipient@" + strings.Repeat("x", 64) + ".test"} {
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

		t.Run("configured section limit", func(t *testing.T) {
			f := newForwardFixture(t, bridge.BoundsPatch{MaxBodyBytes: bridge.Int(4)})
			f.set(plainStructure("abc"), "1", "abc")
			previous := f.prepare(t, false)

			f.set(plainStructure("four"), "1", "four")
			requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errBoundsExceeded)
			f.requireTranscript(t)
			if _, ok := f.store.lookup(previous, time.Now()); !ok {
				t.Fatal("section preflight invalidated the previous handle")
			}
		})

		t.Run("source MIME ceiling", func(t *testing.T) {
			// Soft line breaks keep the complete section one byte above 256 KiB
			// while its decoded body ("ok") fits easily in the preview.
			raw := strings.Repeat("=\r\n", (256<<10)/3) + "ok"
			f := newForwardFixture(t, bridge.BoundsPatch{MaxBodyBytes: bridge.Int(512 << 10)})
			f.set(textStructure("plain", "quoted-printable", utf8Params, len(raw)), "1", raw)
			f.setMIME(map[string]string{"HEADER": qpMIME})

			requireToolError(t, callTool(t, f.client, forwardTool, f.arguments(false)), errBoundsExceeded)
			f.requireTranscript(t)
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
		f.setWrappedText(t, sourceText)
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
		for name, change := range map[string]func(*forwardResult){
			"handle":           func(r *forwardResult) { r.Handle += "x" },
			"from":             func(r *forwardResult) { r.Preview.From = "other@bridge.test" },
			"to":               func(r *forwardResult) { r.Preview.To = "other@recipient.test" },
			"subject":          func(r *forwardResult) { r.Preview.Subject += "x" },
			"body":             func(r *forwardResult) { r.Preview.Body += "x" },
			"htmlAlternatives": func(r *forwardResult) { r.Preview.Omissions.HTMLAlternatives++ },
			"files":            func(r *forwardResult) { r.Preview.Omissions.Files++ },
			"attachedMessages": func(r *forwardResult) { r.Preview.Omissions.AttachedMessages++ },
		} {
			tampered := first
			change(&tampered)
			if _, ok := f.store.lookup(tampered, time.Now()); ok {
				t.Fatalf("accepted change to %s", name)
			}
		}
		if again, ok := f.store.lookup(first, time.Now()); !ok || again != wire {
			t.Fatal("repeated approval lookup changed the serialized bytes")
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
		frameSize := func(name string, args any) int {
			data, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
			if err != nil {
				t.Fatal(err)
			}
			return len(data) + 1
		}

		f := newForwardFixture(t, bridge.BoundsPatch{})
		f.set(plainStructure(""), "1", "")
		base := f.prepare(t, false)
		room := stdioframe.MaxFrameBytes - frameSize("send_forward", base)
		for _, field := range []string{"source", "authored"} {
			for _, unit := range []string{"p", "\"\\<&"} {
				encoded, _ := json.Marshal(unit)
				cost := len(encoded) - 2
				prefix := ""
				if field == "authored" {
					prefix = "�"
				}
				padding := room - len(prefix)
				content := prefix + strings.Repeat(unit, padding/cost) + strings.Repeat("x", padding%cost)
				args := f.arguments(false)
				want := base.Preview.Body + content
				if field == "authored" {
					args["text"] = content + "My note"
					want = content + base.Preview.Body
					f.set(plainStructure(""), "1", "")
				} else {
					f.setWrappedText(t, content)
				}

				var exact forwardResult
				callJSON(t, f.client, forwardTool, args, &exact)
				if exact.Handle == "" || frameSize("send_forward", exact) != stdioframe.MaxFrameBytes || exact.Preview.Body != want {
					t.Fatalf("incomplete exact-fit %s preview", field)
				}

				if field == "authored" {
					args["text"] = content + "My notex"
					if frameSize(forwardTool, args) > stdioframe.MaxFrameBytes {
						t.Fatal("authored-text fixture exceeds the prepare request frame")
					}
				} else {
					f.setWrappedText(t, content+"x")
				}
				requireToolError(t, callTool(t, f.client, forwardTool, args), errBoundsExceeded)
				if _, ok := f.store.lookup(exact, time.Now()); !ok {
					t.Fatal("failed preview displaced prior approval")
				}
			}
		}
	})
}

package bridge

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

const tlsHeaderBodySentinel = "croton-tls-header-body-sentinel"

var tlsReplyHeader = "From: =?utf-8?q?Thr=C3=A9ad_Reply?= <reply@croton.test>\r\n" +
	"To: Reader <reader@croton.test>\r\n" +
	"Subject: Re: large thread member\r\n" +
	"Date: Wed, 04 Nov 2026 23:30:00 +0100\r\n" +
	"Message-ID: <large-reply@croton.test>\r\n" +
	"In-Reply-To: <large-root@croton.test>\r\n" +
	"References: <large-root@croton.test> <large-middle@croton.test>\r\n\r\n"

// tlsHeaderMessages holds a 300KiB reply whose readable headers fit the
// default header bound, plus a small sibling so search windows stay ranged.
func tlsHeaderMessages() []string {
	line := tlsHeaderBodySentinel + " " + strings.Repeat("b", 62) + "\r\n"
	body := strings.Repeat(line, (300<<10)/len(line)+1)

	return []string{
		tlsReplyHeader + body,
		"From: Root <root@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: large thread root\r\nDate: Tue, 03 Nov 2026 09:15:00 -0800\r\nMessage-ID: <large-root@croton.test>\r\n\r\nroot body\r\n",
	}
}

func startTLSHeaderAdapter(t *testing.T, mode testkit.TLSMode, options testkit.Options) (*testkit.Server, *Adapter) {
	t.Helper()

	options.Mode = mode
	server, err := testkit.Start(options)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake server address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse fake server port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	tlsMode := TLSModeStartTLS
	if mode == testkit.ImplicitTLS {
		tlsMode = TLSModeImplicit
	}
	adapter, err := NewAdapter(Config{IMAP: IMAPConfig{
		Host:              host,
		Port:              number,
		TLSMode:           tlsMode,
		CredentialCommand: []string{executable, "-test.run=TestCredentialHelperProcess", "--", "valid"},
		TLS:               TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  1000,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

func searchTLSHeaderID(t *testing.T, adapter *Adapter, uid uint32) string {
	t.Helper()

	results, err := adapter.SearchMail(context.Background(), SearchQuery{Mailbox: "INBOX"})
	if err != nil {
		t.Fatalf("SearchMail: %v", err)
	}
	for _, result := range results {
		if result.UID == uid {
			return result.ID
		}
	}
	t.Fatalf("SearchMail did not return UID %d", uid)

	return ""
}

// assertHeaderOnlyTranscript requires a TLS-only, read-only transcript whose
// every FETCH is a header-only read, so no body was fetched or normalized.
func assertHeaderOnlyTranscript(t *testing.T, server *testkit.Server) {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatalf("authentication transcript: %v", err)
	}

	fetches := 0
	for _, command := range server.Commands() {
		upper := strings.ToUpper(command.Raw)
		if !command.TLS && command.Name != "CAPABILITY" && command.Name != "STARTTLS" {
			t.Fatalf("command %q ran without TLS", command.Name)
		}
		if strings.Contains(upper, "STORE") || strings.Contains(upper, "MOVE") || strings.Contains(upper, "EXPUNGE") || strings.Contains(upper, "SELECT ") {
			t.Fatalf("mutating command %q", command.Raw)
		}
		if !strings.Contains(upper, " FETCH ") {
			continue
		}
		if !strings.Contains(upper, "BODY.PEEK[HEADER]") || strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "BODY[") || strings.Contains(upper, "BINARY") {
			t.Fatalf("FETCH %q is not header-only", command.Raw)
		}
		fetches++
	}
	if fetches == 0 {
		t.Fatal("no header FETCH recorded")
	}
}

func TestMessageHeadersTLS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	modes := []struct {
		name string
		mode testkit.TLSMode
	}{
		{name: "starttls_headers", mode: testkit.StartTLS},
		{name: "implicit_tls_headers", mode: testkit.ImplicitTLS},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			messages := tlsHeaderMessages()
			if len(messages[0]) < 300<<10 || len(messages[0]) <= defaultMaxBodyBytes {
				t.Fatalf("large fixture is %d bytes", len(messages[0]))
			}

			// Readable headers of a 300KiB message come from one header FETCH.
			server, adapter := startTLSHeaderAdapter(t, mode.mode, testkit.Options{Messages: messages, Seen: []bool{true, false}})
			identifier := searchTLSHeaderID(t, adapter, 101)
			result, err := adapter.GetMessageHeaders(context.Background(), identifier)
			if err != nil {
				t.Fatalf("GetMessageHeaders: %v", err)
			}

			metadata := result.Metadata
			if metadata.ID != identifier || metadata.Mailbox != "INBOX" || metadata.UIDValidity != 9001 || metadata.UID != 101 || metadata.Size != int64(len(messages[0])) {
				t.Fatalf("metadata identity = %q %q %d %d size %d", metadata.ID, metadata.Mailbox, metadata.UIDValidity, metadata.UID, metadata.Size)
			}
			if metadata.ReadState != ReadStateRead || metadata.From != "Thréad Reply <reply@croton.test>" || metadata.Date != "2026-11-04T22:30:00Z" || metadata.DateSource != DateSourceHeader || metadata.Subject != "Re: large thread member" {
				t.Fatalf("metadata = %+v", metadata)
			}

			want := CanonicalHeaders{
				Date:       "Wed, 04 Nov 2026 23:30:00 +0100",
				From:       "Thréad Reply <reply@croton.test>",
				To:         "Reader <reader@croton.test>",
				Subject:    "Re: large thread member",
				MessageID:  "<large-reply@croton.test>",
				InReplyTo:  "<large-root@croton.test>",
				References: []string{"<large-root@croton.test>", "<large-middle@croton.test>"},
			}
			if !reflect.DeepEqual(result.Headers, want) || result.Truncation.Any() {
				t.Fatalf("headers = %+v truncation %+v", result.Headers, result.Truncation)
			}

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			if strings.Contains(string(encoded), tlsHeaderBodySentinel) {
				t.Fatal("header result retained body content")
			}

			// A missing UID is stale, and the fully consumed connection serves
			// the next bounded read.
			result, err = adapter.GetMessageHeaders(context.Background(), headerTestID(t, adapter, 9001, 150))
			requireHeaderRefusal(t, "missing UID", result, err, CodeStaleMessageID)
			result, err = adapter.GetMessageHeaders(context.Background(), identifier)
			if err != nil || result.Headers.MessageID != "<large-reply@croton.test>" {
				t.Fatalf("follow-up after missing UID = %+v, %v", result.Headers, err)
			}
			assertHeaderOnlyTranscript(t, server)

			// A changed generation refuses the old ID; a fresh search yields a
			// new ID that reads on the same connection.
			server, adapter = startTLSHeaderAdapter(t, mode.mode, testkit.Options{Messages: messages, Scenario: testkit.Scenario{UIDValidityAfterExamine: 9002}})
			identifier = searchTLSHeaderID(t, adapter, 101)
			result, err = adapter.GetMessageHeaders(context.Background(), identifier)
			requireHeaderRefusal(t, "stale generation", result, err, CodeStaleMessageID)
			fresh := searchTLSHeaderID(t, adapter, 101)
			result, err = adapter.GetMessageHeaders(context.Background(), fresh)
			if err != nil || result.Metadata.UIDValidity != 9002 || result.Headers.InReplyTo != "<large-root@croton.test>" {
				t.Fatalf("follow-up after stale generation = %+v, %v", result, err)
			}
			assertHeaderOnlyTranscript(t, server)

			// A wrong response section is refused code-only; the adapter safely
			// aborts and reopens for the next bounded read.
			server, adapter = startTLSHeaderAdapter(t, mode.mode, testkit.Options{Scenario: testkit.Scenario{FetchResponseSection: "[TEXT]"}})
			result, err = adapter.GetMessageHeaders(context.Background(), headerTestID(t, adapter, 9001, 101))
			requireHeaderRefusal(t, "wrong section", result, err, CodeIMAPProtocol)
			status, err := adapter.Status(context.Background(), "INBOX")
			if err != nil || status.UIDValidity != 9001 {
				t.Fatalf("follow-up Status after wrong section = %+v, %v", status, err)
			}
			connections := map[int]bool{}
			for _, command := range server.Commands() {
				connections[command.ConnectionID] = true
			}
			if len(connections) != 2 {
				t.Fatalf("wrong section used %d connections, want abort and reopen", len(connections))
			}
			assertHeaderOnlyTranscript(t, server)
		})
	}
}

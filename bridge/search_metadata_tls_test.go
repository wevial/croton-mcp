package bridge_test

import (
	"context"
	"encoding/json"
	"net/mail"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

var tlsMetadataMessages = []string{
	"From: Seen Fixture <seen@croton.test>\r\nTo: Reader <reader@croton.test>\r\nDate: Tue, 03 Nov 2026 09:15:00 -0800\r\nSubject: Seen metadata\r\n\r\nseen-body-marker\r\n",
	"From: =?utf-8?q?Unseen_Fixt=C3=BCre?= <unseen@croton.test>\r\nTo: Reader <reader@croton.test>\r\nDate: Wed, 04 Nov 2026 23:30:00 +0100\r\nSubject: Unseen metadata\r\n\r\nunseen-body-marker\r\n",
	"From: Undated Fixture <undated@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Undated metadata\r\n\r\nundated-body-marker\r\n",
}

var tlsMetadataSeen = []bool{true, false, true}

// tlsSourcedMetadata is the serialized view of adapter metadata.
type tlsSourcedMetadata struct {
	ID                string
	UID               uint32
	Subject           string
	From              *string
	Date              *string
	DateSource        *string
	ReadState         *string
	MetadataTruncated *bool
}

func decodeTLSMetadata(t *testing.T, metadata bridge.MessageMetadata) tlsSourcedMetadata {
	t.Helper()

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	var decoded tlsSourcedMetadata
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode serialized metadata: %v", err)
	}
	if decoded.From == nil || decoded.Date == nil || decoded.DateSource == nil || decoded.ReadState == nil || decoded.MetadataTruncated == nil {
		t.Fatalf("serialized metadata %s lacks sourced sender/date/read-state fields", encoded)
	}

	return decoded
}

// isolateMetadataEnvironment keeps the helper and adapter away from any real
// user home, config, cache or temporary directory.
func isolateMetadataEnvironment(t *testing.T) {
	t.Helper()

	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}
}

func startTLSMetadataAdapter(t *testing.T, fake testkit.TLSMode, mode string, flags testkit.MetadataFlagsMode) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: fake, Messages: tlsMetadataMessages, Seen: tlsMetadataSeen, MetadataFlags: flags})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), mode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

// headerOracle independently parses the fixture message headers.
func headerOracle(t *testing.T, index int) (string, string) {
	t.Helper()

	parsed, err := mail.ReadMessage(strings.NewReader(tlsMetadataMessages[index]))
	if err != nil {
		t.Fatalf("oracle parse: %v", err)
	}

	from := parsed.Header.Get("From")
	if strings.HasPrefix(from, "=?") {
		address, err := mail.ParseAddress(from)
		if err != nil {
			t.Fatalf("oracle From: %v", err)
		}
		from = address.Name + " <" + address.Address + ">"
	}

	date := ""
	if value := parsed.Header.Get("Date"); value != "" {
		parsedDate, err := mail.ParseDate(value)
		if err != nil {
			t.Fatalf("oracle Date: %v", err)
		}
		date = parsedDate.UTC().Format(time.RFC3339)
	}

	return from, date
}

// unseenOracle asks the server for UNSEEN UIDs through the adapter's own
// read-only search, independently of FETCH FLAGS.
func unseenOracle(t *testing.T, adapter *bridge.Adapter) map[uint32]bool {
	t.Helper()

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX", Unread: true})
	if err != nil {
		t.Fatalf("UNSEEN search: %v", err)
	}

	unseen := make(map[uint32]bool, len(results))
	for _, result := range results {
		unseen[result.UID] = true
	}

	return unseen
}

func assertMetadataOnlyTranscript(t *testing.T, server *testkit.Server) {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatalf("authentication transcript: %v", err)
	}

	metadataFetches := 0
	for _, command := range server.Commands() {
		upper := strings.ToUpper(command.Raw)
		if !strings.Contains(upper, " FETCH ") {
			continue
		}

		if !command.TLS || !strings.Contains(upper, "BODY.PEEK[HEADER]") || !strings.Contains(upper, " FLAGS") || strings.Contains(upper, "INTERNALDATE") || strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "STORE") {
			t.Fatalf("FETCH %q is not a TLS FLAGS plus header-only metadata read", command.Raw)
		}
		metadataFetches++
	}
	if metadataFetches == 0 {
		t.Fatal("no metadata FETCH recorded")
	}
}

func TestSearchMetadataTLS(t *testing.T) {
	isolateMetadataEnvironment(t)

	modes := []struct {
		name string
		fake testkit.TLSMode
		mode string
	}{
		{name: "implicit TLS", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS},
	}

	t.Run("flags_and_dates", func(t *testing.T) {
		for _, mode := range modes {
			server, adapter := startTLSMetadataAdapter(t, mode.fake, mode.mode, testkit.MetadataFlagsOrdinary)

			unseen := unseenOracle(t, adapter)
			results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil {
				t.Fatalf("%s: SearchMail: %v", mode.name, err)
			}
			if len(results) != len(tlsMetadataMessages) {
				t.Fatalf("%s: SearchMail results = %d, want %d", mode.name, len(results), len(tlsMetadataMessages))
			}

			for _, result := range results {
				selected, err := adapter.GetMessageMetadata(context.Background(), result.ID)
				if err != nil {
					t.Fatalf("%s: GetMessageMetadata: %v", mode.name, err)
				}

				for _, metadata := range []bridge.MessageMetadata{result, selected} {
					got := decodeTLSMetadata(t, metadata)
					index := int(got.UID) - 101
					if index < 0 || index >= len(tlsMetadataMessages) || got.ID != result.ID {
						t.Fatalf("%s: unexpected identity UID %d", mode.name, got.UID)
					}

					wantFrom, wantDate := headerOracle(t, index)
					wantSource := "header"
					if wantDate == "" {
						wantSource = "unknown"
					}
					wantRead := "read"
					if unseen[got.UID] {
						wantRead = "unread"
					}
					if unseen[got.UID] == tlsMetadataSeen[index] {
						t.Fatalf("%s: UNSEEN oracle disagrees with fixture Seen for UID %d", mode.name, got.UID)
					}

					if *got.From != wantFrom || *got.Date != wantDate || *got.DateSource != wantSource || *got.ReadState != wantRead || *got.MetadataTruncated {
						t.Fatalf("%s: UID %d from %q date %q source %q read %q truncated %t, want %q %q %q %q",
							mode.name, got.UID, *got.From, *got.Date, *got.DateSource, *got.ReadState, *got.MetadataTruncated, wantFrom, wantDate, wantSource, wantRead)
					}
				}
			}

			assertMetadataOnlyTranscript(t, server)
		}
	})

	t.Run("missing_duplicate_and_readonly", func(t *testing.T) {
		for _, mode := range modes {
			server, adapter := startTLSMetadataAdapter(t, mode.fake, mode.mode, testkit.MetadataFlagsOmitted)

			before := unseenOracle(t, adapter)
			results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil || len(results) != len(tlsMetadataMessages) {
				t.Fatalf("%s: omitted FLAGS SearchMail = %d results, %v", mode.name, len(results), err)
			}
			for _, result := range results {
				if got := decodeTLSMetadata(t, result); *got.ReadState != "unknown" {
					t.Fatalf("%s: omitted FLAGS UID %d read state %q, want unknown", mode.name, got.UID, *got.ReadState)
				}
			}
			after := unseenOracle(t, adapter)
			if len(before) != 1 || !before[102] || len(after) != len(before) || !after[102] {
				t.Fatalf("%s: UNSEEN before %v after %v, want unchanged {102}", mode.name, before, after)
			}
			assertMetadataOnlyTranscript(t, server)

			server, adapter = startTLSMetadataAdapter(t, mode.fake, mode.mode, testkit.MetadataFlagsDuplicated)
			results, err = adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if bridge.CodeOf(err) != bridge.CodeIMAPProtocol || results != nil {
				t.Fatalf("%s: duplicate FLAGS SearchMail = %#v, %v, want nil and %q", mode.name, results, err, bridge.CodeIMAPProtocol)
			}
			if err.Error() != bridge.CodeIMAPProtocol {
				t.Fatalf("%s: duplicate FLAGS error = %q, want code only", mode.name, err.Error())
			}
			assertMetadataOnlyTranscript(t, server)
		}
	})
}

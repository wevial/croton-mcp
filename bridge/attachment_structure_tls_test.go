package bridge

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

const tlsStructureBodySentinel = "croton-tls-structure-body-sentinel"

// tlsStructureMessage is a 300KiB raw message. Its typed descriptor is
// independent of these bytes.
func tlsStructureMessage() string {
	line := tlsStructureBodySentinel + " " + strings.Repeat("b", 62) + "\r\n"

	return "Subject: large report\r\nContent-Type: application/pdf\r\n\r\n" + strings.Repeat(line, (300<<10)/len(line)+1)
}

// assertStructureOnlyTranscript requires a TLS-only, read-only transcript
// whose every FETCH is the exact structure dispatch, fetches of them in all.
func assertStructureOnlyTranscript(t *testing.T, server *testkit.Server, fetches int) {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatalf("authentication transcript: %v", err)
	}

	seen := 0
	for _, command := range server.Commands() {
		if !command.TLS && command.Name != "CAPABILITY" && command.Name != "STARTTLS" {
			t.Fatalf("command %q ran without TLS", command.Name)
		}
		_, rest, _ := strings.Cut(strings.ToUpper(command.Raw), " ")
		if !strings.Contains(rest, "FETCH") {
			continue
		}
		if rest != "UID FETCH 101 (UID BODYSTRUCTURE)" {
			t.Fatalf("FETCH %q is not the exact structure dispatch", command.Raw)
		}
		seen++
	}
	if seen != fetches {
		t.Fatalf("transcript has %d structure FETCH commands, want %d", seen, fetches)
	}
}

func TestAttachmentStructureTLS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	modes := []struct {
		name string
		mode testkit.TLSMode
	}{
		{name: "starttls_structure", mode: testkit.StartTLS},
		{name: "implicit_tls_structure", mode: testkit.ImplicitTLS},
	}

	report := &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "base64", Size: 307400, Disposition: attachmentDisposition(testkit.BodyParam{Name: "filename", Value: "report.pdf"}, testkit.BodyParam{Name: "size", Value: "224000"})}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			message := tlsStructureMessage()
			server, adapter := startTLSHeaderAdapter(t, mode.mode, testkit.Options{Messages: []string{message}, BodyStructures: []*testkit.BodyPart{report}})

			// The adapter returns the tuple and the plain metadata of the
			// 300KiB message from one structure FETCH.
			identifier := headerTestID(t, adapter, 9001, 101)
			result, err := callAttachmentStructure(t, context.Background(), adapter, identifier)
			if err != nil {
				t.Fatalf("report error = %v", err)
			}
			want := attachmentStructureResult{ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: 101, Attachments: []attachmentStructureView{{
				Metadata: AttachmentMetadata{Filename: "report.pdf", ContentType: "application/pdf", Disposition: "attachment", DeclaredSize: 224000, HasDeclaredSize: true},
			}}}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("report result = %+v, want %+v", result, want)
			}

			assertStructureOnlyTranscript(t, server, 1)

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			for _, detail := range []string{tlsStructureBodySentinel, "TestCredentialHelperProcess", "fixture-user", "fixture-password", "BODYSTRUCTURE"} {
				if strings.Contains(string(encoded), detail) {
					t.Fatalf("result carries raw or helper detail %q", detail)
				}
			}

			// A stale generation is refused on a fresh selection with no
			// structure FETCH.
			stale, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9002, 101))
			requireStructureRefusal(t, "stale generation", stale, err, CodeStaleMessageID)
			assertStructureOnlyTranscript(t, server, 1)
		})
	}
}

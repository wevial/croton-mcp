package bridge

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"reflect"
	"strings"
	"sync"
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

// tlsStructureFlags are FLAGS-only FETCH frames for the target UID, without a
// UID and for another UID. The quarantine must consume all of them.
const tlsStructureFlags = "* 1 FETCH (UID 101 FLAGS (\\Seen))\r\n" +
	"* 1 FETCH (FLAGS (\\Answered))\r\n" +
	"* 2 FETCH (UID 102 FLAGS (\\Flagged))\r\n"

// flagsBeforeStructureConn is the private transport seam that splices
// tlsStructureFlags into the ordinary fixture's stream: once the client sends
// UID FETCH, the next server bytes are preceded by the FLAGS-only frames.
type flagsBeforeStructureConn struct {
	net.Conn

	mu      sync.Mutex
	armed   bool
	spliced bool
	pending []byte
}

func (connection *flagsBeforeStructureConn) Write(buffer []byte) (int, error) {
	if bytes.Contains(bytes.ToUpper(buffer), []byte(" UID FETCH ")) {
		connection.mu.Lock()
		connection.armed = true
		connection.mu.Unlock()
	}

	return connection.Conn.Write(buffer)
}

func (connection *flagsBeforeStructureConn) Read(buffer []byte) (int, error) {
	connection.mu.Lock()
	if len(connection.pending) > 0 {
		count := copy(buffer, connection.pending)
		connection.pending = connection.pending[count:]
		connection.mu.Unlock()
		return count, nil
	}
	connection.mu.Unlock()

	count, err := connection.Conn.Read(buffer)

	connection.mu.Lock()
	defer connection.mu.Unlock()
	if connection.armed && count > 0 {
		connection.armed = false
		connection.spliced = true
		connection.pending = append([]byte(tlsStructureFlags), buffer[:count]...)
		count = copy(buffer, connection.pending)
		connection.pending = connection.pending[count:]
	}
	return count, err
}

// delivered reports whether the frames were spliced and fully read.
func (connection *flagsBeforeStructureConn) delivered() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	return connection.spliced && len(connection.pending) == 0
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

			// The production dedicated reader returns the tuple and the plain
			// metadata of the 300KiB message from one structure FETCH.
			identifier := headerTestID(t, adapter, 9001, 101)
			result, err := callAttachmentStructure(t, context.Background(), adapter, identifier)
			if err != nil {
				t.Fatalf("report error = %v", err)
			}
			want := attachmentStructureResult{ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: 101, Attachments: []attachmentStructureView{{
				Metadata:          AttachmentMetadata{Filename: "report.pdf", ContentType: "application/pdf", Disposition: "attachment", DeclaredSize: 224000, HasDeclaredSize: true},
				HasTransferOctets: true, TransferOctets: 307400,
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
			if int(result.Attachments[0].TransferOctets) == len(message) {
				t.Fatal("transfer octets came from the raw message size")
			}

			// A stale generation is refused on the fresh selection with no
			// structure FETCH.
			stale, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9002, 101))
			requireStructureRefusal(t, "stale generation", stale, err, CodeStaleMessageID)
			assertStructureOnlyTranscript(t, server, 1)

			// FLAGS-only frames for the target, without a UID and for another
			// UID, spliced before the fixture's structure response by the
			// named flagsBeforeStructureConn seam, are consumed without effect
			// through the actual adapter.
			var mu sync.Mutex
			var spliced []*flagsBeforeStructureConn
			wrapStructureTransport(t, adapter, func(_ int, transport net.Conn) net.Conn {
				connection := &flagsBeforeStructureConn{Conn: transport}
				mu.Lock()
				spliced = append(spliced, connection)
				mu.Unlock()
				return connection
			})
			flagged, err := callAttachmentStructure(t, context.Background(), adapter, identifier)
			if err != nil {
				t.Fatalf("report with FLAGS traffic error = %v", err)
			}
			if !reflect.DeepEqual(flagged, want) {
				t.Fatalf("report with FLAGS traffic = %+v, want %+v", flagged, want)
			}
			mu.Lock()
			if len(spliced) != 1 || !spliced[0].delivered() {
				t.Fatal("FLAGS-only frames were not delivered before the structure response")
			}
			mu.Unlock()
			assertStructureOnlyTranscript(t, server, 2)

			adapter.mu.Lock()
			pooled := adapter.session
			adapter.mu.Unlock()
			if pooled != nil {
				t.Fatal("the dedicated operation retained a pooled session")
			}
		})
	}
}

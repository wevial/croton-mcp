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

// tlsStructureFlags are FLAGS-only FETCH frames for the target UID, without a
// UID and for another UID. The quarantine must consume all of them.
const tlsStructureFlags = "* 1 FETCH (UID 101 FLAGS (\\Seen))\r\n" +
	"* 1 FETCH (FLAGS (\\Answered))\r\n" +
	"* 2 FETCH (UID 102 FLAGS (\\Flagged))\r\n"

// tlsStructureMessages holds a 300KiB raw message and a small sibling. Their
// typed descriptors are independent of these bytes.
func tlsStructureMessages() []string {
	line := tlsStructureBodySentinel + " " + strings.Repeat("b", 62) + "\r\n"
	body := strings.Repeat(line, (300<<10)/len(line)+1)

	return []string{
		"Subject: large report\r\nContent-Type: application/pdf\r\n\r\n" + body,
		"Subject: notes\r\n\r\nnotes\r\n",
	}
}

// flagsBeforeStructureConn is the private transport seam that splices
// tlsStructureFlags into the ordinary fixture's stream: once the client sends
// UID FETCH, the next server bytes are preceded by the FLAGS-only frames.
type flagsBeforeStructureConn struct {
	net.Conn

	mu      sync.Mutex
	armed   bool
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
		connection.pending = append([]byte(nil), buffer[:count]...)
		count = copy(buffer, tlsStructureFlags)
		if count < len(tlsStructureFlags) {
			connection.pending = append([]byte(tlsStructureFlags[count:]), connection.pending...)
		}
	}
	return count, err
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
		if rest != "UID FETCH 101 (UID BODYSTRUCTURE)" && rest != "UID FETCH 102 (UID BODYSTRUCTURE)" {
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

	report := &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "base64", Size: 307400, Disposition: &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: "report.pdf"}, {Name: "size", Value: "224000"}}}}
	notes := &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "name", Value: "notes.txt"}}, Size: 0, Lines: 0}
	wantReport := []AttachmentStructureEntry{{
		AttachmentMetadata: AttachmentMetadata{Filename: "report.pdf", ContentType: "application/pdf", Disposition: "attachment", DeclaredSize: 224000, HasDeclaredSize: true},
		HasTransferOctets:  true, TransferOctets: 307400,
	}}
	wantNotes := []AttachmentStructureEntry{{
		AttachmentMetadata: AttachmentMetadata{Filename: "notes.txt", ContentType: "text/plain"},
		HasTransferOctets:  true,
	}}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			messages := tlsStructureMessages()
			server, adapter := startTLSHeaderAdapter(t, mode.mode, testkit.Options{Messages: messages, BodyStructures: []*testkit.BodyPart{report, notes}})
			ctx := context.Background()

			// The production dedicated reader returns the tuple and the plain
			// metadata of the 300KiB message from one structure FETCH.
			identifier := headerTestID(t, adapter, 9001, 101)
			result, err := adapter.GetMessageAttachmentStructure(ctx, identifier)
			if err != nil {
				t.Fatalf("report error = %v", err)
			}
			want := MessageAttachmentStructure{ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: 101, Attachments: wantReport}
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
			if int(result.Attachments[0].TransferOctets) == len(messages[0]) {
				t.Fatal("transfer octets came from the raw message size")
			}

			// A stale generation is refused on the fresh selection with no
			// structure FETCH.
			result, err = adapter.GetMessageAttachmentStructure(ctx, headerTestID(t, adapter, 9002, 101))
			requireStructureRefusal(t, "stale generation", result, err, CodeStaleMessageID)
			assertStructureOnlyTranscript(t, server, 1)

			// FLAGS-only frames for the target, no UID and another UID,
			// spliced before the fixture's structure response by the named
			// flagsBeforeStructureConn seam, are consumed without effect.
			var mu sync.Mutex
			var spliced []*flagsBeforeStructureConn
			adapter.reader = func(ctx context.Context, config ValidatedConfig, request structureRequest) (selectedStructure, error) {
				return readStructure(ctx, config, request, structureSeams{transport: func(transport net.Conn) net.Conn {
					connection := &flagsBeforeStructureConn{Conn: transport}
					mu.Lock()
					spliced = append(spliced, connection)
					mu.Unlock()
					return connection
				}})
			}
			for _, step := range []struct {
				uid  uint32
				want []AttachmentStructureEntry
			}{{uid: 101, want: wantReport}, {uid: 102, want: wantNotes}} {
				identifier := headerTestID(t, adapter, 9001, step.uid)
				result, err := adapter.GetMessageAttachmentStructure(ctx, identifier)
				if err != nil {
					t.Fatalf("UID %d with FLAGS traffic error = %v", step.uid, err)
				}
				want := MessageAttachmentStructure{ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: step.uid, Attachments: step.want}
				if !reflect.DeepEqual(result, want) {
					t.Fatalf("UID %d with FLAGS traffic = %+v, want %+v", step.uid, result, want)
				}
			}
			mu.Lock()
			for _, connection := range spliced {
				if connection.armed || len(connection.pending) != 0 {
					t.Fatal("FLAGS-only frames were not delivered before the structure response")
				}
			}
			count := len(spliced)
			mu.Unlock()
			if count != 2 {
				t.Fatalf("FLAGS seam installed on %d sessions", count)
			}
			assertStructureOnlyTranscript(t, server, 3)

			adapter.mu.Lock()
			pooled := adapter.session
			adapter.mu.Unlock()
			if pooled != nil {
				t.Fatal("the dedicated operation retained a pooled session")
			}
		})
	}
}

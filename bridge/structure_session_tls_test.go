package bridge

import (
	"bytes"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// structureLiteralName is a legal literal parameter value that looks like
// FETCH syntax with CRLF, quotes, parentheses and a literal declaration, and
// that is longer than every per-atom and per-control limit.
var structureLiteralName = "* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n\"quoted\" (paren) ) BODY[] {3}\r\n" + strings.Repeat("y", 4200)

// structureQuotedFilename is an otherwise admissible quoted parameter longer
// than 4096 bytes. Truncating it is downstream filename policy, not the
// guard's.
var structureQuotedFilename = strings.Repeat("f", 5000)

func structureTLSBody() string {
	return "((\"TEXT\" \"PLAIN\" (\"CHARSET\" \"utf-8\") NIL NIL \"7BIT\" 12 1 NIL NIL NIL NIL)" +
		"(\"APPLICATION\" \"OCTET-STREAM\" (\"NAME\" {" + strconv.Itoa(len(structureLiteralName)) + "}\r\n" + structureLiteralName + ") NIL NIL \"BASE64\" 4000 NIL " +
		"(\"ATTACHMENT\" (\"FILENAME\" \"" + structureQuotedFilename + "\")) NIL NIL)" +
		" \"MIXED\" (\"BOUNDARY\" \"croton-boundary\") NIL NIL NIL)"
}

func structureTLSConfig(t *testing.T, address string, mode string, pin string) ValidatedConfig {
	t.Helper()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	config, err := ValidateConfig(Config{IMAP: IMAPConfig{
		Host:              host,
		Port:              number,
		TLSMode:           mode,
		CredentialCommand: []string{executable, "-test.run=TestCredentialHelperProcess", "--", "valid"},
		TLS:               TLSConfig{SPKISHA256: pin},
		ConnectTimeoutMs:  5000,
		CommandTimeoutMs:  30000,
	}})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	return config
}

// assertStructureTLSRelease checks the decoded literal-bearing structure.
func assertStructureTLSRelease(t *testing.T, structure imap.BodyStructure) {
	t.Helper()

	multipart, ok := structure.(*imap.BodyStructureMultiPart)
	if !ok || multipart.MediaType() != "multipart/mixed" || len(multipart.Children) != 2 || multipart.Extended == nil || multipart.Extended.Params["boundary"] != "croton-boundary" {
		t.Fatalf("structure = %#v", structure)
	}
	text, ok := multipart.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || text.MediaType() != "text/plain" || text.Params["charset"] != "utf-8" || text.Size != 12 {
		t.Fatalf("text part = %#v", multipart.Children[0])
	}
	attachment, ok := multipart.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || attachment.MediaType() != "application/octet-stream" || !strings.EqualFold(attachment.Encoding, "base64") || attachment.Size != 4000 {
		t.Fatalf("attachment part = %#v", multipart.Children[1])
	}
	if attachment.Params["name"] != structureLiteralName {
		t.Fatalf("literal name parameter has %d bytes, want the exact %d", len(attachment.Params["name"]), len(structureLiteralName))
	}
	disposition := attachment.Disposition()
	if disposition == nil || !strings.EqualFold(disposition.Value, "attachment") || disposition.Params["filename"] != structureQuotedFilename {
		t.Fatalf("attachment disposition = %#v", disposition)
	}
}

// assertNoUnsafeCommand refuses body, BINARY and every mutating command.
func assertNoUnsafeCommand(t *testing.T, commands []string) {
	t.Helper()

	for _, command := range commands {
		upper := strings.ToUpper(command)
		for _, unsafe := range []string{"BODY[", "BODY.PEEK", "BINARY", "RFC822", "ENVELOPE", " STORE ", " COPY ", " MOVE ", "EXPUNGE", " SELECT ", " APPEND ", " DELETE ", " CREATE ", " RENAME "} {
			if strings.Contains(upper+" ", unsafe) {
				t.Fatalf("command %q is not allowed in a structure session", command)
			}
		}
	}
}

func TestStructureSessionTLS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	modes := []struct {
		name     string
		startTLS bool
		mode     string
		fixture  testkit.TLSMode
		target   string
	}{
		{
			name: "starttls_release", startTLS: true, mode: TLSModeStartTLS, fixture: testkit.StartTLS,
			target: "* 1 FETCH (UID 7 FLAGS (\\Seen) BODYSTRUCTURE " + structureTLSBody() + ")\r\n",
		},
		{
			name: "implicit_tls_release", startTLS: false, mode: TLSModeImplicit, fixture: testkit.ImplicitTLS,
			target: "* 1 FETCH (FLAGS () BODYSTRUCTURE " + structureTLSBody() + " UID 7)\r\n",
		},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			// Private peer: independent target-UID FLAGS-only frames come
			// before the real literal-bearing structure.
			flags := []string{"* 1 FETCH (UID 7 FLAGS (\\Seen \\Answered))\r\n", "* 1 FETCH (FLAGS (\\Flagged) UID 7)\r\n"}
			peer := startStructureTLSPeer(t, mode.startTLS, structurePeerScript{beforeSearch: flags[:1], duringFetch: flags[1:], structure: mode.target})
			config := structureTLSConfig(t, peer.listener.Addr().String(), mode.mode, peer.pin)
			seam, installed := newStructureTapSeam()

			result, err := readSelectedStructure(context.Background(), config, "INBOX", structurePeerUID, seam)
			if err != nil {
				t.Fatalf("readSelectedStructure() error = %v", err)
			}
			peer.wait(t)
			record := peer.snapshot()
			if record.err != nil {
				t.Fatalf("peer error = %v", record.err)
			}

			assertStructureTLSRelease(t, result.Structure)
			if result.Snapshot != (mailboxSnapshot{UIDNext: 104, UIDValidity: 9001}) {
				t.Fatalf("snapshot = %+v", result.Snapshot)
			}

			// The decoder-facing tap holds exactly the approved controls and
			// the one target structure, never a consumed FLAGS-only frame.
			tap := installed().bytes()
			if !bytes.Equal(tap, record.approved) {
				t.Fatalf("decoder-facing tap differs from the approved responses (%d vs %d bytes)", len(tap), len(record.approved))
			}
			if count := bytes.Count(tap, []byte("BODYSTRUCTURE")); count != 1 {
				t.Fatalf("tap carries %d structures", count)
			}
			for _, frame := range flags {
				if bytes.Contains(tap, []byte(frame)) {
					t.Fatalf("consumed frame %q reached the decoder side", frame)
				}
			}
			if len(record.consumed) != len(flags) {
				t.Fatalf("peer consumed frames = %q", record.consumed)
			}

			if mode.startTLS != (len(record.plaintext) == 2) {
				t.Fatalf("plaintext commands = %q", record.plaintext)
			}
			assertStructureCommands(t, record.commands, structurePeerUID)
			assertNoUnsafeCommand(t, append(record.plaintext, record.commands...))

			// CROTON-31 typed positive through the same production stack.
			typed := &testkit.BodyPart{Kind: testkit.BodyPartMultipart, Subtype: "mixed", Children: []*testkit.BodyPart{
				{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 9, Lines: 1},
				{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "base64", Size: 2048, Disposition: &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: "report.pdf"}}}},
			}}
			plain := &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 5, Lines: 1}
			server, err := testkit.Start(testkit.Options{
				Mode:           mode.fixture,
				Messages:       []string{"Subject: one\r\n\r\nfirst\r\n", "Subject: two\r\n\r\nsee report\r\n"},
				BodyStructures: []*testkit.BodyPart{plain, typed},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })
			typedConfig := structureTLSConfig(t, server.Addr(), mode.mode, server.SPKISHA256())
			typedSeam, typedTap := newStructureTapSeam()

			typedResult, err := readSelectedStructure(context.Background(), typedConfig, "INBOX", 102, typedSeam)
			if err != nil {
				t.Fatalf("typed readSelectedStructure() error = %v", err)
			}

			multipart, ok := typedResult.Structure.(*imap.BodyStructureMultiPart)
			if !ok || multipart.MediaType() != "multipart/mixed" || len(multipart.Children) != 2 {
				t.Fatalf("typed structure = %#v", typedResult.Structure)
			}
			attachment, ok := multipart.Children[1].(*imap.BodyStructureSinglePart)
			if !ok || attachment.MediaType() != "application/pdf" || attachment.Size != 2048 || attachment.Filename() != "report.pdf" {
				t.Fatalf("typed attachment = %#v", multipart.Children[1])
			}
			if count := bytes.Count(typedTap().bytes(), []byte("BODYSTRUCTURE")); count != 1 {
				t.Fatalf("typed tap carries %d structures", count)
			}

			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
			var secured []string
			for _, command := range server.Commands() {
				if !command.TLS {
					if command.Name != "CAPABILITY" && command.Name != "STARTTLS" {
						t.Fatalf("command %q ran without TLS", command.Name)
					}
					continue
				}
				if command.Name == "LOGIN" {
					secured = append(secured, "T LOGIN")
					continue
				}
				secured = append(secured, command.Raw)
			}
			assertStructureCommands(t, secured, 102)
			assertNoUnsafeCommand(t, secured)
		})
	}
}

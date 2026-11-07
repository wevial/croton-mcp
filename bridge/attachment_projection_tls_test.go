package bridge

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// projectionLongFilename is an otherwise admissible quoted filename longer
// than 4096 bytes; only the downstream filename limit cuts it.
var projectionLongFilename = strings.Repeat("f", 5000)

// projectionTLSBody is a wire BODYSTRUCTURE with raw lowercase RFC 2231
// continuation and extended keys, qualified and unqualified embedded
// messages that each hide an attachment, a qualified multipart and an
// overlong filename.
func projectionTLSBody() string {
	envelope := func(subject string) string {
		return "(NIL \"" + subject + "\" NIL NIL NIL NIL NIL NIL NIL NIL)"
	}
	hidden := "((\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 5 1 NIL NIL NIL NIL)" +
		"(\"APPLICATION\" \"PDF\" NIL NIL NIL \"BASE64\" 40 NIL (\"ATTACHMENT\" (\"FILENAME\" \"hidden.pdf\")) NIL NIL)" +
		" \"MIXED\" (\"BOUNDARY\" \"inner\") NIL NIL NIL)"

	return "(" +
		"(\"TEXT\" \"PLAIN\" (\"CHARSET\" \"utf-8\") NIL NIL \"7BIT\" 12 1 NIL (\"INLINE\" NIL) NIL NIL)" +
		"(\"APPLICATION\" \"PDF\" NIL NIL NIL \"BASE64\" 2048 NIL (\"ATTACHMENT\" (\"filename*0*\" \"utf-8''caf%C3%A9\" \"filename*1\" \".pdf\" \"size\" \"1500\")) NIL NIL)" +
		"(\"APPLICATION\" \"OCTET-STREAM\" (\"name*\" \"UTF-8''r%C3%A9sum%C3%A9.pdf\") NIL NIL \"BASE64\" 0 NIL NIL NIL NIL)" +
		"(\"MESSAGE\" \"RFC822\" NIL NIL NIL \"7BIT\" 900 " + envelope("fwd") + " " + hidden + " 20 NIL (\"ATTACHMENT\" (\"FILENAME\" \"fwd.eml\")) NIL NIL)" +
		"(\"MESSAGE\" \"GLOBAL\" NIL NIL NIL \"8BIT\" 300 " + envelope("intl") + " " + hidden + " 6 NIL NIL NIL NIL)" +
		"(\"MESSAGE\" \"RFC822\" NIL NIL NIL \"7BIT\" 200 " + envelope("note") + " " + hidden + " 4 NIL NIL NIL NIL)" +
		"((\"TEXT\" \"HTML\" NIL NIL NIL \"7BIT\" 30 1 NIL NIL NIL NIL)(\"IMAGE\" \"PNG\" (\"NAME\" \"logo.png\") NIL NIL \"BASE64\" 64 NIL NIL NIL NIL)" +
		" \"RELATED\" (\"BOUNDARY\" \"rel\") (\"ATTACHMENT\" (\"FILENAME\" \"page.zip\")) NIL NIL)" +
		"(\"APPLICATION\" \"OCTET-STREAM\" NIL NIL NIL \"BASE64\" 7000 NIL (\"ATTACHMENT\" (\"FILENAME\" \"" + projectionLongFilename + "\")) NIL NIL)" +
		" \"MIXED\" (\"BOUNDARY\" \"outer\") NIL NIL NIL)"
}

// structurePeerAdapter is a production adapter whose dedicated reads go to
// peer over verified loopback TLS in mode.
func structurePeerAdapter(t *testing.T, peer *structureTLSPeer, mode string) *Adapter {
	t.Helper()

	config := structureTLSConfig(t, peer.listener.Addr().String(), mode, peer.pin)
	adapter, err := NewAdapter(Config{IMAP: config.IMAP})
	if err != nil {
		t.Fatalf("NewAdapter() error = %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return adapter
}

func TestAttachmentProjectionTLS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	modes := []struct {
		name     string
		startTLS bool
		mode     string
		target   string
	}{
		{
			name: "starttls_projection", startTLS: true, mode: TLSModeStartTLS,
			target: "* 1 FETCH (UID 7 BODYSTRUCTURE " + projectionTLSBody() + ")\r\n",
		},
		{
			name: "implicit_tls_projection", startTLS: false, mode: TLSModeImplicit,
			target: "* 1 FETCH (BODYSTRUCTURE " + projectionTLSBody() + " UID 7)\r\n",
		},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			flags := []string{"* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n", "* 1 FETCH (FLAGS (\\Flagged))\r\n"}
			peer := startStructureTLSPeer(t, mode.startTLS, structurePeerScript{duringFetch: flags, structure: mode.target})
			adapter := structurePeerAdapter(t, peer, mode.mode)

			identifier := headerTestID(t, adapter, 9001, structurePeerUID)
			result, err := callAttachmentStructure(t, context.Background(), adapter, identifier)
			if err != nil {
				t.Fatalf("projection error = %v", err)
			}
			peer.wait(t)
			record := peer.snapshot()
			if record.err != nil {
				t.Fatalf("peer error = %v", record.err)
			}

			// Independent expectations: decoded names are literals, each
			// qualified outer entity is one entry, both unqualified embedded
			// messages are omitted with MIMEDepth, and the long name is cut
			// by the filename limit.
			want := attachmentStructureResult{
				ID: identifier, Mailbox: "INBOX", UIDValidity: 9001, UID: structurePeerUID,
				Attachments: []attachmentStructureView{
					{Metadata: AttachmentMetadata{Filename: "café.pdf", ContentType: "application/pdf", Disposition: "attachment", DeclaredSize: 1500, HasDeclaredSize: true}, HasTransferOctets: true, TransferOctets: 2048},
					{Metadata: AttachmentMetadata{Filename: "résumé.pdf", ContentType: "application/octet-stream"}, HasTransferOctets: true},
					{Metadata: AttachmentMetadata{Filename: "fwd.eml", ContentType: "message/rfc822", Disposition: "attachment"}, HasTransferOctets: true, TransferOctets: 900},
					{Metadata: AttachmentMetadata{Filename: "page.zip", ContentType: "multipart/related", Disposition: "attachment"}},
					{Metadata: AttachmentMetadata{Filename: strings.Repeat("f", defaultMaxFilenameBytes), ContentType: "application/octet-stream", Disposition: "attachment"}, HasTransferOctets: true, TransferOctets: 7000},
				},
				Truncation: Truncation{MIMEDepth: true, Attachments: true},
			}
			if !reflect.DeepEqual(result, want) {
				t.Fatalf("projection result = %+v, want %+v", result, want)
			}

			// One UID FETCH of UID and BODYSTRUCTURE only, after the
			// selection and the existence check, and every FLAGS-only frame
			// consumed.
			assertStructureCommands(t, record.commands, structurePeerUID)
			order := map[string]int{}
			for index, command := range record.commands {
				_, rest, _ := strings.Cut(strings.ToUpper(command), " ")
				for _, name := range []string{"EXAMINE", "UID SEARCH", "UID FETCH"} {
					if strings.HasPrefix(rest, name+" ") {
						order[name] = index + 1
					}
				}
				for _, forbidden := range []string{"BODY[", "BODY.PEEK", "BINARY", "FLAGS", "HEADER", "RFC822", "STORE", "MOVE", "EXPUNGE"} {
					if strings.Contains(rest, forbidden) {
						t.Fatalf("command %q requests %s", command, forbidden)
					}
				}
			}
			if order["EXAMINE"] == 0 || order["UID SEARCH"] <= order["EXAMINE"] || order["UID FETCH"] <= order["UID SEARCH"] {
				t.Fatalf("transcript %q does not check the generation before the FETCH", record.commands)
			}
			if len(record.consumed) != len(flags) {
				t.Fatalf("peer consumed frames = %q", record.consumed)
			}
			if mode.startTLS != (len(record.plaintext) == 2) {
				t.Fatalf("plaintext commands = %q", record.plaintext)
			}

			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal result: %v", err)
			}
			for _, detail := range []string{"hidden.pdf", "logo.png", "BODYSTRUCTURE", "TestCredentialHelperProcess", "%C3"} {
				if strings.Contains(string(encoded), detail) {
					t.Fatalf("result carries raw, hidden or helper detail %q", detail)
				}
			}

			// A stale generation is refused on a new connection before any
			// FETCH, the session is closed, and the error names no helper or
			// peer detail.
			stalePeer := startStructureTLSPeer(t, mode.startTLS, structurePeerScript{structure: mode.target})
			staleAdapter := structurePeerAdapter(t, stalePeer, mode.mode)
			stale, err := callAttachmentStructure(t, context.Background(), staleAdapter, headerTestID(t, staleAdapter, 9002, structurePeerUID))
			requireStructureRefusal(t, "stale generation", stale, err, CodeStaleMessageID)
			stalePeer.wait(t)
			for _, command := range stalePeer.snapshot().commands {
				if strings.Contains(strings.ToUpper(command), " FETCH ") {
					t.Fatalf("stale generation issued %q", command)
				}
			}
			address := stalePeer.listener.Addr().String()
			for _, detail := range []string{"TestCredentialHelperProcess", address, "127.0.0.1", "structure peer", "9002"} {
				if strings.Contains(err.Error(), detail) {
					t.Fatalf("stale error %q carries helper or peer detail %q", err, detail)
				}
			}
		})
	}
}

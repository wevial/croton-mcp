package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const (
	structureTLSBodySentinel       = "croton-structure-tls-body-sentinel"
	structureTLSAttachmentSentinel = "croton-structure-tls-attachment-sentinel"
	structureTLSFreeBodySentinel   = "croton-structure-tls-free-body-sentinel"

	// structureTLSTransferOctets is the descriptor-declared transfer size of
	// the large attachment part. It is private and must never be published.
	structureTLSTransferOctets = 297131
)

// structureTLSFiller repeats one sentinel line past 300KiB.
func structureTLSFiller(sentinel string) string {
	line := sentinel + " " + strings.Repeat("s", 60) + "\r\n"

	return strings.Repeat(line, (300<<10)/len(line)+1)
}

// structureTLSMessages holds a 300KiB multipart message with attachments
// and a 300KiB attachment-free message. Their BODYSTRUCTURE comes only from
// structureTLSDescriptors, never from these bytes.
func structureTLSMessages() []string {
	header := func(subject, messageID string) string {
		return "From: Sender <sender@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: " + subject + "\r\n" +
			"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\nMessage-ID: <" + messageID + "@croton.test>\r\nMIME-Version: 1.0\r\n"
	}

	report := header("Structure witness report", "structure-report") +
		"Content-Type: multipart/mixed; boundary=\"structure-tls\"\r\n\r\n" +
		"--structure-tls\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + structureTLSBodySentinel + "\r\n" +
		"--structure-tls\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"ledger.bin\"; size=2048\r\n\r\n" +
		structureTLSFiller(structureTLSAttachmentSentinel) +
		"--structure-tls\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=\"empty.bin\"; size=0\r\n\r\n\r\n" +
		"--structure-tls\r\nContent-Type: application/octet-stream; name=\"unsized.bin\"\r\nContent-Disposition: attachment\r\n\r\n" + structureTLSAttachmentSentinel + "\r\n" +
		"--structure-tls--\r\n"
	free := header("Structure witness plain", "structure-plain") +
		"Content-Type: text/plain; charset=utf-8\r\n\r\n" + structureTLSFiller(structureTLSFreeBodySentinel)

	return []string{report, free}
}

// structureTLSDescriptors are the literal, independently written trees for
// structureTLSMessages: a multipart with text and three attachments, then
// one plain text part.
func structureTLSDescriptors() []*testkit.BodyPart {
	octets := func(params []testkit.BodyParam, size uint32, disposition []testkit.BodyParam) *testkit.BodyPart {
		return &testkit.BodyPart{
			Kind: testkit.BodyPartBasic, Type: "application", Subtype: "octet-stream", Params: params, Encoding: "base64", Size: size,
			Disposition: &testkit.BodyDisposition{Value: "attachment", Params: disposition},
		}
	}
	report := &testkit.BodyPart{
		Kind: testkit.BodyPartMultipart, Subtype: "mixed", Params: []testkit.BodyParam{{Name: "boundary", Value: "structure-tls"}},
		Children: []*testkit.BodyPart{
			{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 36, Lines: 1},
			octets(nil, structureTLSTransferOctets, []testkit.BodyParam{{Name: "filename", Value: "ledger.bin"}, {Name: "size", Value: "2048"}}),
			octets(nil, 2, []testkit.BodyParam{{Name: "filename", Value: "empty.bin"}, {Name: "size", Value: "0"}}),
			octets([]testkit.BodyParam{{Name: "name", Value: "unsized.bin"}}, 42, nil),
		},
	}
	free := &testkit.BodyPart{
		Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 307230, Lines: 4831,
	}

	return []*testkit.BodyPart{report, free}
}

// countingStructureMail delegates to the real adapter and counts each public
// adapter operation separately.
type countingStructureMail struct {
	*bridge.Adapter

	structureCalls int
	metaCalls      int
	bodyCalls      int
	headerCalls    int
	searchCalls    int
}

func (mail *countingStructureMail) GetMessageAttachmentStructure(ctx context.Context, identifier string) (bridge.MessageAttachmentStructure, error) {
	mail.structureCalls++

	return mail.Adapter.GetMessageAttachmentStructure(ctx, identifier)
}

func (mail *countingStructureMail) GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error) {
	mail.metaCalls++

	return mail.Adapter.GetMessageMetadata(ctx, identifier)
}

func (mail *countingStructureMail) GetMessageBody(ctx context.Context, identifier string) ([]byte, error) {
	mail.bodyCalls++

	return mail.Adapter.GetMessageBody(ctx, identifier)
}

func (mail *countingStructureMail) GetMessageHeaders(ctx context.Context, identifier string) (bridge.MessageHeaders, error) {
	mail.headerCalls++

	return mail.Adapter.GetMessageHeaders(ctx, identifier)
}

func (mail *countingStructureMail) SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error) {
	mail.searchCalls++

	return mail.Adapter.SearchMailPage(ctx, query)
}

func (mail *countingStructureMail) reset() {
	mail.structureCalls, mail.metaCalls, mail.bodyCalls, mail.headerCalls, mail.searchCalls = 0, 0, 0, 0, 0
}

// startStructureTLSAdapter starts the structure fixture in mode and a
// production adapter pinned to its generated certificate.
func startStructureTLSAdapter(t *testing.T, mode testkit.TLSMode) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: mode, Messages: structureTLSMessages(), BodyStructures: structureTLSDescriptors()})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	tlsMode := bridge.TLSModeStartTLS
	if mode == testkit.ImplicitTLS {
		tlsMode = bridge.TLSModeImplicit
	}
	adapter, err := bridge.NewAdapter(bridge.Config{IMAP: bridge.IMAPConfig{
		Host:              host,
		Port:              port,
		TLSMode:           tlsMode,
		CredentialCommand: []string{executable, "-test.run=TestMCPCredentialHelperProcess", "--", "valid"},
		TLS:               bridge.TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  2000,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

// requireStructureOnlyListing requires one listing's transcript slice to be a
// single fresh connection that authenticates once over TLS and issues exactly
// one UID FETCH of the wanted UID requesting UID BODYSTRUCTURE only.
func requireStructureOnlyListing(t *testing.T, mode testkit.TLSMode, slice []testkit.Command, earlier map[int]bool, wantUID uint32) int {
	t.Helper()

	if len(slice) == 0 {
		t.Fatal("listing issued no IMAP commands")
	}

	connection := slice[0].ConnectionID
	if earlier[connection] {
		t.Fatalf("listing reused connection %d", connection)
	}

	logins, fetches := 0, 0
	for _, command := range slice {
		upper := strings.ToUpper(command.Raw)
		if command.ConnectionID != connection {
			t.Fatalf("listing used connections %d and %d", connection, command.ConnectionID)
		}
		if !command.TLS && (mode == testkit.ImplicitTLS || command.Name != "CAPABILITY" && command.Name != "STARTTLS") {
			t.Fatalf("listing command %q ran without TLS", command.Name)
		}
		fields := strings.Fields(upper)
		verb := strings.Join(fields[1:min(len(fields), 3)], " ")
		if command.Name != "UID" && len(fields) > 1 {
			verb = fields[1]
		}
		switch verb {
		case "LOGIN", "AUTHENTICATE":
			logins++
		case "CAPABILITY", "STARTTLS", "NOOP", "EXAMINE", "UID SEARCH", "LOGOUT":
		case "UID FETCH":
			fetches++
		default:
			t.Fatalf("listing issued %q outside the read-only structure path", command.Raw)
		}
		if verb != "UID FETCH" {
			continue
		}

		if strings.Join(fields[1:], " ") != "UID FETCH "+strconv.FormatUint(uint64(wantUID), 10)+" (UID BODYSTRUCTURE)" {
			t.Fatalf("listing FETCH %q is not a selected UID BODYSTRUCTURE read", command.Raw)
		}
		for _, item := range []string{"BODY[", "BODY.PEEK", "BINARY", "FLAGS", "RFC822", "ENVELOPE", "HEADER", "TEXT"} {
			if strings.Contains(upper, item) {
				t.Fatalf("listing FETCH %q requested %s", command.Raw, item)
			}
		}
	}
	if logins != 1 || fetches != 1 {
		t.Fatalf("listing authenticated %d and fetched %d times, want 1 and 1", logins, fetches)
	}

	return connection
}

func TestListAttachmentsStructureTLS(t *testing.T) {
	isolateFixtureEnvironment(t)

	for _, tc := range []struct {
		name string
		mode testkit.TLSMode
	}{
		{name: "starttls_structure", mode: testkit.StartTLS},
		{name: "implicit_tls_structure", mode: testkit.ImplicitTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, adapter := startStructureTLSAdapter(t, tc.mode)
			mail := &countingStructureMail{Adapter: adapter}
			var audit bytes.Buffer
			session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&audit)})

			// Setup: one bounded search acquires the opaque IDs.
			searched := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "subject": "Structure witness", "limit": 2})), "results")
			if len(searched) != 2 {
				t.Fatalf("setup search = %v, want two messages", searched)
			}
			byUID := make(map[uint32]string)
			for _, item := range searched {
				id, _ := item["id"].(string)
				byUID[flatTuple(t, item).UID] = id
			}
			reportID, freeID := byUID[101], byUID[102]
			if reportID == "" || freeID == "" || reportID == freeID {
				t.Fatalf("setup IDs by UID = %v", byUID)
			}

			sentinels := []string{
				structureTLSBodySentinel, structureTLSAttachmentSentinel, structureTLSFreeBodySentinel,
				strconv.Itoa(structureTLSTransferOctets), "307230",
			}
			connections := make(map[int]bool)
			for _, command := range server.Commands() {
				connections[command.ConnectionID] = true
			}

			for _, listing := range []struct {
				id    string
				tuple readTuple
				want  []wantAttachment
				text  []string
			}{
				{
					id:    reportID,
					tuple: readTuple{Mailbox: "INBOX", UIDValidity: 9001, UID: 101},
					want:  []wantAttachment{{filename: "ledger.bin", size: 2048, known: true}, {filename: "empty.bin", size: 0, known: true}, {filename: "unsized.bin"}},
				},
				{
					id:    freeID,
					tuple: readTuple{Mailbox: "INBOX", UIDValidity: 9001, UID: 102},
					want:  []wantAttachment{},
					text:  []string{`"attachments":[]`, `"truncation":{}`},
				},
			} {
				mail.reset()
				before := len(server.Commands())

				result := callTool(t, session, "list_attachments", map[string]any{"messageId": listing.id})
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("marshal SDK result: %v", err)
				}
				for _, sentinel := range sentinels {
					if bytes.Contains(raw, []byte(sentinel)) {
						t.Fatalf("listing disclosed %q", sentinel)
					}
				}
				for _, fragment := range listing.text {
					if !strings.Contains(resultText(t, result), fragment) {
						t.Fatalf("listing = %s, want %s", resultText(t, result), fragment)
					}
				}
				object := rawObject(t, result)
				requireClosedKeys(t, tc.name, object, listAttachmentsKeys)
				if object["id"] != listing.id || flatTuple(t, object) != listing.tuple {
					t.Fatalf("listing identity = %v/%+v, want %+v", object["id"], flatTuple(t, object), listing.tuple)
				}
				requireAttachmentEntries(t, tc.name, object, listing.want)
				requireReasons(t, tc.name, object, listAttachmentsReasons)

				counts := [5]int{mail.structureCalls, mail.metaCalls, mail.bodyCalls, mail.headerCalls, mail.searchCalls}
				if counts != [5]int{1, 0, 0, 0, 0} {
					t.Fatalf("operations structure/metadata/body/header/search = %v, want [1 0 0 0 0]", counts)
				}
				connections[requireStructureOnlyListing(t, tc.mode, server.Commands()[before:], connections, listing.tuple.UID)] = true
			}

			requireReadOnlyTranscript(t, server)
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}

			listings := 0
			for _, event := range auditLines(t, &audit) {
				requireAllowlistedKeys(t, event)
				if event["tool"] == "list_attachments" {
					listings++
					if event["event"] != "tool_call" || event["outcome"] != "ok" {
						t.Fatalf("listing audit event = %v", event)
					}
				}
			}
			if listings != 2 {
				t.Fatalf("listing audit events = %d, want 2", listings)
			}
			private := append(sentinels, reportID, freeID, "INBOX", "ledger.bin", "empty.bin", "unsized.bin", "fixture-user", "fixture-password", "TestMCPCredentialHelperProcess")
			for _, leaked := range private {
				if strings.Contains(audit.String(), leaked) {
					t.Fatalf("audit recorded %q", leaked)
				}
			}
		})
	}
}

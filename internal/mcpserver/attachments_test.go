package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

// GetMessageAttachmentStructure arranges a structure result from the fixture's
// metadata identity and normalized body without recording any read; it is
// fixture arrangement, never evidence of the bridge projector.
func (mail *fakeMail) GetMessageAttachmentStructure(ctx context.Context, identifier string) (bridge.MessageAttachmentStructure, error) {
	normalized, err := bridge.NormalizeMessage(bytes.NewReader(mail.body), bridge.NormalizeOptions{})
	if err != nil {
		return bridge.MessageAttachmentStructure{}, err
	}

	attachments := make([]bridge.AttachmentStructureEntry, 0, len(normalized.Attachments))
	for _, attachment := range normalized.Attachments {
		attachments = append(attachments, bridge.AttachmentStructureEntry{AttachmentMetadata: attachment})
	}

	return bridge.MessageAttachmentStructure{
		ID:          mail.metadata.ID,
		Mailbox:     mail.metadata.Mailbox,
		UIDValidity: mail.metadata.UIDValidity,
		UID:         mail.metadata.UID,
		Attachments: attachments,
		Truncation:  normalized.Truncation,
	}, nil
}

// structureMail answers the selected structure read with one independently
// arranged result or error and counts its calls; the embedded fakeMail keeps
// counting every metadata, body and header read.
type structureMail struct {
	*fakeMail
	structure      bridge.MessageAttachmentStructure
	structureErr   error
	structureCalls []string
}

func (mail *structureMail) GetMessageAttachmentStructure(ctx context.Context, identifier string) (bridge.MessageAttachmentStructure, error) {
	mail.structureCalls = append(mail.structureCalls, identifier)
	if mail.structureErr != nil {
		return bridge.MessageAttachmentStructure{}, mail.structureErr
	}

	return mail.structure, nil
}

// legacyMail exposes only the mandatory Mail methods, so it truly lacks the
// optional structure reader.
type legacyMail struct {
	mail *fakeMail
}

func (mail legacyMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	return mail.mail.ListFolders(ctx)
}

func (mail legacyMail) Status(ctx context.Context, mailbox string) (bridge.MailboxStatus, error) {
	return mail.mail.Status(ctx, mailbox)
}

func (mail legacyMail) SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error) {
	return mail.mail.SearchMailPage(ctx, query)
}

func (mail legacyMail) GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error) {
	return mail.mail.GetMessageMetadata(ctx, identifier)
}

func (mail legacyMail) GetMessageBody(ctx context.Context, identifier string) ([]byte, error) {
	return mail.mail.GetMessageBody(ctx, identifier)
}

func requireNoRawReads(t *testing.T, label string, mail *fakeMail) {
	t.Helper()

	if calls := len(mail.metaCalls) + len(mail.bodyCalls) + len(mail.headerCalls); calls != 0 {
		t.Fatalf("%s made %d metadata, body or header reads", label, calls)
	}
}

// selectedStructure is an arranged structure result whose attachments carry
// the given metadata and a private transfer octet count.
func selectedStructure(id string, tuple readTuple, attachments ...bridge.AttachmentMetadata) bridge.MessageAttachmentStructure {
	entries := make([]bridge.AttachmentStructureEntry, 0, len(attachments))
	for _, attachment := range attachments {
		entries = append(entries, bridge.AttachmentStructureEntry{AttachmentMetadata: attachment, HasTransferOctets: true, TransferOctets: 7654321})
	}

	return bridge.MessageAttachmentStructure{ID: id, Mailbox: tuple.Mailbox, UIDValidity: tuple.UIDValidity, UID: tuple.UID, Attachments: entries}
}

func octetAttachment(filename string) bridge.AttachmentMetadata {
	return bridge.AttachmentMetadata{Filename: filename, ContentType: "application/octet-stream", Disposition: "attachment"}
}

func TestListAttachmentsStructureMCP(t *testing.T) {
	t.Parallel()

	tuple := readTuple{Mailbox: "Folders/Selected", UIDValidity: 9001, UID: 102}

	t.Run("selected_only", func(t *testing.T) {
		t.Parallel()

		// The raw fixture lists report.pdf; the arranged result lists only
		// selected.bin, so any raw-derived entry is observable.
		mail := &structureMail{
			fakeMail: &fakeMail{
				metadata: withReadIdentity(bridge.MessageMetadata{ID: "opaque-selected", Mailbox: "INBOX", Subject: "Raw fixture"}, 1, 1),
				body:     []byte(syntheticAttachmentMIME),
			},
			structure: selectedStructure("opaque-selected", tuple, octetAttachment("selected.bin")),
		}
		listing := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "list_attachments", map[string]any{"messageId": "opaque-selected"}))

		if len(mail.structureCalls) != 1 || mail.structureCalls[0] != "opaque-selected" {
			t.Fatalf("structure calls = %q, want exactly the request id once", mail.structureCalls)
		}
		requireNoRawReads(t, "capable list_attachments", mail.fakeMail)
		if listing["id"] != "opaque-selected" || flatTuple(t, listing) != tuple {
			t.Fatalf("listing identity = %v/%+v, want the selected result", listing["id"], flatTuple(t, listing))
		}
		requireAttachmentEntries(t, "selected listing", listing, []wantAttachment{{filename: "selected.bin"}})

		legacy := &fakeMail{
			metadata: withReadIdentity(bridge.MessageMetadata{ID: "opaque-legacy", Mailbox: "INBOX"}, 9001, 102),
			body:     []byte(syntheticAttachmentMIME),
		}
		result := callTool(t, connectTestClient(t, Options{Mail: legacyMail{mail: legacy}}), "list_attachments", map[string]any{"messageId": "opaque-legacy"})
		requireRefusedWithoutTuple(t, result, errUnsupported)
		if text := resultText(t, result); strings.Contains(text, "INBOX") || strings.Contains(text, "report.pdf") {
			t.Fatalf("unsupported refusal carried detail: %s", text)
		}
		requireNoRawReads(t, "legacy list_attachments", legacy)
	})

	t.Run("schema_sizes", func(t *testing.T) {
		t.Parallel()

		mail := &structureMail{fakeMail: &fakeMail{}, structure: selectedStructure("opaque-sizes", tuple,
			declaredAttachment("positive.bin", 1234, true),
			declaredAttachment("unflagged.bin", 55, false),
		)}
		session := connectTestClient(t, Options{Mail: mail})

		result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-sizes"})
		listing := rawObject(t, result)
		requireClosedKeys(t, "list_attachments", listing, listAttachmentsKeys)
		if listing["id"] != "opaque-sizes" || flatTuple(t, listing) != tuple {
			t.Fatalf("listing identity = %v/%+v, want %+v", listing["id"], flatTuple(t, listing), tuple)
		}
		requireAttachmentEntries(t, "sizes listing", listing, []wantAttachment{
			{filename: "positive.bin", size: 1234, known: true},
			{filename: "unflagged.bin"},
		})
		requireReasons(t, "sizes listing", listing, listAttachmentsReasons)
		for _, leaked := range []string{"7654321", "transfer", "octets", "size\"", "text", "headers", "readState", "partId", "envelope"} {
			if strings.Contains(resultText(t, result), leaked) {
				t.Fatalf("listing exposed %q: %s", leaked, resultText(t, result))
			}
		}
		requireNoRawReads(t, "sizes listing", mail.fakeMail)

		// A mismatched or incomplete identity refuses without a tuple.
		for name, structure := range map[string]bridge.MessageAttachmentStructure{
			"mismatched id":   selectedStructure("opaque-other", tuple),
			"empty mailbox":   selectedStructure("opaque-identity", readTuple{UIDValidity: 9001, UID: 102}),
			"zero uid":        selectedStructure("opaque-identity", readTuple{Mailbox: "INBOX", UIDValidity: 9001}),
			"zero generation": selectedStructure("opaque-identity", readTuple{Mailbox: "INBOX", UID: 102}),
		} {
			mail.structure = structure
			result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-identity"})
			requireRefusedWithoutTuple(t, result, errUnavailable)
			if strings.Contains(resultText(t, result), "INBOX") || strings.Contains(resultText(t, result), "opaque-other") {
				t.Fatalf("%s refusal carried identity detail: %s", name, resultText(t, result))
			}
		}
		requireNoRawReads(t, "identity refusals", mail.fakeMail)
	})

	t.Run("omissions_output", func(t *testing.T) {
		t.Parallel()

		mail := &structureMail{fakeMail: &fakeMail{}}
		session := connectTestClient(t, Options{Mail: mail})

		// Header, text, reference and thread limits do not mark a listing.
		mail.structure = selectedStructure("opaque-controls", tuple, octetAttachment("kept.bin"))
		mail.structure.Truncation = bridge.Truncation{Text: true, References: true, Headers: true, HeaderCount: true, ThreadCycle: true, ThreadDepth: true, ThreadMessages: true}
		listing := rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-controls"}))
		requireAttachmentEntries(t, "controls listing", listing, []wantAttachment{{filename: "kept.bin"}})
		requireReasons(t, "controls listing", listing, listAttachmentsReasons)

		// One unfittable entry falls back to the identity-only listing.
		huge := octetAttachment(strings.Repeat("y", maxToolResultBytes))
		mail.structure = selectedStructure("opaque-fallback", tuple, huge)
		mail.structure.Truncation = bridge.Truncation{Attachments: true}
		fallback := rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-fallback"}))
		if fallback["id"] != "opaque-fallback" || flatTuple(t, fallback) != tuple || len(rawItems(t, fallback, "attachments")) != 0 {
			t.Fatalf("fallback listing = %v", fallback)
		}
		requireReasons(t, "fallback listing", fallback, listAttachmentsReasons, "attachments", "output")

		requireNoRawReads(t, "omission listings", mail.fakeMail)
	})

	t.Run("errors_no_fallback", func(t *testing.T) {
		t.Parallel()

		var audit bytes.Buffer
		mail := &structureMail{fakeMail: &fakeMail{}}
		session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&audit)})

		for name, arguments := range map[string]json.RawMessage{
			"unknown":      json.RawMessage(`{"messageId":"opaque-errors","mailbox":"INBOX"}`),
			"empty":        json.RawMessage(`{"messageId":""}`),
			"oversized id": json.RawMessage(`{"messageId":"` + strings.Repeat("x", maxMessageIDArgumentBytes+1) + `"}`),
		} {
			if result, code := runListAttachments(context.Background(), Options{Mail: mail}, arguments); result != nil || code != errInvalidArgument {
				t.Fatalf("%s arguments = %v/%q, want code-only invalid_argument", name, result, code)
			}
		}
		if len(mail.structureCalls) != 0 {
			t.Fatalf("invalid arguments reached the adapter %d times", len(mail.structureCalls))
		}
		requireNoRawReads(t, "invalid arguments", mail.fakeMail)

		const detail = "peer-detail-sentinel helper-sentinel INBOX report.pdf"
		for _, tc := range []struct {
			name string
			err  error
			want string
		}{
			{"message_not_found", &bridge.Error{Code: bridge.CodeMessageNotFound}, errNotFound},
			{"credential_timeout", &bridge.Error{Code: bridge.CodeCredentialTimeout}, errTimedOut},
			{"protocol", fmt.Errorf("%s: %w", detail, &bridge.Error{Code: bridge.CodeIMAPProtocol}), errUnavailable},
			{"unknown_wrapped", fmt.Errorf("%s: %w", detail, errors.New("synthetic unknown")), errInternal},
		} {
			before := len(mail.structureCalls)
			mail.structureErr = tc.err
			result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-errors"})
			requireRefusedWithoutTuple(t, result, tc.want)
			for _, leaked := range []string{"peer-detail-sentinel", "helper-sentinel", "INBOX", "report.pdf", "attachments"} {
				if strings.Contains(resultText(t, result), leaked) {
					t.Fatalf("%s refusal carried %q: %s", tc.name, leaked, resultText(t, result))
				}
			}
			if calls := len(mail.structureCalls) - before; calls != 1 {
				t.Fatalf("%s made %d structure calls, want exactly 1 without replay", tc.name, calls)
			}
		}
		requireNoRawReads(t, "adapter errors", mail.fakeMail)

		for _, event := range auditLines(t, &audit) {
			requireAllowlistedKeys(t, event)
		}
		for _, leaked := range []string{"opaque-errors", "peer-detail-sentinel", "helper-sentinel", "INBOX", "report.pdf"} {
			if strings.Contains(audit.String(), leaked) {
				t.Fatalf("audit recorded %q", leaked)
			}
		}
	})

	t.Run("docs_contract", func(t *testing.T) {
		t.Parallel()

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		documentation := strings.Join(strings.Fields(string(contents)), " ")
		for _, statement := range []string{
			"Each attachment listing opens a fresh dedicated TLS session, runs the credential helper and logs in again; it does not reuse the pooled session.",
			"A large raw message body alone is not a reason to refuse a listing.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing listing statement: %q", statement)
			}
		}
		for _, stale := range []string{
			"Listing attachments still fetches and parses",
			"`list_attachments` reasons are `headers`",
		} {
			if strings.Contains(documentation, stale) {
				t.Errorf("Mail documentation keeps the raw listing claim %q", stale)
			}
		}
	})
}

func TestListAttachmentsReturnsMetadataOnly(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadata: withReadIdentity(bridge.MessageMetadata{ID: "opaque-2", Mailbox: "INBOX", Subject: "Attachment fixture"}, 9001, 102),
		body:     []byte(syntheticAttachmentMIME),
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-2"})

	var decoded struct {
		ID          string `json:"id"`
		Mailbox     string `json:"mailbox"`
		Attachments []struct {
			Filename     string `json:"filename"`
			ContentType  string `json:"contentType"`
			Disposition  string `json:"disposition"`
			DeclaredSize int64  `json:"declaredSize"`
		} `json:"attachments"`
		Truncated bool `json:"truncated"`
	}
	decodeResult(t, result, &decoded)
	if decoded.ID != "opaque-2" || decoded.Mailbox != "INBOX" {
		t.Fatalf("unexpected identity: %+v", decoded)
	}
	if len(decoded.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(decoded.Attachments))
	}
	if decoded.Attachments[0].Filename != "report.pdf" || decoded.Attachments[0].Disposition != "attachment" {
		t.Fatalf("unexpected attachment: %+v", decoded.Attachments[0])
	}

	text := resultText(t, result)
	if strings.Contains(text, "JVBERi0xLjQK") || strings.Contains(text, "See attachment.") {
		t.Fatal("attachment or body content leaked into list_attachments result")
	}
	requireNoRawReads(t, "list_attachments", mail)
}

func TestListAttachmentsValidatesIdentifier(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "list_attachments", map[string]any{"messageId": strings.Repeat("x", maxMessageIDArgumentBytes+1)})

	requireToolError(t, result, errInvalidArgument)
	if len(mail.metaCalls)+len(mail.bodyCalls) != 0 {
		t.Fatal("adapter reached despite invalid identifier")
	}
}

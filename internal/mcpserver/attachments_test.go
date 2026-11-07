package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

// GetMessageAttachmentStructure arranges a selected structure result from the
// fixture's original metadata identity and its legacy attachment metadata. It
// records no metadata, body or header call; the normalization here is only
// fixture arrangement, never evidence of the bridge projector.
func (mail *fakeMail) GetMessageAttachmentStructure(ctx context.Context, identifier string) (bridge.MessageAttachmentStructure, error) {
	if mail.err != nil {
		return bridge.MessageAttachmentStructure{}, mail.err
	}

	metadata, raw := mail.metadata, mail.body
	if mail.metadataByID != nil {
		found, ok := mail.metadataByID[identifier]
		if !ok {
			return bridge.MessageAttachmentStructure{}, &bridge.Error{Code: bridge.CodeStaleMessageID}
		}
		metadata = found
	}
	if mail.bodyByID != nil {
		found, ok := mail.bodyByID[identifier]
		if !ok {
			return bridge.MessageAttachmentStructure{}, &bridge.Error{Code: bridge.CodeStaleMessageID}
		}
		raw = found
	}

	normalized, err := bridge.NormalizeMessage(bytes.NewReader(raw), bridge.NormalizeOptions{})
	if err != nil {
		return bridge.MessageAttachmentStructure{}, err
	}

	attachments := make([]bridge.AttachmentStructureEntry, 0, len(normalized.Attachments))
	for _, attachment := range normalized.Attachments {
		attachments = append(attachments, bridge.AttachmentStructureEntry{AttachmentMetadata: attachment})
	}

	return bridge.MessageAttachmentStructure{
		ID:          metadata.ID,
		Mailbox:     metadata.Mailbox,
		UIDValidity: metadata.UIDValidity,
		UID:         metadata.UID,
		Attachments: attachments,
		Truncation: bridge.Truncation{
			MIMEParts:   normalized.Truncation.MIMEParts,
			MIMEDepth:   normalized.Truncation.MIMEDepth,
			Attachments: normalized.Truncation.Attachments,
		},
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

// requireNoRawReads fails when any metadata, body or header read ran.
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
		var _ Mail = legacyMail{mail: legacy}
		if _, capable := any(legacyMail{mail: legacy}).(interface {
			GetMessageAttachmentStructure(context.Context, string) (bridge.MessageAttachmentStructure, error)
		}); capable {
			t.Fatal("legacy Mail unexpectedly implements the structure reader")
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

		maximum := readTuple{Mailbox: "INBOX", UIDValidity: math.MaxUint32, UID: math.MaxUint32}
		positive, zero, absent, negative, unflagged := octetAttachment("positive.bin"), octetAttachment("zero.bin"), octetAttachment("absent.bin"), octetAttachment("negative.bin"), octetAttachment("unflagged.bin")
		positive.HasDeclaredSize, positive.DeclaredSize = true, 1234
		zero.HasDeclaredSize = true
		negative.HasDeclaredSize, negative.DeclaredSize = true, -7
		unflagged.DeclaredSize = 55
		mail := &structureMail{fakeMail: &fakeMail{}, structure: selectedStructure("opaque-sizes", maximum, positive, zero, absent, negative, unflagged)}
		session := connectTestClient(t, Options{Mail: mail})

		result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-sizes"})
		listing := rawObject(t, result)
		requireClosedKeys(t, "list_attachments", listing, listAttachmentsKeys)
		if listing["id"] != "opaque-sizes" || flatTuple(t, listing) != maximum {
			t.Fatalf("listing identity = %v/%+v, want %+v", listing["id"], flatTuple(t, listing), maximum)
		}
		if listing["uidvalidity"] != json.Number("4294967295") || listing["uid"] != json.Number("4294967295") {
			t.Fatalf("listing did not serialize the maximum exactly: %v", listing)
		}
		requireAttachmentEntries(t, "sizes listing", listing, []wantAttachment{
			{filename: "positive.bin", size: 1234, known: true},
			{filename: "zero.bin", size: 0, known: true},
			{filename: "absent.bin"},
			{filename: "negative.bin"},
			{filename: "unflagged.bin"},
		})
		requireReasons(t, "sizes listing", listing, listAttachmentsReasons)
		for _, leaked := range []string{"7654321", "transfer", "octets", "size\"", "text", "headers", "readState", "partId", "envelope"} {
			if strings.Contains(resultText(t, result), leaked) {
				t.Fatalf("listing exposed %q: %s", leaked, resultText(t, result))
			}
		}
		requireNoRawReads(t, "sizes listing", mail.fakeMail)

		// A complete empty structure lists [] with an empty truncation object.
		mail.structure = selectedStructure("opaque-empty", tuple)
		mail.structure.Attachments = nil
		empty := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-empty"})
		if !strings.Contains(resultText(t, empty), `"attachments":[]`) || !strings.Contains(resultText(t, empty), `"truncation":{}`) {
			t.Fatalf("complete empty listing = %s, want [] and {}", resultText(t, empty))
		}
		emptyObject := rawObject(t, empty)
		requireClosedKeys(t, "empty listing", emptyObject, listAttachmentsKeys)
		requireReasons(t, "empty listing", emptyObject, listAttachmentsReasons)

		// A mismatched or incomplete identity refuses without a tuple.
		for name, structure := range map[string]bridge.MessageAttachmentStructure{
			"mismatched id":   selectedStructure("opaque-other", tuple),
			"empty id":        selectedStructure("", tuple),
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
		listed := []bridge.AttachmentMetadata{octetAttachment("kept.bin")}

		for _, tc := range []struct {
			name        string
			truncation  bridge.Truncation
			attachments []bridge.AttachmentMetadata
			want        []string
		}{
			{name: "mime_parts", truncation: bridge.Truncation{MIMEParts: true}, attachments: listed, want: []string{"mimeParts"}},
			{name: "mime_depth", truncation: bridge.Truncation{MIMEDepth: true}, attachments: listed, want: []string{"mimeDepth"}},
			{name: "attachments", truncation: bridge.Truncation{Attachments: true}, attachments: listed, want: []string{"attachments"}},
			{name: "combined", truncation: bridge.Truncation{MIMEParts: true, MIMEDepth: true, Attachments: true}, attachments: listed, want: []string{"mimeParts", "mimeDepth", "attachments"}},
			{name: "empty_incomplete", truncation: bridge.Truncation{MIMEDepth: true}, want: []string{"mimeDepth"}},
			{name: "text_reference_header_controls", truncation: bridge.Truncation{Text: true, References: true, Headers: true, HeaderCount: true, ThreadCycle: true, ThreadDepth: true, ThreadMessages: true}, attachments: listed},
		} {
			mail.structure = selectedStructure("opaque-"+tc.name, tuple, tc.attachments...)
			mail.structure.Truncation = tc.truncation
			listing := rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-" + tc.name}))
			requireClosedKeys(t, tc.name, listing, listAttachmentsKeys)
			if listing["id"] != "opaque-"+tc.name || flatTuple(t, listing) != tuple {
				t.Fatalf("%s identity = %v/%+v", tc.name, listing["id"], flatTuple(t, listing))
			}
			wantEntries := []wantAttachment{}
			for _, attachment := range tc.attachments {
				wantEntries = append(wantEntries, wantAttachment{filename: attachment.Filename})
			}
			requireAttachmentEntries(t, tc.name, listing, wantEntries)
			requireReasons(t, tc.name, listing, listAttachmentsReasons, tc.want...)
		}

		// Many entries shrink whole through the existing list encoder and
		// keep the prior structural reason.
		pressure := make([]bridge.AttachmentMetadata, 0, 600)
		for index := range 600 {
			pressure = append(pressure, octetAttachment(fmt.Sprintf("%03d-%s", index, strings.Repeat("b", 250))))
		}
		mail.structure = selectedStructure("opaque-pressure", tuple, pressure...)
		mail.structure.Truncation = bridge.Truncation{MIMEDepth: true}
		result := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-pressure"})
		if text := resultText(t, result); len(text) > maxToolResultBytes || !utf8.ValidString(text) {
			t.Fatalf("shrunk listing is %d bytes or invalid UTF-8", len(text))
		}
		shrunk := rawObject(t, result)
		if shrunk["id"] != "opaque-pressure" || flatTuple(t, shrunk) != tuple {
			t.Fatalf("shrunk identity = %v/%+v", shrunk["id"], flatTuple(t, shrunk))
		}
		if kept := rawItems(t, shrunk, "attachments"); len(kept) == 0 || len(kept) >= len(pressure) {
			t.Fatalf("shrunk listing kept %d of %d entries", len(kept), len(pressure))
		}
		requireReasons(t, "shrunk listing", shrunk, listAttachmentsReasons, "mimeDepth", "output")

		// One unfittable entry falls back to the identity-only listing.
		huge := octetAttachment(strings.Repeat("y", maxToolResultBytes))
		mail.structure = selectedStructure("opaque-fallback", tuple, huge)
		mail.structure.Truncation = bridge.Truncation{Attachments: true}
		fallback := rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-fallback"}))
		if fallback["id"] != "opaque-fallback" || flatTuple(t, fallback) != tuple || len(rawItems(t, fallback, "attachments")) != 0 {
			t.Fatalf("fallback listing = %v", fallback)
		}
		requireReasons(t, "fallback listing", fallback, listAttachmentsReasons, "attachments", "output")

		// An identity that cannot fit refuses rather than shortening.
		mail.structure = selectedStructure("opaque-unfittable", readTuple{Mailbox: "Folders/" + strings.Repeat("x", maxToolResultBytes), UIDValidity: 9, UID: 9})
		unfittable := callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-unfittable"})
		requireRefusedWithoutTuple(t, unfittable, errBoundsExceeded)
		if strings.Contains(resultText(t, unfittable), "Folders/") {
			t.Fatal("unfittable refusal carried the mailbox")
		}
		requireNoRawReads(t, "omission listings", mail.fakeMail)
	})

	t.Run("errors_no_fallback", func(t *testing.T) {
		t.Parallel()

		var audit bytes.Buffer
		mail := &structureMail{fakeMail: &fakeMail{
			metadata: withReadIdentity(bridge.MessageMetadata{ID: "opaque-errors", Mailbox: "INBOX"}, 9001, 102),
			body:     []byte(syntheticAttachmentMIME),
		}}
		session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&audit)})

		for name, arguments := range map[string]json.RawMessage{
			"missing":        json.RawMessage(`{}`),
			"unknown":        json.RawMessage(`{"messageId":"opaque-errors","mailbox":"INBOX"}`),
			"null":           json.RawMessage(`null`),
			"null id":        json.RawMessage(`{"messageId":null}`),
			"duplicate":      json.RawMessage(`{"messageId":"opaque-errors","messageId":"opaque-other"}`),
			"case alias":     json.RawMessage(`{"messageId":"opaque-errors","MessageId":"opaque-other"}`),
			"empty":          json.RawMessage(`{"messageId":""}`),
			"oversized id":   json.RawMessage(`{"messageId":"` + strings.Repeat("x", maxMessageIDArgumentBytes+1) + `"}`),
			"oversized args": json.RawMessage(strings.Repeat(" ", maxToolArgumentsBytes) + `{"messageId":"opaque-errors"}`),
		} {
			if result, code := runListAttachments(context.Background(), Options{Mail: mail}, arguments); result != nil || code != errInvalidArgument {
				t.Fatalf("%s arguments = %v/%q, want code-only invalid_argument", name, result, code)
			}
			if name == "oversized args" {
				// The client compacts raw arguments, so only the direct
				// handler call above sees the oversized form.
				continue
			}
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_attachments", Arguments: arguments})
			if err == nil && !result.IsError {
				t.Fatalf("%s arguments returned a normal result: %s", name, resultText(t, result))
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
			{"stale", &bridge.Error{Code: bridge.CodeStaleMessageID}, errStaleID},
			{"message_not_found", &bridge.Error{Code: bridge.CodeMessageNotFound}, errNotFound},
			{"mailbox_not_found", &bridge.Error{Code: bridge.CodeMailboxNotFound}, errNotFound},
			{"unsupported", &bridge.Error{Code: bridge.CodeUnsupported}, errUnsupported},
			{"bounds", &bridge.Error{Code: bridge.CodeBoundsExceeded}, errBoundsExceeded},
			{"timeout", &bridge.Error{Code: bridge.CodeCommandTimedOut}, errTimedOut},
			{"credential_timeout", &bridge.Error{Code: bridge.CodeCredentialTimeout}, errTimedOut},
			{"cancel", &bridge.Error{Code: bridge.CodeOperationCanceled}, errCanceled},
			{"auth", &bridge.Error{Code: bridge.CodeAuthentication}, errUnavailable},
			{"protocol", fmt.Errorf("%s: %w", detail, &bridge.Error{Code: bridge.CodeIMAPProtocol}), errUnavailable},
			{"helper", &bridge.Error{Code: bridge.CodeCredentialCommand}, errUnavailable},
			{"malformed_mime", &bridge.NormalizeError{Code: "malformed_mime"}, errInternal},
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

		// Missing capability refuses without any fallback read.
		legacy := &fakeMail{metadata: mail.metadata, body: mail.body}
		requireRefusedWithoutTuple(t, callTool(t, connectTestClient(t, Options{Mail: legacyMail{mail: legacy}}), "list_attachments", map[string]any{"messageId": "opaque-errors"}), errUnsupported)
		requireNoRawReads(t, "missing capability", legacy)

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
			"Listing attachments requests UID BODYSTRUCTURE only; it does not fetch or parse the message body.",
			"`list_attachments` reasons are `mimeParts`, `mimeDepth`, `attachments` and `output`.",
			"Each attachment listing opens a fresh dedicated TLS session, runs the credential helper and logs in again; it does not reuse the pooled session.",
			"A large raw message body alone is not a reason to refuse a listing.",
			"`get_message` still fetches and parses the bounded raw MIME message with `BODY.PEEK[]`",
			"The value is a sender-controlled declaration, not a count of decoded bytes or of the transferred part.",
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

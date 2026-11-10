package mcpserver

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wevial/croton-mcp/bridge"
)

func TestOversizeResultsShrinkToValidBoundedJSON(t *testing.T) {
	t.Parallel()

	folders := make([]bridge.Folder, 900)
	for index := range folders {
		folders[index] = bridge.Folder{Name: "Folders/" + strings.Repeat("х", 200), Delimiter: "/"}
	}
	var audit bytes.Buffer
	session := connectTestClient(t, Options{Audit: NewAuditor(&audit), Mail: &fakeMail{
		folders:  folders,
		metadata: identity("opaque-big", 101),
		body:     []byte("Subject: Oversize\r\n\r\n" + strings.Repeat("☃", 80000) + "\r\n"),
	}})

	listed := callTool(t, session, "list_folders", map[string]any{})
	var kept listFoldersResult
	decodeResult(t, listed, &kept)
	if len(resultText(t, listed)) > maxToolResultBytes || !kept.Truncated || len(kept.Folders) == 0 || len(kept.Folders) >= len(folders) {
		t.Fatalf("folders kept %d of %d (truncated %v)", len(kept.Folders), len(folders), kept.Truncated)
	}
	for _, folder := range kept.Folders {
		if folder != (folderResult{Name: folders[0].Name, Delimiter: "/"}) {
			t.Fatalf("folder entry altered by truncation: %.40q", folder.Name)
		}
	}

	message := callTool(t, session, "get_message", map[string]any{"messageId": "opaque-big"})
	var decoded getMessageResult
	decodeResult(t, message, &decoded)
	if len(resultText(t, message)) > maxToolResultBytes || !utf8.ValidString(decoded.Text) || strings.ContainsRune(decoded.Text, utf8.RuneError) {
		t.Fatalf("message text is not a bounded rune-safe prefix: %d bytes", len(resultText(t, message)))
	}
	if decoded.ID != "opaque-big" || decoded.UIDValidity != 3001 || decoded.UID != 101 || decoded.Truncation != (messageTruncation{Output: true}) || !decoded.Truncated {
		t.Fatalf("truncated message lost identity or reasons: %+v", decoded.Truncation)
	}

	for _, event := range auditLines(t, &audit) {
		if event["truncated"] != true {
			t.Fatalf("audit event lacks truncation metadata: %v", event)
		}
	}
}

func TestUnshrinkableOversizeResultIsBoundsExceeded(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("é", maxToolResultBytes)
	var audit bytes.Buffer
	session := connectTestClient(t, Options{Audit: NewAuditor(&audit), Mail: &fakeMail{
		metadata:  bridge.MessageMetadata{ID: huge, Mailbox: "INBOX", UIDValidity: 3001, UID: 7},
		body:      []byte("Subject: Oversize identity\r\n\r\nbody\r\n"),
		structure: bridge.MessageAttachmentStructure{ID: "opaque-target", Mailbox: huge, UIDValidity: 3001, UID: 7},
	}})

	for _, tool := range []string{"get_message", "list_attachments"} {
		result := callTool(t, session, tool, map[string]any{"messageId": "opaque-target"})
		requireToolError(t, result, errBoundsExceeded)
		if strings.Contains(resultText(t, result), "é") {
			t.Fatalf("%s wrote oversize content: %.80s", tool, resultText(t, result))
		}
	}

	events := auditLines(t, &audit)
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2", len(events))
	}
	for _, event := range events {
		if event["outcome"] != "error" || event["code"] != errBoundsExceeded {
			t.Fatalf("audit event = %v, want bounds_exceeded", event)
		}
	}

	attachments := &listAttachmentsResult{ID: "opaque-att", Mailbox: "INBOX", UIDValidity: 3001, UID: 7, Truncation: attachmentListTruncation{MIMEParts: true}}
	for range 4000 {
		attachments.Attachments = append(attachments.Attachments, attachmentResult{Filename: strings.Repeat("f", 100)})
	}
	encoded, truncated, err := encodeBounded(attachments)
	var kept listAttachmentsResult
	if err != nil || !truncated || json.Unmarshal(encoded, &kept) != nil || kept.ID != "opaque-att" || kept.UID != 7 || kept.Truncation != (attachmentListTruncation{MIMEParts: true, Output: true}) {
		t.Fatalf("attachment shrink = %.120s, %v, %v", encoded, truncated, err)
	}
}

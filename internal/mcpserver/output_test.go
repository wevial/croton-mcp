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

func TestEncodeBoundedFallbacks(t *testing.T) {
	t.Parallel()

	huge := strings.Repeat("é", maxToolResultBytes)

	encoded, truncated, err := encodeBounded(map[string]string{"blob": huge})
	if err != nil || !truncated || string(encoded) != `{"truncated":true}` {
		t.Fatalf("unshrinkable value = %s, %v, %v", encoded, truncated, err)
	}

	thread := &getThreadResult{ID: huge, Mailbox: "INBOX", Nodes: []threadNodeResult{{Key: "opaque-only", Mailbox: "INBOX", UIDValidity: 3001, UID: 7, Subject: huge}},
		UnavailableMembers: []threadUnavailableMember{{Reason: threadUnavailableStaleID}}}
	encoded, truncated, err = encodeBounded(thread)
	var fallback map[string]any
	if err != nil || !truncated || json.Unmarshal(encoded, &fallback) != nil {
		t.Fatalf("thread fallback = %.80s, %v, %v", encoded, truncated, err)
	}
	if id, _ := fallback["id"].(string); len(fallback) != 4 || len(id) > 1024 || !strings.HasPrefix(huge, id) || fallback["truncated"] != true || len(fallback["nodes"].([]any)) != 0 {
		t.Fatalf("thread fallback is not empty bounded context: %.200s", encoded)
	}

	for _, result := range []any{
		&getMessageResult{ID: huge, Mailbox: "INBOX", UIDValidity: 3001, UID: 7},
		&listAttachmentsResult{ID: huge, Mailbox: "INBOX", UIDValidity: 3001, UID: 7},
	} {
		if _, _, err := encodeBounded(result); err != errUnretainableIdentity {
			t.Fatalf("unretainable identity %T: err = %v", result, err)
		}
	}

	attachments := &listAttachmentsResult{ID: "opaque-att", Mailbox: "INBOX", UIDValidity: 3001, UID: 7, Truncation: attachmentListTruncation{MIMEParts: true}}
	for range 4000 {
		attachments.Attachments = append(attachments.Attachments, attachmentResult{Filename: strings.Repeat("f", 100)})
	}
	encoded, truncated, err = encodeBounded(attachments)
	var kept listAttachmentsResult
	if err != nil || !truncated || json.Unmarshal(encoded, &kept) != nil || kept.ID != "opaque-att" || kept.UID != 7 || kept.Truncation != (attachmentListTruncation{MIMEParts: true, Output: true}) {
		t.Fatalf("attachment shrink = %.120s, %v, %v", encoded, truncated, err)
	}
}

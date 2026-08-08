package mcpserver

import (
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func TestListAttachmentsReturnsMetadataOnly(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadata: bridge.MessageMetadata{ID: "opaque-2", Mailbox: "INBOX", Subject: "Attachment fixture"},
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

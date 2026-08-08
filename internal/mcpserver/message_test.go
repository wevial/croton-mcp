package mcpserver

import (
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

const syntheticPlainMIME = "From: Croton Fixture <fixture@croton.test>\r\n" +
	"To: Reader <reader@croton.test>\r\n" +
	"Subject: Welcome to Croton\r\n" +
	"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
	"Message-ID: <welcome@croton.test>\r\n" +
	"\r\n" +
	"Welcome to Croton. This synthetic message exists only for tests.\r\n"

const syntheticAttachmentMIME = "From: Croton Fixture <fixture@croton.test>\r\n" +
	"To: Reader <reader@croton.test>\r\n" +
	"Subject: Attachment fixture\r\n" +
	"Message-ID: <attach@croton.test>\r\n" +
	"MIME-Version: 1.0\r\n" +
	"Content-Type: multipart/mixed; boundary=croton-boundary\r\n" +
	"\r\n" +
	"--croton-boundary\r\n" +
	"Content-Type: text/plain; charset=utf-8\r\n" +
	"\r\n" +
	"See attachment.\r\n" +
	"--croton-boundary\r\n" +
	"Content-Type: application/pdf\r\n" +
	"Content-Disposition: attachment; filename=\"report.pdf\"\r\n" +
	"Content-Transfer-Encoding: base64\r\n" +
	"\r\n" +
	"JVBERi0xLjQK\r\n" +
	"--croton-boundary--\r\n"

type getMessageDecoded struct {
	ID      string `json:"id"`
	Mailbox string `json:"mailbox"`
	Size    int64  `json:"size"`
	Headers struct {
		From      string `json:"from"`
		To        string `json:"to"`
		Subject   string `json:"subject"`
		MessageID string `json:"messageId"`
	} `json:"headers"`
	Text        string `json:"text"`
	TextSource  string `json:"textSource"`
	Attachments []struct {
		Filename    string `json:"filename"`
		ContentType string `json:"contentType"`
	} `json:"attachments"`
	Truncated bool `json:"truncated"`
}

func TestGetMessageComposesMetadataBodyAndNormalization(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadata: bridge.MessageMetadata{ID: "opaque-1", Mailbox: "INBOX", Subject: "Welcome to Croton", Size: int64(len(syntheticPlainMIME))},
		body:     []byte(syntheticPlainMIME),
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_message", map[string]any{"messageId": "opaque-1"})

	var decoded getMessageDecoded
	decodeResult(t, result, &decoded)
	if decoded.ID != "opaque-1" || decoded.Mailbox != "INBOX" {
		t.Fatalf("unexpected identity: %+v", decoded)
	}
	if decoded.Headers.Subject != "Welcome to Croton" || decoded.Headers.MessageID != "<welcome@croton.test>" {
		t.Fatalf("unexpected headers: %+v", decoded.Headers)
	}
	if decoded.TextSource != "plain" || !strings.Contains(decoded.Text, "Welcome to Croton.") {
		t.Fatalf("unexpected text: source=%q text=%q", decoded.TextSource, decoded.Text)
	}
	if len(mail.metaCalls) != 1 || len(mail.bodyCalls) != 1 || mail.bodyCalls[0] != "opaque-1" {
		t.Fatalf("unexpected adapter calls: meta=%v body=%v", mail.metaCalls, mail.bodyCalls)
	}
}

func TestGetMessageReturnsAttachmentMetadataWithoutBytes(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadata: bridge.MessageMetadata{ID: "opaque-2", Mailbox: "INBOX", Subject: "Attachment fixture"},
		body:     []byte(syntheticAttachmentMIME),
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_message", map[string]any{"messageId": "opaque-2"})

	var decoded getMessageDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Attachments) != 1 {
		t.Fatalf("attachments = %d, want 1", len(decoded.Attachments))
	}
	if decoded.Attachments[0].Filename != "report.pdf" || decoded.Attachments[0].ContentType != "application/pdf" {
		t.Fatalf("unexpected attachment: %+v", decoded.Attachments[0])
	}
	if strings.Contains(resultText(t, result), "JVBERi0xLjQK") {
		t.Fatal("attachment bytes leaked into tool result")
	}
}

func TestGetMessageValidatesIdentifierArgument(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments map[string]any
	}{
		{"missing", map[string]any{}},
		{"empty", map[string]any{"messageId": ""}},
		{"oversize", map[string]any{"messageId": strings.Repeat("x", maxMessageIDArgumentBytes+1)}},
		{"wrong type", map[string]any{"messageId": 12}},
		{"unknown field", map[string]any{"messageId": "opaque-1", "raw": true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mail := &fakeMail{}
			session := connectTestClient(t, Options{Mail: mail})

			result := callTool(t, session, "get_message", testCase.arguments)

			requireToolError(t, result, errInvalidArgument)
			if len(mail.metaCalls)+len(mail.bodyCalls) != 0 {
				t.Fatal("adapter reached despite invalid identifier")
			}
		})
	}
}

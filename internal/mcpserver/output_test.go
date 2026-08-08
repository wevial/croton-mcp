package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/wevial/croton-mcp/bridge"
)

func TestOversizeMessageTextTruncatesToValidJSONOnRuneBoundary(t *testing.T) {
	t.Parallel()

	// Multibyte body large enough that the serialized result exceeds the
	// result budget; naive byte-slicing would split a rune or the JSON.
	snowmen := strings.Repeat("☃", 80000)
	body := "From: Croton Fixture <fixture@croton.test>\r\n" +
		"Subject: Oversize fixture\r\n" +
		"Message-ID: <oversize@croton.test>\r\n" +
		"\r\n" + snowmen + "\r\n"
	mail := &fakeMail{
		metadata: bridge.MessageMetadata{ID: "opaque-big", Mailbox: "INBOX", Subject: "Oversize fixture"},
		body:     []byte(body),
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_message", map[string]any{"messageId": "opaque-big"})

	text := resultText(t, result)
	if len(text) > maxToolResultBytes {
		t.Fatalf("result bytes = %d, want <= %d", len(text), maxToolResultBytes)
	}
	if !utf8.ValidString(text) {
		t.Fatal("truncated result is not valid UTF-8")
	}
	var decoded getMessageDecoded
	decodeResult(t, result, &decoded)
	if !decoded.Truncated {
		t.Fatal("oversize result not flagged as truncated")
	}
	if !utf8.ValidString(decoded.Text) || strings.Contains(decoded.Text, "�") {
		t.Fatal("truncated text broke a multibyte rune")
	}
	if decoded.ID != "opaque-big" || decoded.Headers.Subject != "Oversize fixture" {
		t.Fatalf("truncation dropped identity fields: %+v", decoded)
	}
}

func TestOversizeFolderListTruncatesWholeItems(t *testing.T) {
	t.Parallel()

	folders := make([]bridge.Folder, 900)
	for index := range folders {
		folders[index] = bridge.Folder{Name: "Folders/" + strings.Repeat("х", 200) + "-" + string(rune('a'+index%26)), Delimiter: "/"}
	}
	mail := &fakeMail{folders: folders}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "list_folders", map[string]any{})

	text := resultText(t, result)
	if len(text) > maxToolResultBytes {
		t.Fatalf("result bytes = %d, want <= %d", len(text), maxToolResultBytes)
	}
	var decoded struct {
		Folders []struct {
			Name string `json:"name"`
		} `json:"folders"`
		Truncated bool `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(text), &decoded); err != nil {
		t.Fatalf("oversize folder result is not valid JSON: %v", err)
	}
	if !decoded.Truncated {
		t.Fatal("oversize folder list not flagged as truncated")
	}
	if len(decoded.Folders) == 0 || len(decoded.Folders) >= 900 {
		t.Fatalf("folders = %d, want a truncated non-empty prefix", len(decoded.Folders))
	}
	for _, folder := range decoded.Folders {
		if !strings.HasPrefix(folder.Name, "Folders/") {
			t.Fatalf("folder entry mangled by truncation: %q", folder.Name)
		}
	}
}

func TestEncodeBoundedNeverReturnsInvalidJSON(t *testing.T) {
	t.Parallel()

	// A value with no shrink support must fall back to a minimal valid
	// envelope rather than sliced bytes.
	huge := map[string]string{"blob": strings.Repeat("x", maxToolResultBytes*2)}

	encoded, truncated, err := encodeBounded(huge)
	if err != nil {
		t.Fatalf("encodeBounded: %v", err)
	}
	if len(encoded) > maxToolResultBytes {
		t.Fatalf("encoded bytes = %d, want <= %d", len(encoded), maxToolResultBytes)
	}
	if !truncated {
		t.Fatal("fallback envelope not flagged truncated")
	}
	if !json.Valid(encoded) {
		t.Fatalf("fallback envelope is invalid JSON: %q", encoded)
	}
}

func TestEncodeBoundedPreservesToolSpecificShapeOnFallback(t *testing.T) {
	t.Parallel()

	result := &getThreadResult{
		ID:      strings.Repeat("x", maxToolResultBytes*2),
		Mailbox: "INBOX",
		Nodes:   []threadNodeResult{{Key: "target"}},
	}
	encoded, truncated, err := encodeBounded(result)
	if err != nil {
		t.Fatalf("encodeBounded: %v", err)
	}
	if !truncated {
		t.Fatal("oversize tool result was not marked truncated")
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("decode fallback: %v", err)
	}
	for _, key := range []string{"id", "mailbox", "nodes", "truncated"} {
		if _, ok := fields[key]; !ok {
			t.Fatalf("fallback dropped get_thread field %q: %s", key, encoded)
		}
	}
}

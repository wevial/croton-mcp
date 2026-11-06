package mcpserver

import (
	"encoding/json"
	"fmt"
	"reflect"
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
		metadata: bridge.MessageMetadata{ID: "opaque-big", Mailbox: "INBOX", UIDValidity: 9001, UID: 101, Subject: "Oversize fixture"},
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
	var reasons struct {
		Truncation map[string]bool `json:"truncation"`
	}
	decodeResult(t, result, &reasons)
	if len(reasons.Truncation) != 1 || !reasons.Truncation["output"] {
		t.Fatalf("truncation = %v, want only the output reason", reasons.Truncation)
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

// encodedThreadNodes returns each source node's own serialized form by key.
func encodedThreadNodes(t *testing.T, nodes []threadNodeResult) map[string]map[string]any {
	t.Helper()

	source := make(map[string]map[string]any, len(nodes))
	for _, node := range nodes {
		encoded, err := json.Marshal(node)
		if err != nil {
			t.Fatalf("marshal node: %v", err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(encoded)))
		decoder.UseNumber()
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			t.Fatalf("decode node: %v", err)
		}
		source[node.Key] = object
	}
	return source
}

// requireThreadOutputControls pins get_thread output shrinking: whole
// unavailable records and whole nodes are removed with truncated set, every
// retained node keeps its complete identity and flags, and the final
// fallback is empty bounded context.
func requireThreadOutputControls(t *testing.T) {
	t.Helper()

	records := func(count int) []threadUnavailableMember {
		reasons := []string{threadUnavailableStaleID, threadUnavailableInvalidIdentity, threadUnavailableMalformedHeader}
		result := make([]threadUnavailableMember, 0, count)
		for index := range count {
			result = append(result, threadUnavailableMember{Reason: reasons[index%len(reasons)]})
		}
		return result
	}
	requireValid := func(t *testing.T, label string, encoded []byte, truncated bool, err error) map[string]any {
		t.Helper()

		if err != nil || !truncated || !utf8.Valid(encoded) {
			t.Fatalf("%s: truncated=%v err=%v validUTF8=%v", label, truncated, err, utf8.Valid(encoded))
		}
		return decodeBounded(t, encoded)
	}
	requireRetainedNodes := func(t *testing.T, label string, object map[string]any, source map[string]map[string]any, total int) {
		t.Helper()

		nodes := rawItems(t, object, "nodes")
		if len(nodes) == 0 || len(nodes) > total {
			t.Fatalf("%s retained %d of %d nodes", label, len(nodes), total)
		}
		for _, node := range nodes {
			key, _ := node["key"].(string)
			if want, ok := source[key]; !ok || !reflect.DeepEqual(node, want) {
				t.Fatalf("%s retained node %.60q altered or unknown", label, key)
			}
		}
	}

	t.Run("nodes and records", func(t *testing.T) {
		thread := &getThreadResult{ID: "opaque-target", Mailbox: "INBOX", UnavailableMembers: records(maxThreadMessages - 1)}
		for index := range maxThreadMessages {
			thread.Nodes = append(thread.Nodes, threadNodeResult{
				Key: fmt.Sprintf("opaque-%02d", index), Mailbox: "INBOX", UIDValidity: 3001, UID: uint32(100 + index),
				Subject:    strings.Repeat("☃", 3000),
				Truncation: threadNodeTruncation{Headers: index%2 == 0, HeaderCount: index%3 == 0, References: index%5 == 0},
			})
		}
		source := encodedThreadNodes(t, thread.Nodes)

		encoded, truncated, err := encodeBounded(thread)
		object := requireValid(t, "nodes and records", encoded, truncated, err)
		requireRetainedNodes(t, "nodes and records", object, source, maxThreadMessages-1)
		if _, present := object["unavailableMembers"]; present {
			t.Fatalf("records outlived node removal: %.200s", encoded)
		}
	})

	t.Run("records only", func(t *testing.T) {
		thread := &getThreadResult{ID: "opaque-target", Mailbox: "INBOX", Nodes: []threadNodeResult{
			{Key: "opaque-only", Mailbox: "INBOX", UIDValidity: 3001, UID: 7, Truncation: threadNodeTruncation{References: true}},
		}}
		base, err := json.Marshal(thread)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		thread.Nodes[0].Subject = strings.Repeat("s", maxToolResultBytes-len(base)-60)
		thread.UnavailableMembers = records(maxThreadMessages - 1)
		source := encodedThreadNodes(t, thread.Nodes)

		encoded, truncated, err := encodeBounded(thread)
		object := requireValid(t, "records only", encoded, truncated, err)
		requireRetainedNodes(t, "records only", object, source, 1)
		if kept := len(thread.UnavailableMembers); kept >= maxThreadMessages-1 {
			t.Fatalf("records were not shrunk: %d", kept)
		}
		if _, present := object["unavailableMembers"]; present {
			for _, record := range rawItems(t, object, "unavailableMembers") {
				if len(record) != 1 || record["reason"] == "" {
					t.Fatalf("shrunk record is not reason-only: %v", record)
				}
			}
		}
	})

	t.Run("fallback", func(t *testing.T) {
		longID := strings.Repeat("é", maxToolResultBytes)
		longMailbox := "Folders/" + strings.Repeat("☃", maxToolResultBytes)
		thread := &getThreadResult{ID: longID, Mailbox: longMailbox, UnavailableMembers: records(5), Nodes: []threadNodeResult{
			{Key: "opaque-only", Mailbox: longMailbox, UIDValidity: 3001, UID: 11, Truncation: threadNodeTruncation{Headers: true}},
		}}

		encoded, truncated, err := encodeBounded(thread)
		object := requireValid(t, "fallback", encoded, truncated, err)
		requireNoIdentityKeys(t, "thread fallback", object)
		if nodes := rawItems(t, object, "nodes"); len(nodes) != 0 {
			t.Fatalf("fallback fabricated nodes: %v", nodes)
		}
		if _, present := object["unavailableMembers"]; present {
			t.Fatalf("fallback fabricated unavailable members: %.200s", encoded)
		}
		id, _ := object["id"].(string)
		mailbox, _ := object["mailbox"].(string)
		if id == "" || len(id) > 1024 || !strings.HasPrefix(longID, id) || mailbox == "" || len(mailbox) > 1024 || !strings.HasPrefix(longMailbox, mailbox) {
			t.Fatalf("fallback context is not bounded aggregate context: id=%d mailbox=%d bytes", len(id), len(mailbox))
		}
	})
}

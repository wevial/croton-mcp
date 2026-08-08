package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func auditLines(t *testing.T, buffer *bytes.Buffer) []map[string]any {
	t.Helper()

	var lines []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buffer.String()), "\n") {
		if line == "" {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			t.Fatalf("audit line is not valid JSON: %v: %q", err, line)
		}
		lines = append(lines, decoded)
	}
	return lines
}

func requireAllowlistedKeys(t *testing.T, event map[string]any) {
	t.Helper()

	allowed := map[string]bool{"event": true, "tool": true, "outcome": true, "code": true, "truncated": true}
	for key := range event {
		if !allowed[key] {
			t.Fatalf("audit event carries non-allowlisted key %q: %v", key, event)
		}
	}
}

func TestAuditRecordsMetadataOnlySuccessEvent(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	mail := &fakeMail{folders: []bridge.Folder{{Name: "Folders/Private-Project", Delimiter: "/"}}}
	session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&buffer)})

	callTool(t, session, "list_folders", map[string]any{})

	events := auditLines(t, &buffer)
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1: %s", len(events), buffer.String())
	}
	requireAllowlistedKeys(t, events[0])
	if events[0]["event"] != "tool_call" || events[0]["tool"] != "list_folders" || events[0]["outcome"] != "ok" {
		t.Fatalf("unexpected audit event: %v", events[0])
	}
	if strings.Contains(buffer.String(), "Private-Project") {
		t.Fatalf("audit leaked folder name: %s", buffer.String())
	}
}

func TestAuditNeverRecordsAdversarialInputsOrWrappedErrors(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	secretErr := errors.New("LOGIN secret-user@mail.test hunter2 at 127.0.0.1:1143")
	mail := &fakeMail{err: secretErr}
	session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&buffer)})

	result := callTool(t, session, "search_mail", map[string]any{
		"mailbox": "Secret-Mailbox-Name",
		"sender":  "target-person@victim.test",
		"subject": "password hunter2 attachment.pdf",
	})
	requireToolError(t, result, errInternal)

	events := auditLines(t, &buffer)
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1: %s", len(events), buffer.String())
	}
	requireAllowlistedKeys(t, events[0])
	if events[0]["outcome"] != "error" || events[0]["code"] != errInternal {
		t.Fatalf("unexpected audit event: %v", events[0])
	}
	raw := buffer.String()
	for _, fragment := range []string{"Secret-Mailbox-Name", "victim.test", "hunter2", "secret-user", "127.0.0.1", "attachment.pdf", "LOGIN"} {
		if strings.Contains(raw, fragment) {
			t.Fatalf("audit leaked %q: %s", fragment, raw)
		}
	}
}

func TestAuditSanitizesUnknownErrorCodes(t *testing.T) {
	t.Parallel()

	// A hypothetical future bridge code outside the audit allowlist must be
	// collapsed rather than copied through verbatim.
	var buffer bytes.Buffer
	mail := &fakeMail{err: &bridge.Error{Code: "sensitive-detail-in-code hunter2"}}
	session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&buffer)})

	result := callTool(t, session, "list_folders", map[string]any{})
	requireToolError(t, result, errInternal)

	events := auditLines(t, &buffer)
	if len(events) != 1 || events[0]["code"] != errInternal {
		t.Fatalf("unexpected audit events: %s", buffer.String())
	}
	if strings.Contains(buffer.String(), "hunter2") {
		t.Fatalf("audit leaked adversarial code: %s", buffer.String())
	}
}

func TestNilAuditorRecordsNothingAndNeverPanics(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{}
	session := connectTestClient(t, Options{Mail: mail})

	callTool(t, session, "list_folders", map[string]any{})
}

func TestAuditRecordsTruncationMetadata(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	folders := make([]bridge.Folder, 900)
	for index := range folders {
		folders[index] = bridge.Folder{Name: "Folders/" + strings.Repeat("x", 200), Delimiter: "/"}
	}
	mail := &fakeMail{folders: folders}
	session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&buffer)})

	callTool(t, session, "list_folders", map[string]any{})

	events := auditLines(t, &buffer)
	if len(events) != 1 {
		t.Fatalf("audit events = %d, want 1", len(events))
	}
	if truncated, ok := events[0]["truncated"].(bool); !ok || !truncated {
		t.Fatalf("truncated metadata missing: %v", events[0])
	}
}

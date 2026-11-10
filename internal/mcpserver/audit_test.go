package mcpserver

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
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

func TestAuditNeverRecordsInputsOrWrappedErrors(t *testing.T) {
	t.Parallel()

	var buffer bytes.Buffer
	mail := &fakeMail{err: errors.New("LOGIN secret-user@mail.test hunter2 at 127.0.0.1:1143")}
	session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&buffer)})

	requireToolError(t, callTool(t, session, "search_mail", map[string]any{
		"mailbox": "Secret-Mailbox-Name",
		"sender":  "target-person@victim.test",
		"subject": "password hunter2 attachment.pdf",
	}), errInternal)

	mail.err = nil

	callTool(t, session, "list_folders", map[string]any{})

	events := auditLines(t, &buffer)
	if len(events) != 2 {
		t.Fatalf("audit events = %d, want 2: %s", len(events), buffer.String())
	}
	for _, event := range events {
		requireAllowlistedKeys(t, event)
	}
	if events[0]["tool"] != "search_mail" || events[0]["outcome"] != "error" || events[0]["code"] != errInternal {
		t.Fatalf("unexpected error event: %v", events[0])
	}
	if events[1]["tool"] != "list_folders" || events[1]["outcome"] != "ok" || events[1]["code"] != nil {
		t.Fatalf("unexpected success event: %v", events[1])
	}
	for _, fragment := range []string{"Secret-Mailbox-Name", "victim.test", "hunter2", "secret-user", "127.0.0.1", "attachment.pdf", "LOGIN"} {
		if strings.Contains(buffer.String(), fragment) {
			t.Fatalf("audit leaked %q: %s", fragment, buffer.String())
		}
	}

	NewAuditor(nil).ToolCall("list_folders", "ok", "", false)
}

package mcpserver

import (
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
)

type searchMailDecoded struct {
	Results []struct {
		ID      string `json:"id"`
		Mailbox string `json:"mailbox"`
		Subject string `json:"subject"`
		Size    int64  `json:"size"`
	} `json:"results"`
	Truncated bool `json:"truncated"`
}

func TestSearchMailPassesBoundedStructuredQuery(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{searched: []bridge.MessageMetadata{
		{ID: "opaque-1", Mailbox: "INBOX", Subject: "Croton status", Size: 512},
		{ID: "opaque-2", Mailbox: "INBOX", Subject: "Welcome", Size: 1024},
	}}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "search_mail", map[string]any{
		"mailbox":    "INBOX",
		"since":      "2026-08-01T00:00:00Z",
		"before":     "2026-08-08T00:00:00Z",
		"sender":     "fixture@croton.test",
		"subject":    "status",
		"unreadOnly": true,
	})

	var decoded searchMailDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(decoded.Results))
	}
	if decoded.Results[0].ID != "opaque-1" || decoded.Results[0].Subject != "Croton status" {
		t.Fatalf("unexpected first result: %+v", decoded.Results[0])
	}

	if len(mail.searchCalls) != 1 {
		t.Fatalf("adapter search calls = %d, want 1", len(mail.searchCalls))
	}
	query := mail.searchCalls[0]
	if query.Mailbox != "INBOX" || query.Sender != "fixture@croton.test" || query.Subject != "status" || !query.Unread {
		t.Fatalf("unexpected adapter query: %+v", query)
	}
	wantSince := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if !query.Since.Equal(wantSince) {
		t.Fatalf("query since = %v, want %v", query.Since, wantSince)
	}
	if !query.Before.Equal(time.Date(2026, 8, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("query before = %v", query.Before)
	}
}

func TestSearchMailLimitSlicesResults(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{searched: []bridge.MessageMetadata{
		{ID: "a", Mailbox: "INBOX"}, {ID: "b", Mailbox: "INBOX"}, {ID: "c", Mailbox: "INBOX"},
	}}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 2})

	var decoded searchMailDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Results) != 2 || !decoded.Truncated {
		t.Fatalf("results = %d truncated = %v, want 2 true", len(decoded.Results), decoded.Truncated)
	}
}

func TestSearchMailPropagatesAdapterTruncation(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		searched:        []bridge.MessageMetadata{{ID: "a", Mailbox: "INBOX"}},
		searchTruncated: true,
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 10})

	var decoded searchMailDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Results) != 1 || !decoded.Truncated {
		t.Fatalf("results = %d truncated = %v, want 1 true", len(decoded.Results), decoded.Truncated)
	}
}

func TestSearchMailRejectsAndClampsMaliciousArguments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments map[string]any
		wantCode  string
	}{
		{"missing mailbox", map[string]any{}, errInvalidArgument},
		{"empty mailbox", map[string]any{"mailbox": ""}, errInvalidArgument},
		{"oversize mailbox", map[string]any{"mailbox": strings.Repeat("A", maxMailboxArgumentBytes+1)}, errInvalidArgument},
		{"oversize sender", map[string]any{"mailbox": "INBOX", "sender": strings.Repeat("s", maxSearchTermBytes+1)}, errInvalidArgument},
		{"oversize subject", map[string]any{"mailbox": "INBOX", "subject": strings.Repeat("s", maxSearchTermBytes+1)}, errInvalidArgument},
		{"malformed since", map[string]any{"mailbox": "INBOX", "since": "yesterday"}, errInvalidArgument},
		{"malformed before", map[string]any{"mailbox": "INBOX", "before": "08/08/2026"}, errInvalidArgument},
		{"limit wrong type", map[string]any{"mailbox": "INBOX", "limit": "10"}, errInvalidArgument},
		{"limit zero", map[string]any{"mailbox": "INBOX", "limit": 0}, errInvalidArgument},
		{"limit negative", map[string]any{"mailbox": "INBOX", "limit": -5}, errInvalidArgument},
		{"unknown argument", map[string]any{"mailbox": "INBOX", "rawQuery": "DELETED"}, errInvalidArgument},
		{"mailbox wrong type", map[string]any{"mailbox": 7}, errInvalidArgument},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mail := &fakeMail{}
			session := connectTestClient(t, Options{Mail: mail})

			result := callTool(t, session, "search_mail", testCase.arguments)

			requireToolError(t, result, testCase.wantCode)
			if len(mail.searchCalls) != 0 {
				t.Fatalf("adapter reached despite invalid input: %+v", mail.searchCalls)
			}
		})
	}
}

func TestSearchMailClampsOversizedLimitInsteadOfTrusting(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 1000000})

	var decoded searchMailDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Results) != 0 {
		t.Fatalf("results = %d, want 0", len(decoded.Results))
	}
	if len(mail.searchCalls) != 1 {
		t.Fatalf("adapter search calls = %d, want 1", len(mail.searchCalls))
	}
}

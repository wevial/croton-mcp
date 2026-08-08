package mcpserver

import (
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
)

type digestDecoded struct {
	Mailbox    string `json:"mailbox"`
	Candidates []struct {
		ID      string `json:"id"`
		Mailbox string `json:"mailbox"`
		Subject string `json:"subject"`
		Size    int64  `json:"size"`
	} `json:"candidates"`
	TotalMessages int  `json:"totalMessages"`
	UnseenCount   int  `json:"unseenCount"`
	Truncated     bool `json:"truncated"`
}

func TestSelectDigestCandidatesComposesStatusAndBoundedSearch(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		status: bridge.MailboxStatus{Messages: 40, Unseen: 5},
		searched: []bridge.MessageMetadata{
			{ID: "id-1", Mailbox: "INBOX", Subject: "Croton status", Size: 512},
			{ID: "id-2", Mailbox: "INBOX", Subject: "Welcome", Size: 1024},
		},
	}
	session := connectTestClient(t, Options{Mail: mail})

	before := time.Now()
	result := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"})
	after := time.Now()

	var decoded digestDecoded
	decodeResult(t, result, &decoded)
	if decoded.Mailbox != "INBOX" || decoded.TotalMessages != 40 || decoded.UnseenCount != 5 {
		t.Fatalf("unexpected mailbox summary: %+v", decoded)
	}
	if len(decoded.Candidates) != 2 || decoded.Candidates[0].ID != "id-1" {
		t.Fatalf("unexpected candidates: %+v", decoded.Candidates)
	}

	if len(mail.searchCalls) != 1 {
		t.Fatalf("search calls = %d, want 1", len(mail.searchCalls))
	}
	query := mail.searchCalls[0]
	if query.Mailbox != "INBOX" || !query.Unread {
		t.Fatalf("unexpected digest query: %+v", query)
	}
	wantEarliest := before.Add(-time.Duration(defaultDigestHours)*time.Hour - time.Minute)
	wantLatest := after.Add(-time.Duration(defaultDigestHours)*time.Hour + time.Minute)
	if query.Since.Before(wantEarliest) || query.Since.After(wantLatest) {
		t.Fatalf("digest since = %v, want about %d hours before now", query.Since, defaultDigestHours)
	}
	if query.Before.IsZero() || query.Before.Before(before) {
		t.Fatalf("digest before = %v, want a closed upper date bound", query.Before)
	}
	if query.Sender != "" || query.Subject != "" {
		t.Fatalf("digest query carries unexpected content terms: %+v", query)
	}
}

func TestSelectDigestCandidatesHonorsExplicitWindowAndLimit(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{searched: []bridge.MessageMetadata{
		{ID: "id-1", Mailbox: "INBOX"}, {ID: "id-2", Mailbox: "INBOX"}, {ID: "id-3", Mailbox: "INBOX"},
	}}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "select_digest_candidates", map[string]any{
		"mailbox":    "INBOX",
		"sinceHours": 48,
		"limit":      2,
		"unreadOnly": false,
	})

	var decoded digestDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Candidates) != 2 || !decoded.Truncated {
		t.Fatalf("candidates = %d truncated = %v, want 2 true", len(decoded.Candidates), decoded.Truncated)
	}
	if mail.searchCalls[0].Unread {
		t.Fatalf("unreadOnly=false ignored: %+v", mail.searchCalls[0])
	}
}

func TestSelectDigestCandidatesPropagatesAdapterTruncation(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		searched:        []bridge.MessageMetadata{{ID: "id-1", Mailbox: "INBOX"}},
		searchTruncated: true,
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "select_digest_candidates", map[string]any{
		"mailbox": "INBOX",
		"limit":   10,
	})

	var decoded digestDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Candidates) != 1 || !decoded.Truncated {
		t.Fatalf("candidates = %d truncated = %v, want 1 true", len(decoded.Candidates), decoded.Truncated)
	}
}

func TestSelectDigestCandidatesValidatesArguments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments map[string]any
	}{
		{"missing mailbox", map[string]any{}},
		{"sinceHours zero", map[string]any{"mailbox": "INBOX", "sinceHours": 0}},
		{"sinceHours wrong type", map[string]any{"mailbox": "INBOX", "sinceHours": "24"}},
		{"limit negative", map[string]any{"mailbox": "INBOX", "limit": -1}},
		{"unknown field", map[string]any{"mailbox": "INBOX", "delete": true}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mail := &fakeMail{}
			session := connectTestClient(t, Options{Mail: mail})

			result := callTool(t, session, "select_digest_candidates", testCase.arguments)

			requireToolError(t, result, errInvalidArgument)
			if len(mail.searchCalls) != 0 {
				t.Fatal("adapter reached despite invalid arguments")
			}
		})
	}
}

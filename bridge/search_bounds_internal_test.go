package bridge

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// boundedSearchSession implements only what the adapter calls; the embedded
// nil interface fills the rest of bridgeSession.
type boundedSearchSession struct {
	bridgeSession
	examineCalls int
	snapshot     mailboxSnapshot
	uids         []uint32
	metadata     []MessageMetadata
}

func (session *boundedSearchSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	session.examineCalls++
	return session.snapshot, nil
}
func (session *boundedSearchSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	return session.uids, nil
}
func (session *boundedSearchSession) UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error) {
	return session.metadata, nil
}
func (session *boundedSearchSession) Logout(context.Context) error { return nil }
func (session *boundedSearchSession) Abort() error                 { return nil }

func newBoundedSearchAdapter(session bridgeSession, maxOutputBytes int) *Adapter {
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP: IMAPConfig{CommandTimeoutMs: 1000},
			Bounds: Bounds{
				MaxSearchResults: 1,
				MaxHeaderBytes:   4096,
				MaxOutputBytes:   maxOutputBytes,
			},
		},
		factory: func(context.Context) (bridgeSession, error) { return session, nil },
		gate:    make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}
	return adapter
}

func TestSearchMailRejectsOversizedInputsBeforeOpeningSession(t *testing.T) {
	tests := []struct {
		name  string
		query SearchQuery
	}{
		{name: "mailbox", query: SearchQuery{Mailbox: strings.Repeat("m", maxMailboxNameBytes+1)}},
		{name: "sender", query: SearchQuery{Mailbox: "INBOX", Sender: strings.Repeat("s", maxSearchTermBytes+1)}},
		{name: "subject", query: SearchQuery{Mailbox: "INBOX", Subject: strings.Repeat("s", maxSearchTermBytes+1)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := &boundedSearchSession{}
			adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)
			page, err := adapter.SearchMailPage(context.Background(), test.query)
			results := page.Messages
			if CodeOf(err) != CodeBoundsExceeded {
				t.Fatalf("SearchMailPage error = %v, want %q", err, CodeBoundsExceeded)
			}
			if results != nil {
				t.Fatalf("SearchMailPage results = %#v, want nil", results)
			}
			if session.examineCalls != 0 {
				t.Fatalf("EXAMINE calls = %d, want 0", session.examineCalls)
			}
		})
	}
}

func TestEncodeMessageIDRejectsPayloadBeyondDecoderBudget(t *testing.T) {
	adapter := &Adapter{}
	_, err := adapter.encodeMessageID(messageIDPayload{
		Mailbox:     strings.Repeat("\x00", maxMailboxNameBytes),
		UIDValidity: 9001,
		UID:         1,
	})
	if CodeOf(err) != CodeBoundsExceeded {
		t.Fatalf("encodeMessageID error = %v, want %q", err, CodeBoundsExceeded)
	}
}

func TestEncodeMessageIDRejectsSemanticallyInvalidPayload(t *testing.T) {
	tests := []messageIDPayload{
		{UIDValidity: 9001, UID: 1},
		{Mailbox: "INBOX", UID: 1},
		{Mailbox: "INBOX", UIDValidity: 9001},
	}
	for _, payload := range tests {
		if _, err := (&Adapter{}).encodeMessageID(payload); CodeOf(err) != CodeIMAPProtocol {
			t.Fatalf("encodeMessageID(%#v) error = %v, want %q", payload, err, CodeIMAPProtocol)
		}
	}
}

func TestSearchMailEmptyResultsRespectExactOutputBudget(t *testing.T) {
	for _, maxOutputBytes := range []int{2, 3} {
		t.Run(string(rune('0'+maxOutputBytes)), func(t *testing.T) {
			adapter := newBoundedSearchAdapter(&boundedSearchSession{snapshot: mailboxSnapshot{UIDNext: 2, UIDValidity: 9001}}, maxOutputBytes)
			page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
			if err != nil {
				t.Fatalf("SearchMailPage: %v", err)
			}

			encoded, err := json.Marshal(page.Messages)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			if string(encoded) != "[]" {
				t.Fatalf("encoded results = %s, want [] for budget %d", encoded, maxOutputBytes)
			}
		})
	}
}

func TestSearchMailRejectsAggregateOutputBeyondConfiguredBudget(t *testing.T) {
	session := &boundedSearchSession{
		snapshot: mailboxSnapshot{UIDNext: 2, UIDValidity: 9001},
		uids:     []uint32{1},
		metadata: []MessageMetadata{{Subject: strings.Repeat("s", 64), uid: 1}},
	}
	adapter := newBoundedSearchAdapter(session, 32)
	page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
	results := page.Messages
	if CodeOf(err) != CodeBoundsExceeded {
		t.Fatalf("SearchMailPage error = %v, want %q", err, CodeBoundsExceeded)
	}
	if results != nil {
		t.Fatalf("SearchMailPage results = %#v, want nil", results)
	}
}

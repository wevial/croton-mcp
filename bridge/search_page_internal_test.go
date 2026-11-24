package bridge

import (
	"context"
	"testing"
)

type searchPageSession struct {
	boundedSearchSession
	snapshot mailboxSnapshot
	uids     []uint32
}

func (session *searchPageSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return session.snapshot, nil
}

func (session *searchPageSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	return session.uids, nil
}

func TestSearchMailPageReportsUnsearchedMailboxRange(t *testing.T) {
	session := &searchPageSession{
		snapshot: mailboxSnapshot{UIDNext: searchWindowWidth + 2, UIDValidity: 9001},
		uids:     []uint32{2},
	}
	session.metadata = []MessageMetadata{{Subject: "match", uid: 2}}
	adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)

	page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
	if err != nil {
		t.Fatalf("SearchMailPage: %v", err)
	}
	if len(page.Messages) != 1 || !page.Truncated {
		t.Fatalf("page = %#v, want one message and truncated=true", page)
	}
}

func TestSearchMailPageReportsCompleteMailboxScan(t *testing.T) {
	session := &searchPageSession{
		snapshot: mailboxSnapshot{UIDNext: 2, UIDValidity: 9001},
		uids:     []uint32{1},
	}
	session.metadata = []MessageMetadata{{Subject: "match", uid: 1}}
	adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)

	page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
	if err != nil {
		t.Fatalf("SearchMailPage: %v", err)
	}
	if len(page.Messages) != 1 || page.Truncated {
		t.Fatalf("page = %#v, want one message and truncated=false", page)
	}
}

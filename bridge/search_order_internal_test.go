package bridge

import (
	"context"
	"fmt"
	"math/rand"
	"slices"
	"testing"
)

// orderedSearchSession answers each UID window from a fixed membership in a
// shuffled wire order and replies to metadata FETCH in reverse request order,
// recording every searched window.
type orderedSearchSession struct {
	boundedSearchSession
	snapshot mailboxSnapshot
	members  []uint32

	windows []uidWindow
}

func (session *orderedSearchSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return session.snapshot, nil
}

func (session *orderedSearchSession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, _ int) ([]uint32, error) {
	session.windows = append(session.windows, window)

	uids := make([]uint32, 0)
	for _, uid := range session.members {
		if uid >= window.Start && uid <= window.End {
			uids = append(uids, uid)
		}
	}
	random := rand.New(rand.NewSource(int64(window.End)))
	random.Shuffle(len(uids), func(i, j int) { uids[i], uids[j] = uids[j], uids[i] })
	return uids, nil
}

func (session *orderedSearchSession) UIDFetchMetadata(_ context.Context, _ string, uids []uint32, _ int) ([]MessageMetadata, error) {
	metadata := make([]MessageMetadata, 0, len(uids))
	for index := len(uids) - 1; index >= 0; index-- {
		metadata = append(metadata, MessageMetadata{Subject: syntheticOrderSubject(uids[index]), Size: int64(uids[index]), uid: uids[index]})
	}
	return metadata, nil
}

func syntheticOrderSubject(uid uint32) string {
	return fmt.Sprintf("Synthetic order fixture %d", uid)
}

func uidRange(start, end uint32) []uint32 {
	uids := make([]uint32, 0, end-start+1)
	for uid := start; uid <= end; uid++ {
		uids = append(uids, uid)
	}
	return uids
}

// requireDescendingUIDs checks that the page lists want in order and that
// every item's fetched fields belong to its own UID rather than its position.
func requireDescendingUIDs(t *testing.T, page SearchPage, want []uint32) {
	t.Helper()

	if len(page.Messages) != len(want) {
		t.Fatalf("page has %d messages, want %d", len(page.Messages), len(want))
	}
	for index, message := range page.Messages {
		if message.UID != want[index] {
			t.Fatalf("message %d UID = %d, want %d", index, message.UID, want[index])
		}
		if message.Subject != syntheticOrderSubject(message.UID) || message.Size != int64(message.UID) {
			t.Fatalf("message %d fields %q/%d belong to another UID than %d", index, message.Subject, message.Size, message.UID)
		}
	}
}

func TestSearchOrderAdapter(t *testing.T) {
	t.Run("dense_window", func(t *testing.T) {
		session := &orderedSearchSession{snapshot: mailboxSnapshot{UIDNext: 101, UIDValidity: 9001}, members: uidRange(1, 100)}
		adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)
		adapter.config.Bounds.MaxSearchResults = 50

		page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
		if err != nil {
			t.Fatalf("SearchMailPage: %v", err)
		}

		want := uidRange(51, 100)
		slices.Reverse(want)
		requireDescendingUIDs(t, page, want)
		if !page.Truncated {
			t.Fatal("dense bottom window with omitted matches reported truncated=false")
		}
	})

	t.Run("search_fetch_order", func(t *testing.T) {
		session := &orderedSearchSession{
			snapshot: mailboxSnapshot{UIDNext: 201, UIDValidity: 7007},
			members:  []uint32{3, 7, 40, 93, 101, 110, 150, 199, 200},
		}
		adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)
		adapter.config.Bounds.MaxSearchResults = 20

		page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
		if err != nil {
			t.Fatalf("SearchMailPage: %v", err)
		}

		requireDescendingUIDs(t, page, []uint32{200, 199, 150, 110, 101, 93, 40, 7, 3})
		if page.Truncated {
			t.Fatal("complete two-window traversal reported truncated=true")
		}
	})

	t.Run("bounds_and_completion", func(t *testing.T) {
		sparse := make([]uint32, 0, 10)
		for uid := uint32(1000); uid > 0; uid -= searchWindowWidth {
			sparse = append([]uint32{uid}, sparse...)
		}

		for _, test := range []struct {
			name          string
			uidNext       uint32
			members       []uint32
			budget        int
			wantCount     int
			wantTruncated bool
			wantSearches  int
		}{
			{name: "exactly_complete_bottom_window", uidNext: 101, members: uidRange(51, 100), budget: 50, wantCount: 50, wantTruncated: false, wantSearches: 1},
			{name: "unsearched_older_range", uidNext: 1001, members: sparse, budget: 250, wantCount: 8, wantTruncated: true, wantSearches: maxSearchWindows},
			{name: "dense_hard_budget", uidNext: 1001, members: uidRange(1, 1000), budget: hardMaxSearchResults, wantCount: hardMaxSearchResults, wantTruncated: true, wantSearches: 3},
		} {
			t.Run(test.name, func(t *testing.T) {
				session := &orderedSearchSession{snapshot: mailboxSnapshot{UIDNext: test.uidNext, UIDValidity: 9001}, members: test.members}
				adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)
				adapter.config.Bounds.MaxSearchResults = test.budget

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if err != nil {
					t.Fatalf("SearchMailPage: %v", err)
				}

				want := make([]uint32, 0, test.wantCount)
				for index := len(test.members) - 1; index >= 0 && len(want) < test.wantCount; index-- {
					want = append(want, test.members[index])
				}
				requireDescendingUIDs(t, page, want)
				if page.Truncated != test.wantTruncated {
					t.Fatalf("truncated = %v, want %v", page.Truncated, test.wantTruncated)
				}
				if len(session.windows) != test.wantSearches {
					t.Fatalf("SEARCH windows = %d, want %d", len(session.windows), test.wantSearches)
				}
			})
		}

		for _, test := range []struct {
			name    string
			members []uint32
			want    string
		}{
			{name: "out_of_window_uid", members: []uint32{100, 101}, want: CodeBoundsExceeded},
			{name: "duplicate_uid", members: []uint32{100, 100}, want: CodeIMAPProtocol},
			{name: "zero_uid", members: []uint32{0, 100}, want: CodeIMAPProtocol},
		} {
			t.Run(test.name, func(t *testing.T) {
				session := &searchPageSession{snapshot: mailboxSnapshot{UIDNext: 101, UIDValidity: 9001}, uids: test.members}
				adapter := newBoundedSearchAdapter(session, defaultMaxOutputBytes)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if CodeOf(err) != test.want {
					t.Fatalf("SearchMailPage error = %v, want %q", err, test.want)
				}
				if page.Messages != nil || page.Truncated {
					t.Fatalf("refusal returned partial page %#v", page)
				}
			})
		}
	})
}

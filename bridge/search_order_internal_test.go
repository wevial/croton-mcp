package bridge

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
)

// orderedSearchSession answers each UID window from a fixed membership in a
// caller-chosen wire order and replies to metadata FETCH in reverse request
// order, recording every window, decoder budget and fetched UID.
type orderedSearchSession struct {
	boundedSearchSession
	snapshot mailboxSnapshot
	members  []uint32
	shuffle  bool

	windows []uidWindow
	limits  []int
	fetched [][]uint32
}

func (session *orderedSearchSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	session.examineCalls++
	return session.snapshot, nil
}

func (session *orderedSearchSession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, limit int) ([]uint32, error) {
	session.windows = append(session.windows, window)
	session.limits = append(session.limits, limit)

	uids := make([]uint32, 0)
	for _, uid := range session.members {
		if uid >= window.Start && uid <= window.End {
			uids = append(uids, uid)
		}
	}
	if session.shuffle {
		random := rand.New(rand.NewSource(int64(window.End)))
		random.Shuffle(len(uids), func(i, j int) { uids[i], uids[j] = uids[j], uids[i] })
	}
	return uids, nil
}

func (session *orderedSearchSession) UIDFetchMetadata(_ context.Context, _ string, uids []uint32, _ int) ([]MessageMetadata, error) {
	session.fetched = append(session.fetched, append([]uint32(nil), uids...))

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

func newOrderedSearchAdapter(session readSession, maxSearchResults int) *Adapter {
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 1000},
			Bounds: Bounds{MaxSearchResults: maxSearchResults, MaxHeaderBytes: 4096, MaxOutputBytes: defaultMaxOutputBytes},
		},
		factory: func(context.Context) (readSession, error) { return session, nil },
		gate:    make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}
	return adapter
}

// requireDescendingIdentities checks that the page is strictly UID-descending,
// equals want, and that every opaque ID, public tuple and fetched field names
// the item's own UID rather than its position.
func requireDescendingIdentities(t *testing.T, adapter *Adapter, page SearchPage, uidValidity uint32, want []uint32) {
	t.Helper()

	if len(page.Messages) != len(want) {
		t.Fatalf("page has %d messages, want %d", len(page.Messages), len(want))
	}
	for index, message := range page.Messages {
		if message.UID != want[index] {
			t.Fatalf("message %d UID = %d, want %d", index, message.UID, want[index])
		}
		if index > 0 && message.UID >= page.Messages[index-1].UID {
			t.Fatalf("message %d UID %d does not descend from %d", index, message.UID, page.Messages[index-1].UID)
		}
		payload, err := adapter.decodeMessageID(message.ID)
		if err != nil {
			t.Fatalf("decode message %d ID: %v", index, err)
		}
		if payload != (messageIDPayload{Mailbox: "INBOX", UIDValidity: uidValidity, UID: message.UID}) {
			t.Fatalf("message %d ID binds %#v, want UID %d", index, payload, message.UID)
		}
		if message.uid != message.UID || message.Mailbox != "INBOX" || message.UIDValidity != uidValidity {
			t.Fatalf("message %d tuple = (%q, %d, %d/%d)", index, message.Mailbox, message.UIDValidity, message.UID, message.uid)
		}
		if message.Subject != syntheticOrderSubject(message.UID) || message.Size != int64(message.UID) {
			t.Fatalf("message %d fields %q/%d belong to another UID than %d", index, message.Subject, message.Size, message.UID)
		}
	}
}

func fetchedCount(session *orderedSearchSession) int {
	count := 0
	for _, uids := range session.fetched {
		count += len(uids)
	}
	return count
}

func TestSearchOrderAdapter(t *testing.T) {
	t.Run("dense_window", func(t *testing.T) {
		for _, budget := range []int{1, 50} {
			t.Run(fmt.Sprintf("budget_%d", budget), func(t *testing.T) {
				session := &orderedSearchSession{snapshot: mailboxSnapshot{UIDNext: 101, UIDValidity: 9001}, members: uidRange(1, 100)}
				adapter := newOrderedSearchAdapter(session, budget)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if err != nil {
					t.Fatalf("SearchMailPage: %v", err)
				}

				want := uidRange(uint32(101-budget), 100)
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
				requireDescendingIdentities(t, adapter, page, 9001, want)
				if !page.Truncated {
					t.Fatal("dense bottom window with omitted matches reported truncated=false")
				}
				if len(session.windows) != 1 || session.windows[0] != (uidWindow{Start: 1, End: 100}) || session.limits[0] != 100 {
					t.Fatalf("windows = %#v limits = %v, want one width-100 window decoded at width", session.windows, session.limits)
				}
				if len(session.fetched) != 1 || fetchedCount(session) != budget {
					t.Fatalf("fetched = %v, want exactly %d highest UIDs in one FETCH", session.fetched, budget)
				}
			})
		}
	})

	t.Run("search_fetch_order", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			shuffle bool
		}{
			{name: "ascending", shuffle: false},
			{name: "shuffled", shuffle: true},
		} {
			t.Run(test.name, func(t *testing.T) {
				session := &orderedSearchSession{
					snapshot: mailboxSnapshot{UIDNext: 201, UIDValidity: 7007},
					members:  []uint32{3, 7, 40, 93, 101, 110, 150, 199, 200},
					shuffle:  test.shuffle,
				}
				adapter := newOrderedSearchAdapter(session, 20)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if err != nil {
					t.Fatalf("SearchMailPage: %v", err)
				}

				requireDescendingIdentities(t, adapter, page, 7007, []uint32{200, 199, 150, 110, 101, 93, 40, 7, 3})
				if page.Truncated {
					t.Fatal("complete two-window traversal reported truncated=true")
				}
				if len(session.windows) != 2 || len(session.fetched) != 2 {
					t.Fatalf("windows = %#v fetched = %v, want two windows and two FETCHes", session.windows, session.fetched)
				}
			})
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
			{name: "empty_membership", uidNext: 251, budget: 50, wantCount: 0, wantTruncated: false, wantSearches: 3},
			{name: "exactly_complete_bottom_window", uidNext: 101, members: uidRange(51, 100), budget: 50, wantCount: 50, wantTruncated: false, wantSearches: 1},
			{name: "omitted_within_bottom_window", uidNext: 101, members: uidRange(50, 100), budget: 50, wantCount: 50, wantTruncated: true, wantSearches: 1},
			{name: "unsearched_older_range", uidNext: 1001, members: sparse, budget: 250, wantCount: 8, wantTruncated: true, wantSearches: maxSearchWindows},
			{name: "dense_hard_budget", uidNext: 1001, members: uidRange(1, 1000), budget: hardMaxSearchResults, wantCount: hardMaxSearchResults, wantTruncated: true, wantSearches: 3},
		} {
			t.Run(test.name, func(t *testing.T) {
				session := &orderedSearchSession{snapshot: mailboxSnapshot{UIDNext: test.uidNext, UIDValidity: 9001}, members: test.members, shuffle: true}
				adapter := newOrderedSearchAdapter(session, test.budget)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if err != nil {
					t.Fatalf("SearchMailPage: %v", err)
				}

				want := make([]uint32, 0, test.wantCount)
				for index := len(test.members) - 1; index >= 0 && len(want) < test.wantCount; index-- {
					want = append(want, test.members[index])
				}
				requireDescendingIdentities(t, adapter, page, 9001, want)
				if page.Truncated != test.wantTruncated {
					t.Fatalf("truncated = %v, want %v", page.Truncated, test.wantTruncated)
				}
				if len(session.windows) != test.wantSearches || len(session.windows) > maxSearchWindows {
					t.Fatalf("SEARCH windows = %d, want %d (at most %d)", len(session.windows), test.wantSearches, maxSearchWindows)
				}
				for index, window := range session.windows {
					width := int(window.End - window.Start + 1)
					if width > int(searchWindowWidth) || session.limits[index] != width {
						t.Fatalf("window %#v decoded with limit %d, want width at most %d", window, session.limits[index], searchWindowWidth)
					}
				}
				if len(session.fetched) > maxSearchWindows || fetchedCount(session) > test.budget || fetchedCount(session) != test.wantCount {
					t.Fatalf("fetched %d entries in %d FETCHes, want %d within budget %d", fetchedCount(session), len(session.fetched), test.wantCount, test.budget)
				}
				if session.examineCalls != 1 {
					t.Fatalf("EXAMINE calls = %d, want 1", session.examineCalls)
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
				session := &rawWindowSession{orderedSearchSession: orderedSearchSession{snapshot: mailboxSnapshot{UIDNext: 101, UIDValidity: 9001}}, uids: test.members}
				adapter := newOrderedSearchAdapter(session, 1)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if CodeOf(err) != test.want {
					t.Fatalf("SearchMailPage error = %v, want %q", err, test.want)
				}
				if page.Messages != nil || page.Truncated {
					t.Fatalf("refusal returned partial page %#v", page)
				}
				if len(session.fetched) != 0 {
					t.Fatalf("refused window fetched %v", session.fetched)
				}
			})
		}
	})
}

// rawWindowSession returns its UIDs verbatim, regardless of the window.
type rawWindowSession struct {
	orderedSearchSession
	uids []uint32
}

func (session *rawWindowSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	return append([]uint32(nil), session.uids...), nil
}

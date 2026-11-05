package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math"
	"testing"
)

// publicIdentitySession is a synthetic read session whose mailbox generation,
// UID membership and FETCH answers are fixed by the test.
type publicIdentitySession struct {
	boundedSearchSession
	snapshot mailboxSnapshot
	members  []uint32
	fetched  []MessageMetadata
}

func (session *publicIdentitySession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return session.snapshot, nil
}

func (session *publicIdentitySession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, _ int) ([]uint32, error) {
	uids := make([]uint32, 0, len(session.members))
	for _, uid := range session.members {
		if uid >= window.Start && uid <= window.End {
			uids = append(uids, uid)
		}
	}
	return uids, nil
}

func (session *publicIdentitySession) UIDFetchMetadata(_ context.Context, _ string, uids []uint32, _ int) ([]MessageMetadata, error) {
	if session.fetched != nil {
		return session.fetched, nil
	}

	metadata := make([]MessageMetadata, 0, len(uids))
	for _, uid := range uids {
		metadata = append(metadata, MessageMetadata{Subject: "Synthetic identity fixture", Size: 64, uid: uid})
	}
	return metadata, nil
}

func newPublicIdentityAdapter(sessions ...readSession) *Adapter {
	factoryCalls := 0
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 1000},
			Bounds: Bounds{MaxSearchResults: 10, MaxHeaderBytes: 4096, MaxOutputBytes: defaultMaxOutputBytes},
		},
		factory: func(context.Context) (readSession, error) {
			session := sessions[factoryCalls]
			factoryCalls++
			return session, nil
		},
		gate: make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}
	return adapter
}

// requireBoundIdentity proves the exported tuple is exactly the one the
// adapter bound into the opaque ID and the fixture's independent tuple.
func requireBoundIdentity(t *testing.T, adapter *Adapter, metadata MessageMetadata, want messageIDPayload) {
	t.Helper()

	payload, err := adapter.decodeMessageID(metadata.ID)
	if err != nil {
		t.Fatalf("decode adapter ID: %v", err)
	}
	if payload != want || metadata.Mailbox != want.Mailbox || metadata.UIDValidity != want.UIDValidity || metadata.UID != want.UID {
		t.Fatalf("identity = (%q, %d, %d) bound %#v, want %#v", metadata.Mailbox, metadata.UIDValidity, metadata.UID, payload, want)
	}

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}
	var serialized struct {
		UIDValidity json.Number
		UID         json.Number
	}
	if err := json.Unmarshal(encoded, &serialized); err != nil {
		t.Fatalf("decode serialized metadata: %v", err)
	}
	if serialized.UIDValidity.String() != jsonUint(want.UIDValidity) || serialized.UID.String() != jsonUint(want.UID) {
		t.Fatalf("serialized identity = %s, want (%d, %d)", encoded, want.UIDValidity, want.UID)
	}
}

func jsonUint(value uint32) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

// forgedMessageID signs a payload the encoder would refuse, so the decoder's
// own zero-identity refusal is exercised.
func forgedMessageID(adapter *Adapter, payload messageIDPayload) string {
	encoded, _ := json.Marshal(payload)
	mac := hmac.New(sha256.New, adapter.idKey[:])
	_, _ = mac.Write(encoded)
	return base64.RawURLEncoding.EncodeToString(append(encoded, mac.Sum(nil)...))
}

func TestPublicReadIdentityAdapter(t *testing.T) {
	t.Run("valid_tuple", func(t *testing.T) {
		for _, fixture := range []struct {
			name        string
			uidValidity uint32
			members     []uint32
		}{
			{name: "minimum", uidValidity: 1, members: []uint32{1, 2}},
			{name: "maximum generation", uidValidity: math.MaxUint32, members: []uint32{7, 42}},
		} {
			t.Run(fixture.name, func(t *testing.T) {
				// UIDNEXT is one past the largest fixture UID; it never wraps.
				session := &publicIdentitySession{
					snapshot: mailboxSnapshot{UIDValidity: fixture.uidValidity, UIDNext: fixture.members[len(fixture.members)-1] + 1},
					members:  fixture.members,
				}
				adapter := newPublicIdentityAdapter(session, session)

				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if err != nil {
					t.Fatalf("SearchMailPage: %v", err)
				}
				if len(page.Messages) != len(fixture.members) || page.Truncated {
					t.Fatalf("page = %#v, want %d complete results", page, len(fixture.members))
				}
				for index, message := range page.Messages {
					requireBoundIdentity(t, adapter, message, messageIDPayload{Mailbox: "INBOX", UIDValidity: fixture.uidValidity, UID: fixture.members[index]})
				}

				selected, err := adapter.GetMessageMetadata(context.Background(), page.Messages[0].ID)
				if err != nil {
					t.Fatalf("GetMessageMetadata: %v", err)
				}
				requireBoundIdentity(t, adapter, selected, messageIDPayload{Mailbox: "INBOX", UIDValidity: fixture.uidValidity, UID: fixture.members[0]})
			})
		}

		t.Run("selected boundaries", func(t *testing.T) {
			for _, want := range []messageIDPayload{
				{Mailbox: "INBOX", UIDValidity: 1, UID: 1},
				{Mailbox: "Folders/Synthetic", UIDValidity: math.MaxUint32, UID: math.MaxUint32},
			} {
				// Selected metadata needs no UIDNEXT, so the maximum UID is
				// exercised without a wrapping mailbox snapshot.
				session := &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: want.UIDValidity}, members: []uint32{want.UID}}
				adapter := newPublicIdentityAdapter(session)
				identifier, err := adapter.encodeMessageID(want)
				if err != nil {
					t.Fatalf("encodeMessageID: %v", err)
				}

				selected, err := adapter.GetMessageMetadata(context.Background(), identifier)
				if err != nil {
					t.Fatalf("GetMessageMetadata(%#v): %v", want, err)
				}
				if selected.ID != identifier {
					t.Fatalf("selected ID changed: %q", selected.ID)
				}
				requireBoundIdentity(t, adapter, selected, want)
			}
		})
	})

	t.Run("invalid_tuple", func(t *testing.T) {
		searches := []struct {
			name    string
			session *publicIdentitySession
		}{
			{
				name:    "zero UIDVALIDITY",
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDNext: 3}, members: []uint32{1, 2}},
			},
			{
				name: "zero fetched UID after a valid UID",
				session: &publicIdentitySession{
					snapshot: mailboxSnapshot{UIDValidity: 9001, UIDNext: 3},
					members:  []uint32{1, 2},
					fetched:  []MessageMetadata{{Subject: "valid", uid: 1}, {Subject: "zero", uid: 0}},
				},
			},
		}
		for _, test := range searches {
			t.Run(test.name, func(t *testing.T) {
				adapter := newPublicIdentityAdapter(test.session)
				page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
				if CodeOf(err) != CodeIMAPProtocol {
					t.Fatalf("SearchMailPage error = %v, want %q", err, CodeIMAPProtocol)
				}
				if page.Messages != nil || page.Truncated {
					t.Fatalf("SearchMailPage returned partial page %#v", page)
				}
			})
		}

		selected := []struct {
			name    string
			payload messageIDPayload
			session *publicIdentitySession
			forged  bool
		}{
			{
				name:    "changed UIDVALIDITY",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: 1},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: 9002}, members: []uint32{1}},
			},
			{
				name:    "missing UID",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: 1},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: 9001}},
			},
			{
				name:    "FETCH answers another UID",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: 1},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: 9001}, members: []uint32{1}, fetched: []MessageMetadata{{uid: 2}}},
			},
			{
				name:    "FETCH answers extra UIDs",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: 1},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: 9001}, members: []uint32{1}, fetched: []MessageMetadata{{uid: 1}, {uid: 2}}},
			},
			{
				name:    "forged zero UIDVALIDITY",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 0, UID: 1},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{}, members: []uint32{1}},
				forged:  true,
			},
			{
				name:    "forged zero UID",
				payload: messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: 0},
				session: &publicIdentitySession{snapshot: mailboxSnapshot{UIDValidity: 9001}, members: []uint32{0}},
				forged:  true,
			},
		}
		for _, test := range selected {
			t.Run(test.name, func(t *testing.T) {
				adapter := newPublicIdentityAdapter(test.session)
				identifier := forgedMessageID(adapter, test.payload)
				if !test.forged {
					var err error
					identifier, err = adapter.encodeMessageID(test.payload)
					if err != nil {
						t.Fatalf("encodeMessageID: %v", err)
					}
				}

				metadata, err := adapter.GetMessageMetadata(context.Background(), identifier)
				if CodeOf(err) != CodeStaleMessageID {
					t.Fatalf("GetMessageMetadata error = %v, want %q", err, CodeStaleMessageID)
				}
				if metadata.ID != "" || metadata.UIDValidity != 0 || metadata.UID != 0 {
					t.Fatalf("refused read returned identity %#v", metadata)
				}
			})
		}
	})

	t.Run("retry_isolation", func(t *testing.T) {
		first := &replaySearchSession{
			snapshot:      mailboxSnapshot{UIDNext: 201, UIDValidity: 7001},
			searchResults: [][]uint32{{150}, {1}},
			metadataResults: []replaySearchResponse{
				{metadata: []MessageMetadata{{Subject: "first attempt", uid: 150}}},
				{err: errorCode(CodeBridgeUnreachable)},
			},
		}
		second := &replaySearchSession{
			snapshot:        mailboxSnapshot{UIDNext: 101, UIDValidity: 7002},
			searchResults:   [][]uint32{{2}},
			metadataResults: []replaySearchResponse{{metadata: []MessageMetadata{{Subject: "replacement", uid: 2}}}},
		}
		adapter := newPublicIdentityAdapter(first, second)

		page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
		if err != nil {
			t.Fatalf("SearchMailPage: %v", err)
		}
		if len(page.Messages) != 1 || page.Messages[0].Subject != "replacement" {
			t.Fatalf("page = %#v, want only the replacement attempt", page)
		}
		requireBoundIdentity(t, adapter, page.Messages[0], messageIDPayload{Mailbox: "INBOX", UIDValidity: 7002, UID: 2})
		if first.metadataCalls != 2 || second.metadataCalls != 1 {
			t.Fatalf("metadata calls = %d/%d, want a failed first attempt and one replay", first.metadataCalls, second.metadataCalls)
		}
	})
}

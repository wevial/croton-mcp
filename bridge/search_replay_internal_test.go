package bridge

import (
	"context"
	"testing"
)

type replaySearchResponse struct {
	metadata []MessageMetadata
	err      error
}

// replaySearchSession implements only what the adapter calls; the embedded
// nil interface fills the rest of bridgeSession.
type replaySearchSession struct {
	bridgeSession
	snapshot        mailboxSnapshot
	searchResults   [][]uint32
	searchCalls     int
	metadataResults []replaySearchResponse
	metadataCalls   int
}

func (session *replaySearchSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return session.snapshot, nil
}

func (session *replaySearchSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	result := session.searchResults[session.searchCalls]
	session.searchCalls++
	return result, nil
}

func (session *replaySearchSession) UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error) {
	result := session.metadataResults[session.metadataCalls]
	session.metadataCalls++
	return result.metadata, result.err
}

func (session *replaySearchSession) Logout(context.Context) error {
	return nil
}

func (session *replaySearchSession) Abort() error {
	return nil
}

func TestSearchMailDiscardsPartialResultsBeforeTransportReplay(t *testing.T) {
	first := &replaySearchSession{
		snapshot:      mailboxSnapshot{UIDNext: 201, UIDValidity: 9001},
		searchResults: [][]uint32{{101}, {1}},
		metadataResults: []replaySearchResponse{
			{metadata: []MessageMetadata{{uid: 101}}},
			{err: errorCode(CodeBridgeUnreachable)},
		},
	}
	second := &replaySearchSession{
		snapshot:        mailboxSnapshot{UIDNext: 101, UIDValidity: 9002},
		searchResults:   [][]uint32{{2}},
		metadataResults: []replaySearchResponse{{metadata: []MessageMetadata{{uid: 2}}}},
	}
	sessions := []*replaySearchSession{first, second}
	factoryCalls := 0
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 1000},
			Bounds: Bounds{MaxSearchResults: 2, MaxOutputBytes: defaultMaxOutputBytes},
		},
		factory: func(context.Context) (bridgeSession, error) {
			session := sessions[factoryCalls]
			factoryCalls++
			return session, nil
		},
		gate: make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}

	page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
	results := page.Messages
	if err != nil {
		t.Fatalf("SearchMailPage: %v", err)
	}
	if len(results) != 1 || results[0].uid != 2 {
		t.Fatalf("SearchMailPage replay results = %#v, want only replacement generation UID 2", results)
	}
	if factoryCalls != 2 {
		t.Fatalf("session factory calls = %d, want 2", factoryCalls)
	}
}

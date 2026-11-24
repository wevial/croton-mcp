package bridge

import "context"

type replaySearchResponse struct {
	metadata []MessageMetadata
	err      error
}

type replaySearchSession struct {
	boundedSearchSession
	snapshot        mailboxSnapshot
	searchResults   [][]uint32
	metadataResults []replaySearchResponse
	metadataCalls   int
}

func (session *replaySearchSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return session.snapshot, nil
}

func (session *replaySearchSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	result := session.searchResults[0]
	session.searchResults = session.searchResults[1:]
	return result, nil
}

func (session *replaySearchSession) UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error) {
	result := session.metadataResults[session.metadataCalls]
	session.metadataCalls++
	return result.metadata, result.err
}

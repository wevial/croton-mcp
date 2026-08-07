package bridge

import (
	"context"
	"testing"
)

type oversizedListSession struct {
	folders []Folder
	aborts  int
}

func (session *oversizedListSession) Authenticate(context.Context, Credentials) error {
	return nil
}

func (session *oversizedListSession) List(context.Context, int) ([]Folder, error) {
	return session.folders, nil
}

func (session *oversizedListSession) Status(context.Context, string) (MailboxStatus, error) {
	return MailboxStatus{}, nil
}

func (session *oversizedListSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return mailboxSnapshot{}, nil
}

func (session *oversizedListSession) UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error) {
	return nil, nil
}

func (session *oversizedListSession) UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error) {
	return nil, nil
}

func (session *oversizedListSession) UIDFetchBody(context.Context, uint32, int) ([]byte, error) {
	return nil, nil
}

func (session *oversizedListSession) Logout(context.Context) error {
	return nil
}

func (session *oversizedListSession) Abort() error {
	session.aborts++
	return nil
}

func TestAdapterListFoldersAbortsWhenSessionExceedsConfiguredCap(t *testing.T) {
	session := &oversizedListSession{folders: []Folder{{Name: "INBOX"}, {Name: "Archive"}}}
	adapter := &Adapter{
		config:  ValidatedConfig{Bounds: Bounds{MaxFolderResults: 1}},
		factory: func(context.Context) (readSession, error) { return session, nil },
		gate:    make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}

	folders, err := adapter.ListFolders(context.Background())
	if CodeOf(err) != CodeBoundsExceeded {
		t.Fatalf("ListFolders error = %v, want %q", err, CodeBoundsExceeded)
	}
	if folders != nil {
		t.Fatalf("ListFolders folders = %#v, want no partial result", folders)
	}
	if session.aborts != 1 {
		t.Fatalf("session aborts = %d, want 1", session.aborts)
	}
}

package bridge

import (
	"context"
	"testing"
)

// oversizedListSession implements only what the adapter calls; the embedded
// nil interface fills the rest of bridgeSession.
type oversizedListSession struct {
	bridgeSession
	folders []Folder
	aborts  int
}

func (session *oversizedListSession) List(context.Context, int) ([]Folder, error) {
	return session.folders, nil
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
		factory: func(context.Context) (bridgeSession, error) { return session, nil },
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

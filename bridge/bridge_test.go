package bridge_test

import (
	"context"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterListsFoldersInBothTLSModes(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		fake testkit.TLSMode
		mode string
	}{
		{name: "implicit TLS", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{Mode: test.fake})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), test.mode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			folders, err := adapter.ListFolders(context.Background())
			if err != nil {
				t.Fatalf("ListFolders: %v", err)
			}
			if len(folders) != 1 || folders[0].Name != "INBOX" {
				t.Fatalf("folders = %#v, want only INBOX", folders)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

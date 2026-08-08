package bridge_test

import (
	"context"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterAbortsLISTWhenServerExceedsFolderCap(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{ListResponseCount: 2},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxFolderResults = bridge.Int(1)
	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.ListFolders(context.Background()); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("ListFolders error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
	assertTranscriptContains(t, server.Commands(), "LIST \"\" \"*\"")
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
}

func TestAdapterRejectsOversizedLISTMailboxLiteral(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{ListMailboxLiteralBytes: 128 << 10},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.ListFolders(context.Background()); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("ListFolders oversized mailbox literal error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

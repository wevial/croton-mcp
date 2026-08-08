package bridge_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterHonorsDeadlineWhileInitializingDelayedSession(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{ResponseDelay: 500 * time.Millisecond},
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

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()

	started := time.Now()
	_, err = adapter.ListFolders(ctx)
	elapsed := time.Since(started)
	if bridge.CodeOf(err) != bridge.CodeCommandTimedOut {
		t.Fatalf("ListFolders delayed initialization error = %v, want %q", err, bridge.CodeCommandTimedOut)
	}
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("ListFolders returned after %s, want cancellation before delayed greeting", elapsed)
	}
}

func TestAdapterCloseLogsOutAndPreventsFutureOperations(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}

	if _, err := adapter.ListFolders(context.Background()); err != nil {
		t.Fatalf("ListFolders: %v", err)
	}
	if err := adapter.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := adapter.ListFolders(context.Background()); bridge.CodeOf(err) != bridge.CodeAdapterClosed {
		t.Fatalf("ListFolders after Close error = %v, want %q", err, bridge.CodeAdapterClosed)
	}

	transcript := strings.Join(commandStrings(server.Commands()), "\n")
	if strings.Count(transcript, "LOGOUT") != 1 {
		t.Fatalf("LOGOUT count = %d, want one: %q", strings.Count(transcript, "LOGOUT"), transcript)
	}
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
}

package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterRejectsOpaqueIDAfterUIDValidityChanges(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{UIDValidityAfterExamine: 9002},
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

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(results) != 1 {
		t.Fatalf("SearchMail = %#v, %v", results, err)
	}
	if _, err := adapter.GetMessageMetadata(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeStaleMessageID {
		t.Fatalf("GetMessageMetadata error = %v, want %q", err, bridge.CodeStaleMessageID)
	}

	commands := server.Commands()
	transcript := make([]string, 0, len(commands))
	for _, command := range commands {
		transcript = append(transcript, command.Raw)
	}
	joined := strings.Join(transcript, "\n")
	if strings.Count(joined, "EXAMINE INBOX") != 2 {
		t.Fatalf("transcript = %q, want two read-only EXAMINE commands", joined)
	}
	if strings.Count(joined, "UID FETCH") != 1 {
		t.Fatalf("transcript = %q, want no FETCH after stale generation", joined)
	}
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
}

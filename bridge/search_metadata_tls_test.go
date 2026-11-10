package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

var tlsMetadataMessages = []string{
	"From: Seen Fixture <seen@croton.test>\r\nSubject: Seen metadata\r\n\r\nseen-body-marker\r\n",
	"From: Unseen Fixture <unseen@croton.test>\r\nSubject: Unseen metadata\r\n\r\nunseen-body-marker\r\n",
}

func startTLSMetadataAdapter(t *testing.T, flags testkit.MetadataFlagsMode) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Messages: tlsMetadataMessages, Seen: []bool{true}, MetadataFlags: flags})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

// TestSearchMetadataFlagsShape pins the read state to the FLAGS item the
// server actually returned: omitted is unknown and changes no Seen state,
// and a duplicated item refuses the whole page with a code-only error.
func TestSearchMetadataFlagsShape(t *testing.T) {
	server, adapter := startTLSMetadataAdapter(t, testkit.MetadataFlagsOmitted)

	page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(page.Messages) != len(tlsMetadataMessages) {
		t.Fatalf("omitted FLAGS SearchMailPage = %d results, %v", len(page.Messages), err)
	}
	for _, result := range page.Messages {
		if result.ReadState != bridge.ReadStateUnknown {
			t.Fatalf("omitted FLAGS UID %d read state %v, want unknown", result.UID, result.ReadState)
		}
	}
	unseen, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX", Unread: true})
	if err != nil || len(unseen.Messages) != 1 || unseen.Messages[0].UID != 102 {
		t.Fatalf("UNSEEN after metadata reads = %+v, %v, want only UID 102", unseen.Messages, err)
	}
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	for _, command := range server.Commands() {
		upper := strings.ToUpper(command.Raw)
		if strings.Contains(upper, " FETCH ") && (!strings.Contains(upper, "BODY.PEEK[HEADER]") || !strings.Contains(upper, " FLAGS") || strings.Contains(upper, "BODY.PEEK[]")) {
			t.Fatalf("FETCH %q is not a FLAGS plus header-only metadata read", command.Raw)
		}
	}

	_, adapter = startTLSMetadataAdapter(t, testkit.MetadataFlagsDuplicated)
	page, err = adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if bridge.CodeOf(err) != bridge.CodeIMAPProtocol || err.Error() != bridge.CodeIMAPProtocol || page.Messages != nil {
		t.Fatalf("duplicate FLAGS SearchMailPage = %#v, %v, want nil and code-only %q", page.Messages, err, bridge.CodeIMAPProtocol)
	}
}

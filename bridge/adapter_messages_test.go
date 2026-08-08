package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterReadsMailboxStatusAndMessageMetadataInBothTLSModes(t *testing.T) {
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

			status, err := adapter.Status(context.Background(), "INBOX")
			if err != nil {
				t.Fatalf("Status: %v", err)
			}
			if status.UIDValidity != 9001 || status.UIDNext != 103 || status.Messages != 2 {
				t.Fatalf("status = %#v", status)
			}

			results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX", Subject: "Welcome"})
			if err != nil {
				t.Fatalf("SearchMail: %v", err)
			}
			if len(results) != 1 || results[0].Subject != "Welcome to Croton" || results[0].ID == "" {
				t.Fatalf("search results = %#v", results)
			}

			metadata, err := adapter.GetMessageMetadata(context.Background(), results[0].ID)
			if err != nil {
				t.Fatalf("GetMessageMetadata: %v", err)
			}
			if metadata.Subject != "Welcome to Croton" || metadata.Size == 0 {
				t.Fatalf("metadata = %#v", metadata)
			}

			body, err := adapter.GetMessageBody(context.Background(), results[0].ID)
			if err != nil {
				t.Fatalf("GetMessageBody: %v", err)
			}
			if !strings.Contains(string(body), "Welcome to Croton") {
				t.Fatalf("body = %q", body)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

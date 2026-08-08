package bridge_test

import (
	"context"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterRejectsMultipleUIDAttributesInMetadataFetch(t *testing.T) {
	tests := []struct {
		name     string
		scenario testkit.Scenario
	}{
		{name: "unrequested before requested", scenario: testkit.Scenario{FetchExtraUIDBefore: 999}},
		{name: "unrequested after requested", scenario: testkit.Scenario{FetchExtraUIDAfter: 999}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Scenario: test.scenario})
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
			if bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
				t.Fatalf("SearchMail error = %v, results = %#v, want %q", err, results, bridge.CodeIMAPProtocol)
			}
			if results != nil {
				t.Fatalf("SearchMail results = %#v, want nil", results)
			}
		})
	}
}

package bridge_test

import (
	"context"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterStatusRejectsMissingSubstitutedAndIncompleteResponses(t *testing.T) {
	tests := []struct {
		name     string
		scenario testkit.Scenario
	}{
		{name: "omitted", scenario: testkit.Scenario{OmitStatusResponse: true}},
		{name: "substituted mailbox", scenario: testkit.Scenario{StatusResponse: `* STATUS "Archive" (MESSAGES 2 UIDNEXT 103 UIDVALIDITY 9001 UNSEEN 1)`}},
		{name: "missing messages", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (UIDNEXT 103 UIDVALIDITY 9001 UNSEEN 1)`}},
		{name: "missing UIDNEXT", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (MESSAGES 2 UIDVALIDITY 9001 UNSEEN 1)`}},
		{name: "missing UIDVALIDITY", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (MESSAGES 2 UIDNEXT 103 UNSEEN 1)`}},
		{name: "missing unseen", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (MESSAGES 2 UIDNEXT 103 UIDVALIDITY 9001)`}},
		{name: "zero UIDNEXT", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (MESSAGES 2 UIDNEXT 0 UIDVALIDITY 9001 UNSEEN 1)`}},
		{name: "zero UIDVALIDITY", scenario: testkit.Scenario{StatusResponse: `* STATUS "INBOX" (MESSAGES 2 UIDNEXT 103 UIDVALIDITY 0 UNSEEN 1)`}},
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

			status, err := adapter.Status(context.Background(), "INBOX")
			if bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
				t.Fatalf("Status error = %v, want %q", err, bridge.CodeIMAPProtocol)
			}
			if status != (bridge.MailboxStatus{}) {
				t.Fatalf("Status = %#v, want zero result", status)
			}
		})
	}
}

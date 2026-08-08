package bridge_test

import (
	"context"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestSearchMailMissingFetchedUIDRemainsProtocolError(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{OmitMetadataFetchResponse: true},
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

	if _, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
		t.Fatalf("SearchMail error = %v, want %q", err, bridge.CodeIMAPProtocol)
	}
}

func TestAdapterAcceptsMatchingTagESearchProof(t *testing.T) {
	for _, test := range []struct {
		name        string
		response    string
		wantCode    string
		wantSuccess bool
	}{
		{name: "matching result", response: `* ESEARCH (TAG "{TAG}") UID ALL 101`, wantSuccess: true},
		{name: "matching empty", response: `* ESEARCH (TAG "{TAG}") UID`, wantCode: bridge.CodeStaleMessageID},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode:     testkit.ImplicitTLS,
				Scenario: testkit.Scenario{ExactUIDSearchResponse: test.response},
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
			_, err = adapter.GetMessageMetadata(context.Background(), results[0].ID)
			if test.wantSuccess && err != nil {
				t.Fatalf("GetMessageMetadata: %v", err)
			}
			if !test.wantSuccess && bridge.CodeOf(err) != test.wantCode {
				t.Fatalf("GetMessageMetadata error = %v, want %q", err, test.wantCode)
			}
		})
	}
}

func TestAdapterRejectsMismatchedTagESearchProof(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode: testkit.ImplicitTLS,
		Scenario: testkit.Scenario{
			ExactUIDSearchResponse: `* ESEARCH (TAG "stale") UID ALL 101`,
		},
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
	if _, err := adapter.GetMessageMetadata(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
		t.Fatalf("GetMessageMetadata error = %v, want %q", err, bridge.CodeIMAPProtocol)
	}
}

func TestAdapterRejectsTaggedOnlyExactUIDSearch(t *testing.T) {
	for _, lookup := range []struct {
		name string
		call func(context.Context, *bridge.Adapter, string) error
	}{
		{name: "metadata", call: func(ctx context.Context, adapter *bridge.Adapter, id string) error {
			_, err := adapter.GetMessageMetadata(ctx, id)
			return err
		}},
		{name: "body", call: func(ctx context.Context, adapter *bridge.Adapter, id string) error {
			_, err := adapter.GetMessageBody(ctx, id)
			return err
		}},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode:     testkit.ImplicitTLS,
				Scenario: testkit.Scenario{OmitExactUIDSearchData: true},
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
			if err := lookup.call(context.Background(), adapter, results[0].ID); bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
				t.Fatalf("lookup error = %v, want %q", err, bridge.CodeIMAPProtocol)
			}
		})
	}
}

func TestAdapterReportsExpungedOpaqueIDsAsStale(t *testing.T) {
	tests := []struct {
		name     string
		scenario testkit.Scenario
		lookup   func(context.Context, *bridge.Adapter, string) error
	}{
		{
			name:     "metadata",
			scenario: testkit.Scenario{OmitExactUIDSearchResponse: true},
			lookup: func(ctx context.Context, adapter *bridge.Adapter, id string) error {
				_, err := adapter.GetMessageMetadata(ctx, id)
				return err
			},
		},
		{
			name:     "body",
			scenario: testkit.Scenario{OmitExactUIDSearchResponse: true},
			lookup: func(ctx context.Context, adapter *bridge.Adapter, id string) error {
				_, err := adapter.GetMessageBody(ctx, id)
				return err
			},
		},
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
			if err != nil || len(results) != 1 {
				t.Fatalf("SearchMail = %#v, %v", results, err)
			}
			if err := test.lookup(context.Background(), adapter, results[0].ID); bridge.CodeOf(err) != bridge.CodeStaleMessageID {
				t.Fatalf("lookup error = %v, want %q", err, bridge.CodeStaleMessageID)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

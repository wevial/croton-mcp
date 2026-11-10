package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterRejectsZeroMaxHeaderBytes(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxHeaderBytes = bridge.Int(0)

	if _, err := bridge.NewAdapter(config); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("NewAdapter zero maxHeaderBytes error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterRejectsOversizedControlResponses(t *testing.T) {
	for _, test := range []struct {
		name      string
		command   string
		operation func(*bridge.Adapter) error
	}{
		{
			name: "session setup",
			operation: func(adapter *bridge.Adapter) error {
				_, err := adapter.ListFolders(context.Background())
				return err
			},
		},
		{
			name:    "status",
			command: "STATUS",
			operation: func(adapter *bridge.Adapter) error {
				_, err := adapter.Status(context.Background(), "INBOX")
				return err
			},
		},
		{
			name:    "examine",
			command: "EXAMINE",
			operation: func(adapter *bridge.Adapter) error {
				_, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode: testkit.ImplicitTLS,
				Scenario: testkit.Scenario{
					OversizedResponseBytes:   128 << 10,
					OversizedResponseCommand: test.command,
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

			if err := test.operation(adapter); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
				t.Fatalf("oversized %s response error = %v, want %q", test.name, err, bridge.CodeBoundsExceeded)
			}
		})
	}
}

func TestAdapterRejectsOversizedESearchRangeWithoutMaterializingIt(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{SearchResponse: "* ESEARCH (TAG \"{TAG}\") UID ALL 1:100000000"},
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

	if _, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("SearchMailPage oversized ESEARCH range error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterRejectsSearchResponseBeyondInputBudget(t *testing.T) {
	for _, test := range []struct {
		name     string
		response string
		wantCode string
	}{
		{name: "within budget", response: "* SEARCH 101"},
		{name: "beyond budget", response: "* OK " + strings.Repeat("X", 8<<10) + "\r\n* SEARCH 101", wantCode: bridge.CodeBoundsExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode:     testkit.ImplicitTLS,
				Scenario: testkit.Scenario{SearchResponse: test.response},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
			config.Bounds = bridge.BoundsPatch{MaxSearchResults: bridge.Int(1)}
			adapter, err := bridge.NewAdapter(config)
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})

			if test.wantCode == "" {
				if err != nil || len(page.Messages) != 1 {
					t.Fatalf("SearchMailPage = %#v, %v, want one result", page, err)
				}
				return
			}
			if bridge.CodeOf(err) != test.wantCode || page.Messages != nil {
				t.Fatalf("SearchMailPage oversized response = %#v, %v, want %q and no results", page, err, test.wantCode)
			}
		})
	}
}

func TestAdapterReconnectsAfterBoundedAbort(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{OversizedBodyLiteralBytes: 33},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxBodyBytes = bridge.Int(32)
	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	results := page.Messages
	if err != nil || len(results) != 1 {
		t.Fatalf("SearchMailPage = %#v, %v", results, err)
	}
	if _, err := adapter.GetMessage(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("GetMessage error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
	if _, err := adapter.Status(context.Background(), "INBOX"); err != nil {
		t.Fatalf("Status after bounded abort: %v", err)
	}

	var connections []int
	for _, command := range server.Commands() {
		if command.Name == "STATUS" {
			connections = append(connections, command.ConnectionID)
		}
	}
	if len(connections) != 1 || connections[0] < 2 {
		t.Fatalf("status did not use a post-abort connection: %+v", server.Commands())
	}
}

func TestAdapterMapsLoginRejectionToAuthentication(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Scenario: testkit.Scenario{RejectAuthentication: true}})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.ListFolders(context.Background()); bridge.CodeOf(err) != bridge.CodeAuthentication {
		t.Fatalf("ListFolders rejected login error = %v, want %q", err, bridge.CodeAuthentication)
	}
	if transcript := strings.Join(commandStrings(server.Commands()), "\n"); !strings.Contains(transcript, "LOGIN") {
		t.Fatalf("authentication test transcript = %q, want LOGIN", transcript)
	}
}

func TestAdapterRejectsOversizedSearchResponseBudget(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{SearchResponse: "* SEARCH" + strings.Repeat(" 101", 1100)},
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

	if _, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("SearchMailPage oversized SEARCH response error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func commandStrings(commands []testkit.Command) []string {
	result := make([]string, 0, len(commands))
	for _, command := range commands {
		result = append(result, command.Raw)
	}
	return result
}

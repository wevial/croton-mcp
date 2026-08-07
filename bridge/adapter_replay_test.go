package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterReplaysOneTransportFailure(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		fakeMode        testkit.TLSMode
		adapterMode     string
		disconnectAfter int
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit, disconnectAfter: 10},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS, disconnectAfter: 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{
				Mode:     test.fakeMode,
				Scenario: testkit.Scenario{DisconnectAfterCommand: test.disconnectAfter},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), test.adapterMode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			messages, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil || len(messages) != 1 {
				t.Fatalf("SearchMail = %#v, %v", messages, err)
			}

			body, err := adapter.GetMessageBody(context.Background(), messages[0].ID)
			if err != nil {
				t.Fatalf("GetMessageBody after disconnect: %v", err)
			}
			if !strings.Contains(string(body), "Welcome to Croton") {
				t.Fatalf("GetMessageBody = %q", body)
			}

			commands := server.Commands()
			var examineConnections []int
			var bodyFetchConnections []int
			loginCount := 0
			for _, command := range commands {
				if command.Name == "LOGIN" {
					loginCount++
				}
				if command.Name == "EXAMINE" {
					examineConnections = append(examineConnections, command.ConnectionID)
				}
				if strings.Contains(command.Raw, "BODY.PEEK[]") {
					bodyFetchConnections = append(bodyFetchConnections, command.ConnectionID)
				}
			}
			if loginCount != 2 {
				t.Fatalf("LOGIN count = %d, want exactly two connections: %+v", loginCount, commands)
			}
			if len(examineConnections) != 3 {
				t.Fatalf("EXAMINE count = %d, want initial search and one body attempt/replay: %+v", len(examineConnections), commands)
			}
			if len(bodyFetchConnections) != 1 {
				t.Fatalf("BODY.PEEK[] fetch count = %d, want one successful replay fetch: %+v", len(bodyFetchConnections), commands)
			}
			if examineConnections[1] == examineConnections[2] || bodyFetchConnections[0] != examineConnections[2] {
				t.Fatalf("replay did not use a new connection: EXAMINE=%v bodyFetch=%v", examineConnections, bodyFetchConnections)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

func TestAdapterRejectsStaleMessageIDAfterTransportReplay(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		fakeMode        testkit.TLSMode
		adapterMode     string
		disconnectAfter int
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit, disconnectAfter: 10},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS, disconnectAfter: 12},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{
				Mode: test.fakeMode,
				Scenario: testkit.Scenario{
					DisconnectAfterCommand:  test.disconnectAfter,
					UIDValidityAfterExamine: 9002,
				},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), test.adapterMode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			messages, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil || len(messages) != 1 {
				t.Fatalf("SearchMail = %#v, %v", messages, err)
			}

			if _, err := adapter.GetMessageBody(context.Background(), messages[0].ID); bridge.CodeOf(err) != bridge.CodeStaleMessageID {
				t.Fatalf("GetMessageBody after reconnect error = %v, want %q", err, bridge.CodeStaleMessageID)
			}

			commands := server.Commands()
			bodyFetchCount := 0
			loginCount := 0
			for _, command := range commands {
				if command.Name == "LOGIN" {
					loginCount++
				}
				if strings.Contains(command.Raw, "BODY.PEEK[]") {
					bodyFetchCount++
				}
			}
			if loginCount != 2 {
				t.Fatalf("LOGIN count = %d, want transport replay: %+v", loginCount, commands)
			}
			if bodyFetchCount != 0 {
				t.Fatalf("BODY.PEEK[] fetch count = %d, want no fetch after UIDVALIDITY changed: %+v", bodyFetchCount, commands)
			}
		})
	}
}

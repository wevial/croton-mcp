package bridge_test

import (
	"context"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// TestAdapterReplaysIdleConnectionClose closes the session connection on the
// first command after a completed read, as when Bridge drops an idle
// connection, and expects the next read to reconnect once and succeed.
func TestAdapterReplaysIdleConnectionClose(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name            string
		fakeMode        testkit.TLSMode
		adapterMode     string
		disconnectAfter int
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit, disconnectAfter: 9},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS, disconnectAfter: 11},
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

			page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			messages := page.Messages
			if err != nil || len(messages) != 1 {
				t.Fatalf("SearchMailPage = %#v, %v", messages, err)
			}
			if idle := len(server.Commands()); idle != test.disconnectAfter-1 {
				t.Fatalf("search sent %d commands, want %d so the close hits an idle connection: %+v", idle, test.disconnectAfter-1, server.Commands())
			}

			body, err := adapter.GetMessageBody(context.Background(), messages[0].ID)
			if err != nil || !strings.Contains(string(body), "Welcome to Croton") {
				t.Fatalf("GetMessageBody after idle close = %q, %v", body, err)
			}

			commands := server.Commands()
			loginConnections := []int{}
			bodyFetchConnections := []int{}
			for _, command := range commands {
				if command.Name == "LOGIN" {
					loginConnections = append(loginConnections, command.ConnectionID)
				}
				if strings.Contains(command.Raw, "BODY.PEEK[]") {
					bodyFetchConnections = append(bodyFetchConnections, command.ConnectionID)
				}
			}
			if len(loginConnections) != 2 || loginConnections[0] == loginConnections[1] {
				t.Fatalf("LOGIN connections = %v, want one reconnect: %+v", loginConnections, commands)
			}
			if len(bodyFetchConnections) != 1 || bodyFetchConnections[0] != loginConnections[1] {
				t.Fatalf("BODY.PEEK[] connections = %v, want one fetch on the new connection: %+v", bodyFetchConnections, commands)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
		})
	}
}

func TestAdapterSynchronizesPostLoginCapabilityBeforeOperation(t *testing.T) {
	for _, mode := range []struct {
		name        string
		fakeMode    testkit.TLSMode
		adapterMode string
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS},
	} {
		t.Run(mode.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{Mode: mode.fakeMode})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), mode.adapterMode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })
			if _, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); err != nil {
				t.Fatalf("SearchMailPage: %v", err)
			}

			betweenLoginAndExamine := map[string]int{}
			seenLogin := false
			for _, command := range server.Commands() {
				if command.Name == "LOGIN" {
					seenLogin = true
					continue
				}
				if seenLogin && command.Name == "EXAMINE" {
					break
				}
				if seenLogin {
					betweenLoginAndExamine[command.Name]++
				}
			}
			if betweenLoginAndExamine["CAPABILITY"] != 1 || betweenLoginAndExamine["NOOP"] != 1 {
				t.Fatalf("commands between LOGIN and EXAMINE = %v, want one automatic CAPABILITY and one synchronization NOOP: %+v", betweenLoginAndExamine, server.Commands())
			}
		})
	}
}

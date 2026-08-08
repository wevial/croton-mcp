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
		name        string
		fakeMode    testkit.TLSMode
		adapterMode string
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{
				Mode:     test.fakeMode,
				Scenario: testkit.Scenario{DisconnectOnExactUIDSearch: true},
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
			if _, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); err != nil {
				t.Fatalf("SearchMail: %v", err)
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

func TestAdapterReplaysPostLoginCapabilityFailure(t *testing.T) {
	for _, mode := range []struct {
		name        string
		fakeMode    testkit.TLSMode
		adapterMode string
	}{
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS},
	} {
		t.Run(mode.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode:     mode.fakeMode,
				Scenario: testkit.Scenario{DisconnectOnPostLoginCapability: true},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), mode.adapterMode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			messages, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil || len(messages) != 1 {
				t.Fatalf("SearchMail after post-login CAPABILITY disconnect = %#v, %v", messages, err)
			}
			assertTwoLogins(t, server.Commands())
		})
	}
}

func TestAdapterReplaysExactUIDSearchFailure(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{DisconnectOnExactUIDSearch: true},
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

	messages, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(messages) != 1 {
		t.Fatalf("SearchMail = %#v, %v", messages, err)
	}
	body, err := adapter.GetMessageBody(context.Background(), messages[0].ID)
	if err != nil || !strings.Contains(string(body), "Welcome to Croton") {
		t.Fatalf("GetMessageBody after exact UID SEARCH disconnect = %q, %v", body, err)
	}
	commands := server.Commands()
	assertTwoLogins(t, commands)
	exactSearches := 0
	for _, command := range commands {
		if command.Name == "UID" && strings.Contains(command.Raw, " SEARCH ") && strings.Contains(command.Raw, "UID 101") {
			exactSearches++
		}
	}
	if exactSearches != 2 {
		t.Fatalf("exact UID SEARCH count = %d, want one failed attempt and one replay: %+v", exactSearches, commands)
	}
}

func assertTwoLogins(t *testing.T, commands []testkit.Command) {
	t.Helper()
	loginCount := 0
	for _, command := range commands {
		if command.Name == "LOGIN" {
			loginCount++
		}
	}
	if loginCount != 2 {
		t.Fatalf("LOGIN count = %d, want one replay: %+v", loginCount, commands)
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
		{name: "implicit TLS", fakeMode: testkit.ImplicitTLS, adapterMode: bridge.TLSModeImplicit, disconnectAfter: 9},
		{name: "STARTTLS", fakeMode: testkit.StartTLS, adapterMode: bridge.TLSModeStartTLS, disconnectAfter: 11},
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

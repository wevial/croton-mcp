package bridge_test

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestDialTrustVerifiedFakeServerInBothTLSModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []struct {
		name string
		fake testkit.TLSMode
		mode string
	}{
		{name: "implicit TLS", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit},
		{name: "STARTTLS", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS},
	} {
		t.Run(mode.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{Mode: mode.fake})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			trustFile := writeTrustAnchor(t, server.LeafPEM())
			config := fakeServerConfig(t, server.Addr(), mode.mode, bridge.TLSConfig{TrustAnchorFile: trustFile, SPKISHA256: server.SPKISHA256()})
			connection, err := bridge.Dial(context.Background(), config)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })

			commands := server.Commands()
			if mode.fake == testkit.StartTLS {
				if len(commands) != 2 || commands[0].Name != "CAPABILITY" || commands[1].Name != "STARTTLS" || commands[0].TLS || commands[1].TLS {
					t.Fatalf("pre-TLS commands = %+v, want only CAPABILITY then STARTTLS", commands)
				}
			} else if len(commands) != 0 {
				t.Fatalf("implicit TLS sent unexpected pre-close commands: %+v", commands)
			}
		})
	}
}

func TestDialRejectsMismatchedTrust(t *testing.T) {
	t.Parallel()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: strings.Repeat("a", 64)})
	if _, err := bridge.Dial(context.Background(), config); bridge.CodeOf(err) != bridge.CodeTLSMismatch {
		t.Fatalf("Dial mismatch error = %v, want %q", err, bridge.CodeTLSMismatch)
	}

	unrelatedServer, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start unrelated fake server: %v", err)
	}
	t.Cleanup(func() { _ = unrelatedServer.Close() })

	config = fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{TrustAnchorFile: writeTrustAnchor(t, unrelatedServer.LeafPEM())})
	if _, err := bridge.Dial(context.Background(), config); bridge.CodeOf(err) != bridge.CodeTLSMismatch {
		t.Fatalf("Dial mismatched trust-anchor error = %v, want %q", err, bridge.CodeTLSMismatch)
	}

	config = fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{
		TrustAnchorFile: writeTrustAnchor(t, server.LeafPEM()),
		SPKISHA256:      strings.Repeat("a", 64),
	})
	if _, err := bridge.Dial(context.Background(), config); bridge.CodeOf(err) != bridge.CodeTLSMismatch {
		t.Fatalf("Dial mismatched trust-anchor and pin error = %v, want %q", err, bridge.CodeTLSMismatch)
	}
}

func TestDialAcceptsExactTrustAnchorWithoutSPKIPin(t *testing.T) {
	t.Parallel()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{TrustAnchorFile: writeTrustAnchor(t, server.LeafPEM())})
	connection, err := bridge.Dial(context.Background(), config)
	if err != nil {
		t.Fatalf("Dial exact trust anchor: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })
}

func TestDialFailsClosedWhenStartTLSIsUnavailable(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	commands := make(chan string, 1)
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = connection.Write([]byte("* OK fixture without TLS\r\n"))
		buffer := make([]byte, 256)
		count, _ := connection.Read(buffer)
		commands <- string(buffer[:count])
		_, _ = connection.Write([]byte("* CAPABILITY IMAP4rev1 LOGINDISABLED\r\na001 OK CAPABILITY completed\r\n"))
	}()

	config := fakeServerConfig(t, listener.Addr().String(), bridge.TLSModeStartTLS, bridge.TLSConfig{SPKISHA256: strings.Repeat("a", 64)})
	if _, err := bridge.Dial(context.Background(), config); bridge.CodeOf(err) != bridge.CodeTLSNegotiation {
		t.Fatalf("Dial without STARTTLS error = %v, want %q", err, bridge.CodeTLSNegotiation)
	}
	select {
	case command := <-commands:
		if !strings.Contains(command, "CAPABILITY") || strings.Contains(command, "STARTTLS") || strings.Contains(command, "LOGIN") || strings.Contains(command, "AUTHENTICATE") {
			t.Fatalf("pre-TLS command = %q, want CAPABILITY only", command)
		}
	case <-time.After(time.Second):
		t.Fatal("fake server did not observe the capability command")
	}
}

func TestDialSTARTTLSRejectsNonOKGreeting(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.StartTLS,
		Scenario: testkit.Scenario{Greeting: "* BYE fake server unavailable"},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeStartTLS, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	connection, err := bridge.Dial(context.Background(), config)
	if connection != nil {
		_ = connection.Close()
		t.Fatal("Dial returned a connection for a BYE greeting")
	}
	if bridge.CodeOf(err) != bridge.CodeTLSNegotiation {
		t.Fatalf("Dial error = %v, want %q", err, bridge.CodeTLSNegotiation)
	}
}

func TestDialSTARTTLSAcceptsWellFormedGreetings(t *testing.T) {
	for _, greeting := range []string{
		"* OK ",
		"* OK [X] ",
		"* OK [CAPABILITY IMAP4rev1 STARTTLS] ready",
		"* OK [X-CODE arg[more] ready",
		"* OK [X}Y arg] ready",
	} {
		t.Run(greeting, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{
				Mode:     testkit.StartTLS,
				Scenario: testkit.Scenario{Greeting: greeting},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			config := fakeServerConfig(t, server.Addr(), bridge.TLSModeStartTLS, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
			connection, err := bridge.Dial(context.Background(), config)
			if err != nil {
				t.Fatalf("Dial: %v", err)
			}
			t.Cleanup(func() { _ = connection.Close() })
		})
	}
}

func TestDialSTARTTLSRejectsMalformedOKGreetings(t *testing.T) {
	tests := []struct {
		name     string
		greeting string
	}{
		{name: "bare LF", greeting: "* OK ready\n"},
		{name: "embedded CR", greeting: "* OK ready\rinside\r\n"},
		{name: "NUL", greeting: "* OK ready\x00\r\n"},
		{name: "invalid empty response-code atom", greeting: "* OK [ ] ready\r\n"},
		{name: "invalid parenthesis response-code atom", greeting: "* OK [(] ready\r\n"},
		{name: "invalid quote response-code atom", greeting: "* OK [X\"Y arg] ready\r\n"},
		{name: "invalid backslash response-code atom", greeting: "* OK [X\\Y arg] ready\r\n"},
		{name: "unclosed response code", greeting: "* OK [CAPABILITY IMAP4rev1 ready\r\n"},
		{name: "empty response code", greeting: "* OK [] ready\r\n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = listener.Close() })
			commands := make(chan string, 1)
			go func() {
				connection, err := listener.Accept()
				if err != nil {
					return
				}
				defer connection.Close()
				_, _ = connection.Write([]byte(test.greeting))
				_ = connection.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
				buffer := make([]byte, 256)
				count, _ := connection.Read(buffer)
				commands <- string(buffer[:count])
			}()

			config := fakeServerConfig(t, listener.Addr().String(), bridge.TLSModeStartTLS, bridge.TLSConfig{SPKISHA256: strings.Repeat("a", 64)})
			connection, err := bridge.Dial(context.Background(), config)
			if connection != nil {
				_ = connection.Close()
				t.Fatal("Dial returned a connection for malformed greeting")
			}
			if bridge.CodeOf(err) != bridge.CodeTLSNegotiation {
				t.Fatalf("Dial error = %v, want %q", err, bridge.CodeTLSNegotiation)
			}
			select {
			case command := <-commands:
				if command != "" {
					t.Fatalf("malformed greeting triggered plaintext command %q", command)
				}
			case <-time.After(time.Second):
				t.Fatal("fake server did not finish")
			}
		})
	}
}

func fakeServerConfig(t *testing.T, address, mode string, tlsConfig bridge.TLSConfig) bridge.Config {
	t.Helper()
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split fake address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}

	return bridge.Config{IMAP: bridge.IMAPConfig{Host: host, Port: number, TLSMode: mode, CredentialCommand: credentialHelper(t, "valid"), TLS: tlsConfig, ConnectTimeoutMs: 1000}}
}

func writeTrustAnchor(t *testing.T, contents []byte) string {
	t.Helper()
	path := t.TempDir() + "/bridge-ca.pem"
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write trust anchor: %v", err)
	}
	return path
}

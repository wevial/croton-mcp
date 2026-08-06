package testkit

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"
)

const testNetworkTimeout = 2 * time.Second

func TestServerSupportsImplicitTLSAndStartTLS(t *testing.T) {
	t.Parallel()

	for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
		mode := mode
		t.Run(mode.String(), func(t *testing.T) {
			t.Parallel()

			server, err := Start(Options{Mode: mode})
			if err != nil {
				t.Fatalf("start server: %v", err)
			}
			t.Cleanup(func() {
				if err := server.Close(); err != nil {
					t.Errorf("close server: %v", err)
				}
			})

			client := connectClient(t, server, mode)
			defer client.Close()
			client.command(t, "a1 LOGIN fixture-user@client.test not-a-secret", "a1 OK LOGIN completed")
			client.command(t, "a2 LIST \"\" \"*\"", "* LIST (\\HasNoChildren) \"/\" \"INBOX\"")
			if line := client.readLine(t); line != "a2 OK LIST completed" {
				t.Fatalf("LIST completion = %q", line)
			}
			client.command(t, "a3 LOGOUT", "* BYE fake IMAP server logging out")
			if line := client.readLine(t); line != "a3 OK LOGOUT completed" {
				t.Fatalf("LOGOUT completion = %q", line)
			}

			commands := server.Commands()
			wantCommandCount := 3
			if mode == StartTLS {
				wantCommandCount++
			}
			if len(commands) != wantCommandCount {
				t.Fatalf("recorded commands = %d, want %d", len(commands), wantCommandCount)
			}
			for _, command := range commands {
				if command.Name != "STARTTLS" && !command.TLS {
					t.Fatalf("recorded command %+v before TLS", command)
				}
			}
		})
	}
}

func TestServerBindsOnlyToIPv4Loopback(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("parse listener address: %v", err)
	}
	if host != "127.0.0.1" || port == "0" || port == "" {
		t.Fatalf("listener address = %q, want 127.0.0.1 with an ephemeral assigned port", server.Addr())
	}
}

func TestServerTrustsOnlyItsGeneratedCA(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: ImplicitTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	trusted := dialTLS(t, server.Addr(), server.ClientTLSConfig())
	_ = trusted.Close()

	mismatched := server.ClientTLSConfig()
	mismatched.RootCAs = nil
	mismatched.InsecureSkipVerify = false //nolint:gosec // explicitly verifies the mismatch.
	untrusted, err := tls.DialWithDialer(&net.Dialer{Timeout: testNetworkTimeout}, "tcp", server.Addr(), mismatched)
	if err == nil {
		_ = untrusted.Close()
		t.Fatal("dial with mismatched trust unexpectedly succeeded")
	}
}

func TestServerExposesGeneratedTrustMaterial(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: ImplicitTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	block, rest := pem.Decode(server.CAPEM())
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		t.Fatal("CA material is not exactly one PEM certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}
	if !certificate.IsCA {
		t.Fatal("generated trust anchor is not a CA")
	}

	pin, err := hex.DecodeString(server.SPKISHA256())
	if err != nil {
		t.Fatalf("decode SPKI pin: %v", err)
	}
	if len(pin) != sha256.Size {
		t.Fatalf("SPKI pin length = %d, want %d", len(pin), sha256.Size)
	}
}

func TestServerCapturesAndRejectsPreTLSAuthentication(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: StartTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	connection := dialTCP(t, server.Addr())
	defer connection.Close()
	client := &imapClient{connection: connection, reader: bufio.NewReader(connection)}
	if line := client.readLine(t); line != "* OK fake IMAP server ready" {
		t.Fatalf("greeting = %q", line)
	}
	client.command(t, "a1 LOGIN fixture-user@client.test not-a-secret", "a1 NO TLS required")

	if err := server.AssertNoInsecureAuthentication(); err == nil {
		t.Fatal("pre-TLS LOGIN was not rejected by assertion helper")
	}
}

func TestServerAdvertisesStartTLSBeforeUpgrade(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: StartTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	connection := dialTCP(t, server.Addr())
	defer connection.Close()
	client := &imapClient{connection: connection, reader: bufio.NewReader(connection)}
	if line := client.readLine(t); line != "* OK fake IMAP server ready" {
		t.Fatalf("greeting = %q", line)
	}
	client.command(t, "a1 CAPABILITY", "* CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED")
	if line := client.readLine(t); line != "a1 OK CAPABILITY completed" {
		t.Fatalf("CAPABILITY completion = %q", line)
	}
}

func TestServerSupportsAuthenticatePlainContinuation(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: ImplicitTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	client := connectClient(t, server, ImplicitTLS)
	defer client.Close()
	client.command(t, "a1 CAPABILITY", "* CAPABILITY IMAP4rev1 AUTH=PLAIN")
	if line := client.readLine(t); line != "a1 OK CAPABILITY completed" {
		t.Fatalf("CAPABILITY completion = %q", line)
	}
	client.command(t, "a2 AUTHENTICATE PLAIN", "+")
	client.command(t, "AGZpeHR1cmUAZml4dHVyZQ==", "a2 OK AUTHENTICATE completed")
	client.command(t, "a3 LIST \"\" \"*\"", "* LIST (\\HasNoChildren) \"/\" \"INBOX\"")
	if line := client.readLine(t); line != "a3 OK LIST completed" {
		t.Fatalf("LIST completion = %q", line)
	}
}

func TestServerRejectsCancelledAndMalformedPlainAuthentication(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: ImplicitTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	client := connectClient(t, server, ImplicitTLS)
	defer client.Close()
	client.command(t, "a1 AUTHENTICATE PLAIN", "+")
	client.command(t, "*", "a1 BAD AUTHENTICATE cancelled")
	client.command(t, "a2 AUTHENTICATE PLAIN", "+")
	client.command(t, "not-base64", "a2 BAD invalid PLAIN response")
	client.command(t, "a3 AUTHENTICATE PLAIN", "+")
	client.command(t, "AGZpeHR1cmUA", "a3 BAD invalid PLAIN response")
	client.command(t, "a4 LIST \"\" \"*\"", "a4 NO authenticate first")
}

func TestServerFlagsMutationsAndUnsafeBodyFetches(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Mode: ImplicitTLS})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close server: %v", err)
		}
	})

	client := connectClient(t, server, ImplicitTLS)
	defer client.Close()
	client.command(t, "a1 STORE 1 +FLAGS (\\Seen)", "a1 BAD unsupported command")
	client.command(t, "a2 FETCH 1 (BODY[])", "a2 BAD unsupported command")
	client.command(t, "a3 FETCH 1 (BODY.PEEK[])", "a3 BAD unsupported command")

	if err := server.AssertReadOnlyCommands(); err == nil {
		t.Fatal("unsafe commands were not flagged")
	}
}

func TestReadOnlyAssertionsHandleUIDCommands(t *testing.T) {
	t.Parallel()

	for _, command := range []Command{
		{Sequence: 1, Raw: "a1 SELECT INBOX", Name: "SELECT", TLS: true},
		{Sequence: 1, Raw: "a1 CLOSE", Name: "CLOSE", TLS: true},
		{Sequence: 1, Raw: "a1 SUBSCRIBE Archive", Name: "SUBSCRIBE", TLS: true},
		{Sequence: 1, Raw: "a1 UNSUBSCRIBE Archive", Name: "UNSUBSCRIBE", TLS: true},
		{Sequence: 1, Raw: "a1 SETACL INBOX fixture lrswipkxte", Name: "SETACL", TLS: true},
		{Sequence: 1, Raw: "a1 DELETEACL INBOX fixture", Name: "DELETEACL", TLS: true},
		{Sequence: 1, Raw: "a1 SETQUOTA root (STORAGE 1)", Name: "SETQUOTA", TLS: true},
		{Sequence: 1, Raw: "a1 REPLACE 1 {5}", Name: "REPLACE", TLS: true},
		{Sequence: 1, Raw: "a1 SETMETADATA INBOX (/shared/comment value)", Name: "SETMETADATA", TLS: true},
		{Sequence: 1, Raw: "a1 SETANNOTATION INBOX /comment value.shared value", Name: "SETANNOTATION", TLS: true},
		{Sequence: 1, Raw: "a1 RESETKEY INBOX", Name: "RESETKEY", TLS: true},
		{Sequence: 1, Raw: "a1 X-UNKNOWN", Name: "X-UNKNOWN", TLS: true},
		{Sequence: 1, Raw: "a1 UID STORE 1 +FLAGS (\\Seen)", Name: "UID", TLS: true},
		{Sequence: 1, Raw: "a1 UID COPY 1 Archive", Name: "UID", TLS: true},
		{Sequence: 1, Raw: "a1 UID FETCH 1 (BODY[])", Name: "UID", TLS: true},
		{Sequence: 1, Raw: "a1 FETCH 1 (BODY[] BODY.PEEK[HEADER])", Name: "FETCH", TLS: true},
		{Sequence: 1, Raw: "a1 UID FETCH 1 (BINARY[] BODY.PEEK[HEADER])", Name: "UID", TLS: true},
		{Sequence: 1, Raw: "a1 FETCH 1 RFC822", Name: "FETCH", TLS: true},
		{Sequence: 1, Raw: "a1 FETCH 1 RFC822.TEXT", Name: "FETCH", TLS: true},
	} {
		server := &Server{commands: []Command{command}}
		if err := server.AssertReadOnlyCommands(); err == nil {
			t.Fatalf("unsafe UID command %q was not flagged", command.Raw)
		}
	}

	server := &Server{commands: []Command{
		{Sequence: 1, Raw: "a1 CAPABILITY", Name: "CAPABILITY", TLS: false},
		{Sequence: 2, Raw: "a2 STARTTLS", Name: "STARTTLS", TLS: false},
		{Sequence: 3, Raw: "a3 LOGIN fixture-user@client.test not-a-secret", Name: "LOGIN", TLS: true},
		{Sequence: 4, Raw: "a4 LIST \"\" \"*\"", Name: "LIST", TLS: true},
		{Sequence: 5, Raw: "a5 EXAMINE INBOX", Name: "EXAMINE", TLS: true},
		{Sequence: 6, Raw: "a6 STATUS INBOX (MESSAGES UIDNEXT)", Name: "STATUS", TLS: true},
		{Sequence: 7, Raw: "a7 UID SEARCH ALL", Name: "UID", TLS: true},
		{Sequence: 8, Raw: "a8 UID FETCH 1 (BODY.PEEK[] BINARY.PEEK[])", Name: "UID", TLS: true},
		{Sequence: 9, Raw: "a9 FETCH 1 (RFC822.SIZE RFC822.HEADER)", Name: "FETCH", TLS: true},
		{Sequence: 10, Raw: "a10 LOGOUT", Name: "LOGOUT", TLS: true},
	}}
	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("safe commands were rejected: %v", err)
	}
}

func TestServerScenarioHooks(t *testing.T) {
	t.Parallel()

	t.Run("bad authentication", func(t *testing.T) {
		server, err := Start(Options{Mode: ImplicitTLS, Scenario: Scenario{RejectAuthentication: true}})
		if err != nil {
			t.Fatalf("start server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		client := connectClient(t, server, ImplicitTLS)
		defer client.Close()
		client.command(t, "a1 LOGIN fixture-user@client.test not-a-secret", "a1 NO authentication failed")
	})

	t.Run("delay", func(t *testing.T) {
		server, err := Start(Options{Scenario: Scenario{ResponseDelay: 100 * time.Millisecond}})
		if err != nil {
			t.Fatalf("start server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		connection := dialTCP(t, server.Addr())
		defer connection.Close()
		if err := connection.SetReadDeadline(time.Now().Add(20 * time.Millisecond)); err != nil {
			t.Fatalf("set deadline: %v", err)
		}
		_, err = bufio.NewReader(connection).ReadString('\n')
		if !errors.Is(err, net.ErrClosed) && !isTimeout(err) {
			t.Fatalf("read delayed greeting: %v, want timeout", err)
		}
	})

	t.Run("disconnect", func(t *testing.T) {
		server, err := Start(Options{Mode: ImplicitTLS, Scenario: Scenario{DisconnectAfterCommand: 1}})
		if err != nil {
			t.Fatalf("start server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		client := connectClient(t, server, ImplicitTLS)
		if _, err := fmt.Fprint(client.connection, "a1 CAPABILITY\r\n"); err != nil {
			t.Fatalf("write command: %v", err)
		}
		if _, err := client.reader.ReadString('\n'); err == nil {
			t.Fatal("disconnect scenario returned a response")
		}
		_ = client.Close()

		reconnected := connectClient(t, server, ImplicitTLS)
		defer reconnected.Close()
		reconnected.command(t, "a2 CAPABILITY", "* CAPABILITY IMAP4rev1 AUTH=PLAIN")
		if line := reconnected.readLine(t); line != "a2 OK CAPABILITY completed" {
			t.Fatalf("reconnected CAPABILITY completion = %q", line)
		}
		commands := server.Commands()
		if len(commands) != 2 || commands[0].ConnectionID == 0 || commands[1].ConnectionID == 0 || commands[0].ConnectionID == commands[1].ConnectionID {
			t.Fatalf("reconnect transcript did not distinguish connections: %+v", commands)
		}
	})

	t.Run("malformed response", func(t *testing.T) {
		server, err := Start(Options{Mode: ImplicitTLS, Scenario: Scenario{MalformedResponse: "malformed response\r\n"}})
		if err != nil {
			t.Fatalf("start server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		client := connectClient(t, server, ImplicitTLS)
		defer client.Close()
		client.command(t, "a1 CAPABILITY", "malformed response")
	})

	t.Run("oversized response", func(t *testing.T) {
		server, err := Start(Options{Mode: ImplicitTLS, Scenario: Scenario{OversizedResponseBytes: 8192}})
		if err != nil {
			t.Fatalf("start server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		client := connectClient(t, server, ImplicitTLS)
		defer client.Close()
		if _, err := fmt.Fprint(client.connection, "a1 CAPABILITY\r\n"); err != nil {
			t.Fatalf("write command: %v", err)
		}
		line := client.readLine(t)
		if len(line) != 8192 || !strings.HasPrefix(line, "*") {
			t.Fatalf("oversized response length/prefix = %d/%q", len(line), line[:1])
		}
	})
}

func TestServerCloseInterruptsResponseDelay(t *testing.T) {
	t.Parallel()

	server, err := Start(Options{Scenario: Scenario{ResponseDelay: time.Hour}})
	if err != nil {
		t.Fatalf("start server: %v", err)
	}
	connection := dialTCP(t, server.Addr())
	defer connection.Close()

	accepted := time.NewTimer(testNetworkTimeout)
	defer accepted.Stop()
	for {
		server.mu.Lock()
		active := len(server.connections) > 0
		server.mu.Unlock()
		if active {
			break
		}
		select {
		case <-accepted.C:
			t.Fatal("server did not accept delayed connection")
		default:
			time.Sleep(time.Millisecond)
		}
	}

	closed := make(chan error, 1)
	go func() { closed <- server.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close delayed server: %v", err)
		}
	case <-time.After(testNetworkTimeout):
		t.Fatal("Close did not interrupt response delay")
	}
}

func TestSyntheticMIMESeedsUseReservedAddresses(t *testing.T) {
	t.Parallel()

	for _, seed := range SyntheticMIMESeeds() {
		if !strings.Contains(seed, ".test") {
			t.Fatalf("seed does not contain a reserved address: %q", seed)
		}
		if !strings.Contains(seed, "MIME-Version: 1.0") {
			t.Fatalf("seed is not MIME content: %q", seed)
		}
	}
}

type imapClient struct {
	connection net.Conn
	reader     *bufio.Reader
}

func dialTCP(t *testing.T, address string) net.Conn {
	t.Helper()

	connection, err := net.DialTimeout("tcp", address, testNetworkTimeout)
	if err != nil {
		t.Fatalf("dial server: %v", err)
	}

	if err := connection.SetDeadline(time.Now().Add(testNetworkTimeout)); err != nil {
		_ = connection.Close()
		t.Fatalf("set connection deadline: %v", err)
	}

	return connection
}

func dialTLS(t *testing.T, address string, config *tls.Config) *tls.Conn {
	t.Helper()

	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: testNetworkTimeout}, "tcp", address, config)
	if err != nil {
		t.Fatalf("dial TLS server: %v", err)
	}

	if err := connection.SetDeadline(time.Now().Add(testNetworkTimeout)); err != nil {
		_ = connection.Close()
		t.Fatalf("set TLS connection deadline: %v", err)
	}

	return connection
}

func connectClient(t *testing.T, server *Server, mode TLSMode) *imapClient {
	t.Helper()

	if mode == ImplicitTLS {
		connection := dialTLS(t, server.Addr(), server.ClientTLSConfig())
		client := &imapClient{connection: connection, reader: bufio.NewReader(connection)}

		if line := client.readLine(t); line != "* OK fake IMAP server ready" {
			t.Fatalf("greeting = %q", line)
		}
		return client
	}

	connection := dialTCP(t, server.Addr())
	client := &imapClient{connection: connection, reader: bufio.NewReader(connection)}

	if line := client.readLine(t); line != "* OK fake IMAP server ready" {
		t.Fatalf("greeting = %q", line)
	}
	client.command(t, "a0 STARTTLS", "a0 OK Begin TLS negotiation")

	tlsConnection := tls.Client(connection, server.ClientTLSConfig())
	if err := tlsConnection.Handshake(); err != nil {
		t.Fatalf("STARTTLS handshake: %v", err)
	}
	client.connection = tlsConnection
	client.reader = bufio.NewReader(tlsConnection)

	return client
}

func (client *imapClient) command(t *testing.T, command, want string) {
	t.Helper()

	if _, err := fmt.Fprint(client.connection, command+"\r\n"); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}

	if line := client.readLine(t); line != want {
		t.Fatalf("response to %q = %q, want %q", command, line, want)
	}
}

func (client *imapClient) readLine(t *testing.T) string {
	t.Helper()

	line, err := client.reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read server response: %v", err)
	}

	return strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
}

func (client *imapClient) Close() error {
	return client.connection.Close()
}

func isTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

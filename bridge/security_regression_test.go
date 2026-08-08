package bridge_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestNewTLSConfigRejectsAmbiguousTrustAnchorFiles(t *testing.T) {
	t.Parallel()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	certificate := server.LeafPEM()
	for _, test := range []struct {
		name     string
		contents []byte
	}{
		{name: "multiple certificates", contents: append(append([]byte(nil), certificate...), certificate...)},
		{name: "leading junk", contents: append([]byte("not a certificate\n"), certificate...)},
		{name: "trailing non PEM data", contents: append(append([]byte(nil), certificate...), []byte("not a certificate")...)},
		{name: "oversized file", contents: append(append([]byte(nil), certificate...), []byte(strings.Repeat("x", 64*1024))...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := writeTrustAnchor(t, test.contents)
			if _, err := bridge.NewTLSConfig(bridge.TLSConfig{TrustAnchorFile: path}); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
				t.Fatalf("NewTLSConfig(%s) error = %v, want %q", test.name, err, bridge.CodeInvalidConfig)
			}
		})
	}

	if _, err := bridge.NewTLSConfig(bridge.TLSConfig{TrustAnchorFile: os.DevNull}); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("NewTLSConfig(non-regular file) error = %v, want %q", err, bridge.CodeInvalidConfig)
	}
}

func TestNewTLSConfigRejectsCertificateAuthorityTrustAnchor(t *testing.T) {
	t.Parallel()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	// A CA authorizes sibling leaves. Bridge exports one exact leaf certificate,
	// so accepting this CA would reintroduce a sibling-leaf impersonation path.
	path := writeTrustAnchor(t, server.CAPEM())
	if _, err := bridge.NewTLSConfig(bridge.TLSConfig{TrustAnchorFile: path}); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("NewTLSConfig(CA trust anchor) error = %v, want %q", err, bridge.CodeInvalidConfig)
	}
}

func TestValidateConfigRejectsDurationOverflow(t *testing.T) {
	t.Parallel()

	maxInt := int(^uint(0) >> 1)
	config := bridge.Config{IMAP: bridge.IMAPConfig{
		CredentialCommand: credentialHelper(t, "valid"),
		TLS:               bridge.TLSConfig{SPKISHA256: strings.Repeat("a", 64)},
		ConnectTimeoutMs:  maxInt,
	}}
	if _, err := bridge.ValidateConfig(config); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("ValidateConfig overflow timeout error = %v, want %q", err, bridge.CodeInvalidConfig)
	}
}

func TestDialBoundsUnauthenticatedStartTLSInput(t *testing.T) {
	for _, test := range []struct {
		name   string
		handle func(*bufio.Reader, net.Conn)
	}{
		{
			name: "unterminated greeting",
			handle: func(_ *bufio.Reader, connection net.Conn) {
				_, _ = fmt.Fprint(connection, strings.Repeat("x", 8192))
			},
		},
		{
			name: "unterminated response line",
			handle: func(reader *bufio.Reader, connection net.Conn) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				_, _ = fmt.Fprint(connection, "* "+strings.Repeat("x", 8192))
			},
		},
		{
			name: "response count flood",
			handle: func(reader *bufio.Reader, connection net.Conn) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				for range 128 {
					_, _ = fmt.Fprint(connection, "* OK untrusted response\r\n")
				}
			},
		},
		{
			name: "total response flood across commands",
			handle: func(reader *bufio.Reader, connection net.Conn) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				_, _ = fmt.Fprint(connection, "* CAPABILITY IMAP4rev1 STARTTLS\r\n")
				for range 8 {
					_, _ = fmt.Fprint(connection, "* "+strings.Repeat("x", 1800)+"\r\n")
				}
				_, _ = fmt.Fprint(connection, "a001 OK CAPABILITY completed\r\n")
				_, _ = reader.ReadString('\n')
				for range 2 {
					_, _ = fmt.Fprint(connection, "* "+strings.Repeat("x", 1800)+"\r\n")
				}
			},
		},
		{
			name: "total response flood",
			handle: func(reader *bufio.Reader, connection net.Conn) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				for range 16 {
					_, _ = fmt.Fprint(connection, "* "+strings.Repeat("x", 2048)+"\r\n")
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := startPlainIMAPServer(t, test.handle)
			config := plainServerConfig(t, listener.Addr().String())
			assertDialReturnsPromptly(t, config)
		})
	}
}

func TestDialRequiresStartTLSCapabilityToken(t *testing.T) {
	listener := startPlainIMAPServer(t, func(reader *bufio.Reader, connection net.Conn) {
		_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
		_, _ = reader.ReadString('\n')
		_, _ = fmt.Fprint(connection, "* CAPABILITY IMAP4rev1 XSTARTTLS\r\na001 OK CAPABILITY completed\r\n")
	})

	if _, err := bridge.Dial(context.Background(), plainServerConfig(t, listener.Addr().String())); bridge.CodeOf(err) != bridge.CodeTLSNegotiation {
		t.Fatalf("Dial XSTARTTLS error = %v, want %q", err, bridge.CodeTLSNegotiation)
	}
}

func TestDialCountsActualStartTLSWireBytes(t *testing.T) {
	listener := startPlainIMAPServer(t, func(reader *bufio.Reader, connection net.Conn) {
		_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
		_, _ = reader.ReadString('\n')

		for range 13 {
			_, _ = fmt.Fprintf(connection, "* %s\r\n", strings.Repeat("x", 1020))
		}
		_, _ = fmt.Fprintf(connection, "* CAPABILITY IMAP4rev1 STARTTLS %s\r\n", strings.Repeat("x", 989))
		_, _ = fmt.Fprintf(connection, "a001 OK %s\r\n", strings.Repeat("x", 1014))
		_, _ = reader.ReadString('\n')
		_, _ = fmt.Fprintf(connection, "a002 OK %s\r\n", strings.Repeat("x", 1014))
	})

	_, err := bridge.Dial(context.Background(), plainServerConfig(t, listener.Addr().String()))
	if bridge.CodeOf(err) != bridge.CodeBridgeUnreachable {
		t.Fatalf("Dial error after exact raw response budget = %v, want TLS handshake failure %q", err, bridge.CodeBridgeUnreachable)
	}
}

func TestDialStartTLSReadHonorsContextCancellation(t *testing.T) {
	accepted := make(chan struct{})
	listener := startPlainIMAPServer(t, func(_ *bufio.Reader, _ net.Conn) {
		close(accepted)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		_, err := bridge.Dial(ctx, plainServerConfig(t, listener.Addr().String()))
		result <- err
	}()

	select {
	case <-accepted:
		cancel()
	case <-time.After(time.Second):
		t.Fatal("server did not accept Dial connection")
	}

	select {
	case err := <-result:
		if bridge.CodeOf(err) != bridge.CodeBridgeUnreachable {
			t.Fatalf("canceled Dial error = %v, want %q", err, bridge.CodeBridgeUnreachable)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("canceled Dial remained blocked on plaintext greeting")
	}
}

func TestDialStartTLSCancellationClosesBlockedProtocolPhases(t *testing.T) {
	for _, test := range []struct {
		name   string
		handle func(*bufio.Reader, net.Conn, chan<- struct{})
	}{
		{
			name: "greeting",
			handle: func(_ *bufio.Reader, _ net.Conn, reached chan<- struct{}) {
				close(reached)
			},
		},
		{
			name: "CAPABILITY response",
			handle: func(reader *bufio.Reader, connection net.Conn, reached chan<- struct{}) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				close(reached)
			},
		},
		{
			name: "STARTTLS response",
			handle: func(reader *bufio.Reader, connection net.Conn, reached chan<- struct{}) {
				_, _ = fmt.Fprint(connection, "* OK fixture\r\n")
				_, _ = reader.ReadString('\n')
				_, _ = fmt.Fprint(connection, "* CAPABILITY IMAP4rev1 STARTTLS\r\na001 OK CAPABILITY completed\r\n")
				_, _ = reader.ReadString('\n')
				close(reached)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			reached := make(chan struct{})
			listener := startPlainIMAPServer(t, func(reader *bufio.Reader, connection net.Conn) {
				test.handle(reader, connection, reached)
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			result := make(chan error, 1)
			go func() {
				_, err := bridge.Dial(ctx, plainServerConfig(t, listener.Addr().String()))
				result <- err
			}()

			select {
			case <-reached:
				cancel()
			case <-time.After(time.Second):
				t.Fatal("server did not reach blocked protocol phase")
			}

			select {
			case err := <-result:
				if bridge.CodeOf(err) != bridge.CodeBridgeUnreachable {
					t.Fatalf("canceled Dial error = %v, want %q", err, bridge.CodeBridgeUnreachable)
				}
			case <-time.After(300 * time.Millisecond):
				t.Fatal("canceled Dial remained blocked")
			}
		})
	}
}

func TestPlainIMAPServerCleanupClosesAcceptedConnection(t *testing.T) {
	var client net.Conn
	t.Cleanup(func() {
		if client != nil {
			_ = client.Close()
		}
	})

	t.Cleanup(func() {
		if client == nil {
			return
		}

		_ = client.SetReadDeadline(time.Now().Add(time.Second))
		if _, err := client.Read(make([]byte, 1)); err == nil {
			t.Error("plaintext IMAP server connection remained open after cleanup")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Error("plaintext IMAP server cleanup did not close accepted connection")
		}
	})

	accepted := make(chan struct{})
	listener := startPlainIMAPServer(t, func(_ *bufio.Reader, _ net.Conn) {
		close(accepted)
	})

	connection, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("dial plaintext IMAP server: %v", err)
	}
	client = connection

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("plaintext IMAP server did not accept connection")
	}
}

func startPlainIMAPServer(t *testing.T, handle func(*bufio.Reader, net.Conn)) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	var connection net.Conn
	var connectionMu sync.Mutex
	closing := false
	finished := make(chan struct{})
	t.Cleanup(func() {
		_ = listener.Close()

		connectionMu.Lock()
		closing = true
		if connection != nil {
			_ = connection.Close()
		}
		connectionMu.Unlock()

		select {
		case <-finished:
		case <-time.After(time.Second):
			t.Error("plaintext IMAP server handler did not exit")
		}
	})

	go func() {
		defer close(finished)
		acceptedConnection, err := listener.Accept()
		if err != nil {
			return
		}

		connectionMu.Lock()
		connection = acceptedConnection
		if closing {
			_ = acceptedConnection.Close()
		}
		connectionMu.Unlock()

		defer acceptedConnection.Close()

		handle(bufio.NewReader(acceptedConnection), acceptedConnection)
		_, _ = acceptedConnection.Read(make([]byte, 1))
	}()

	return listener
}

func plainServerConfig(t *testing.T, address string) bridge.Config {
	t.Helper()

	return fakeServerConfig(t, address, bridge.TLSModeStartTLS, bridge.TLSConfig{SPKISHA256: strings.Repeat("a", 64)})
}

func assertDialReturnsPromptly(t *testing.T, config bridge.Config) {
	t.Helper()

	result := make(chan error, 1)
	go func() {
		_, err := bridge.Dial(context.Background(), config)
		result <- err
	}()

	select {
	case <-result:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("Dial remained blocked on untrusted STARTTLS input")
	}
}

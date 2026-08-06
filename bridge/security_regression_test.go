package bridge_test

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strings"
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
		CredentialCommand: []string{"/bin/true"},
		TLS:               bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)},
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

func startPlainIMAPServer(t *testing.T, handle func(*bufio.Reader, net.Conn)) net.Listener {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()

		handle(bufio.NewReader(connection), connection)
		_, _ = connection.Read(make([]byte, 1))
	}()

	return listener
}

func plainServerConfig(t *testing.T, address string) bridge.Config {
	t.Helper()

	return fakeServerConfig(t, address, bridge.TLSModeStartTLS, bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)})
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

package bridge

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestDialStartTLSHandoffSurvivesTimeoutContextCleanup(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.StartTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, port, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake server address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse fake server port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	config := Config{IMAP: IMAPConfig{
		Host:              host,
		Port:              number,
		TLSMode:           TLSModeStartTLS,
		CredentialCommand: []string{executable},
		TLS:               TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  1000,
	}}

	for range 100 {
		connection, err := Dial(context.Background(), config)
		if err != nil {
			t.Fatalf("Dial: %v", err)
		}

		if _, err := fmt.Fprint(connection.connection, "a003 NOOP\r\n"); err != nil {
			_ = connection.Close()
			t.Fatalf("write after Dial handoff: %v", err)
		}
		_ = connection.connection.SetReadDeadline(time.Now().Add(time.Second))
		response, err := bufio.NewReader(connection.connection).ReadString('\n')
		_ = connection.Close()
		if err != nil {
			t.Fatalf("read after Dial handoff: %v", err)
		}
		if response != "a003 BAD unsupported command\r\n" {
			t.Fatalf("NOOP response = %q", response)
		}
	}
}

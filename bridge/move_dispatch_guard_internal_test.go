package bridge

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// scriptedIMAP is a minimal in-memory IMAP peer that records every client
// line and answers each with a tagged OK.
type scriptedIMAP struct {
	server net.Conn
	mu     sync.Mutex
	lines  []string
	done   chan struct{}
}

func startScriptedIMAP(t *testing.T, capabilities string) (*imapSession, *scriptedIMAP) {
	t.Helper()
	client, server := net.Pipe()
	peer := &scriptedIMAP{server: server, done: make(chan struct{})}
	t.Cleanup(func() {
		_ = client.Close()
		_ = server.Close()
		<-peer.done
	})

	go func() {
		defer close(peer.done)
		if _, err := server.Write([]byte("* OK [CAPABILITY " + capabilities + "] synthetic\r\n")); err != nil {
			return
		}

		reader := bufio.NewReader(server)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			peer.mu.Lock()
			peer.lines = append(peer.lines, strings.TrimSuffix(line, "\r\n"))
			peer.mu.Unlock()

			tag, _, _ := strings.Cut(line, " ")
			if _, err := server.Write([]byte(tag + " OK done\r\n")); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	session, err := newIMAPSessionOver(ctx, client, false)
	if err != nil {
		t.Fatalf("newIMAPSessionOver: %v", err)
	}

	return session, peer
}

func (peer *scriptedIMAP) recorded() []string {
	peer.mu.Lock()
	defer peer.mu.Unlock()
	return append([]string(nil), peer.lines...)
}

// The capability check passes, then the server replaces the client's cached
// capabilities without MOVE before dispatch. go-imap now takes its
// COPY/STORE/EXPUNGE fallback; the guard must refuse it with nothing sent.
func TestDispatchMoveRefusesFallbackAfterCapabilityChange(t *testing.T) {
	session, peer := startScriptedIMAP(t, "IMAP4rev1 MOVE")
	if !session.SupportsMove() {
		t.Fatal("synthetic greeting did not advertise MOVE")
	}

	if _, err := peer.server.Write([]byte("* CAPABILITY IMAP4rev1\r\n")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for session.SupportsMove() {
		if time.Now().After(deadline) {
			t.Fatal("unsolicited CAPABILITY did not replace the cached capabilities")
		}
		time.Sleep(time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := session.dispatchMove(ctx, 101, "Folders/Existing")
	if CodeOf(err) != CodeUnsupported {
		t.Fatalf("dispatchMove after capability change = %v, want %s", err, CodeUnsupported)
	}

	_ = session.Abort()
	<-peer.done
	if lines := peer.recorded(); len(lines) != 0 {
		t.Fatalf("guard let %d command line(s) reach the server: %q", len(lines), lines)
	}
}

func TestDispatchMoveSendsOneNativeMove(t *testing.T) {
	session, peer := startScriptedIMAP(t, "IMAP4rev1 MOVE")

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := session.MoveUID(ctx, 101, "Folders/Existing"); err != nil {
		t.Fatalf("MoveUID: %v", err)
	}

	lines := peer.recorded()
	if len(lines) != 1 || !strings.HasSuffix(lines[0], ` UID MOVE 101 "Folders/Existing"`) {
		t.Fatalf("recorded %q, want one native UID MOVE", lines)
	}
}

func TestNativeMoveLineAllowsOnlyOneExactMove(t *testing.T) {
	prefix := "UID MOVE 101 "
	for _, test := range []struct {
		line string
		want bool
	}{
		{"T7 UID MOVE 101 \"Folders/Existing\"\r\n", true},
		{"T7 UID MOVE 101 INBOX\r\n", true},
		{"T7 UID COPY 101 \"Folders/Existing\"\r\n", false},
		{"T7 UID STORE 101 +FLAGS.SILENT (\\Deleted)\r\n", false},
		{"T7 EXPUNGE\r\n", false},
		{"T7 CAPABILITY\r\n", false},
		{"T7 UID MOVE 1010 \"Folders/Existing\"\r\n", false},
		{"T7 UID MOVE 101:102 \"Folders/Existing\"\r\n", false},
		{"T7 UID MOVE 101 \r\n", false},
		{"T7 UID MOVE 101 \"Folders/Existing\"", false},
		{"T7 UID MOVE 101 \"A\"\r\nT8 EXPUNGE\r\n", false},
		{"T7 UID MOVE 101 {3}\r\n", false},
		{"T7 UID MOVE 101 {3+}\r\nabc\r\n", false},
		{" UID MOVE 101 \"Folders/Existing\"\r\n", false},
	} {
		if got := nativeMoveLine([]byte(test.line), prefix); got != test.want {
			t.Errorf("nativeMoveLine(%q) = %t, want %t", test.line, got, test.want)
		}
	}
}

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
	mu    sync.Mutex
	lines []string
	done  chan struct{}
}

func startScriptedIMAP(t *testing.T, capabilities string) (*imapSession, *scriptedIMAP) {
	t.Helper()
	client, server := net.Pipe()
	peer := &scriptedIMAP{done: make(chan struct{})}
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

func TestMoveUIDSendsOneNativeMove(t *testing.T) {
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

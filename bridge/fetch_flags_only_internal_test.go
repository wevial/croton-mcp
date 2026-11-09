package bridge

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	flagsOnlyUID    = 7
	flagsOnlyHeader = "Subject: Synthetic\r\nFrom: sender@example.test\r\n\r\n"
	flagsOnlyBody   = "hello"
)

// flagsOnlyPeer answers every UID FETCH with the real response for its items.
// The first racedFetches FETCHes are preceded by an unsolicited flag update
// for the same UID, which go-imap routes to the pending command in place of
// the real response.
func flagsOnlyPeer(t *testing.T, racedFetches int) (*imapSession, *atomic.Int32) {
	t.Helper()

	client, server := net.Pipe()
	t.Cleanup(func() { _ = client.Close(); _ = server.Close() })
	fetches := &atomic.Int32{}

	go func() {
		writer := bufio.NewWriter(server)
		send := func(lines ...string) {
			for _, line := range lines {
				_, _ = writer.WriteString(line)
			}
			_ = writer.Flush()
		}

		send("* OK [CAPABILITY IMAP4rev1] ready\r\n")
		reader := bufio.NewReader(server)
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				return
			}
			tag, command, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
			if !strings.HasPrefix(command, "UID FETCH ") {
				send(tag + " BAD unexpected\r\n")
				continue
			}

			if int(fetches.Add(1)) <= racedFetches {
				send(fmt.Sprintf("* 1 FETCH (UID %d FLAGS (\\Seen))\r\n", flagsOnlyUID))
			}
			send(flagsOnlyReply(command), tag+" OK FETCH completed\r\n")
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	session, err := newIMAPSessionOver(ctx, client, false)
	if err != nil {
		t.Fatalf("newIMAPSessionOver: %v", err)
	}

	return session, fetches
}

func flagsOnlyReply(command string) string {
	switch {
	case strings.Contains(command, "BODYSTRUCTURE"):
		return fmt.Sprintf("* 1 FETCH (UID %d BODYSTRUCTURE (\"text\" \"plain\" (\"charset\" \"us-ascii\") NIL NIL \"7bit\" %d 1 NIL NIL NIL NIL))\r\n", flagsOnlyUID, len(flagsOnlyBody))
	case strings.Contains(command, "BODY.PEEK[HEADER]"):
		return fmt.Sprintf("* 1 FETCH (UID %d FLAGS () RFC822.SIZE %d BODY[HEADER] {%d}\r\n%s)\r\n", flagsOnlyUID, len(flagsOnlyHeader)+len(flagsOnlyBody), len(flagsOnlyHeader), flagsOnlyHeader)
	default:
		return fmt.Sprintf("* 1 FETCH (UID %d BODY[]<0> {%d}\r\n%s)\r\n", flagsOnlyUID, len(flagsOnlyBody), flagsOnlyBody)
	}
}

func TestFetchRetriesOnceAfterFlagsOnlyReply(t *testing.T) {
	fetches := []struct {
		name  string
		fetch func(context.Context, *imapSession) (string, error)
		want  string
	}{
		{
			name: "body",
			fetch: func(ctx context.Context, session *imapSession) (string, error) {
				body, err := session.UIDFetchBody(ctx, flagsOnlyUID, 64)
				return string(body), err
			},
			want: flagsOnlyBody,
		},
		{
			name: "headers",
			fetch: func(ctx context.Context, session *imapSession) (string, error) {
				headers, err := session.UIDFetchHeaders(ctx, flagsOnlyUID, 1024)
				return fmt.Sprintf("%s %v %d", headers.Metadata.Subject, headers.Metadata.ReadState, headers.Metadata.Size), err
			},
			want: fmt.Sprintf("Synthetic %v %d", ReadStateUnread, len(flagsOnlyHeader)+len(flagsOnlyBody)),
		},
		{
			name: "metadata",
			fetch: func(ctx context.Context, session *imapSession) (string, error) {
				items, err := session.UIDFetchMetadata(ctx, "INBOX", []uint32{flagsOnlyUID}, 1024)
				if len(items) != 1 {
					return fmt.Sprintf("%d items", len(items)), err
				}
				return fmt.Sprintf("%d %s %v", items[0].uid, items[0].Subject, items[0].ReadState), err
			},
			want: fmt.Sprintf("%d Synthetic %v", flagsOnlyUID, ReadStateUnread),
		},
		{
			name: "structure",
			fetch: func(ctx context.Context, session *imapSession) (string, error) {
				structure, err := session.fetchUIDStructure(ctx, flagsOnlyUID)
				if structure == nil {
					return "", err
				}
				return structure.MediaType(), err
			},
			want: "text/plain",
		},
	}

	for _, test := range fetches {
		t.Run(test.name+"/raced_once", func(t *testing.T) {
			session, sent := flagsOnlyPeer(t, 1)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := test.fetch(ctx, session)

			if err != nil || got != test.want {
				t.Fatalf("fetch = %q, %v; want %q", got, err, test.want)
			}
			if sent.Load() != 2 {
				t.Fatalf("peer received %d FETCH commands, want 2", sent.Load())
			}
		})

		t.Run(test.name+"/raced_twice", func(t *testing.T) {
			session, sent := flagsOnlyPeer(t, 2)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			got, err := test.fetch(ctx, session)

			if CodeOf(err) != CodeIMAPProtocol {
				t.Fatalf("fetch = %q, %v; want %q", got, err, CodeIMAPProtocol)
			}
			if (&Adapter{}).canReplay(ctx, err) {
				t.Fatalf("fetch error %v permits transport replay", err)
			}
			if sent.Load() != 2 {
				t.Fatalf("peer received %d FETCH commands, want 2", sent.Load())
			}
		})
	}
}

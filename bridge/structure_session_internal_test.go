package bridge

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	imap "github.com/emersion/go-imap/v2"
)

const structureSessionTarget = "(\"TEXT\" \"PLAIN\" (\"CHARSET\" \"utf-8\") NIL NIL \"7BIT\" 12 1 NIL NIL NIL NIL)"

// structureSessionRun is one completed dedicated session over a pipe.
type structureSessionRun struct {
	structure    imap.BodyStructure
	snapshot     mailboxSnapshot
	record       structurePeerRecord
	tap          []byte
	sessionBytes int
	responses    int
	work         int
}

// runStructureSession drives the dedicated constructor and its whole
// lifetime against the scripted peer and checks the lifetime invariants
// every run shares.
func runStructureSession(t *testing.T, startTLS bool, script structurePeerScript) structureSessionRun {
	t.Helper()

	client, server := net.Pipe()
	script.greeting = !startTLS
	peer := newStructurePeer(script)
	go peer.serve(server)
	t.Cleanup(func() { _ = client.Close() })

	ctx := deadlineContext(t)
	seam, installed := newStructureTapSeam()
	session, err := newStructureSessionOver(ctx, client, startTLS, structurePeerUID, seam)
	if err != nil {
		t.Fatalf("newStructureSessionOver() error = %v", err)
	}

	// The quarantine is the reader directly above the transport, below the
	// tap, the budget layer and any synthetic greeting, before any command.
	reader, ok := installed().Conn.(*quarantineConn)
	if !ok || reader.quarantine != session.quarantine || reader.Conn != net.Conn(session.session.conn) || session.session.conn.Conn != client {
		t.Fatal("dedicated stack does not place the quarantine directly on the transport")
	}
	if session.session.budget.Conn != net.Conn(installed()) || session.session.guard != nil {
		t.Fatal("dedicated stack does not read the client through the quarantine only")
	}
	if dispatch := quarantineDispatchState(session.quarantine); dispatch != quarantineIdle {
		t.Fatalf("dispatch after construction = %d", dispatch)
	}

	if err := session.Authenticate(ctx, Credentials{Username: []byte("fixture-user"), Password: []byte("fixture-password")}); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	snapshot, err := session.Select(ctx, "INBOX")
	if err != nil {
		t.Fatalf("Select() error = %v", err)
	}
	if dispatch := quarantineDispatchState(session.quarantine); dispatch != quarantineIdle {
		t.Fatalf("dispatch before FetchStructure = %d", dispatch)
	}

	structure, err := session.FetchStructure(ctx)
	if err != nil {
		t.Fatalf("FetchStructure() error = %v", err)
	}
	if dispatch := quarantineDispatchState(session.quarantine); dispatch != quarantineDelivered {
		t.Fatalf("dispatch after FetchStructure = %d", dispatch)
	}

	// No second dispatch and no reselection: the calls are refused before
	// any write and leave the quarantine unrefused.
	commands := len(peer.snapshot().commands)
	if _, err := session.FetchStructure(ctx); CodeOf(err) != CodeIMAPProtocol {
		t.Fatalf("second FetchStructure() error = %v", err)
	}
	if _, err := session.Select(ctx, "INBOX"); CodeOf(err) != CodeIMAPProtocol {
		t.Fatalf("second Select() error = %v", err)
	}
	if got := len(peer.snapshot().commands); got != commands {
		t.Fatalf("refused calls wrote %d commands", got-commands)
	}
	if err := session.quarantine.terminalError(); err != nil {
		t.Fatalf("quarantine terminal = %v", err)
	}

	if err := session.Close(ctx); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	peer.wait(t)
	if err := session.Close(ctx); err != nil {
		t.Fatalf("repeated Close() error = %v", err)
	}
	if _, err := session.FetchStructure(ctx); CodeOf(err) != CodeIMAPProtocol {
		t.Fatalf("FetchStructure() after Close error = %v", err)
	}

	record := peer.snapshot()
	if record.err != nil {
		t.Fatalf("peer error = %v", record.err)
	}
	assertStructureCommands(t, record.commands, structurePeerUID)

	session.quarantine.mu.Lock()
	run := structureSessionRun{
		structure:    structure,
		snapshot:     snapshot,
		record:       record,
		tap:          installed().bytes(),
		sessionBytes: session.quarantine.sessionBytes,
		responses:    session.quarantine.responses,
		work:         session.quarantine.work,
	}
	session.quarantine.mu.Unlock()

	// Every server byte crossed the quarantine and only approved responses
	// reached the decoder side; the synthetic greeting crossed neither.
	if run.sessionBytes != record.written || run.responses != record.responses {
		t.Fatalf("quarantine counted %d bytes/%d responses, peer sent %d/%d", run.sessionBytes, run.responses, record.written, record.responses)
	}
	if !bytes.Equal(run.tap, record.approved) {
		t.Fatalf("decoder-facing tap = %q, want approved %q", run.tap, record.approved)
	}
	if bytes.Contains(run.tap, []byte(syntheticStartTLSGreeting)) {
		t.Fatal("synthetic greeting crossed the quarantine")
	}
	if snapshot != (mailboxSnapshot{UIDNext: 104, UIDValidity: 9001}) {
		t.Fatalf("snapshot = %+v", snapshot)
	}

	return run
}

func quarantineDispatchState(quarantine *structureQuarantine) quarantineDispatch {
	quarantine.mu.Lock()
	defer quarantine.mu.Unlock()

	return quarantine.dispatch
}

// assertStructureCommands requires a read-only transcript with exactly one
// FETCH, for UID and extended BODYSTRUCTURE of the selected UID only.
func assertStructureCommands(t *testing.T, commands []string, uid uint32) {
	t.Helper()

	allowed := map[string]bool{"CAPABILITY": true, "LOGIN": true, "NOOP": true, "EXAMINE": true, "UID SEARCH": true, "UID FETCH": true, "LOGOUT": true}
	fetches := 0
	for _, command := range commands {
		_, rest, _ := strings.Cut(command, " ")
		fields := strings.Fields(strings.ToUpper(rest))
		name := fields[0]
		if name == "UID" && len(fields) > 1 {
			name += " " + fields[1]
		}
		if !allowed[name] {
			t.Fatalf("command %q is outside the dedicated session", command)
		}
		if name != "UID FETCH" {
			continue
		}

		fetches++
		if strings.ToUpper(rest) != fmt.Sprintf("UID FETCH %d (UID BODYSTRUCTURE)", uid) {
			t.Fatalf("FETCH %q is not the exact structure dispatch", command)
		}
	}
	if fetches != 1 || commands[len(commands)-1] == "" || !strings.HasSuffix(strings.ToUpper(commands[len(commands)-1]), " LOGOUT") {
		t.Fatalf("transcript %q needs one structure FETCH and a final LOGOUT", commands)
	}
}

func TestStructureSessionLifetime(t *testing.T) {
	target := "* 1 FETCH (UID 7 BODYSTRUCTURE " + structureSessionTarget + ")\r\n"

	t.Run("dedicated_lifetime", func(t *testing.T) {
		for _, transport := range []struct {
			name     string
			startTLS bool
		}{
			{name: "synthetic_starttls_greeting", startTLS: true},
			{name: "server_greeting", startTLS: false},
		} {
			t.Run(transport.name, func(t *testing.T) {
				run := runStructureSession(t, transport.startTLS, structurePeerScript{structure: target})

				single, ok := run.structure.(*imap.BodyStructureSinglePart)
				if !ok || single.MediaType() != "text/plain" || single.Params["charset"] != "utf-8" || single.Size != 12 || single.Extended == nil {
					t.Fatalf("structure = %#v", run.structure)
				}
				// The server greeting, when there is one, is the first byte
				// the quarantine framed.
				if !transport.startTLS && !bytes.HasPrefix(run.tap, []byte("* OK [CAPABILITY ")) {
					t.Fatalf("tap does not start with the server greeting: %q", run.tap)
				}
			})
		}

		// The dedicated constructor needs one finite operation deadline and a
		// positive UID, and closes the transport it refuses.
		for _, refused := range []struct {
			name string
			ctx  context.Context
			uid  uint32
		}{
			{name: "no_deadline", ctx: context.Background(), uid: structurePeerUID},
			{name: "zero_uid", ctx: deadlineContext(t), uid: 0},
		} {
			client, server := net.Pipe()
			if _, err := newStructureSessionOver(refused.ctx, client, false, refused.uid, nil); CodeOf(err) != CodeInvalidRequest {
				t.Fatalf("%s: newStructureSessionOver() error = %v", refused.name, err)
			}
			_ = server.SetReadDeadline(time.Now().Add(structurePeerTimeout))
			if _, err := server.Read(make([]byte, 1)); err == nil {
				t.Fatalf("%s: refused transport stayed open", refused.name)
			}
		}

		// The pooled constructor keeps its own stack without a quarantine.
		client, server := net.Pipe()
		peer := newStructurePeer(structurePeerScript{greeting: true})
		go peer.serve(server)
		ctx := deadlineContext(t)
		pooled, err := newIMAPSessionOver(ctx, client, false)
		if err != nil {
			t.Fatalf("newIMAPSessionOver() error = %v", err)
		}
		if pooled.budget.Conn != net.Conn(pooled.guard) || pooled.guard.Conn != net.Conn(pooled.conn) || pooled.conn.Conn != client {
			t.Fatal("pooled constructor stack changed")
		}
		if err := pooled.Logout(ctx); err != nil {
			t.Fatalf("pooled Logout() error = %v", err)
		}
		peer.wait(t)
	})

	t.Run("single_dispatch_and_flags", func(t *testing.T) {
		baseline := runStructureSession(t, true, structurePeerScript{structure: target})

		beforeSearch := []string{
			"* 1 FETCH (FLAGS (\\Seen))\r\n",
			"* 2 FETCH (UID 8 FLAGS ())\r\n",
			"* 1 FETCH (FLAGS (\\Flagged) UID 7)\r\n",
		}
		duringFetch := []string{
			"* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n",
			"* 2 FETCH (FLAGS (\\Answered) UID 8)\r\n",
			"* 3 FETCH (FLAGS ())\r\n",
			"* 1 FETCH (UID 7 FLAGS (\\Seen \\Flagged))\r\n",
		}
		orders := []struct {
			name   string
			target string
		}{
			{name: "uid_first", target: "* 1 FETCH (UID 7 FLAGS (\\Seen) BODYSTRUCTURE " + structureSessionTarget + ")\r\n"},
			{name: "uid_last", target: "* 1 FETCH (FLAGS () BODYSTRUCTURE " + structureSessionTarget + " UID 7)\r\n"},
		}

		for _, order := range orders {
			t.Run(order.name, func(t *testing.T) {
				run := runStructureSession(t, true, structurePeerScript{beforeSearch: beforeSearch, duringFetch: duringFetch, structure: order.target})

				// The structure still arrived exactly once, unchanged by the
				// target-UID FLAGS-only frames before and during dispatch.
				if !reflect.DeepEqual(run.structure, baseline.structure) {
					t.Fatalf("structure = %#v, want %#v", run.structure, baseline.structure)
				}
				if count := bytes.Count(run.tap, []byte("BODYSTRUCTURE")); count != 1 {
					t.Fatalf("tap carries %d structures", count)
				}

				// Each FLAGS-only frame was consumed whole: none of its bytes
				// reached the decoder side, and all of it counted.
				consumed := append(append([]string(nil), beforeSearch...), duringFetch...)
				if !reflect.DeepEqual(run.record.consumed, consumed) {
					t.Fatalf("peer consumed frames = %q", run.record.consumed)
				}
				consumedBytes, consumedTokens := 0, 0
				for _, frame := range consumed {
					if bytes.Contains(run.tap, []byte(frame)) {
						t.Fatalf("consumed frame %q reached the decoder side", frame)
					}
					consumedBytes += len(frame)
					consumedTokens += flatFetchTokens(frame)
				}
				targetBytes := len(order.target) - len(target)
				targetTokens := flatFetchTokens(order.target) - flatFetchTokens(target)
				if got := run.sessionBytes - baseline.sessionBytes; got != consumedBytes+targetBytes {
					t.Fatalf("session bytes grew by %d, want %d", got, consumedBytes+targetBytes)
				}
				if got := run.responses - baseline.responses; got != len(consumed) {
					t.Fatalf("session responses grew by %d, want %d", got, len(consumed))
				}
				if got := run.work - baseline.work; got != consumedTokens+targetTokens {
					t.Fatalf("session work grew by %d, want %d", got, consumedTokens+targetTokens)
				}
			})
		}
	})
}

func deadlineContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), structurePeerTimeout)
	t.Cleanup(cancel)
	return ctx
}

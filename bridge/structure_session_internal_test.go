package bridge

import (
	"bytes"
	"context"
	"errors"
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
	operation, bounded := session.session.conn.Conn.(*operationConn)
	if !ok || reader.quarantine != session.quarantine || reader.Conn != net.Conn(session.session.conn) || !bounded || operation.Conn != client {
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
	snapshot, err := session.Examine(ctx, "INBOX")
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
	if _, err := session.Examine(ctx, "INBOX"); CodeOf(err) != CodeIMAPProtocol {
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

				// Each FLAGS-only frame was consumed whole and all of it counted.
				consumed := append(append([]string(nil), beforeSearch...), duringFetch...)
				if !reflect.DeepEqual(run.record.consumed, consumed) {
					t.Fatalf("peer consumed frames = %q", run.record.consumed)
				}
				consumedBytes, consumedTokens := 0, 0
				for _, frame := range consumed {
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

// pendingHarness drives one dedicated session over a pipe whose UID FETCH
// reply is a gated schedule, with taps on both sides of the quarantine.
type pendingHarness struct {
	t         *testing.T
	ctx       context.Context
	cancel    context.CancelFunc
	session   *structureSession
	peer      *structurePeer
	tap       *structureTap
	source    *structureSourceTap
	gates     []chan struct{}
	writes    [][]structurePeerPart
	commanded chan struct{}
	fetched   chan fetchOutcome

	// written and approved are what the peer sent before each released
	// gate, independently of the quarantine.
	written  int
	approved []byte
}

type fetchOutcome struct {
	structure bodyStructure
	err       error
}

// startPending selects the target and waits until the decoder-facing Read
// and the quarantine's source Read are both outstanding before the target
// dispatch, then dispatches it.
func startPending(t *testing.T, writes [][]structurePeerPart, complete bool) *pendingHarness {
	t.Helper()

	harness := &pendingHarness{t: t, writes: writes, commanded: make(chan struct{}), fetched: make(chan fetchOutcome, 1)}
	schedule := make([]structurePeerWrite, len(writes))
	for index, parts := range writes {
		gate := make(chan struct{})
		harness.gates = append(harness.gates, gate)
		schedule[index] = structurePeerWrite{gate: gate, parts: parts}
	}
	adverse := &structurePeerAdverse{complete: complete, commanded: harness.commanded, schedule: func(structurePeerRecord) []structurePeerWrite { return schedule }}

	client, server := net.Pipe()
	harness.source = &structureSourceTap{Conn: client}
	harness.peer = newStructurePeer(structurePeerScript{greeting: true, adverse: adverse})
	go harness.peer.serve(server)
	harness.ctx, harness.cancel = context.WithTimeout(context.Background(), structurePeerTimeout)
	t.Cleanup(func() {
		harness.cancel()
		_ = client.Close()
		for _, gate := range harness.gates {
			select {
			case <-gate:
			default:
				close(gate)
			}
		}
	})

	seam, installed := newStructureTapSeam()
	session, err := newStructureSessionOver(harness.ctx, harness.source, false, structurePeerUID, seam)
	if err != nil {
		t.Fatalf("newStructureSessionOver() error = %v", err)
	}
	harness.session, harness.tap = session, installed()
	if err := session.Authenticate(harness.ctx, Credentials{Username: []byte("fixture-user"), Password: []byte("fixture-password")}); err != nil {
		t.Fatalf("Authenticate() error = %v", err)
	}
	if _, err := session.Examine(harness.ctx, "INBOX"); err != nil {
		t.Fatalf("Examine() error = %v", err)
	}

	harness.peer.awaitCommand(t)
	record := harness.peer.snapshot()
	harness.written, harness.approved = record.written, record.approved
	harness.settle()
	if dispatch := quarantineDispatchState(session.quarantine); dispatch != quarantineIdle {
		t.Fatalf("dispatch before FetchStructure = %d", dispatch)
	}

	// The Read that will receive the target reply predates its dispatch.
	go func() {
		structure, err := session.FetchStructure(harness.ctx)
		harness.fetched <- fetchOutcome{structure, err}
	}()
	<-harness.commanded
	return harness
}

// settle waits until the guard consumed every byte the peer wrote and the
// decoder holds exactly the complete approved lines, with both Reads
// outstanding. An approved control the peer has only partly written is
// withheld.
func (harness *pendingHarness) settle() {
	harness.t.Helper()

	harness.source.await(harness.t, "guard input and a pending source Read", func() bool {
		return len(harness.source.consumed) == harness.written && harness.source.pending > 0
	})
	complete := harness.approved[:bytes.LastIndex(harness.approved, []byte("\r\n"))+2]
	harness.tap.awaitBytes(harness.t, complete)
}

// release opens the gate of one scheduled write and settles on it.
func (harness *pendingHarness) release(step int) {
	harness.t.Helper()

	for _, part := range harness.writes[step] {
		harness.written += len(part.text)
	}
	harness.releaseOnly(step)
	harness.settle()
}

// releaseOnly opens a gate without waiting for the guard to settle, for a
// write the guard is expected to refuse.
func (harness *pendingHarness) releaseOnly(step int) {
	for _, part := range harness.writes[step] {
		if part.kind == peerApproved {
			harness.approved = append(harness.approved, part.text...)
		}
	}
	close(harness.gates[step])
}

func (harness *pendingHarness) outcome() fetchOutcome {
	harness.t.Helper()

	select {
	case got := <-harness.fetched:
		return got
	case <-time.After(structurePeerTimeout):
		harness.t.Fatal("FetchStructure did not return")
		return fetchOutcome{}
	}
}

// refusedWith requires FetchStructure to fail with code only, the quarantine
// to hold that category, the decoder to hold only the approved bytes, and
// the peer to observe an abort without drain, logout or replay.
func (harness *pendingHarness) refusedWith(code string, trailing string) {
	harness.t.Helper()
	t := harness.t

	got := harness.outcome()
	if got.structure != nil || CodeOf(got.err) != code || got.err.Error() != code {
		t.Fatalf("FetchStructure() = %v, %v; want %s", got.structure, got.err, code)
	}
	harness.session.Abort(got.err)
	harness.peer.wait(t)

	if err := harness.session.quarantine.terminalError(); CodeOf(err) != code {
		t.Fatalf("quarantine terminal = %v, want %s", err, code)
	}
	if tap := harness.tap.bytes(); !bytes.Equal(tap, harness.approved) {
		t.Fatalf("decoder-facing tap = %q, want approved %q", tap, harness.approved)
	}
	record := harness.peer.snapshot()
	if record.err != nil || !record.closed || string(record.trailing) != trailing {
		t.Fatalf("peer err = %v, closed = %t, trailing = %q", record.err, record.closed, record.trailing)
	}
}

// withinTimeout fails instead of hanging when call blocks.
func withinTimeout(t *testing.T, what string, call func() error) error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(structurePeerTimeout):
		t.Fatalf("%s blocked behind the pending Read", what)
		return nil
	}
}

func TestStructureSessionRefusal(t *testing.T) {
	const second = "* OK [ALERT] second note\r\n"
	const refused = "* 1 FETCH (UID 7 BODY[] {6}\r\nsecret BODYSTRUCTURE " + structureSessionTarget + ")\r\n"
	target := "* 1 FETCH (UID 7 FLAGS (\\Seen) BODYSTRUCTURE " + structureSessionTarget + ")\r\n"

	t.Run("pending_read", func(t *testing.T) {
		// A control fragmented across writes is withheld until it is
		// complete; coalesced with a refused prefix it is released alone.
		// Write and Close of the transport and inspection of the reader
		// state complete while both Reads are outstanding.
		t.Run("write", func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{
				{{kind: peerConsumed, text: structureFlagsFrame}, {kind: peerApproved, text: structureNoteControl[:9]}},
				{{kind: peerApproved, text: structureNoteControl[9:]}, {kind: peerRefused, text: refused[:12]}},
				{{kind: peerRefused, text: refused[12:]}},
			}, false)

			harness.release(0)
			harness.release(1)
			if err := withinTimeout(t, "terminal state", func() error { return harness.session.quarantine.terminalError() }); err != nil {
				t.Fatalf("quarantine terminal = %v before the refusal", err)
			}
			if err := withinTimeout(t, "Write", func() error {
				_, err := harness.tap.Write([]byte("X1 NOOP\r\n"))
				return err
			}); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			harness.releaseOnly(2)

			harness.refusedWith(CodeIMAPProtocol, "X1 NOOP\r\n")
		})

		t.Run("cancel", func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{
				{{kind: peerApproved, text: structureNoteControl}, {kind: peerRefused, text: structureIncompleteFetch}},
			}, false)

			harness.release(0)
			harness.cancel()

			harness.refusedWith(CodeOperationCanceled, "")
		})

		t.Run("close", func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{
				{{kind: peerApproved, text: structureNoteControl}, {kind: peerRefused, text: structureIncompleteFetch}},
			}, false)

			harness.release(0)
			if err := withinTimeout(t, "Close", harness.tap.Close); err != nil {
				t.Fatalf("Close() error = %v", err)
			}

			harness.refusedWith(CodeIMAPProtocol, "")
		})

		// The source returns an approved control and an incomplete FETCH
		// together with an error: the control is released, the FETCH bytes
		// are not.
		t.Run("bytes_with_error", func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{
				{{kind: peerApproved, text: structureNoteControl}},
				{{kind: peerApproved, text: second}, {kind: peerRefused, text: structureIncompleteFetch}},
			}, false)

			harness.release(0)
			harness.source.update(func() { harness.source.fail = errors.New("peer secret text") })
			harness.releaseOnly(1)

			harness.refusedWith(CodeIMAPProtocol, "")
		})

		// The Read outstanding since before the dispatch still delivers the
		// target fragmented after an approved control, exactly once.
		t.Run("epoch_release", func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{
				{{kind: peerConsumed, text: structureFlagsFrame}, {kind: peerApproved, text: structureNoteControl}},
				{{kind: peerApproved, text: target[:20]}},
				{{kind: peerApproved, text: target[20:]}},
			}, true)

			harness.release(0)
			harness.releaseOnly(1)
			harness.source.await(t, "fragmented target prefix", func() bool {
				return len(harness.source.consumed) == harness.written+20
			})
			if tap := harness.tap.bytes(); !bytes.Equal(tap, harness.approved[:len(harness.approved)-20]) {
				t.Fatalf("decoder-facing tap holds an unfinished target: %q", tap)
			}
			harness.releaseOnly(2)

			got := harness.outcome()
			if got.err != nil || got.structure == nil {
				t.Fatalf("FetchStructure() = %v, %v", got.structure, got.err)
			}
			if err := harness.session.Close(harness.ctx); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			harness.peer.wait(t)
			record := harness.peer.snapshot()
			if !bytes.Equal(harness.tap.bytes(), record.approved) || record.err != nil {
				t.Fatalf("decoder-facing tap differs from approved responses, peer err = %v", record.err)
			}
		})
	})

	t.Run("terminal_state", func(t *testing.T) {
		root := t.TempDir()
		for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
			t.Setenv(name, root)
		}

		// A bounds refusal is held at the decoder boundary while the
		// operation is canceled; the refusal category survives cleanup.
		t.Run("refusal_then_cancellation", func(t *testing.T) {
			peer := startStructureTLSPeer(t, false, structurePeerScript{adverse: &structurePeerAdverse{schedule: func(structurePeerRecord) []structurePeerWrite {
				return []structurePeerWrite{{parts: []structurePeerPart{{kind: peerApproved, text: structureNoteControl}, {kind: peerRefused, text: nestedStructure(depthLimit - 1)}}}}
			}}})
			config := structureTLSConfig(t, peer.listener.Addr().String(), TLSModeImplicit, peer.pin)
			hold, held := make(chan struct{}), make(chan struct{})
			seam, installed := newHeldStructureTapSeam(hold, held)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()

			done := make(chan error, 1)
			go func() {
				_, err := readSelectedStructure(ctx, config, "INBOX", structurePeerUID, seam)
				done <- err
			}()
			select {
			case <-held:
			case <-time.After(structurePeerTimeout):
				t.Fatal("refusal did not reach the decoder boundary")
			}
			cancel()
			close(hold)
			err := <-done
			peer.wait(t)

			if CodeOf(err) != CodeBoundsExceeded || err.Error() != CodeBoundsExceeded {
				t.Fatalf("readSelectedStructure() error = %v, want %s", err, CodeBoundsExceeded)
			}
			quarantine := installed().quarantine(t)
			if terminal := quarantine.terminalError(); CodeOf(terminal) != CodeBoundsExceeded {
				t.Fatalf("quarantine terminal = %v", terminal)
			}
			if count, readErr := quarantine.Read(make([]byte, 64)); count != 0 || CodeOf(readErr) != CodeBoundsExceeded {
				t.Fatalf("Read() after refusal = %d, %v", count, readErr)
			}
			record := peer.snapshot()
			if !bytes.Equal(installed().bytes(), record.approved) || !record.closed || len(record.trailing) != 0 {
				t.Fatalf("tap/peer after refusal: closed = %t, trailing = %q", record.closed, record.trailing)
			}

			// The refused session is never reused: the same endpoint has no
			// second connection to give, and a fresh endpoint gets a fresh
			// guarded connection with its own quarantine.
			againSeam, again := newStructureTapSeam()
			if _, err := readSelectedStructure(context.Background(), config, "INBOX", structurePeerUID, againSeam); CodeOf(err) != CodeBridgeUnreachable || again() != nil {
				t.Fatalf("second read over the refused endpoint = %v", err)
			}
			fresh := startStructureTLSPeer(t, false, structurePeerScript{structure: target})
			freshSeam, freshTap := newStructureTapSeam()
			if _, err := readSelectedStructure(context.Background(), structureTLSConfig(t, fresh.listener.Addr().String(), TLSModeImplicit, fresh.pin), "INBOX", structurePeerUID, freshSeam); err != nil {
				t.Fatalf("fresh read error = %v", err)
			}
			if freshTap().quarantine(t) == quarantine {
				t.Fatal("fresh read reused the refused quarantine")
			}
		})

		// Abort with the already mapped cancellation category, as the
		// operation passes it, ends an unrefused reader with that category.
		t.Run("abort_"+CodeOperationCanceled, func(t *testing.T) {
			harness := startPending(t, [][]structurePeerPart{{{kind: peerApproved, text: structureNoteControl}}}, false)

			harness.session.Abort(errorCode(CodeOperationCanceled))

			got := harness.outcome()
			if got.err == nil {
				t.Fatal("FetchStructure() succeeded after Abort")
			}
			harness.peer.wait(t)
			if err := harness.session.quarantine.terminalError(); CodeOf(err) != CodeOperationCanceled {
				t.Fatalf("quarantine terminal = %v, want %s", err, CodeOperationCanceled)
			}
			if record := harness.peer.snapshot(); !record.closed || len(record.trailing) != 0 {
				t.Fatalf("peer closed = %t, trailing = %q", record.closed, record.trailing)
			}
		})

		// The whole-operation deadline bounds the transport from
		// construction through close; no stage resets it to none.
		t.Run("operation_deadline", func(t *testing.T) {
			client, server := net.Pipe()
			source := &structureSourceTap{Conn: client}
			peer := newStructurePeer(structurePeerScript{greeting: true, structure: target})
			go peer.serve(server)
			t.Cleanup(func() { _ = client.Close() })
			ctx := deadlineContext(t)
			deadline, _ := ctx.Deadline()

			session, err := newStructureSessionOver(ctx, source, false, structurePeerUID, nil)
			if err != nil {
				t.Fatalf("newStructureSessionOver() error = %v", err)
			}
			if err := session.Authenticate(ctx, Credentials{Username: []byte("fixture-user"), Password: []byte("fixture-password")}); err != nil {
				t.Fatalf("Authenticate() error = %v", err)
			}
			if _, err := session.Examine(ctx, "INBOX"); err != nil {
				t.Fatalf("Examine() error = %v", err)
			}
			if _, err := session.FetchStructure(ctx); err != nil {
				t.Fatalf("FetchStructure() error = %v", err)
			}
			if err := session.Close(ctx); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			peer.wait(t)

			source.mu.Lock()
			defer source.mu.Unlock()
			if len(source.deadlines) == 0 {
				t.Fatal("transport deadline was never set")
			}
			for index, got := range source.deadlines {
				if !got.Equal(deadline) {
					t.Fatalf("transport deadline %d = %v, want the operation deadline %v", index, got, deadline)
				}
			}
		})
	})

	t.Run("negative_control", func(t *testing.T) {
		schedule := func(structurePeerRecord) []structurePeerWrite {
			return []structurePeerWrite{{parts: []structurePeerPart{{kind: peerApproved, text: structureNoteControl}, {kind: peerRefused, text: refused}}}}
		}

		// The same tap below the normal pooled stack records the refused
		// frame, so the zero-delivery witness observes real input.
		client, server := net.Pipe()
		t.Cleanup(func() { _ = client.Close() })
		peer := newStructurePeer(structurePeerScript{greeting: true, adverse: &structurePeerAdverse{schedule: schedule}})
		go peer.serve(server)
		ctx := deadlineContext(t)
		tap := &structureTap{Conn: client}
		pooled, err := newIMAPSessionOver(ctx, tap, false)
		if err != nil {
			t.Fatalf("newIMAPSessionOver() error = %v", err)
		}
		if _, err := pooled.fetchUIDStructure(ctx, structurePeerUID); err == nil {
			t.Fatal("pooled fetch accepted a BODY[] section")
		}
		_ = pooled.Abort()
		peer.wait(t)
		if !bytes.Contains(tap.bytes(), []byte(refused)) {
			t.Fatalf("pooled tap = %q lacks the refused frame", tap.bytes())
		}
	})
}

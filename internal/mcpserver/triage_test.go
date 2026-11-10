package mcpserver

import (
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// triageSeeds is an INBOX of ten messages, even UIDs read, beside an ordinary
// folder and attribute-marked archive and trash mailboxes with non-standard
// names.
func triageSeeds() *testkit.StatefulOptions {
	inbox := testkit.MailboxSeed{Name: "INBOX", UIDValidity: 7001}
	for uid := uint32(101); uid <= 110; uid++ {
		flags := []string{`\Flagged`}
		if uid%2 == 0 {
			flags = append(flags, `\Seen`)
		}
		inbox.Messages = append(inbox.Messages, testkit.MessageSeed{UID: uid, Flags: flags, Body: fmt.Sprintf("Subject: Synthetic %d\r\n\r\nsynthetic-%d\r\n", uid, uid)})
	}

	return &testkit.StatefulOptions{Move: true, Mailboxes: []testkit.MailboxSeed{
		inbox,
		{Name: "Folders/Existing", UIDValidity: 7002, Messages: []testkit.MessageSeed{{UID: 501, Body: "Subject: destination\r\n\r\nsynthetic\r\n"}}},
		{Name: "Archivage", Attributes: []string{`\Archive`}},
		{Name: "Corbeille", Attributes: []string{`\Trash`}},
		{Name: "Folders", Attributes: []string{`\Noselect`}},
	}}
}

func startTriage(t *testing.T, seeds *testkit.StatefulOptions) (*testkit.Server, *mcp.ClientSession) {
	t.Helper()

	server, adapter := startIntegrationAdapter(t, testkit.Options{Stateful: seeds})
	session := connectTestClient(t, Options{Mail: adapter, Triage: adapter})

	return server, session
}

func triageArgs(uids []uint32, destination string) map[string]any {
	args := map[string]any{"mailbox": "INBOX", "uidvalidity": 7001, "uids": uids}
	if destination != "" {
		args["destination"] = destination
	}
	return args
}

// requireOutcomes checks the per-UID rows in input order. Each outcome is
// written "outcome" or "outcome:code".
func requireOutcomes(t *testing.T, result *mcp.CallToolResult, uids []uint32, outcomes ...string) {
	t.Helper()

	var got triageResult
	decodeResult(t, result, &got)
	want := triageResult{}
	for index, uid := range uids {
		outcome, code, _ := strings.Cut(outcomes[index], ":")
		want.Results = append(want.Results, triageResultRow{UID: uid, Outcome: outcome, Code: code})
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("results = %+v, want %+v", got, want)
	}
}

// dispatches returns the UID sets of every write with the given verb, after
// checking that each one names exactly one UID over TLS.
func dispatches(t *testing.T, server *testkit.Server, verb string) []testkit.TranscriptCommand {
	t.Helper()

	commands, err := testkit.ParseTranscript(server.Commands())
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}

	var out []testkit.TranscriptCommand
	for _, command := range commands {
		if command.Verb == "UID STORE" && (len(command.Flags) != 1 || command.Flags[0] != `\Seen`) {
			t.Fatalf("STORE changed more than Seen: %+v", command)
		}
		if command.Verb != verb {
			continue
		}
		if !command.TLS || len(command.Ranges) != 1 || command.Ranges[0].Start != command.Ranges[0].Stop {
			t.Fatalf("%s is not one exact UID over TLS: %+v", verb, command)
		}
		out = append(out, command)
	}
	return out
}

func requireSets(t *testing.T, commands []testkit.TranscriptCommand, uids ...uint32) {
	t.Helper()

	var sets []string
	for _, command := range commands {
		sets = append(sets, command.Set)
	}
	var want []string
	for _, uid := range uids {
		want = append(want, fmt.Sprint(uid))
	}
	if !slices.Equal(sets, want) {
		t.Fatalf("dispatched UIDs = %v, want %v", sets, want)
	}
}

func requireNoWrites(t *testing.T, server *testkit.Server, before []testkit.MailboxState) {
	t.Helper()

	if !reflect.DeepEqual(before, server.Snapshot()) {
		t.Fatal("mailbox state changed")
	}
	if len(dispatches(t, server, "UID STORE"))+len(dispatches(t, server, "UID MOVE")) != 0 {
		t.Fatal("a write reached the wire")
	}
}

// wantSeen is before with Seen set or cleared on the given INBOX UIDs.
func wantSeen(before []testkit.MailboxState, seen bool, uids ...uint32) []testkit.MailboxState {
	want := cloneStates(before)
	for _, mailbox := range want {
		if mailbox.Name != "INBOX" {
			continue
		}
		for index := range mailbox.Messages {
			message := &mailbox.Messages[index]
			if !slices.Contains(uids, message.UID) {
				continue
			}
			message.Flags = slices.DeleteFunc(message.Flags, func(flag string) bool { return flag == `\Seen` })
			if seen {
				message.Flags = append(message.Flags, `\Seen`)
			}
		}
	}
	return want
}

// wantMoved is before with the given INBOX UIDs appended to destination in
// order, each taking the destination's next UID.
func wantMoved(before []testkit.MailboxState, destination string, uids ...uint32) []testkit.MailboxState {
	want := cloneStates(before)
	source := slices.IndexFunc(want, func(mailbox testkit.MailboxState) bool { return mailbox.Name == "INBOX" })
	target := slices.IndexFunc(want, func(mailbox testkit.MailboxState) bool { return mailbox.Name == destination })
	for _, uid := range uids {
		index := slices.IndexFunc(want[source].Messages, func(message testkit.MessageState) bool { return message.UID == uid })
		message := want[source].Messages[index]
		want[source].Messages = slices.Delete(want[source].Messages, index, index+1)

		message.UID = want[target].UIDNext
		want[target].UIDNext++
		want[target].Messages = append(want[target].Messages, message)
	}
	return want
}

func cloneStates(states []testkit.MailboxState) []testkit.MailboxState {
	out := slices.Clone(states)
	for index := range out {
		out[index].Messages = slices.Clone(out[index].Messages)
		for message := range out[index].Messages {
			out[index].Messages[message].Flags = slices.Clone(out[index].Messages[message].Flags)
		}
	}
	return out
}

// sortedFlags makes Seen comparisons independent of flag order.
func sortedFlags(states []testkit.MailboxState) []testkit.MailboxState {
	out := cloneStates(states)
	for _, mailbox := range out {
		for _, message := range mailbox.Messages {
			slices.Sort(message.Flags)
		}
	}
	return out
}

func requireState(t *testing.T, server *testkit.Server, want []testkit.MailboxState) {
	t.Helper()

	if got := server.Snapshot(); !reflect.DeepEqual(sortedFlags(got), sortedFlags(want)) {
		t.Fatalf("mailbox state = %+v, want %+v", got, want)
	}
}

func TestTriageToolsRegisterOnlyWithTriage(t *testing.T) {
	t.Parallel()

	_, adapter := startIntegrationAdapter(t, testkit.Options{Stateful: triageSeeds()})
	session := connectTestClient(t, Options{Mail: adapter, Triage: adapter})

	tools, err := session.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	for _, name := range []string{"mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"} {
		if !slices.Contains(names, name) {
			t.Fatalf("catalog %v lacks %s", names, name)
		}
	}
}

func TestTriageSeen(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		tool  string
		seen  bool
		store string
	}{
		{"mark_read", true, "+FLAGS"},
		{"mark_unread", false, "-FLAGS"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			t.Parallel()

			server, session := startTriage(t, triageSeeds())
			before := server.Snapshot()

			uids := []uint32{103, 999, 102, 101}
			requireOutcomes(t, callTool(t, session, test.tool, triageArgs(uids, "")), uids, "applied", "refused:not_found", "applied", "applied")
			requireState(t, server, wantSeen(before, test.seen, 103, 102, 101))
			stores := dispatches(t, server, "UID STORE")
			requireSets(t, stores, 103, 102, 101)
			for _, store := range stores {
				if store.Store != test.store {
					t.Fatalf("STORE direction = %q, want %q", store.Store, test.store)
				}
			}
			if len(dispatches(t, server, "UID MOVE")) != 0 {
				t.Fatal("a Seen tool moved mail")
			}
		})
	}
}

func TestTriageMoves(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		tool, destination, mailbox string
	}{
		{"move_mail", "Folders/Existing", "Folders/Existing"},
		{"archive_mail", "", "Archivage"},
		{"trash_mail", "", "Corbeille"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			t.Parallel()

			server, session := startTriage(t, triageSeeds())
			before := server.Snapshot()

			uids := []uint32{103, 999, 101}
			requireOutcomes(t, callTool(t, session, test.tool, triageArgs(uids, test.destination)), uids, "applied", "refused:not_found", "applied")
			requireState(t, server, wantMoved(before, test.mailbox, 103, 101))
			moves := dispatches(t, server, "UID MOVE")
			requireSets(t, moves, 103, 101)
			for _, move := range moves {
				if move.Destination != test.mailbox {
					t.Fatalf("MOVE destination = %q, want %q", move.Destination, test.mailbox)
				}
			}
		})
	}
}

// A destination that does not exist, or is not a selectable folder, refuses
// every UID before any write.
func TestTriageMoveRefusesMissingDestination(t *testing.T) {
	t.Parallel()

	for _, destination := range []string{"Folders/Missing", "Folders"} {
		t.Run(destination, func(t *testing.T) {
			t.Parallel()

			server, session := startTriage(t, triageSeeds())
			before := server.Snapshot()

			uids := []uint32{101, 102}
			requireOutcomes(t, callTool(t, session, "move_mail", triageArgs(uids, destination)), uids, "refused:unsupported", "refused:unsupported")
			requireNoWrites(t, server, before)
		})
	}

	t.Run("no-trash-mailbox", func(t *testing.T) {
		t.Parallel()

		seeds := triageSeeds()
		seeds.Mailboxes = slices.DeleteFunc(seeds.Mailboxes, func(mailbox testkit.MailboxSeed) bool { return mailbox.Name == "Corbeille" })
		server, session := startTriage(t, seeds)
		before := server.Snapshot()

		uids := []uint32{101}
		requireOutcomes(t, callTool(t, session, "trash_mail", triageArgs(uids, "")), uids, "refused:unsupported")
		requireNoWrites(t, server, before)
	})
}

// A connection dropped after the fixture applied a write leaves that UID
// unknown and stops the batch: the write is never replayed and later UIDs are
// not attempted.
func TestTriageDroppedConnectionStopsBatchWithoutReplay(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		tool, destination, verb string
	}{
		{"mark_read", "", "UID STORE"},
		{"move_mail", "Folders/Existing", "UID MOVE"},
	} {
		t.Run(test.tool, func(t *testing.T) {
			t.Parallel()

			server, session := startTriage(t, triageSeeds())
			before := server.Snapshot()
			fault, err := server.InjectFault(testkit.MutationFault{Command: test.verb, UIDs: "103", Boundary: testkit.AfterApplication, Action: testkit.DropConnection})
			if err != nil {
				t.Fatalf("inject fault: %v", err)
			}

			uids := []uint32{101, 103, 105}
			requireOutcomes(t, callTool(t, session, test.tool, triageArgs(uids, test.destination)), uids, "applied", "unknown:unavailable", "not_attempted")
			waitFor(t, fault.Finished(), "fault connection closed")

			if test.destination == "" {
				requireState(t, server, wantSeen(before, true, 101, 103))
			} else {
				requireState(t, server, wantMoved(before, test.destination, 101, 103))
			}
			requireSets(t, dispatches(t, server, test.verb), 101, 103)
		})
	}
}

// A decoded folder name containing "&" is an exact, valid destination even
// when it resembles a modified UTF-7 shift. Its wire form escapes each
// ampersand as "&-".
func TestTriageMoveExactFolderWithLiteralAmpersand(t *testing.T) {
	t.Parallel()

	for _, folder := range []struct{ decoded, wire string }{
		{"Folders/Research&-Development", "Folders/Research&--Development"},
		{"Folders/R&D-2026", "Folders/R&-D-2026"},
	} {
		t.Run(folder.wire, func(t *testing.T) {
			t.Parallel()

			seeds := triageSeeds()
			seeds.Mailboxes = append(seeds.Mailboxes, testkit.MailboxSeed{Name: folder.wire, UIDValidity: 8200})
			server, session := startTriage(t, seeds)
			before := server.Snapshot()

			uids := []uint32{102, 101}
			requireOutcomes(t, callTool(t, session, "move_mail", triageArgs(uids, folder.decoded)), uids, "applied", "applied")
			requireState(t, server, wantMoved(before, folder.wire, 102, 101))
			for _, move := range dispatches(t, server, "UID MOVE") {
				if move.Destination != folder.wire {
					t.Fatalf("MOVE destination = %q, want %q", move.Destination, folder.wire)
				}
			}
		})
	}
}

// Destinations are resolved from a full ordinary LIST, which decodes every
// wire name. A malformed modified UTF-7 name anywhere in the account fails
// that listing before any write, so the whole request fails closed.
func TestTriageMoveFailsClosedOnMalformedWireName(t *testing.T) {
	t.Parallel()

	seeds := triageSeeds()
	seeds.Mailboxes = append(seeds.Mailboxes, testkit.MailboxSeed{Name: "Folders/&A-", UIDValidity: 8100})
	server, session := startTriage(t, seeds)
	before := server.Snapshot()

	result := callTool(t, session, "move_mail", triageArgs([]uint32{101}, "Folders/Existing"))
	if !result.IsError {
		t.Fatalf("move after an undecodable listing = %s, want a tool error", resultText(t, result))
	}
	requireNoWrites(t, server, before)
}

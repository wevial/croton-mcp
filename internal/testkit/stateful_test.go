package testkit

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const statefulNetworkTimeout = 10 * time.Second

func TestStatefulIMAP(t *testing.T) {
	t.Parallel()

	t.Run("shared_state_local_selection", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
			server := startStateful(t, mode, StatefulOptions{Mailboxes: triageMailboxes()})
			baseline := server.Snapshot()

			untrusted := server.ClientTLSConfig()
			untrusted.RootCAs = nil
			if client, err := dialStatefulWithConfig(server, mode, untrusted); err == nil {
				_ = client.Close()
				t.Fatalf("%s: untrusted client completed TLS verification", mode)
			}

			writer := dialStateful(t, server, mode)
			reader := dialStateful(t, server, mode)
			selectMailbox(t, writer, "INBOX", false)
			selectMailbox(t, reader, "Archive", true)

			storeSeen(t, writer, 1, imap.StoreFlagsAdd, true)

			if got := searchUIDs(t, reader, &imap.SearchCriteria{}); !slices.Equal(got, []imap.UID{1}) {
				t.Fatalf("%s: reader selection followed another connection: UIDs %v", mode, got)
			}
			if got := searchUIDs(t, writer, &imap.SearchCriteria{}); !slices.Equal(got, []imap.UID{1, 2, 3}) {
				t.Fatalf("%s: writer selection changed: UIDs %v", mode, got)
			}
			if status := mailboxStatus(t, reader, "INBOX"); *status.NumUnseen != 2 {
				t.Fatalf("%s: shared INBOX unseen = %d, want 2", mode, *status.NumUnseen)
			}
			if err := reader.Store(imap.UIDSetNum(1), seenFlags(imap.StoreFlagsAdd, true), nil).Close(); err == nil {
				t.Fatalf("%s: EXAMINE selection accepted STORE", mode)
			}

			expected := withFlags(baseline, "INBOX", 1, "\\Seen")
			requireSnapshot(t, server, expected)

			snapshot := server.Snapshot()
			snapshot[0].Messages[0].Flags[0] = "\\Tampered"
			snapshot[0].Messages[0].Body = "tampered"
			snapshot[0].Attributes = append(snapshot[0].Attributes, "\\Tampered")
			snapshot[1].Messages = nil
			if state, ok := server.MailboxSnapshot("inbox"); !ok || !reflect.DeepEqual(state, expected[0]) {
				t.Fatalf("%s: snapshot edit reached fixture: %+v", mode, state)
			}
			requireSnapshot(t, server, expected)

			_ = writer.Close()
			reconnected := dialStateful(t, server, mode)
			if _, err := reconnected.UIDSearch(&imap.SearchCriteria{}, nil).Wait(); err == nil {
				t.Fatalf("%s: selection survived reconnect", mode)
			}
			selectMailbox(t, reconnected, "INBOX", true)
			if got := searchUIDs(t, reconnected, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}); !slices.Equal(got, []imap.UID{2, 3}) {
				t.Fatalf("%s: reconnected UNSEEN = %v", mode, got)
			}
			requireFetchMatches(t, reconnected, expected[0])
		}
	})

	t.Run("list_attributes_capabilities", func(t *testing.T) {
		t.Parallel()

		common := []MailboxSeed{
			{Name: "INBOX"},
			{Name: "Folders", Attributes: []string{`\Noselect`, `\HasChildren`}},
			{Name: "Folders/Existing", Attributes: []string{`\HasNoChildren`}},
			{Name: "Folders/Ghost", Attributes: []string{`\NonExistent`}},
			{Name: "Labels", Attributes: []string{`\Noselect`, `\HasChildren`}},
			{Name: "Labels/Synthetic", Attributes: []string{`\HasNoChildren`}},
		}
		cases := map[string][]MailboxSeed{
			"unique": {{Name: "Archive", Attributes: []string{`\Archive`}}, {Name: "Trash", Attributes: []string{`\Trash`}}},
			"absent": {{Name: "Folders/Archive"}, {Name: "Folders/Trash"}},
			"duplicate": {
				{Name: "Archive", Attributes: []string{`\Archive`}},
				{Name: "Folders/Old Archive", Attributes: []string{`\Archive`, `\HasNoChildren`}},
				{Name: "Trash", Attributes: []string{`\Trash`}},
				{Name: "Folders/Bin", Attributes: []string{`\HasNoChildren`, `\Trash`}},
			},
		}

		for name, special := range cases {
			for _, move := range []bool{true, false} {
				seeds := append(slices.Clone(common), special...)
				server := startStateful(t, ImplicitTLS, StatefulOptions{Mailboxes: seeds, Move: move})
				client := dialStateful(t, server, ImplicitTLS)

				capabilities, err := client.Capability().Wait()
				if err != nil {
					t.Fatalf("%s/move=%t: CAPABILITY: %v", name, move, err)
				}
				if _, advertised := capabilities[imap.CapMove]; advertised != move {
					t.Fatalf("%s/move=%t: MOVE advertised = %t", name, move, advertised)
				}
				if _, advertised := capabilities[imap.CapSpecialUse]; advertised {
					t.Fatalf("%s/move=%t: SPECIAL-USE advertised", name, move)
				}
				requireList(t, client, seeds)

				if err := server.SetMailboxAttributes("Folders/Existing", `\Archive`); err != nil {
					t.Fatalf("set attributes: %v", err)
				}
				if err := server.RemoveMailbox("Labels/Synthetic"); err != nil {
					t.Fatalf("remove mailbox: %v", err)
				}
				changed := slices.Clone(seeds)
				changed[2].Attributes = []string{`\Archive`}
				changed = slices.Delete(changed, 5, 6)
				requireList(t, client, changed)

				if _, err := client.Select("Folders", nil).Wait(); err == nil {
					t.Fatalf("%s/move=%t: nonselectable mailbox was selected", name, move)
				}
			}
		}

		legacy, err := Start(Options{})
		if err != nil {
			t.Fatalf("start legacy server: %v", err)
		}
		t.Cleanup(func() { _ = legacy.Close() })
		if legacy.Snapshot() != nil || legacy.RemoveMailbox("INBOX") == nil {
			t.Fatal("legacy server exposed stateful controls")
		}
	})

	t.Run("seen_flags_persist", func(t *testing.T) {
		t.Parallel()

		server := startStateful(t, ImplicitTLS, StatefulOptions{Mailboxes: []MailboxSeed{{
			Name: "INBOX",
			Messages: []MessageSeed{
				{Body: syntheticBody(1)},
				{Body: syntheticBody(2), Flags: []string{`\Answered`, `\Flagged`}},
				{Body: syntheticBody(3), Flags: []string{`\Seen`}},
			},
		}}})
		baseline := server.Snapshot()
		seen := withFlags(baseline, "INBOX", 2, `\Answered`, `\Flagged`, `\Seen`)

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "INBOX", false)

		for _, step := range []struct {
			op     imap.StoreFlagsOp
			silent bool
			want   []MailboxState
		}{
			{imap.StoreFlagsAdd, false, seen},
			{imap.StoreFlagsDel, false, baseline},
			{imap.StoreFlagsAdd, true, seen},
			{imap.StoreFlagsDel, true, baseline},
			{imap.StoreFlagsAdd, true, seen},
		} {
			updates := storeSeen(t, client, 2, step.op, step.silent)
			if step.silent && len(updates) != 0 || !step.silent && (len(updates) != 1 || !reflect.DeepEqual(flagStrings(updates[0].Flags), step.want[0].Messages[1].Flags)) {
				t.Fatalf("STORE op=%v silent=%t updates = %+v", step.op, step.silent, updates)
			}
			requireSnapshot(t, server, step.want)
		}

		reconnected := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, reconnected, "INBOX", true)
		requireFetchMatches(t, reconnected, seen[0])
		if got := searchUIDs(t, reconnected, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}); !slices.Equal(got, []imap.UID{1}) {
			t.Fatalf("UID SEARCH UNSEEN = %v, want [1]", got)
		}
		if status := mailboxStatus(t, reconnected, "INBOX"); *status.NumUnseen != 1 || *status.NumMessages != 3 {
			t.Fatalf("STATUS = %+v", status)
		}

		if err := reconnected.Store(imap.UIDSetNum(2), seenFlags(imap.StoreFlagsDel, false), nil).Close(); err == nil {
			t.Fatal("STORE after EXAMINE succeeded")
		}
		requireSnapshot(t, server, seen)
	})

	t.Run("move_three_of_ten", func(t *testing.T) {
		t.Parallel()

		source := make([]MessageSeed, 0, 10)
		for uid := 1; uid <= 10; uid++ {
			message := MessageSeed{Body: syntheticBody(uid)}
			if uid%2 == 0 {
				message.Flags = []string{`\Seen`}
			}
			if uid == 5 {
				message.Flags = []string{`\Flagged`, `\Answered`}
			}
			source = append(source, message)
		}
		server := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: []MailboxSeed{
			{Name: "INBOX", Messages: source},
			{Name: "Archive", Attributes: []string{`\Archive`}, Messages: []MessageSeed{
				{Body: syntheticBody(101), Flags: []string{`\Seen`}},
				{Body: syntheticBody(102)},
			}},
		}})
		baseline := server.Snapshot()

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "INBOX", false)
		moved, err := client.Move(imap.UIDSetNum(2, 5, 9), "Archive").Wait()
		if err != nil {
			t.Fatalf("UID MOVE: %v", err)
		}
		if moved.UIDValidity != baseline[1].UIDValidity || moved.SourceUIDs.String() != "2,5,9" || moved.DestUIDs.String() != "3:5" {
			t.Fatalf("COPYUID = %d %v %v", moved.UIDValidity, moved.SourceUIDs, moved.DestUIDs)
		}
		if mailbox := client.Mailbox(); mailbox == nil || mailbox.NumMessages != 7 {
			t.Fatalf("client did not consume EXPUNGE notifications: %+v", mailbox)
		}

		want := slices.Clone(baseline)
		want[0].Messages = nil
		want[1].Messages = slices.Clone(baseline[1].Messages)
		want[1].UIDNext = 6
		for _, message := range baseline[0].Messages {
			if message.UID != 2 && message.UID != 5 && message.UID != 9 {
				want[0].Messages = append(want[0].Messages, message)
				continue
			}
			message.UID = uint32(3 + len(want[1].Messages) - 2)
			want[1].Messages = append(want[1].Messages, message)
		}
		requireSnapshot(t, server, want)

		if _, err := client.Move(imap.UIDSetNum(2, 42, 99), "Archive").Wait(); err != nil {
			t.Fatalf("UID MOVE of missing UIDs: %v", err)
		}
		requireSnapshot(t, server, want)

		fresh := dialStateful(t, server, ImplicitTLS)
		for _, state := range want {
			selectMailbox(t, fresh, state.Name, true)
			requireFetchMatches(t, fresh, state)
		}
		if status := mailboxStatus(t, fresh, "Archive"); status.UIDNext != 6 || *status.NumMessages != 5 {
			t.Fatalf("destination STATUS = %+v", status)
		}
	})

	t.Run("move_refusals_uidvalidity", func(t *testing.T) {
		t.Parallel()

		seeds := triageMailboxes()
		seeds = append(seeds, MailboxSeed{Name: "Folders", Attributes: []string{`\Noselect`, `\HasChildren`}})
		enabled := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: seeds})
		disabled := startStateful(t, ImplicitTLS, StatefulOptions{Mailboxes: seeds})
		baseline := enabled.Snapshot()

		for _, refusal := range []struct {
			name, destination string
			examine, noSelect bool
		}{
			{name: "missing destination", destination: "Nowhere"},
			{name: "nonselectable destination", destination: "Folders"},
			{name: "no selection", destination: "Archive", noSelect: true},
			{name: "EXAMINE selection", destination: "Archive", examine: true},
		} {
			client := dialStateful(t, enabled, ImplicitTLS)
			if !refusal.noSelect {
				selectMailbox(t, client, "INBOX", refusal.examine)
			}
			before := len(enabled.Commands())
			_, err := client.Move(imap.UIDSetNum(1, 2), refusal.destination).Wait()
			var imapError *imap.Error
			if !errors.As(err, &imapError) {
				t.Fatalf("%s: UID MOVE error = %v, want tagged failure", refusal.name, err)
			}
			requireRecorded(t, enabled, before, "UID MOVE")
			requireSnapshot(t, enabled, baseline)
		}

		raw := connectClient(t, disabled, ImplicitTLS)
		defer raw.Close()
		raw.exchange(t, "a1", "LOGIN fixture-user@client.test not-a-secret")
		raw.exchange(t, "a2", "SELECT INBOX")
		if completion := raw.exchange(t, "a3", `UID MOVE 1,2 "Archive"`); !strings.HasPrefix(completion, "a3 BAD") {
			t.Fatalf("MOVE disabled completion = %q", completion)
		}
		requireRecorded(t, disabled, 0, "UID MOVE")
		requireSnapshot(t, disabled, baseline)

		if err := enabled.ReplaceUIDValidity("INBOX", 77); err != nil {
			t.Fatalf("replace UIDVALIDITY: %v", err)
		}
		client := dialStateful(t, enabled, ImplicitTLS)
		for _, readOnly := range []bool{false, true} {
			if data := selectMailbox(t, client, "INBOX", readOnly); data.UIDValidity != 77 {
				t.Fatalf("SELECT/EXAMINE read-only=%t UIDVALIDITY = %d", readOnly, data.UIDValidity)
			}
		}
		if status := mailboxStatus(t, client, "INBOX"); status.UIDValidity != 77 {
			t.Fatalf("STATUS UIDVALIDITY = %d", status.UIDValidity)
		}
		if status := mailboxStatus(t, client, "Archive"); status.UIDValidity != baseline[2].UIDValidity {
			t.Fatalf("destination UIDVALIDITY = %d, want %d", status.UIDValidity, baseline[2].UIDValidity)
		}
		want := slices.Clone(baseline)
		want[0].UIDValidity = 77
		requireSnapshot(t, enabled, want)
	})

	t.Run("forbidden_mutations", func(t *testing.T) {
		t.Parallel()

		seeds := triageMailboxes()
		seeds[0].Messages[2].Flags = []string{`\Deleted`}
		server := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: seeds})
		baseline := server.Snapshot()

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "INBOX", false)
		for _, attempt := range []struct {
			name string
			run  func() error
		}{
			{"COPY", func() error { _, err := client.Copy(imap.SeqSetNum(1), "Archive").Wait(); return err }},
			{"UID COPY", func() error { _, err := client.Copy(imap.UIDSetNum(1), "Archive").Wait(); return err }},
			{"EXPUNGE", func() error { return client.Expunge().Close() }},
			{"UID EXPUNGE", func() error { return client.UIDExpunge(imap.UIDSetNum(3)).Close() }},
			{"DELETE", func() error { return client.Delete("Trash").Wait() }},
			{"CREATE", func() error { return client.Create("Folders/New", nil).Wait() }},
			{"STORE", func() error {
				return client.Store(imap.SeqSetNum(1), seenFlags(imap.StoreFlagsAdd, false), nil).Close()
			}},
			{"UID STORE", func() error {
				return client.Store(imap.UIDSetNum(1), seenFlags(imap.StoreFlagsSet, false), nil).Close()
			}},
			{"UID STORE", func() error { return storeFlags(client, imap.FlagAnswered) }},
			{"UID STORE", func() error { return storeFlags(client, imap.FlagFlagged) }},
			{"UID STORE", func() error { return storeFlags(client, imap.FlagDeleted) }},
			{"UID STORE", func() error { return storeFlags(client, imap.FlagSeen, imap.FlagFlagged) }},
		} {
			before := len(server.Commands())
			var imapError *imap.Error
			if err := attempt.run(); !errors.As(err, &imapError) {
				t.Fatalf("%s error = %v, want tagged failure", attempt.name, err)
			}
			requireRecorded(t, server, before, attempt.name)
			requireSnapshot(t, server, baseline)
		}

		raw := connectClient(t, server, ImplicitTLS)
		defer raw.Close()
		raw.exchange(t, "a1", "LOGIN fixture-user@client.test not-a-secret")
		raw.exchange(t, "a2", "SELECT INBOX")
		before := len(server.Commands())
		message := syntheticBody(99)
		if completion := raw.exchange(t, "a3", fmt.Sprintf("APPEND \"Archive\" {%d}", len(message))); !strings.HasPrefix(completion, "a3 NO") {
			t.Fatalf("APPEND completion = %q, want refusal before continuation", completion)
		}
		requireRecorded(t, server, before, "APPEND")
		if completion := raw.exchange(t, "a4", "NOOP"); completion != "a4 OK NOOP completed" {
			t.Fatalf("connection lost sync after APPEND refusal: %q", completion)
		}
		requireSnapshot(t, server, baseline)
	})
}

func triageMailboxes() []MailboxSeed {
	return []MailboxSeed{
		{Name: "INBOX", Messages: []MessageSeed{{Body: syntheticBody(1)}, {Body: syntheticBody(2), Flags: []string{`\Flagged`}}, {Body: syntheticBody(3)}}},
		{Name: "Folders/Existing", Messages: []MessageSeed{{UID: 40, Body: syntheticBody(40), Flags: []string{`\Seen`}}}},
		{Name: "Archive", Attributes: []string{`\Archive`}, Messages: []MessageSeed{{Body: syntheticBody(50), Flags: []string{`\Seen`}}}},
		{Name: "Trash", Attributes: []string{`\Trash`}},
	}
}

func syntheticBody(index int) string {
	return fmt.Sprintf("From: Fixture <fixture@croton.test>\r\nTo: Reader <reader@croton.test>\r\n"+
		"Subject: Synthetic triage %d\r\nMessage-ID: <triage-%d@croton.test>\r\n\r\nSYNTHETIC_TRIAGE_BODY_%d\r\n", index, index, index)
}

func startStateful(t *testing.T, mode TLSMode, options StatefulOptions) *Server {
	t.Helper()

	server, err := Start(Options{Mode: mode, Stateful: &options})
	if err != nil {
		t.Fatalf("start stateful server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close stateful server: %v", err)
		}
	})

	return server
}

func dialStatefulWithConfig(server *Server, mode TLSMode, config *tls.Config) (*imapclient.Client, error) {
	connection, err := net.DialTimeout("tcp", server.Addr(), statefulNetworkTimeout)
	if err != nil {
		return nil, err
	}
	if err := connection.SetDeadline(time.Now().Add(statefulNetworkTimeout)); err != nil {
		_ = connection.Close()
		return nil, err
	}

	if mode == StartTLS {
		client, err := imapclient.NewStartTLS(connection, &imapclient.Options{TLSConfig: config})
		if err != nil {
			_ = connection.Close()
		}
		return client, err
	}

	tlsConnection := tls.Client(connection, config)
	if err := tlsConnection.Handshake(); err != nil {
		_ = connection.Close()
		return nil, err
	}

	return imapclient.New(tlsConnection, nil), nil
}

func dialStateful(t *testing.T, server *Server, mode TLSMode) *imapclient.Client {
	t.Helper()

	client, err := dialStatefulWithConfig(server, mode, server.ClientTLSConfig())
	if err != nil {
		t.Fatalf("dial %s: %v", mode, err)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Login("fixture-user@client.test", "not-a-secret").Wait(); err != nil {
		t.Fatalf("login over %s: %v", mode, err)
	}

	return client
}

func selectMailbox(t *testing.T, client *imapclient.Client, name string, readOnly bool) *imap.SelectData {
	t.Helper()

	data, err := client.Select(name, &imap.SelectOptions{ReadOnly: readOnly}).Wait()
	if err != nil {
		t.Fatalf("select %q read-only=%t: %v", name, readOnly, err)
	}

	return data
}

func seenFlags(op imap.StoreFlagsOp, silent bool) *imap.StoreFlags {
	return &imap.StoreFlags{Op: op, Silent: silent, Flags: []imap.Flag{imap.FlagSeen}}
}

func storeSeen(t *testing.T, client *imapclient.Client, uid imap.UID, op imap.StoreFlagsOp, silent bool) []*imapclient.FetchMessageBuffer {
	t.Helper()

	updates, err := client.Store(imap.UIDSetNum(uid), seenFlags(op, silent), nil).Collect()
	if err != nil {
		t.Fatalf("UID STORE %d: %v", uid, err)
	}

	return updates
}

func storeFlags(client *imapclient.Client, flags ...imap.Flag) error {
	return client.Store(imap.UIDSetNum(1), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Flags: flags}, nil).Close()
}

func searchUIDs(t *testing.T, client *imapclient.Client, criteria *imap.SearchCriteria) []imap.UID {
	t.Helper()

	data, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		t.Fatalf("UID SEARCH: %v", err)
	}

	return data.AllUIDs()
}

func mailboxStatus(t *testing.T, client *imapclient.Client, name string) *imap.StatusData {
	t.Helper()

	data, err := client.Status(name, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true}).Wait()
	if err != nil {
		t.Fatalf("STATUS %q: %v", name, err)
	}

	return data
}

// requireFetchMatches compares a real client's view of the selected mailbox with a snapshot.
func requireFetchMatches(t *testing.T, client *imapclient.Client, want MailboxState) {
	t.Helper()

	whole := &imap.FetchItemBodySection{Peek: true}
	messages, err := client.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{
		UID: true, Flags: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{whole},
	}).Collect()
	if err != nil {
		t.Fatalf("UID FETCH %q: %v", want.Name, err)
	}

	got := make([]MessageState, 0, len(messages))
	for _, message := range messages {
		got = append(got, MessageState{UID: uint32(message.UID), Flags: flagStrings(message.Flags), Body: string(message.FindBodySection(whole))})
	}
	if !reflect.DeepEqual(got, normalizeMessages(want.Messages)) {
		t.Fatalf("UID FETCH %q = %+v, want %+v", want.Name, got, want.Messages)
	}
	if uids := searchUIDs(t, client, &imap.SearchCriteria{}); len(uids) != len(want.Messages) {
		t.Fatalf("UID SEARCH ALL %q = %v", want.Name, uids)
	}
}

func requireList(t *testing.T, client *imapclient.Client, want []MailboxSeed) {
	t.Helper()

	listed, err := client.List("", "*", nil).Collect()
	if err != nil {
		t.Fatalf("LIST: %v", err)
	}

	var got, expected []string
	for _, mailbox := range listed {
		attributes := make([]string, 0, len(mailbox.Attrs))
		for _, attribute := range mailbox.Attrs {
			attributes = append(attributes, string(attribute))
		}
		got = append(got, fmt.Sprintf("%s %c %v", mailbox.Mailbox, mailbox.Delim, attributes))
	}
	for _, mailbox := range want {
		expected = append(expected, fmt.Sprintf("%s / %v", mailbox.Name, append([]string{}, mailbox.Attributes...)))
	}
	if !slices.Equal(got, expected) {
		t.Fatalf("LIST = %q, want %q", got, expected)
	}
}

func requireSnapshot(t *testing.T, server *Server, want []MailboxState) {
	t.Helper()

	got := server.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v\nwant %+v", got, want)
	}
}

// requireRecorded asserts that a command with this effective name was recorded after index.
func requireRecorded(t *testing.T, server *Server, index int, name string) {
	t.Helper()

	for _, command := range server.Commands()[index:] {
		if effectiveCommandName(command) == name {
			return
		}
	}

	t.Fatalf("%s was not recorded after command %d", name, index)
}

// withFlags returns a deep copy of states with one message's flags replaced.
func withFlags(states []MailboxState, mailbox string, uid uint32, flags ...string) []MailboxState {
	copied := make([]MailboxState, 0, len(states))
	for _, state := range states {
		state.Attributes = slices.Clone(state.Attributes)
		state.Messages = slices.Clone(state.Messages)
		for index := range state.Messages {
			state.Messages[index].Flags = slices.Clone(state.Messages[index].Flags)
			if state.Name == mailbox && state.Messages[index].UID == uid {
				state.Messages[index].Flags = flags
			}
		}
		copied = append(copied, state)
	}

	return copied
}

func flagStrings(flags []imap.Flag) []string {
	values := make([]string, 0, len(flags))
	for _, flag := range flags {
		values = append(values, string(flag))
	}

	return values
}

func normalizeMessages(messages []MessageState) []MessageState {
	normalized := make([]MessageState, 0, len(messages))
	for _, message := range messages {
		message.Flags = append([]string{}, message.Flags...)
		normalized = append(normalized, message)
	}

	return normalized
}

// exchange sends one raw command and returns its tagged completion, failing
// if the server grants a continuation.
func (client *imapClient) exchange(t *testing.T, tag, command string) string {
	t.Helper()

	if _, err := fmt.Fprintf(client.connection, "%s %s\r\n", tag, command); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}

	for {
		line := client.readLine(t)
		if strings.HasPrefix(line, "+") {
			t.Fatalf("%q received continuation %q", command, line)
		}
		if strings.HasPrefix(line, tag+" ") {
			return line
		}
	}
}

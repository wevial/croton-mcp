package testkit

import (
	"crypto/tls"
	"fmt"
	"net"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const statefulNetworkTimeout = 10 * time.Second

// TestStatefulIMAP is the reference for the UID STORE and UID MOVE semantics
// that consumer triage tests rely on.
func TestStatefulIMAP(t *testing.T) {
	t.Parallel()

	t.Run("seen_flags_persist", func(t *testing.T) {
		t.Parallel()

		server := startStateful(t, StatefulOptions{Mailboxes: []MailboxSeed{{
			Name: "INBOX",
			Messages: []MessageSeed{
				{Body: syntheticBody(1)},
				{Body: syntheticBody(2), Flags: []string{`\Answered`, `\Flagged`}},
				{Body: syntheticBody(3), Flags: []string{`\Seen`}},
			},
		}}})
		baseline := server.Snapshot()
		seen := withFlags(baseline, "INBOX", 2, `\Answered`, `\Flagged`, `\Seen`)

		client := dialStateful(t, server)
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

		reconnected := dialStateful(t, server)
		selectMailbox(t, reconnected, "INBOX", true)
		if got := searchUIDs(t, reconnected, &imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}); !slices.Equal(got, []imap.UID{1}) {
			t.Fatalf("UID SEARCH UNSEEN = %v, want [1]", got)
		}

		if err := reconnected.Store(imap.UIDSetNum(2), &imap.StoreFlags{Op: imap.StoreFlagsDel, Flags: []imap.Flag{imap.FlagSeen}}, nil).Close(); err == nil {
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
		server := startStateful(t, StatefulOptions{Move: true, Mailboxes: []MailboxSeed{
			{Name: "INBOX", Messages: source},
			{Name: "Archive", Attributes: []string{`\Archive`}, Messages: []MessageSeed{
				{Body: syntheticBody(101), Flags: []string{`\Seen`}},
				{Body: syntheticBody(102)},
			}},
		}})
		baseline := server.Snapshot()

		client := dialStateful(t, server)
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
	})
}

func syntheticBody(index int) string {
	return fmt.Sprintf("From: Fixture <fixture@croton.test>\r\nTo: Reader <reader@croton.test>\r\n"+
		"Subject: Synthetic triage %d\r\nMessage-ID: <triage-%d@croton.test>\r\n\r\nSYNTHETIC_TRIAGE_BODY_%d\r\n", index, index, index)
}

func startStateful(t *testing.T, options StatefulOptions) *Server {
	t.Helper()

	server, err := Start(Options{Mode: ImplicitTLS, Stateful: &options})
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

func dialStateful(t *testing.T, server *Server) *imapclient.Client {
	t.Helper()

	connection, err := tls.DialWithDialer(&net.Dialer{Timeout: statefulNetworkTimeout}, "tcp", server.Addr(), server.ClientTLSConfig())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if err := connection.SetDeadline(time.Now().Add(statefulNetworkTimeout)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}

	client := imapclient.New(connection, nil)
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Login("fixture-user@client.test", "not-a-secret").Wait(); err != nil {
		t.Fatalf("login: %v", err)
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

func storeSeen(t *testing.T, client *imapclient.Client, uid imap.UID, op imap.StoreFlagsOp, silent bool) []*imapclient.FetchMessageBuffer {
	t.Helper()

	updates, err := client.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: op, Silent: silent, Flags: []imap.Flag{imap.FlagSeen}}, nil).Collect()
	if err != nil {
		t.Fatalf("UID STORE %d: %v", uid, err)
	}

	return updates
}

func searchUIDs(t *testing.T, client *imapclient.Client, criteria *imap.SearchCriteria) []imap.UID {
	t.Helper()

	data, err := client.UIDSearch(criteria, nil).Wait()
	if err != nil {
		t.Fatalf("UID SEARCH: %v", err)
	}

	return data.AllUIDs()
}

func requireSnapshot(t *testing.T, server *Server, want []MailboxState) {
	t.Helper()

	got := server.Snapshot()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("snapshot = %+v\nwant %+v", got, want)
	}
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

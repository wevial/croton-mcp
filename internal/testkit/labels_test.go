package testkit

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

func TestLabelFixture(t *testing.T) {
	t.Parallel()

	t.Run("linked_view_identity", func(t *testing.T) {
		t.Parallel()

		options := LabelOptions{
			Messages: []LabelMessage{
				{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
				{ID: "B", Body: labelBody("B"), Flags: []string{`\Flagged`}},
			},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 501}}},
				{Name: "Labels", Role: LabelRole, Attributes: []string{`\Noselect`, `\HasChildren`}, UIDValidity: 8000},
				{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "B", UID: 101}, {Message: "A", UID: 501}}},
			},
		}
		want := seedLabelState(options)

		for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
			server := startLabels(t, mode, options)
			requireLabelSnapshot(t, server, want)

			untrusted := server.ClientTLSConfig()
			untrusted.RootCAs = nil
			if client, err := dialStatefulWithConfig(server, mode, untrusted); err == nil {
				_ = client.Close()
				t.Fatalf("%s: untrusted client completed TLS verification", mode)
			}

			folder := dialStateful(t, server, mode)
			label := dialStateful(t, server, mode)
			if data := selectMailbox(t, folder, "INBOX", false); data.UIDValidity != 7001 || data.NumMessages != 2 {
				t.Fatalf("%s: SELECT INBOX = %+v", mode, data)
			}
			if data := selectMailbox(t, label, "Labels/One", true); data.UIDValidity != 8001 || data.NumMessages != 2 {
				t.Fatalf("%s: EXAMINE Labels/One = %+v", mode, data)
			}

			for _, view := range []struct {
				client   *imapclient.Client
				name     string
				uid101   string
				seenUIDs []imap.UID
			}{
				{client: folder, name: "INBOX", uid101: labelBody("A"), seenUIDs: []imap.UID{101}},
				{client: label, name: "Labels/One", uid101: labelBody("B"), seenUIDs: []imap.UID{501}},
			} {
				if got := searchUIDs(t, view.client, &imap.SearchCriteria{}); !slices.Equal(got, []imap.UID{101, 501}) {
					t.Fatalf("%s: %s UID SEARCH ALL = %v", mode, view.name, got)
				}
				if got := searchUIDs(t, view.client, &imap.SearchCriteria{Flag: []imap.Flag{imap.FlagSeen}}); !slices.Equal(got, view.seenUIDs) {
					t.Fatalf("%s: %s UID SEARCH SEEN = %v, want %v", mode, view.name, got, view.seenUIDs)
				}
				if got := fetchBody(t, view.client, 101); got != view.uid101 {
					t.Fatalf("%s: %s UID 101 body = %q, want %q", mode, view.name, got, view.uid101)
				}
				requireFetchMatches(t, view.client, viewMailbox(want, view.name))
			}

			snapshot, _ := server.LabelSnapshot()
			snapshot.Messages[0].Body = "tampered"
			snapshot.Messages[0].Flags[0] = `\Tampered`
			snapshot.Views[0].Members[0].UID = 999
			snapshot.Views[0].Attributes = append(snapshot.Views[0].Attributes, `\Tampered`)
			snapshot.Views[2].Members = nil
			requireLabelSnapshot(t, server, want)

			_ = folder.Close()
			reconnected := dialStateful(t, server, mode)
			if _, err := reconnected.UIDSearch(&imap.SearchCriteria{}, nil).Wait(); err == nil {
				t.Fatalf("%s: selection survived reconnect", mode)
			}
			if got := fetchBody(t, label, 101); got != labelBody("B") {
				t.Fatalf("%s: label selection followed another connection: %q", mode, got)
			}
			requireLabelWire(t, server, mode, want)

			if server.Snapshot() != nil {
				t.Fatalf("%s: label server exposed ordinary stateful snapshot", mode)
			}
			if _, err := server.InjectFault(MutationFault{Command: "UID STORE", UIDs: "1", Boundary: BeforeApplication, Action: DropConnection}); err == nil {
				t.Fatalf("%s: label server accepted a mutation fault", mode)
			}
		}

		valid := func() LabelOptions {
			return LabelOptions{
				Messages: []LabelMessage{{ID: "A", Body: labelBody("A")}, {ID: "B", Body: labelBody("A")}},
				Views: []LabelViewSeed{
					{Name: "INBOX", Role: FolderRole, Members: []Membership{{Message: "A", UID: 1}, {Message: "B", UID: 2}}},
					{Name: "Folders/Other", Role: FolderRole},
					{Name: "Labels/One", Role: LabelRole, Members: []Membership{{Message: "A", UID: 1}}},
				},
			}
		}
		equalBodies, err := Start(Options{Labels: new(valid())})
		if err != nil {
			t.Fatalf("valid linked seed with equal bodies rejected: %v", err)
		}
		_ = equalBodies.Close()
		for name, change := range map[string]func(*LabelOptions){
			"missing role":         func(options *LabelOptions) { options.Views[2].Role = 0 },
			"unknown message":      func(options *LabelOptions) { options.Views[2].Members[0].Message = "Z" },
			"duplicate message ID": func(options *LabelOptions) { options.Messages[1].ID = "A" },
			"empty message ID":     func(options *LabelOptions) { options.Messages[0].ID = "" },
			"shared Deleted flag":  func(options *LabelOptions) { options.Messages[0].Flags = []string{`\Deleted`} },
			"duplicate membership": func(options *LabelOptions) {
				options.Views[2].Members = []Membership{{Message: "A", UID: 1}, {Message: "A", UID: 2}}
			},
			"duplicate view UID": func(options *LabelOptions) {
				options.Views[2].Members = []Membership{{Message: "A", UID: 1}, {Message: "B", UID: 1}}
			},
			"zero view UID":          func(options *LabelOptions) { options.Views[2].Members[0].UID = 0 },
			"multiple folders":       func(options *LabelOptions) { options.Views[1].Members = []Membership{{Message: "A", UID: 7}} },
			"no folder":              func(options *LabelOptions) { options.Views[0].Members = options.Views[0].Members[:1] },
			"duplicate mailbox name": func(options *LabelOptions) { options.Views[1].Name = "inbox" },
		} {
			options := valid()
			change(&options)
			if server, err := Start(Options{Labels: &options}); err == nil {
				_ = server.Close()
				t.Fatalf("%s: inconsistent linked seed was accepted", name)
			}
		}
		for name, options := range map[string]Options{
			"stateful": {Labels: new(valid()), Stateful: &StatefulOptions{}},
			"messages": {Labels: new(valid()), Messages: []string{labelBody("A")}},
			"scenario": {Labels: new(valid()), Scenario: Scenario{RejectAuthentication: true}},
		} {
			if server, err := Start(options); err == nil {
				_ = server.Close()
				t.Fatalf("label mode combined with %s was accepted", name)
			}
		}
	})

	t.Run("copy_keeps_folder_and_labels", func(t *testing.T) {
		t.Parallel()

		options := LabelOptions{
			UIDPlus: true,
			Messages: []LabelMessage{
				{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
				{ID: "C", Body: labelBody("C")},
			},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "C", UID: 3}}},
				{Name: "Folders/Work", Role: FolderRole, UIDValidity: 7002, Members: []Membership{{Message: "A", UID: 7}}},
				{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "A", UID: 3}}},
				{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "C", UID: 10}}},
			},
		}
		server := startLabels(t, ImplicitTLS, options)
		want := seedLabelState(options)
		two := expectedView(&want, "Labels/Two")
		two.Members = append(two.Members, Membership{Message: "A", UID: 11})
		two.UIDNext = 12

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "Folders/Work", false)

		copied, err := client.Copy(imap.UIDSetNum(7), "Labels/Two").Wait()
		if err != nil {
			t.Fatalf("UID COPY: %v", err)
		}
		if copied.UIDValidity != 8002 || copied.SourceUIDs.String() != "7" || copied.DestUIDs.String() != "11" {
			t.Fatalf("COPYUID = %d %v %v", copied.UIDValidity, copied.SourceUIDs, copied.DestUIDs)
		}
		requireLabelSnapshot(t, server, want)

		repeated, err := client.Copy(imap.UIDSetNum(7), "Labels/Two").Wait()
		if err != nil || repeated.DestUIDs != nil {
			t.Fatalf("repeated UID COPY = %+v, %v; want OK without a new membership", repeated, err)
		}
		if missing, err := client.Copy(imap.UIDSetNum(8), "Labels/Two").Wait(); err != nil || missing.DestUIDs != nil {
			t.Fatalf("UID COPY of a missing UID = %+v, %v; want an empty OK", missing, err)
		}
		if mailbox := client.Mailbox(); mailbox == nil || mailbox.NumMessages != 1 {
			t.Fatalf("source folder count changed: %+v", mailbox)
		}
		requireLabelSnapshot(t, server, want)
		requireLabelWire(t, server, ImplicitTLS, want)

		if parsed, err := ParseTranscript(server.Commands()); err != nil {
			t.Fatalf("parse transcript: %v", err)
		} else if copies := countVerb(parsed, "UID COPY"); copies != 3 {
			t.Fatalf("recorded %d UID COPY commands, want 3", copies)
		}
	})

	t.Run("deleted_is_view_local", func(t *testing.T) {
		t.Parallel()

		options := LabelOptions{
			Messages: []LabelMessage{
				{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
				{ID: "B", Body: labelBody("B")},
				{ID: "C", Body: labelBody("C"), Flags: []string{`\Flagged`}},
			},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 501}}},
				{Name: "Folders/Other", Role: FolderRole, UIDValidity: 7002, Members: []Membership{{Message: "C", UID: 1}}},
				{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "B", UID: 101}, {Message: "A", UID: 501}, {Message: "C", UID: 502}}},
				{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "A", UID: 20}}},
			},
		}
		server := startLabels(t, ImplicitTLS, options)
		want := seedLabelState(options)

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "Labels/One", false)

		if updates := storeDeleted(t, client, 501); len(updates) != 0 {
			t.Fatalf("silent STORE returned updates %+v", updates)
		}
		expectedView(&want, "Labels/One").Members[1].Deleted = true
		requireLabelSnapshot(t, server, want)
		requireLabelWire(t, server, ImplicitTLS, want)

		storeDeleted(t, client, 101)
		expectedView(&want, "Labels/One").Members[0].Deleted = true
		requireLabelSnapshot(t, server, want)
		requireLabelWire(t, server, ImplicitTLS, want)

		fresh := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, fresh, "INBOX", true)
		if flags := fetchFlags(t, fresh, 101); !slices.Equal(flags, []string{`\Seen`}) {
			t.Fatalf("folder UID 101 (A) flags = %v, want only shared Seen", flags)
		}
		if flags := fetchFlags(t, fresh, 501); len(flags) != 0 {
			t.Fatalf("folder UID 501 (B) flags = %v, want none", flags)
		}
		selectMailbox(t, fresh, "Labels/One", true)
		if got := fetchBody(t, fresh, 101); got != labelBody("B") {
			t.Fatalf("label UID 101 body = %q, want B", got)
		}
	})

	t.Run("scoped_expunge_preserves_collateral", func(t *testing.T) {
		t.Parallel()

		options := LabelOptions{
			UIDPlus: true,
			Messages: []LabelMessage{
				{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
				{ID: "B", Body: labelBody("B"), Flags: []string{`\Flagged`}},
				{ID: "C", Body: labelBody("C")},
			},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 501}, {Message: "C", UID: 502}}},
				{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{
					{Message: "B", UID: 101, Deleted: true},
					{Message: "A", UID: 501, Deleted: true},
					{Message: "C", UID: 502},
				}},
				{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "A", UID: 30}}},
			},
		}
		server := startLabels(t, ImplicitTLS, options)
		want := seedLabelState(options)

		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "Labels/One", false)

		expunged, err := client.UIDExpunge(imap.UIDSetNum(501)).Collect()
		if err != nil || !slices.Equal(expunged, []uint32{2}) {
			t.Fatalf("UID EXPUNGE 501 = %v, %v; want sequence 2", expunged, err)
		}
		if mailbox := client.Mailbox(); mailbox == nil || mailbox.NumMessages != 2 {
			t.Fatalf("client did not consume EXPUNGE: %+v", mailbox)
		}
		one := expectedView(&want, "Labels/One")
		one.Members = slices.Delete(one.Members, 1, 2)
		requireLabelSnapshot(t, server, want)

		for _, uid := range []imap.UID{502, 999, 501} {
			if expunged, err := client.UIDExpunge(imap.UIDSetNum(uid)).Collect(); err != nil || len(expunged) != 0 {
				t.Fatalf("UID EXPUNGE %d = %v, %v; want an empty OK", uid, expunged, err)
			}
		}
		requireLabelSnapshot(t, server, want)

		for range 2 {
			requireLabelWire(t, server, ImplicitTLS, want)
		}
		fresh := dialStateful(t, server, ImplicitTLS)
		if data := selectMailbox(t, fresh, "Labels/One", true); data.NumMessages != 2 {
			t.Fatalf("Labels/One EXISTS = %d, want 2", data.NumMessages)
		}
		if flags := fetchFlags(t, fresh, 101); !slices.Equal(flags, []string{`\Flagged`, `\Deleted`}) || fetchBody(t, fresh, 101) != labelBody("B") {
			t.Fatalf("unrelated marked B changed: flags %v", flags)
		}
		if data := selectMailbox(t, fresh, "INBOX", true); data.NumMessages != 3 {
			t.Fatalf("INBOX EXISTS = %d, want 3", data.NumMessages)
		}
		if got := fetchBody(t, fresh, 101); got != labelBody("A") {
			t.Fatalf("A left its folder: %q", got)
		}
	})

	t.Run("scoped_refusals_and_uidplus", func(t *testing.T) {
		t.Parallel()

		options := func(uidPlus bool) LabelOptions {
			return LabelOptions{
				UIDPlus: uidPlus,
				Messages: []LabelMessage{
					{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
					{ID: "B", Body: labelBody("B")},
					{ID: "C", Body: labelBody("C")},
				},
				Views: []LabelViewSeed{
					{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 102, Deleted: true}}},
					{Name: "Folders/Other", Role: FolderRole, UIDValidity: 7002, Members: []Membership{{Message: "C", UID: 1}}},
					{Name: "Labels/Lookalike", Role: FolderRole, UIDValidity: 7003},
					{Name: "Labels", Role: LabelRole, Attributes: []string{`\Noselect`, `\HasChildren`}, UIDValidity: 8000},
					{Name: "Labels/Ghost", Role: LabelRole, Attributes: []string{`\NonExistent`}, UIDValidity: 8003},
					{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "A", UID: 501, Deleted: true}, {Message: "B", UID: 502, Deleted: true}}},
					{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "B", UID: 9}, {Message: "C", UID: 10}}},
				},
			}
		}
		enabled := startLabels(t, ImplicitTLS, options(true))
		disabled := startLabels(t, ImplicitTLS, options(false))
		baseline := seedLabelState(options(true))

		for _, attempt := range []struct {
			server   *Server
			selected string
			examine  bool
			command  string
			name     string
		}{
			{disabled, "Labels/One", false, "UID EXPUNGE 501", "UID EXPUNGE"},
			{enabled, "", false, "UID EXPUNGE 501", "UID EXPUNGE"},
			{enabled, "", false, `UID STORE 9 +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "", false, `UID COPY 101 "Labels/Two"`, "UID COPY"},
			{enabled, "Labels/One", true, "UID EXPUNGE 501", "UID EXPUNGE"},
			{enabled, "Labels/Two", true, `UID STORE 9 +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "INBOX", true, `UID COPY 101 "Labels/Two"`, "UID COPY"},
			{enabled, "INBOX", false, `UID STORE 101 +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "INBOX", false, "UID EXPUNGE 102", "UID EXPUNGE"},
			{enabled, "INBOX", false, `UID COPY 101 "Labels/Missing"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101 "Labels"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101 "Labels/Ghost"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101 "Folders/Other"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101 "Labels/Lookalike"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101 "INBOX"`, "UID COPY"},
			{enabled, "Labels/One", false, `UID COPY 501 "Labels/Two"`, "UID COPY"},
			{enabled, "INBOX", false, `COPY 1 "Labels/Two"`, "COPY"},
			{enabled, "Labels/Two", false, `STORE 1 +FLAGS.SILENT (\Deleted)`, "STORE"},
			{enabled, "Labels/One", false, "EXPUNGE", "EXPUNGE"},
			{enabled, "Labels/One", false, "CLOSE", "CLOSE"},
			{enabled, "INBOX", false, `UID MOVE 101 "Labels/Two"`, "UID MOVE"},
			{enabled, "Labels/One", false, `UID MOVE 501 "Labels/Two"`, "UID MOVE"},
			{enabled, "Labels/Two", false, `UID STORE 9 FLAGS (\Deleted)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 9 FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/One", false, `UID STORE 501 -FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/One", false, `UID STORE 501 -FLAGS (\Deleted)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 9 +FLAGS.SILENT (\Deleted \Seen)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 9 +FLAGS.SILENT (\Seen)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 9 +FLAGS (\Deleted)`, "UID STORE"},
			{enabled, "INBOX", false, `UID COPY 101,102 "Labels/Two"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101:102 "Labels/Two"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY * "Labels/Two"`, "UID COPY"},
			{enabled, "INBOX", false, `UID COPY 101:* "Labels/Two"`, "UID COPY"},
			{enabled, "Labels/Two", false, `UID STORE 9,10 +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 9:10 +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE * +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/Two", false, `UID STORE 1:* +FLAGS.SILENT (\Deleted)`, "UID STORE"},
			{enabled, "Labels/One", false, "UID EXPUNGE 501,502", "UID EXPUNGE"},
			{enabled, "Labels/One", false, "UID EXPUNGE 501:502", "UID EXPUNGE"},
			{enabled, "Labels/One", false, "UID EXPUNGE *", "UID EXPUNGE"},
			{enabled, "Labels/One", false, "UID EXPUNGE 1:*", "UID EXPUNGE"},
		} {
			raw := connectClient(t, attempt.server, ImplicitTLS)
			raw.exchange(t, "a1", "LOGIN fixture-user@client.test not-a-secret")
			if attempt.selected != "" {
				verb := "SELECT"
				if attempt.examine {
					verb = "EXAMINE"
				}
				if completion := raw.exchange(t, "a2", fmt.Sprintf("%s %q", verb, attempt.selected)); !strings.HasPrefix(completion, "a2 OK") {
					t.Fatalf("%s %s = %q", verb, attempt.selected, completion)
				}
			}

			before := len(attempt.server.Commands())
			completion := raw.exchange(t, "a3", attempt.command)
			if !strings.HasPrefix(completion, "a3 NO") && !strings.HasPrefix(completion, "a3 BAD") {
				t.Fatalf("%s in %q (examine=%t) completion = %q, want refusal", attempt.command, attempt.selected, attempt.examine, completion)
			}
			requireRecorded(t, attempt.server, before, attempt.name)
			requireLabelSnapshot(t, attempt.server, baseline)
			_ = raw.connection.Close()
		}

		for _, capability := range []struct {
			server  *Server
			uidPlus bool
		}{{enabled, true}, {disabled, false}} {
			raw := connectClient(t, capability.server, ImplicitTLS)
			raw.command(t, "a0 CAPABILITY", "* CAPABILITY IMAP4rev1 AUTH=PLAIN")
			_ = raw.connection.Close()

			client := dialStateful(t, capability.server, ImplicitTLS)
			capabilities, err := client.Capability().Wait()
			if err != nil {
				t.Fatalf("CAPABILITY: %v", err)
			}
			if _, advertised := capabilities[imap.CapUIDPlus]; advertised != capability.uidPlus {
				t.Fatalf("authenticated UIDPLUS advertised = %t, want %t", advertised, capability.uidPlus)
			}
			if _, advertised := capabilities[imap.CapMove]; advertised {
				t.Fatal("label mode advertised MOVE")
			}
		}

		client := dialStateful(t, disabled, ImplicitTLS)
		selectMailbox(t, client, "Folders/Other", false)
		copied, err := client.Copy(imap.UIDSetNum(1), "Labels/One").Wait()
		if err != nil || copied.DestUIDs != nil {
			t.Fatalf("UID COPY without UIDPLUS = %+v, %v; want OK without COPYUID", copied, err)
		}
		want := seedLabelState(options(false))
		one := expectedView(&want, "Labels/One")
		one.Members = append(one.Members, Membership{Message: "C", UID: 503})
		one.UIDNext = 504
		requireLabelSnapshot(t, disabled, want)
		requireLabelWire(t, disabled, ImplicitTLS, want)
	})

	t.Run("mode_isolation_and_concurrency", func(t *testing.T) {
		t.Parallel()

		legacy, err := Start(Options{Mode: ImplicitTLS})
		if err != nil {
			t.Fatalf("start legacy server: %v", err)
		}
		t.Cleanup(func() { _ = legacy.Close() })

		triage := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: triageMailboxes()})
		triageBaseline := triage.Snapshot()

		options := LabelOptions{
			UIDPlus: true,
			Messages: []LabelMessage{
				{ID: "A", Body: labelBody("A")},
				{ID: "B", Body: labelBody("B"), Flags: []string{`\Seen`}},
				{ID: "C", Body: labelBody("C")},
				{ID: "D", Body: labelBody("D")},
			},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 102}, {Message: "C", UID: 103}, {Message: "D", UID: 104}}},
				{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "C", UID: 5}}},
				{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "D", UID: 40}}},
			},
		}
		labels := startLabels(t, ImplicitTLS, options)

		if _, ok := legacy.LabelSnapshot(); ok {
			t.Fatal("legacy server exposed label snapshot")
		}
		if _, ok := triage.LabelSnapshot(); ok {
			t.Fatal("triage server exposed label snapshot")
		}

		const copies = 5
		destinations := make(map[string]uint32)
		var destinationsMu sync.Mutex
		errs := make(chan error, 16)
		start := make(chan struct{})
		var group sync.WaitGroup
		run := func(name string, work func() error) {
			group.Go(func() {
				<-start
				if err := work(); err != nil {
					errs <- fmt.Errorf("%s: %w", name, err)
				}
			})
		}

		run("legacy read", func() error { return legacyReadScenario(legacy) })
		run("triage Seen and MOVE", func() error { return triageScenario(triage) })
		for _, message := range []struct {
			id  string
			uid imap.UID
		}{{"A", 101}, {"B", 102}} {
			run("label writer "+message.id, func() error {
				client, err := dialSelected(labels, "INBOX", false)
				if err != nil {
					return err
				}
				defer client.Close()

				for range copies {
					copied, err := client.Copy(imap.UIDSetNum(message.uid), "Labels/Two").Wait()
					if err != nil {
						return err
					}
					if copied.DestUIDs == nil {
						continue
					}

					destinationsMu.Lock()
					_, repeated := destinations[message.id]
					destinations[message.id] = uint32(copied.DestUIDs[0].Start)
					destinationsMu.Unlock()
					if repeated || copied.SourceUIDs.String() != fmt.Sprint(message.uid) {
						return fmt.Errorf("unexpected COPYUID %v %v", copied.SourceUIDs, copied.DestUIDs)
					}
				}

				return nil
			})
		}
		for reader := range 2 {
			run(fmt.Sprintf("label reader %d", reader), func() error {
				client, err := dialSelected(labels, "Labels/Two", true)
				if err != nil {
					return err
				}
				defer client.Close()

				for range 20 {
					snapshot, _ := labels.LabelSnapshot()
					if err := uniqueMemberships(snapshot); err != nil {
						return err
					}

					messages, err := client.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{UID: true, Flags: true}).Collect()
					if err != nil {
						return err
					}
					seen := make(map[imap.UID]bool)
					for _, message := range messages {
						if seen[message.UID] || message.UID < 40 || message.UID > 42 {
							return fmt.Errorf("FETCH returned unexpected UIDs: %+v", messages)
						}
						seen[message.UID] = true
					}
				}

				return nil
			})
		}
		close(start)
		group.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if t.Failed() {
			return
		}

		if err := legacy.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("legacy read scenario: %v", err)
		}

		triageWant := withFlags(triageBaseline, "INBOX", 1, `\Seen`)
		moved := triageWant[0].Messages[1]
		moved.UID = 2
		triageWant[0].Messages = slices.Delete(triageWant[0].Messages, 1, 2)
		triageWant[2].Messages = append(triageWant[2].Messages, moved)
		triageWant[2].UIDNext = 3
		requireSnapshot(t, triage, triageWant)

		if len(destinations) != 2 || !slices.Contains([]uint32{41, 42}, destinations["A"]) || destinations["A"]+destinations["B"] != 83 {
			t.Fatalf("COPYUID destinations = %v, want A and B at 41 and 42", destinations)
		}
		want := seedLabelState(options)
		two := expectedView(&want, "Labels/Two")
		first, second := "A", "B"
		if destinations["B"] == 41 {
			first, second = "B", "A"
		}
		two.Members = append(two.Members, Membership{Message: first, UID: 41}, Membership{Message: second, UID: 42})
		two.UIDNext = 43
		requireLabelSnapshot(t, labels, want)
		requireLabelWire(t, labels, ImplicitTLS, want)

		parsed, err := ParseTranscript(labels.Commands())
		if err != nil {
			t.Fatalf("parse label transcript: %v", err)
		}
		if got := countVerb(parsed, "UID COPY"); got != 2*copies {
			t.Fatalf("recorded %d UID COPY commands, want %d", got, 2*copies)
		}
		for _, verb := range []string{"UID STORE", "UID EXPUNGE", "UID MOVE", "EXPUNGE", "CLOSE"} {
			if got := countVerb(parsed, verb); got != 0 {
				t.Fatalf("label scenario sent %d %s commands", got, verb)
			}
		}
	})
}

// legacyReadScenario performs the legacy fixture's read-only flow.
func legacyReadScenario(server *Server) error {
	client, err := dialStatefulWithConfig(server, ImplicitTLS, server.ClientTLSConfig())
	if err != nil {
		return err
	}
	defer client.Close()

	if err := client.Login("fixture-user@client.test", "not-a-secret").Wait(); err != nil {
		return err
	}

	capabilities, err := client.Capability().Wait()
	if err != nil {
		return err
	}
	if _, advertised := capabilities[imap.CapUIDPlus]; advertised {
		return errors.New("legacy server advertised UIDPLUS")
	}

	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return err
	}
	if _, err := client.UIDSearch(&imap.SearchCriteria{}, nil).Wait(); err != nil {
		return err
	}

	whole := &imap.FetchItemBodySection{Peek: true}
	messages, err := client.Fetch(imap.UIDSetNum(101), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{whole}}).Collect()
	if err != nil {
		return err
	}
	if len(messages) != 1 || string(messages[0].FindBodySection(whole)) != syntheticMessageBody {
		return fmt.Errorf("legacy FETCH = %+v", messages)
	}

	return nil
}

// triageScenario performs the ordinary stateful Seen and native MOVE flow and
// checks that label-mode commands stay refused there.
func triageScenario(server *Server) error {
	client, err := dialSelected(server, "INBOX", false)
	if err != nil {
		return err
	}
	defer client.Close()

	capabilities, err := client.Capability().Wait()
	if err != nil {
		return err
	}
	if _, advertised := capabilities[imap.CapMove]; !advertised {
		return errors.New("triage server did not advertise MOVE")
	}
	if _, advertised := capabilities[imap.CapUIDPlus]; advertised {
		return errors.New("triage server advertised UIDPLUS")
	}

	if err := client.Store(imap.UIDSetNum(1), seenFlags(imap.StoreFlagsAdd, true), nil).Close(); err != nil {
		return fmt.Errorf("Seen STORE: %w", err)
	}
	moved, err := client.Move(imap.UIDSetNum(2), "Archive").Wait()
	if err != nil {
		return fmt.Errorf("UID MOVE: %w", err)
	}
	if moved.DestUIDs.String() != "2" {
		return fmt.Errorf("MOVE COPYUID destination = %v", moved.DestUIDs)
	}

	var imapError *imap.Error
	if _, err := client.Copy(imap.UIDSetNum(3), "Archive").Wait(); !errors.As(err, &imapError) {
		return fmt.Errorf("UID COPY error = %v, want refusal", err)
	}
	if err := client.Store(imap.UIDSetNum(3), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); !errors.As(err, &imapError) {
		return fmt.Errorf("Deleted STORE error = %v, want refusal", err)
	}
	if err := client.UIDExpunge(imap.UIDSetNum(3)).Close(); !errors.As(err, &imapError) {
		return fmt.Errorf("UID EXPUNGE error = %v, want refusal", err)
	}

	return nil
}

func labelBody(id string) string {
	return fmt.Sprintf("From: Fixture <fixture@croton.test>\r\nTo: Reader <reader@croton.test>\r\n"+
		"Subject: Synthetic label %s\r\nMessage-ID: <label-%s@croton.test>\r\n\r\nSYNTHETIC_LABEL_BODY_%s\r\n", id, id, id)
}

func startLabels(t *testing.T, mode TLSMode, options LabelOptions) *Server {
	t.Helper()

	server, err := Start(Options{Mode: mode, Labels: &options})
	if err != nil {
		t.Fatalf("start label server: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close label server: %v", err)
		}
	})

	return server
}

// dialSelected connects over implicit TLS, logs in and selects a mailbox
// without using testing helpers, so goroutines can call it.
func dialSelected(server *Server, mailbox string, readOnly bool) (*imapclient.Client, error) {
	client, err := dialStatefulWithConfig(server, ImplicitTLS, server.ClientTLSConfig())
	if err != nil {
		return nil, err
	}

	if err := client.Login("fixture-user@client.test", "not-a-secret").Wait(); err != nil {
		_ = client.Close()
		return nil, err
	}

	if _, err := client.Select(mailbox, &imap.SelectOptions{ReadOnly: readOnly}).Wait(); err != nil {
		_ = client.Close()
		return nil, err
	}

	return client, nil
}

// seedLabelState declares the snapshot a seed must produce. Seeds list
// members in UID order; UIDNEXT is one past the highest seeded UID.
func seedLabelState(options LabelOptions) LabelState {
	state := LabelState{Messages: make([]LabelMessage, 0, len(options.Messages)), Views: make([]LabelViewState, 0, len(options.Views))}
	for _, message := range options.Messages {
		state.Messages = append(state.Messages, LabelMessage{ID: message.ID, Flags: slices.Clone(message.Flags), Body: message.Body})
	}
	for _, view := range options.Views {
		uidNext := uint32(1)
		for _, member := range view.Members {
			uidNext = max(uidNext, member.UID+1)
		}
		state.Views = append(state.Views, LabelViewState{
			Name:        view.Name,
			Role:        view.Role,
			Attributes:  slices.Clone(view.Attributes),
			UIDValidity: view.UIDValidity,
			UIDNext:     uidNext,
			Members:     append(make([]Membership, 0, len(view.Members)), view.Members...),
		})
	}

	return state
}

func expectedView(state *LabelState, name string) *LabelViewState {
	for index := range state.Views {
		if state.Views[index].Name == name {
			return &state.Views[index]
		}
	}

	panic("unknown label view " + name)
}

// viewMailbox renders the expected wire view: shared flags, then the
// membership's own \Deleted marker.
func viewMailbox(state LabelState, name string) MailboxState {
	view := expectedView(&state, name)
	mailbox := MailboxState{Name: view.Name, Attributes: view.Attributes, UIDValidity: view.UIDValidity, UIDNext: view.UIDNext}
	for _, member := range view.Members {
		index := slices.IndexFunc(state.Messages, func(message LabelMessage) bool { return message.ID == member.Message })
		message := state.Messages[index]
		flags := slices.Clone(message.Flags)
		if member.Deleted {
			flags = append(flags, `\Deleted`)
		}
		mailbox.Messages = append(mailbox.Messages, MessageState{UID: member.UID, Flags: flags, Body: message.Body})
	}

	return mailbox
}

func requireLabelSnapshot(t *testing.T, server *Server, want LabelState) {
	t.Helper()

	got, ok := server.LabelSnapshot()
	if !ok || !reflect.DeepEqual(got, want) {
		t.Fatalf("label snapshot = %+v\nwant %+v", got, want)
	}
}

// requireLabelWire reads every selectable view through a fresh real client.
func requireLabelWire(t *testing.T, server *Server, mode TLSMode, want LabelState) {
	t.Helper()

	client := dialStateful(t, server, mode)
	defer client.Close()

	for _, view := range want.Views {
		if slices.Contains(view.Attributes, `\Noselect`) || slices.Contains(view.Attributes, `\NonExistent`) {
			continue
		}

		data := selectMailbox(t, client, view.Name, true)
		if data.UIDValidity != view.UIDValidity || data.NumMessages != uint32(len(view.Members)) || uint32(data.UIDNext) != view.UIDNext {
			t.Fatalf("EXAMINE %q = %+v, want %+v", view.Name, data, view)
		}
		requireFetchMatches(t, client, viewMailbox(want, view.Name))
	}
}

func uniqueMemberships(state LabelState) error {
	for _, view := range state.Views {
		messages := make(map[string]bool)
		uids := make(map[uint32]bool)
		for _, member := range view.Members {
			if messages[member.Message] || uids[member.UID] {
				return fmt.Errorf("view %q has duplicate membership %+v", view.Name, view.Members)
			}
			messages[member.Message], uids[member.UID] = true, true
		}
	}

	return nil
}

func storeDeleted(t *testing.T, client *imapclient.Client, uid imap.UID) []*imapclient.FetchMessageBuffer {
	t.Helper()

	updates, err := client.Store(imap.UIDSetNum(uid), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Collect()
	if err != nil {
		t.Fatalf("UID STORE %d +FLAGS.SILENT (\\Deleted): %v", uid, err)
	}

	return updates
}

func fetchBody(t *testing.T, client *imapclient.Client, uid imap.UID) string {
	t.Helper()

	whole := &imap.FetchItemBodySection{Peek: true}
	messages, err := client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{whole}}).Collect()
	if err != nil || len(messages) != 1 {
		t.Fatalf("UID FETCH %d = %+v, %v", uid, messages, err)
	}

	return string(messages[0].FindBodySection(whole))
}

func fetchFlags(t *testing.T, client *imapclient.Client, uid imap.UID) []string {
	t.Helper()

	messages, err := client.Fetch(imap.UIDSetNum(uid), &imap.FetchOptions{UID: true, Flags: true}).Collect()
	if err != nil || len(messages) != 1 {
		t.Fatalf("UID FETCH %d FLAGS = %+v, %v", uid, messages, err)
	}

	return flagStrings(messages[0].Flags)
}

func countVerb(commands []TranscriptCommand, verb string) int {
	count := 0
	for _, command := range commands {
		if command.Verb == verb {
			count++
		}
	}

	return count
}

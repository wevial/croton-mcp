package testkit

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// labelStage is one closed label command stage. Expected changes are literal
// edits of the seeded identities in labelFaultOptions.
type labelStage struct {
	name     string
	selector LabelFault
	// mark stores \Deleted on A at Labels/One UID 501 before the stage.
	mark       bool
	command    string
	completion string
	run        func(*imapclient.Client) error
	// pipelined is a second, different mutation sent behind the target.
	pipelined string
	applied   func(*LabelState)
}

func labelStages() []labelStage {
	return []labelStage{
		{
			name:       "copy",
			selector:   LabelFault{Command: "UID COPY", View: "INBOX", UID: "101", Destination: "Labels/Two"},
			command:    `UID COPY 101 "Labels/Two"`,
			completion: " OK [COPYUID 8002 101 502] COPY completed",
			run: func(client *imapclient.Client) error {
				_, err := client.Copy(imap.UIDSetNum(101), "Labels/Two").Wait()
				return err
			},
			pipelined: `UID COPY 102 "Labels/Two"`,
			applied: func(state *LabelState) {
				two := expectedView(state, "Labels/Two")
				two.Members = append(two.Members, Membership{Message: "A", UID: 502})
				two.UIDNext = 503
			},
		},
		{
			name:       "deleted_store",
			selector:   LabelFault{Command: "UID STORE", View: "Labels/One", UID: "501"},
			command:    `UID STORE 501 +FLAGS.SILENT (\Deleted)`,
			completion: " OK STORE completed",
			run: func(client *imapclient.Client) error {
				return client.Store(imap.UIDSetNum(501), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
			},
			pipelined: `UID STORE 502 +FLAGS.SILENT (\Deleted)`,
			applied:   markLabelA,
		},
		{
			name:       "uid_expunge",
			selector:   LabelFault{Command: "UID EXPUNGE", View: "Labels/One", UID: "501"},
			mark:       true,
			command:    "UID EXPUNGE 501",
			completion: " OK EXPUNGE completed",
			run: func(client *imapclient.Client) error {
				return client.UIDExpunge(imap.UIDSetNum(501)).Close()
			},
			// A replayed removal of the unrelated premarked B would be collateral.
			pipelined: "UID EXPUNGE 101",
			applied:   expungeLabelA,
		},
	}
}

func TestLabelFaults(t *testing.T) {
	t.Parallel()

	t.Run("exact_selector_one_shot", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
			server := startLabels(t, mode, labelFaultOptions())
			want := seedLabelState(labelFaultOptions())

			copySelector := LabelFault{Command: "UID COPY", View: "INBOX", UID: "101", Destination: "Labels/Two", Boundary: BeforeApplication, Action: DropConnection}
			storeSelector := LabelFault{Command: "UID STORE", View: "Labels/One", UID: "501", Boundary: BeforeApplication, Action: DropConnection}
			expungeSelector := LabelFault{Command: "UID EXPUNGE", View: "Labels/One", UID: "501", Boundary: BeforeApplication, Action: DropConnection}
			for name, fault := range map[string]LabelFault{
				"zero UID":                     withLabelFault(storeSelector, func(fault *LabelFault) { fault.UID = "0" }),
				"empty UID":                    withLabelFault(storeSelector, func(fault *LabelFault) { fault.UID = "" }),
				"leading-zero UID":             withLabelFault(storeSelector, func(fault *LabelFault) { fault.UID = "0501" }),
				"multi-UID":                    withLabelFault(storeSelector, func(fault *LabelFault) { fault.UID = "501,502" }),
				"UID range":                    withLabelFault(expungeSelector, func(fault *LabelFault) { fault.UID = "501:502" }),
				"UID wildcard":                 withLabelFault(expungeSelector, func(fault *LabelFault) { fault.UID = "*" }),
				"missing view":                 withLabelFault(storeSelector, func(fault *LabelFault) { fault.View = "" }),
				"unknown view":                 withLabelFault(storeSelector, func(fault *LabelFault) { fault.View = "Labels/Missing" }),
				"missing COPY destination":     withLabelFault(copySelector, func(fault *LabelFault) { fault.Destination = "" }),
				"unknown COPY destination":     withLabelFault(copySelector, func(fault *LabelFault) { fault.Destination = "Labels/Missing" }),
				"STORE destination":            withLabelFault(storeSelector, func(fault *LabelFault) { fault.Destination = "Labels/Two" }),
				"UID MOVE":                     withLabelFault(copySelector, func(fault *LabelFault) { fault.Command = "UID MOVE" }),
				"bare EXPUNGE":                 withLabelFault(expungeSelector, func(fault *LabelFault) { fault.Command = "EXPUNGE" }),
				"lowercase verb":               withLabelFault(copySelector, func(fault *LabelFault) { fault.Command = "uid copy" }),
				"read verb":                    withLabelFault(storeSelector, func(fault *LabelFault) { fault.Command = "UID FETCH" }),
				"missing boundary":             withLabelFault(storeSelector, func(fault *LabelFault) { fault.Boundary = 0 }),
				"missing action":               withLabelFault(storeSelector, func(fault *LabelFault) { fault.Action = 0 }),
				"after-application refusal":    withLabelFault(expungeSelector, func(fault *LabelFault) { fault.Boundary, fault.Action = AfterApplication, RejectCommand }),
				"definitive refusal of STORE":  withLabelFault(storeSelector, func(fault *LabelFault) { fault.Action = RejectCommand }),
				"definitive refusal of COPY":   withLabelFault(copySelector, func(fault *LabelFault) { fault.Action = RejectCommand }),
				"unknown action":               withLabelFault(storeSelector, func(fault *LabelFault) { fault.Action = RejectCommand + 1 }),
				"unknown application boundary": withLabelFault(storeSelector, func(fault *LabelFault) { fault.Boundary = AfterApplication + 1 }),
			} {
				if handle, err := server.InjectLabelFault(fault); err == nil || handle != nil {
					t.Fatalf("%s: %s selector %+v was armed", mode, name, fault)
				}
			}
			requireArmedLabelFaults(t, server, 0)

			triage := startStateful(t, mode, StatefulOptions{Move: true, Mailboxes: triageMailboxes()})
			legacy, err := Start(Options{Mode: mode})
			if err != nil {
				t.Fatalf("start legacy server: %v", err)
			}
			t.Cleanup(func() { _ = legacy.Close() })
			for name, outside := range map[string]*Server{"stateful": triage, "legacy": legacy} {
				if handle, err := outside.InjectLabelFault(copySelector); err == nil || handle != nil {
					t.Fatalf("%s: %s server armed a label fault", mode, name)
				}
			}

			copyFault := injectLabelFault(t, server, copySelector)
			storeFault := injectLabelFault(t, server, storeSelector)
			expungeFault := injectLabelFault(t, server, expungeSelector)
			requireArmedLabelFaults(t, server, 3)

			// COPY: the same UID from another folder, another UID, another
			// destination, unsupported grammar, a read, a STORE in a folder and
			// an EXAMINE selection all run normally.
			copier := labelSession(t, server, mode, "c", "Folders/Work")
			requireExchange(t, copier, "c3", `UID COPY 101 "Labels/Two"`, "c3 OK COPY completed")
			requireExchange(t, copier, "c4", "SELECT INBOX", "c4 OK [READ-WRITE] SELECT completed")
			requireExchange(t, copier, "c5", `UID COPY 102 "Labels/Two"`, "c5 OK [COPYUID 8002 102 502] COPY completed")
			requireExchange(t, copier, "c6", `UID COPY 101 "Labels/Three"`, "c6 OK [COPYUID 8003 101 1] COPY completed")
			requireExchange(t, copier, "c7", `UID COPY 101 "Labels/Two" extra`, "c7 BAD UID COPY requires a UID set and a mailbox")
			requireExchange(t, copier, "c8", "UID FETCH 101 (UID FLAGS)", "c8 OK FETCH completed")
			requireExchange(t, copier, "c9", `UID STORE 501 +FLAGS.SILENT (\Deleted)`, `c9 NO [CANNOT] \Deleted STORE requires a selected label view`)
			requireExchange(t, copier, "d1", "EXAMINE INBOX", "d1 OK [READ-ONLY] EXAMINE completed")
			requireExchange(t, copier, "d2", `UID COPY 101 "Labels/Two"`, "d2 NO [READ-ONLY] mailbox was opened with EXAMINE")
			requireExchange(t, copier, "d3", "SELECT INBOX", "d3 OK [READ-WRITE] SELECT completed")
			two := expectedView(&want, "Labels/Two")
			two.Members = append(two.Members, Membership{Message: "B", UID: 502})
			two.UIDNext = 503
			three := expectedView(&want, "Labels/Three")
			three.Members = append(three.Members, Membership{Message: "A", UID: 1})
			three.UIDNext = 2
			requireLabelSnapshot(t, server, want)
			requireNotTriggered(t, copyFault, storeFault, expungeFault)

			requireLost(t, copier, `d4 UID COPY 101 "Labels/Two"`)
			awaitSignal(t, copyFault.Triggered(), "COPY fault trigger")
			awaitSignal(t, copyFault.Finished(), "COPY faulted connection close")
			requireLabelSnapshot(t, server, want)
			requireSessionTarget(t, server, copyFault, "c2", "d4")
			requireNotTriggered(t, storeFault, expungeFault)

			// STORE: the same UID in another label view, another UID, other
			// flag forms, a read and an EXPUNGE of another UID run normally.
			storer := labelSession(t, server, mode, "s", "Labels/Two")
			requireExchange(t, storer, "s3", `UID STORE 501 +FLAGS.SILENT (\Deleted)`, "s3 OK STORE completed")
			requireExchange(t, storer, "s4", `SELECT "Labels/One"`, "s4 OK [READ-WRITE] SELECT completed")
			requireExchange(t, storer, "s5", `UID STORE 502 +FLAGS.SILENT (\Deleted)`, "s5 OK STORE completed")
			requireExchange(t, storer, "s6", `UID STORE 501 +FLAGS (\Deleted)`, "s6 NO [CANNOT] STORE other than +FLAGS.SILENT is refused by the stateful fixture")
			requireExchange(t, storer, "s7", `UID STORE 501 +FLAGS.SILENT (\Seen)`, `s7 NO [CANNOT] changing flags other than \Deleted is refused by the stateful fixture`)
			requireExchange(t, storer, "s8", `UID STORE 501 -FLAGS.SILENT (\Deleted)`, "s8 NO [CANNOT] STORE other than +FLAGS.SILENT is refused by the stateful fixture")
			requireExchange(t, storer, "s9", "UID FETCH 501 (UID FLAGS)", "s9 OK FETCH completed")
			requireExchange(t, storer, "t1", "UID EXPUNGE 502", "t1 OK EXPUNGE completed")
			two.Members[1].Deleted = true
			one := expectedView(&want, "Labels/One")
			one.Members = one.Members[:2]
			requireLabelSnapshot(t, server, want)
			requireNotTriggered(t, storeFault, expungeFault)

			requireLost(t, storer, `t2 UID STORE 501 +FLAGS.SILENT (\Deleted)`)
			awaitSignal(t, storeFault.Triggered(), "STORE fault trigger")
			awaitSignal(t, storeFault.Finished(), "STORE faulted connection close")
			requireLabelSnapshot(t, server, want)
			requireSessionTarget(t, server, storeFault, "s2", "t2")
			requireNotTriggered(t, expungeFault)

			// UID EXPUNGE: the same UID in another label view, another UID, a
			// multi-UID set, bare EXPUNGE and a read run normally.
			expunger := labelSession(t, server, mode, "e", "Labels/Two")
			requireExchange(t, expunger, "e3", "UID EXPUNGE 501", "e3 OK EXPUNGE completed")
			requireExchange(t, expunger, "e4", `SELECT "Labels/One"`, "e4 OK [READ-WRITE] SELECT completed")
			requireExchange(t, expunger, "e5", "UID EXPUNGE 502", "e5 OK EXPUNGE completed")
			requireExchange(t, expunger, "e6", "UID EXPUNGE 501,502", "e6 NO [CANNOT] a UID set other than one UID is refused by the stateful fixture")
			requireExchange(t, expunger, "e7", "EXPUNGE", "e7 NO [CANNOT] EXPUNGE is refused by the stateful fixture")
			requireExchange(t, expunger, "e8", "UID SEARCH UID 501", "e8 OK SEARCH completed")
			two.Members = slices.Delete(two.Members, 1, 2)
			requireLabelSnapshot(t, server, want)
			requireNotTriggered(t, expungeFault)

			requireLost(t, expunger, "e9 UID EXPUNGE 501")
			awaitSignal(t, expungeFault.Triggered(), "UID EXPUNGE fault trigger")
			awaitSignal(t, expungeFault.Finished(), "UID EXPUNGE faulted connection close")
			requireLabelSnapshot(t, server, want)
			requireSessionTarget(t, server, expungeFault, "e2", "e9")
			requireArmedLabelFaults(t, server, 0)

			// Every selector was one-shot: the same commands, issued again on
			// new connections, complete and apply normally.
			replay := labelSession(t, server, mode, "r", "INBOX")
			requireExchange(t, replay, "r3", `UID COPY 101 "Labels/Two"`, "r3 OK [COPYUID 8002 101 503] COPY completed")
			requireExchange(t, replay, "r4", `SELECT "Labels/One"`, "r4 OK [READ-WRITE] SELECT completed")
			requireExchange(t, replay, "r5", `UID STORE 501 +FLAGS.SILENT (\Deleted)`, "r5 OK STORE completed")
			requireExchange(t, replay, "r6", "UID EXPUNGE 501", "r6 OK EXPUNGE completed")

			// Literal final state: B and A joined Labels/Two, D left it; A
			// joined Labels/Three; only the premarked B remains in Labels/One.
			requireLabelSnapshot(t, server, LabelState{
				Messages: seedLabelState(labelFaultOptions()).Messages,
				Views: []LabelViewState{
					{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, UIDNext: 104, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 102}, {Message: "C", UID: 103}}},
					{Name: "Folders/Work", Role: FolderRole, UIDValidity: 7002, UIDNext: 102, Members: []Membership{{Message: "D", UID: 101}}},
					{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, UIDNext: 503, Members: []Membership{{Message: "B", UID: 101, Deleted: true}}},
					{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, UIDNext: 504, Members: []Membership{{Message: "C", UID: 10}, {Message: "B", UID: 502}, {Message: "A", UID: 503}}},
					{Name: "Labels/Three", Role: LabelRole, UIDValidity: 8003, UIDNext: 2, Members: []Membership{{Message: "A", UID: 1}}},
				},
			})
			requireLabelWire(t, server, mode, mustLabelSnapshot(t, server))
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatal(err)
			}
		}

		// A COPY the dispatcher refuses for UID exhaustion leaves its fault
		// armed. A COPY of an existing member still completes there, so it
		// remains eligible.
		exhausted := LabelOptions{
			UIDPlus:  true,
			Messages: []LabelMessage{{ID: "A", Body: labelBody("A")}, {ID: "B", Body: labelBody("B")}},
			Views: []LabelViewSeed{
				{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 102}}},
				{Name: "Labels/Full", Role: LabelRole, UIDValidity: 8009, Members: []Membership{{Message: "B", UID: 4294967294}}},
			},
		}
		server := startLabels(t, ImplicitTLS, exhausted)
		limited := injectLabelFault(t, server, LabelFault{Command: "UID COPY", View: "INBOX", UID: "101", Destination: "Labels/Full", Boundary: BeforeApplication, Action: DropConnection})
		existing := injectLabelFault(t, server, LabelFault{Command: "UID COPY", View: "INBOX", UID: "102", Destination: "Labels/Full", Boundary: BeforeApplication, Action: DropConnection})

		session := labelSession(t, server, ImplicitTLS, "u", "INBOX")
		requireExchange(t, session, "u3", `UID COPY 101 "Labels/Full"`, "u3 NO [LIMIT] destination mailbox has no UIDs left")
		requireNotTriggered(t, limited, existing)
		requireArmedLabelFaults(t, server, 2)
		requireLabelSnapshot(t, server, seedLabelState(exhausted))

		requireLost(t, session, `u4 UID COPY 102 "Labels/Full"`)
		awaitSignal(t, existing.Finished(), "existing-member COPY fault close")
		requireSessionTarget(t, server, existing, "u2", "u4")
		requireNotTriggered(t, limited)
		requireArmedLabelFaults(t, server, 1)
		requireLabelSnapshot(t, server, seedLabelState(exhausted))
	})

	t.Run("lost_stage_responses", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
			for _, stage := range labelStages() {
				for _, boundary := range []FaultBoundary{BeforeApplication, AfterApplication} {
					t.Run(fmt.Sprintf("%s_%s_%s", mode, stage.name, boundary), func(t *testing.T) {
						t.Parallel()

						server := startLabels(t, mode, labelFaultOptions())
						want := seedLabelState(labelFaultOptions())
						client := dialStateful(t, server, mode)
						selectMailbox(t, client, stage.selector.View, false)
						if stage.mark {
							storeDeleted(t, client, 501)
							markLabelA(&want)
						}
						requireLabelSnapshot(t, server, want)

						selector := stage.selector
						selector.Boundary, selector.Action = boundary, DropConnection
						fault := injectLabelFault(t, server, selector)

						var imapError *imap.Error
						if err := stage.run(client); err == nil || errors.As(err, &imapError) {
							t.Fatalf("%s error = %v, want a lost completion", stage.command, err)
						}
						awaitSignal(t, fault.Triggered(), "fault trigger")
						awaitSignal(t, fault.Finished(), "faulted connection close")

						completion := fault.Completion()
						if boundary == AfterApplication {
							stage.applied(&want)
							if !strings.HasSuffix(completion, stage.completion) {
								t.Fatalf("withheld completion = %q, want suffix %q", completion, stage.completion)
							}
						} else if completion != "" {
							t.Fatalf("before-application completion = %q", completion)
						}

						requireLabelSnapshot(t, server, want)
						requireLabelWire(t, server, mode, want)
						if _, matches := requireFaultTarget(t, server, fault); matches != 1 {
							t.Fatalf("recorded %d %s %s commands, want 1", matches, selector.Command, selector.UID)
						}
						if err := server.AssertNoInsecureAuthentication(); err != nil {
							t.Fatal(err)
						}
					})
				}
			}
		}
	})

	t.Run("definitive_expunge_partial_state", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []TLSMode{ImplicitTLS, StartTLS} {
			server := startLabels(t, mode, labelFaultOptions())
			want := seedLabelState(labelFaultOptions())

			session := labelSession(t, server, mode, "p", "Labels/One")
			requireLine(t, session, `p3 UID STORE 501 +FLAGS.SILENT (\Deleted)`, "p3 OK STORE completed")
			markLabelA(&want)
			requireLabelSnapshot(t, server, want)

			fault := injectLabelFault(t, server, LabelFault{Command: "UID EXPUNGE", View: "Labels/One", UID: "501", Boundary: BeforeApplication, Action: RejectCommand})

			// The first line is the tagged NO: no EXPUNGE notification precedes it.
			requireLine(t, session, "p4 UID EXPUNGE 501", "p4 NO [UNAVAILABLE] UID EXPUNGE refused by fixture fault")
			awaitSignal(t, fault.Triggered(), "refusal trigger")
			select {
			case <-fault.Finished():
				t.Fatalf("%s: refusal closed the connection", mode)
			default:
			}
			if completion := fault.Completion(); completion != "" {
				t.Fatalf("%s: refusal withheld completion %q", mode, completion)
			}
			requireSessionTarget(t, server, fault, "p2", "p4")

			// A stays marked in Labels/One beside the unrelated premarked B;
			// the folder and the other labels keep their exact state.
			requireLabelSnapshot(t, server, want)
			requireLabelWire(t, server, mode, want)
			reader := dialStateful(t, server, mode)
			selectMailbox(t, reader, "Labels/One", true)
			if flags := fetchFlags(t, reader, 501); !slices.Equal(flags, []string{`\Seen`, `\Deleted`}) || fetchBody(t, reader, 501) != labelBody("A") {
				t.Fatalf("%s: Labels/One UID 501 (A) flags = %v, want the partial marker", mode, flags)
			}
			if flags := fetchFlags(t, reader, 101); !slices.Equal(flags, []string{`\Flagged`, `\Deleted`}) || fetchBody(t, reader, 101) != labelBody("B") {
				t.Fatalf("%s: Labels/One UID 101 (B) flags = %v", mode, flags)
			}
			selectMailbox(t, reader, "INBOX", true)
			if flags := fetchFlags(t, reader, 101); !slices.Equal(flags, []string{`\Seen`}) || fetchBody(t, reader, 101) != labelBody("A") {
				t.Fatalf("%s: INBOX UID 101 (A) flags = %v", mode, flags)
			}

			// The session stays usable. A new, explicitly issued expunge after
			// the spent fault removes A only.
			requireLine(t, session, "p5 NOOP", "p5 OK NOOP completed")
			requireLine(t, session, "p6 UID EXPUNGE 501", "* 2 EXPUNGE")
			if line := session.readLine(t); line != "p6 OK EXPUNGE completed" {
				t.Fatalf("%s: explicit UID EXPUNGE completion = %q", mode, line)
			}
			expungeLabelA(&want)
			requireLabelSnapshot(t, server, want)
			requireLabelWire(t, server, mode, want)

			_ = session.Close()
			awaitSignal(t, fault.Finished(), "refused connection close")
			parsed := parseRecorded(t, server)
			if got := countVerb(parsed, "UID EXPUNGE"); got != 2 {
				t.Fatalf("%s: recorded %d UID EXPUNGE commands, want the refused one and the explicit one", mode, got)
			}
		}

		// A real client library sees a definitive NO with the fixed code.
		server := startLabels(t, ImplicitTLS, labelFaultOptions())
		client := dialStateful(t, server, ImplicitTLS)
		selectMailbox(t, client, "Labels/One", false)
		storeDeleted(t, client, 501)
		injectLabelFault(t, server, LabelFault{Command: "UID EXPUNGE", View: "Labels/One", UID: "501", Boundary: BeforeApplication, Action: RejectCommand})

		var imapError *imap.Error
		err := client.UIDExpunge(imap.UIDSetNum(501)).Close()
		if !errors.As(err, &imapError) || imapError.Type != imap.StatusResponseTypeNo || imapError.Code != imap.ResponseCodeUnavailable {
			t.Fatalf("refused UID EXPUNGE error = %v, want NO [UNAVAILABLE]", err)
		}
		want := seedLabelState(labelFaultOptions())
		markLabelA(&want)
		requireLabelSnapshot(t, server, want)
		if mailbox := client.Mailbox(); mailbox == nil || mailbox.NumMessages != 3 {
			t.Fatalf("client consumed an EXPUNGE: %+v", mailbox)
		}
	})

	t.Run("held_response_observation", func(t *testing.T) {
		t.Parallel()

		for _, stage := range labelStages() {
			for _, boundary := range []FaultBoundary{BeforeApplication, AfterApplication} {
				t.Run(stage.name+"_"+boundary.String(), func(t *testing.T) {
					t.Parallel()

					server := startLabels(t, ImplicitTLS, labelFaultOptions())
					want := seedLabelState(labelFaultOptions())
					unrelated := dialStateful(t, server, ImplicitTLS)
					client := dialStateful(t, server, ImplicitTLS)
					selectMailbox(t, client, stage.selector.View, false)
					if stage.mark {
						storeDeleted(t, client, 501)
						markLabelA(&want)
					}

					selector := stage.selector
					selector.Boundary, selector.Action = boundary, HoldResponse
					fault := injectLabelFault(t, server, selector)

					result := startCall(func() error { return stage.run(client) })
					awaitSignal(t, fault.Triggered(), "held target")
					if boundary == AfterApplication {
						stage.applied(&want)
					}

					// The hold blocks neither the store nor other connections.
					requireLabelSnapshot(t, server, want)
					requireLabelViews(t, unrelated, want)
					requireLabelWire(t, server, ImplicitTLS, want)

					select {
					case err := <-result:
						t.Fatalf("held %s completed: %v", stage.command, err)
					case <-time.After(faultClientDeadline):
					}
					_ = client.Close()
					if err := awaitResult(t, result, "timed-out target"); err == nil {
						t.Fatalf("timed-out %s reported success", stage.command)
					}
					awaitSignal(t, fault.Finished(), "held connection close")

					if completion := fault.Completion(); boundary == BeforeApplication && completion != "" || boundary == AfterApplication && !strings.HasSuffix(completion, stage.completion) {
						t.Fatalf("withheld completion = %q", completion)
					}
					requireLabelSnapshot(t, server, want)
					requireLabelViews(t, unrelated, want)
					if _, matches := requireFaultTarget(t, server, fault); matches != 1 {
						t.Fatalf("recorded %d held targets, want 1", matches)
					}

					// Server.Close ends a response that is still held and never
					// applies the target afterwards. The same command is
					// eligible again; after application it is a no-op.
					interrupted := injectLabelFault(t, server, selector)
					held := dialStateful(t, server, ImplicitTLS)
					selectMailbox(t, held, stage.selector.View, false)

					result = startCall(func() error { return stage.run(held) })
					awaitSignal(t, interrupted.Triggered(), "second held target")
					if err := awaitResult(t, startCall(server.Close), "fixture Close"); err != nil {
						t.Fatalf("close fixture: %v", err)
					}
					if err := awaitResult(t, result, "interrupted target"); err == nil {
						t.Fatalf("interrupted %s reported success", stage.command)
					}
					awaitSignal(t, interrupted.Finished(), "interrupted connection close")
					requireLabelSnapshot(t, server, want)
				})

				t.Run(stage.name+"_"+boundary.String()+"_pipelined_negative_control", func(t *testing.T) {
					t.Parallel()

					server := startLabels(t, StartTLS, labelFaultOptions())
					want := seedLabelState(labelFaultOptions())
					session := labelSession(t, server, StartTLS, "n", stage.selector.View)
					if stage.mark {
						requireLine(t, session, `n3 UID STORE 501 +FLAGS.SILENT (\Deleted)`, "n3 OK STORE completed")
						markLabelA(&want)
					}

					selector := stage.selector
					selector.Boundary, selector.Action = boundary, HoldResponse
					fault := injectLabelFault(t, server, selector)

					// A deliberate negative control, not Croton behavior: the
					// target and a second mutation are written together.
					if _, err := fmt.Fprintf(session.connection, "n4 %s\r\nn5 %s\r\n", stage.command, stage.pipelined); err != nil {
						t.Fatalf("write pipelined commands: %v", err)
					}
					awaitSignal(t, fault.Triggered(), "held target")
					if boundary == AfterApplication {
						stage.applied(&want)
					}
					requireLabelSnapshot(t, server, want)

					// Half-closing ends the hold only after the server has read
					// every pipelined line. No response byte is released.
					if err := session.connection.(*tls.Conn).CloseWrite(); err != nil {
						t.Fatalf("half-close held session: %v", err)
					}
					if released, _ := io.ReadAll(session.reader); len(released) != 0 {
						t.Fatalf("held session released %q", released)
					}
					awaitSignal(t, fault.Finished(), "held connection close")

					target, _ := requireFaultTarget(t, server, fault)
					if target.Tag != "n4" {
						t.Fatalf("held target tag = %q", target.Tag)
					}
					pipelined := recordedTag(t, server, "n5")
					want5, err := ParseTranscriptCommand(Command{Raw: "n5 " + stage.pipelined})
					if err != nil {
						t.Fatalf("parse expected pipelined command: %v", err)
					}
					if pipelined.Sequence != target.Sequence+1 || pipelined.ConnectionID != target.ConnectionID || !pipelined.TLS ||
						pipelined.Verb != want5.Verb || pipelined.Set != want5.Set {
						t.Fatalf("pipelined command = %+v, target = %+v", pipelined, target)
					}

					// Recorded is not dispatched: only the target's own boundary
					// result is visible, now and on fresh reads.
					requireLabelSnapshot(t, server, want)
					requireLabelWire(t, server, StartTLS, want)
				})
			}
		}
	})

	t.Run("expunge_transcript_operands", func(t *testing.T) {
		t.Parallel()

		valid := []struct {
			raw  string
			want TranscriptCommand
		}{
			{"x1 UID EXPUNGE 501", TranscriptCommand{Tag: "x1", Verb: "UID EXPUNGE", Arguments: "501", Set: "501", Ranges: []SetRange{{501, 501}}}},
			{"x2 UID EXPUNGE 501,7,4294967295", TranscriptCommand{
				Tag: "x2", Verb: "UID EXPUNGE", Arguments: "501,7,4294967295",
				Set: "501,7,4294967295", Ranges: []SetRange{{501, 501}, {7, 7}, {4294967295, 4294967295}},
			}},
			{"x3 UID EXPUNGE 503:501", TranscriptCommand{Tag: "x3", Verb: "UID EXPUNGE", Arguments: "503:501", Set: "503:501", Ranges: []SetRange{{503, 501}}}},
			{"x4 UID EXPUNGE *", TranscriptCommand{Tag: "x4", Verb: "UID EXPUNGE", Arguments: "*", Set: "*", Ranges: []SetRange{{0, 0}}}},
			{"x5 UID EXPUNGE 501:*,9", TranscriptCommand{Tag: "x5", Verb: "UID EXPUNGE", Arguments: "501:*,9", Set: "501:*,9", Ranges: []SetRange{{501, 0}, {9, 9}}}},
			{"x6 uid ExPunge 501", TranscriptCommand{Tag: "x6", Verb: "UID EXPUNGE", Arguments: "501", Set: "501", Ranges: []SetRange{{501, 501}}}},
			{"x7 EXPUNGE", TranscriptCommand{Tag: "x7", Verb: "EXPUNGE"}},
			{"x8 expunge", TranscriptCommand{Tag: "x8", Verb: "EXPUNGE"}},
			{`x9 UID COPY 101 "UID EXPUNGE 501"`, TranscriptCommand{
				Tag: "x9", Verb: "UID COPY", Arguments: `101 "UID EXPUNGE 501"`,
				Set: "101", Ranges: []SetRange{{101, 101}}, Destination: "UID EXPUNGE 501",
			}},
			{`y1 UID STORE 501 +FLAGS.SILENT (\Deleted)`, TranscriptCommand{
				Tag: "y1", Verb: "UID STORE", Arguments: `501 +FLAGS.SILENT (\Deleted)`,
				Set: "501", Ranges: []SetRange{{501, 501}}, Store: "+FLAGS", Silent: true, Flags: []string{`\Deleted`},
			}},
			{`y2 UID MOVE 501 "Labels/Two"`, TranscriptCommand{
				Tag: "y2", Verb: "UID MOVE", Arguments: `501 "Labels/Two"`,
				Set: "501", Ranges: []SetRange{{501, 501}}, Destination: "Labels/Two",
			}},
		}

		commands := make([]Command, 0, len(valid))
		want := make([]TranscriptCommand, 0, len(valid))
		for index, testCase := range valid {
			command := Command{Sequence: 40 + index, ConnectionID: 3 + index%2, Raw: testCase.raw, TLS: index%3 != 0}
			testCase.want.Sequence, testCase.want.ConnectionID, testCase.want.TLS = command.Sequence, command.ConnectionID, command.TLS
			commands = append(commands, command)
			want = append(want, testCase.want)

			if got, err := ParseTranscriptCommand(command); err != nil || !reflect.DeepEqual(got, testCase.want) {
				t.Errorf("parse %q = %+v, %v\nwant %+v", testCase.raw, got, err, testCase.want)
			}
		}
		if got, err := ParseTranscript(commands); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ParseTranscript = %+v, %v\nwant %+v", got, err, want)
		}

		for _, operand := range []string{
			"",
			" ",
			"0",
			"0501",
			"501,0",
			"4294967296",
			"99999999999999999999",
			`"501"`,
			"{3}",
			"{3+}",
			"(501)",
			"501 502",
			"501 ",
			" 501",
			"501,",
			"501::502",
			"501x",
			"-1",
			"501\x00",
			"5\t01",
			"501\x7f",
		} {
			raw := "z1 UID EXPUNGE"
			if operand != "" {
				raw += " " + operand
			}
			got, err := ParseTranscriptCommand(Command{Sequence: 9, ConnectionID: 2, Raw: raw, TLS: true})
			if !errors.Is(err, errMalformedTranscript) || !reflect.DeepEqual(got, TranscriptCommand{}) {
				t.Errorf("parse %q = %+v, %v; want malformed", raw, got, err)
				continue
			}
			if trimmed := strings.TrimSpace(operand); trimmed != "" && strings.Contains(err.Error(), trimmed) {
				t.Errorf("parse %q error echoes the operand: %v", raw, err)
			}
		}

		// A parsed multi-UID set is still refused by the label dispatcher and
		// does not consume an armed single-UID fault.
		server := startLabels(t, ImplicitTLS, labelFaultOptions())
		fault := injectLabelFault(t, server, LabelFault{Command: "UID EXPUNGE", View: "Labels/One", UID: "501", Boundary: BeforeApplication, Action: DropConnection})
		session := labelSession(t, server, ImplicitTLS, "m", "Labels/One")
		requireExchange(t, session, "m3", `UID STORE 501 +FLAGS.SILENT (\Deleted)`, "m3 OK STORE completed")
		for index, set := range []string{"501:502", "501,501", "101:*", "*"} {
			tag := fmt.Sprintf("m%d", 4+index)
			requireExchange(t, session, tag, "UID EXPUNGE "+set, tag+" NO [CANNOT] a UID set other than one UID is refused by the stateful fixture")
			if parsed := recordedTag(t, server, tag); parsed.Verb != "UID EXPUNGE" || parsed.Set != set {
				t.Fatalf("recorded %s = %+v", tag, parsed)
			}
		}
		requireNotTriggered(t, fault)
		state := seedLabelState(labelFaultOptions())
		markLabelA(&state)
		requireLabelSnapshot(t, server, state)
	})

	t.Run("mode_and_lock_isolation", func(t *testing.T) {
		t.Parallel()

		legacy, err := Start(Options{Mode: ImplicitTLS})
		if err != nil {
			t.Fatalf("start legacy server: %v", err)
		}
		t.Cleanup(func() { _ = legacy.Close() })
		triage := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: triageMailboxes()})
		triageBaseline := triage.Snapshot()
		labels := startLabels(t, ImplicitTLS, labelFaultOptions())

		// Selector APIs stay in their own modes, and ordinary selectors keep
		// their existing actions.
		selector := LabelFault{Command: "UID STORE", View: "Labels/One", UID: "501", Boundary: AfterApplication, Action: HoldResponse}
		for name, server := range map[string]*Server{"legacy": legacy, "stateful": triage} {
			if _, err := server.InjectLabelFault(selector); err == nil {
				t.Fatalf("%s server armed a label fault", name)
			}
		}
		if _, err := labels.InjectFault(MutationFault{Command: "UID STORE", UIDs: "501", Boundary: BeforeApplication, Action: DropConnection}); err == nil {
			t.Fatal("label server armed an ordinary mutation fault")
		}
		if _, err := triage.InjectFault(MutationFault{Command: "UID EXPUNGE", UIDs: "3", Boundary: BeforeApplication, Action: DropConnection}); err == nil {
			t.Fatal("ordinary fault accepted UID EXPUNGE")
		}
		if _, err := triage.InjectFault(MutationFault{Command: "UID STORE", UIDs: "3", Boundary: BeforeApplication, Action: RejectCommand}); err == nil {
			t.Fatal("ordinary fault accepted a definitive refusal")
		}

		// One label session is held after its \Deleted STORE is applied.
		labelFault := injectLabelFault(t, labels, selector)
		heldClient := dialStateful(t, labels, ImplicitTLS)
		selectMailbox(t, heldClient, "Labels/One", false)
		held := startCall(func() error {
			return heldClient.Store(imap.UIDSetNum(501), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close()
		})
		awaitSignal(t, labelFault.Triggered(), "held label target")
		want := seedLabelState(labelFaultOptions())
		markLabelA(&want)

		// An ordinary triage hold keeps discarding unrecorded input.
		triageFault := injectFault(t, triage, MutationFault{Command: "UID STORE", UIDs: "3", Boundary: BeforeApplication, Action: HoldResponse})
		triageHeld := connectClient(t, triage, ImplicitTLS)
		t.Cleanup(func() { _ = triageHeld.Close() })
		requireExchange(t, triageHeld, "h1", "LOGIN fixture-user@client.test not-a-secret", "h1 OK LOGIN completed")
		requireExchange(t, triageHeld, "h2", "SELECT INBOX", "h2 OK [READ-WRITE] SELECT completed")
		if _, err := fmt.Fprint(triageHeld.connection, "h3 UID STORE 3 +FLAGS.SILENT (\\Seen)\r\nh4 UID MOVE 3 Trash\r\n"); err != nil {
			t.Fatalf("write triage commands: %v", err)
		}
		awaitSignal(t, triageFault.Triggered(), "held triage target")

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
		run("label success and refusals", func() error { return labelMatrixScenario(labels) })
		for reader := range 3 {
			run(fmt.Sprintf("label reader %d", reader), func() error {
				client, err := dialSelected(labels, "Labels/One", true)
				if err != nil {
					return err
				}
				defer client.Close()

				wantOne := normalizeMessages(viewMailbox(want, "Labels/One").Messages)
				for range 10 {
					snapshot, _ := labels.LabelSnapshot()
					if err := uniqueMemberships(snapshot); err != nil {
						return err
					}

					got, err := fetchView(client)
					if err != nil {
						return err
					}
					if !reflect.DeepEqual(got, wantOne) {
						return fmt.Errorf("Labels/One = %+v, want %+v", got, wantOne)
					}

					if _, err := client.Status("Labels/Two", &imap.StatusOptions{NumMessages: true, UIDNext: true}).Wait(); err != nil {
						return err
					}
				}

				return nil
			})
		}

		done := make(chan struct{})
		go func() {
			group.Wait()
			close(done)
		}()
		close(start)
		awaitSignal(t, done, "independent readers and writers")
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		if t.Failed() {
			return
		}

		select {
		case err := <-held:
			t.Fatalf("held label STORE completed: %v", err)
		default:
		}
		_ = heldClient.Close()
		if err := awaitResult(t, held, "held label target"); err == nil {
			t.Fatal("held label STORE reported success")
		}
		awaitSignal(t, labelFault.Finished(), "held label connection close")
		_ = triageHeld.Close()
		awaitSignal(t, triageFault.Finished(), "held triage connection close")

		if err := legacy.AssertReadOnlyCommands(); err != nil {
			t.Fatalf("legacy read scenario: %v", err)
		}
		for _, command := range triage.Commands() {
			if strings.HasPrefix(command.Raw, "h4 ") {
				t.Fatalf("ordinary hold recorded discarded input at %d", command.Sequence)
			}
		}
		triageWant := withFlags(triageBaseline, "INBOX", 1, `\Seen`)
		moved := triageWant[0].Messages[1]
		moved.UID = 2
		triageWant[0].Messages = slices.Delete(triageWant[0].Messages, 1, 2)
		triageWant[2].Messages = append(triageWant[2].Messages, moved)
		triageWant[2].UIDNext = 3
		requireSnapshot(t, triage, triageWant)

		three := expectedView(&want, "Labels/Three")
		three.Members = append(three.Members, Membership{Message: "B", UID: 1})
		three.UIDNext = 2
		requireLabelSnapshot(t, labels, want)
		requireLabelWire(t, labels, ImplicitTLS, want)
		if _, matches := requireFaultTarget(t, labels, labelFault); matches != 1 {
			t.Fatalf("recorded %d held label targets, want 1", matches)
		}
	})
}

// labelFaultOptions seeds literal identities: A, B and C in INBOX and D in
// Folders/Work. Labels/One holds the unrelated premarked B, A and C;
// Labels/Two holds C, and D at the UID A has in Labels/One.
func labelFaultOptions() LabelOptions {
	return LabelOptions{
		UIDPlus: true,
		Messages: []LabelMessage{
			{ID: "A", Body: labelBody("A"), Flags: []string{`\Seen`}},
			{ID: "B", Body: labelBody("B"), Flags: []string{`\Flagged`}},
			{ID: "C", Body: labelBody("C")},
			{ID: "D", Body: labelBody("D")},
		},
		Views: []LabelViewSeed{
			{Name: "INBOX", Role: FolderRole, UIDValidity: 7001, Members: []Membership{{Message: "A", UID: 101}, {Message: "B", UID: 102}, {Message: "C", UID: 103}}},
			{Name: "Folders/Work", Role: FolderRole, UIDValidity: 7002, Members: []Membership{{Message: "D", UID: 101}}},
			{Name: "Labels/One", Role: LabelRole, UIDValidity: 8001, Members: []Membership{{Message: "B", UID: 101, Deleted: true}, {Message: "A", UID: 501}, {Message: "C", UID: 502}}},
			{Name: "Labels/Two", Role: LabelRole, UIDValidity: 8002, Members: []Membership{{Message: "C", UID: 10}, {Message: "D", UID: 501}}},
			{Name: "Labels/Three", Role: LabelRole, UIDValidity: 8003},
		},
	}
}

// markLabelA marks A, Labels/One UID 501, as \Deleted.
func markLabelA(state *LabelState) {
	one := expectedView(state, "Labels/One")
	one.Members[1] = Membership{Message: "A", UID: 501, Deleted: true}
}

// expungeLabelA removes A, Labels/One UID 501, leaving B and C.
func expungeLabelA(state *LabelState) {
	one := expectedView(state, "Labels/One")
	one.Members = []Membership{{Message: "B", UID: 101, Deleted: true}, {Message: "C", UID: 502}}
}

func withLabelFault(fault LabelFault, change func(*LabelFault)) LabelFault {
	change(&fault)
	return fault
}

func injectLabelFault(t *testing.T, server *Server, selector LabelFault) *FaultHandle {
	t.Helper()

	fault, err := server.InjectLabelFault(selector)
	if err != nil {
		t.Fatalf("inject label fault: %v", err)
	}

	return fault
}

func requireArmedLabelFaults(t *testing.T, server *Server, want int) {
	t.Helper()

	server.mu.Lock()
	armed := len(server.labelFaults)
	server.mu.Unlock()

	if armed != want {
		t.Fatalf("armed label faults = %d, want %d", armed, want)
	}
}

func requireNotTriggered(t *testing.T, faults ...*FaultHandle) {
	t.Helper()

	for _, fault := range faults {
		select {
		case <-fault.Triggered():
			command, _ := fault.Command()
			t.Fatalf("fault %+v was consumed by command %d", fault.label, command.Sequence)
		default:
		}
	}
}

func mustLabelSnapshot(t *testing.T, server *Server) LabelState {
	t.Helper()

	state, ok := server.LabelSnapshot()
	if !ok {
		t.Fatal("server is not in label mode")
	}

	return state
}

// labelSession opens a raw verified-TLS session, logs in with tag prefix+"1"
// and, unless view is empty, selects it read-write with tag prefix+"2".
func labelSession(t *testing.T, server *Server, mode TLSMode, prefix, view string) *imapClient {
	t.Helper()

	client := connectClient(t, server, mode)
	t.Cleanup(func() { _ = client.Close() })
	if err := client.connection.SetDeadline(time.Now().Add(statefulNetworkTimeout)); err != nil {
		t.Fatalf("set session deadline: %v", err)
	}

	requireExchange(t, client, prefix+"1", "LOGIN fixture-user@client.test not-a-secret", prefix+"1 OK LOGIN completed")
	if view != "" {
		requireExchange(t, client, prefix+"2", "SELECT "+quoteMailbox(view), prefix+"2 OK [READ-WRITE] SELECT completed")
	}

	return client
}

func requireExchange(t *testing.T, client *imapClient, tag, command, want string) {
	t.Helper()

	if completion := client.exchange(t, tag, command); completion != want {
		t.Fatalf("%s %s completion = %q, want %q", tag, command, completion, want)
	}
}

// requireLine sends one command and requires its first response line.
func requireLine(t *testing.T, client *imapClient, command, want string) {
	t.Helper()

	client.command(t, command, want)
}

// requireLost sends one command and requires the connection to end without
// any response line.
func requireLost(t *testing.T, client *imapClient, command string) {
	t.Helper()

	if _, err := fmt.Fprintf(client.connection, "%s\r\n", command); err != nil {
		t.Fatalf("write %q: %v", command, err)
	}

	if line, err := client.reader.ReadString('\n'); err == nil || line != "" {
		t.Fatalf("%q response = %q, %v; want closure without a response", command, line, err)
	}
}

// parseRecorded parses every recorded command that has exact grammar.
// Malformed commands cannot be fault targets and are skipped.
func parseRecorded(t *testing.T, server *Server) []TranscriptCommand {
	t.Helper()

	var parsed []TranscriptCommand
	for _, command := range server.Commands() {
		if entry, err := ParseTranscriptCommand(command); err == nil {
			parsed = append(parsed, entry)
		}
	}

	return parsed
}

func recordedTag(t *testing.T, server *Server, tag string) TranscriptCommand {
	t.Helper()

	var found []TranscriptCommand
	for _, command := range parseRecorded(t, server) {
		if command.Tag == tag {
			found = append(found, command)
		}
	}
	if len(found) != 1 {
		t.Fatalf("recorded %d commands tagged %q, want 1", len(found), tag)
	}

	return found[0]
}

// requireFaultTarget checks the fault's recorded command against the
// independent transcript and selector. It returns the target and how many
// recorded commands have the target's verb and UID set.
func requireFaultTarget(t *testing.T, server *Server, fault *FaultHandle) (TranscriptCommand, int) {
	t.Helper()

	command, ok := fault.Command()
	if !ok {
		t.Fatal("fault has no recorded command")
	}

	var target TranscriptCommand
	matches := 0
	for _, entry := range parseRecorded(t, server) {
		if entry.Sequence == command.Sequence {
			target = entry
		}
		if entry.Verb == fault.label.Command && entry.Set == fault.label.UID {
			matches++
		}
	}

	if target.Sequence == 0 || target.ConnectionID != command.ConnectionID || target.TLS != command.TLS || !target.TLS ||
		target.Verb != fault.label.Command || target.Set != fault.label.UID || target.Ranges[0].Start != target.Ranges[0].Stop {
		t.Fatalf("fault command = %+v, transcript target = %+v", command, target)
	}
	if fault.label.Command == "UID COPY" && target.Destination != fault.label.Destination {
		t.Fatalf("fault COPY destination = %q, want %q", target.Destination, fault.label.Destination)
	}

	return target, matches
}

// requireSessionTarget checks that the fault consumed the command tagged
// targetTag on the same connection that selected with selectTag.
func requireSessionTarget(t *testing.T, server *Server, fault *FaultHandle, selectTag, targetTag string) {
	t.Helper()

	target, _ := requireFaultTarget(t, server, fault)
	selected := recordedTag(t, server, selectTag)
	if target.Tag != targetTag || target.ConnectionID != selected.ConnectionID || target.Sequence <= selected.Sequence {
		t.Fatalf("fault target = %+v, session SELECT = %+v", target, selected)
	}
}

// requireLabelViews reads every selectable view on an existing connection.
func requireLabelViews(t *testing.T, client *imapclient.Client, want LabelState) {
	t.Helper()

	for _, view := range want.Views {
		if slices.Contains(view.Attributes, `\Noselect`) || slices.Contains(view.Attributes, `\NonExistent`) {
			continue
		}

		selectMailbox(t, client, view.Name, true)
		requireFetchMatches(t, client, viewMailbox(want, view.Name))
	}
}

// fetchView reads the selected view without testing helpers, so goroutines
// can call it.
func fetchView(client *imapclient.Client) ([]MessageState, error) {
	whole := &imap.FetchItemBodySection{Peek: true}
	messages, err := client.Fetch(imap.UIDSet{imap.UIDRange{Start: 1, Stop: 0}}, &imap.FetchOptions{
		UID: true, Flags: true, BodySection: []*imap.FetchItemBodySection{whole},
	}).Collect()
	if err != nil {
		return nil, err
	}

	got := make([]MessageState, 0, len(messages))
	for _, message := range messages {
		got = append(got, MessageState{UID: uint32(message.UID), Flags: flagStrings(message.Flags), Body: string(message.FindBodySection(whole))})
	}

	return got, nil
}

// labelMatrixScenario runs an accepted label COPY and refusals that must stay
// refused while another session is held.
func labelMatrixScenario(server *Server) error {
	client, err := dialSelected(server, "INBOX", false)
	if err != nil {
		return err
	}
	defer client.Close()

	copied, err := client.Copy(imap.UIDSetNum(102), "Labels/Three").Wait()
	if err != nil {
		return fmt.Errorf("UID COPY: %w", err)
	}
	if copied.UIDValidity != 8003 || copied.SourceUIDs.String() != "102" || copied.DestUIDs.String() != "1" {
		return fmt.Errorf("COPYUID = %d %v %v", copied.UIDValidity, copied.SourceUIDs, copied.DestUIDs)
	}

	var imapError *imap.Error
	if _, err := client.Copy(imap.UIDSet{imap.UIDRange{Start: 101, Stop: 103}}, "Labels/Three").Wait(); !errors.As(err, &imapError) {
		return fmt.Errorf("range UID COPY error = %v, want refusal", err)
	}
	if err := client.Store(imap.UIDSetNum(101), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); !errors.As(err, &imapError) {
		return fmt.Errorf("folder Deleted STORE error = %v, want refusal", err)
	}
	if _, err := client.Select("Labels/Two", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		return err
	}
	if err := client.Store(imap.UIDSetNum(10), &imap.StoreFlags{Op: imap.StoreFlagsAdd, Silent: true, Flags: []imap.Flag{imap.FlagDeleted}}, nil).Close(); !errors.As(err, &imapError) {
		return fmt.Errorf("EXAMINE Deleted STORE error = %v, want refusal", err)
	}
	if err := client.UIDExpunge(imap.UIDSetNum(10)).Close(); !errors.As(err, &imapError) {
		return fmt.Errorf("EXAMINE UID EXPUNGE error = %v, want refusal", err)
	}

	return nil
}

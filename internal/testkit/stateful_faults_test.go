package testkit

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

// faultClientDeadline bounds how long a client waits for a held response.
// Holds never release, so expiry does not race with a completion.
const faultClientDeadline = 100 * time.Millisecond

type faultTarget struct {
	name       string
	command    string
	completion string
	run        func(*imapclient.Client, imap.UID) error
	applied    func([]MailboxState, uint32) []MailboxState
}

func faultTargets() []faultTarget {
	return []faultTarget{
		{
			name:       "uid_store",
			command:    "UID STORE",
			completion: " OK STORE completed",
			run: func(client *imapclient.Client, uid imap.UID) error {
				return client.Store(imap.UIDSetNum(uid), seenFlags(imap.StoreFlagsAdd, false), nil).Close()
			},
			applied: func(states []MailboxState, uid uint32) []MailboxState {
				state, _ := findState(states, "INBOX")
				flags := slices.Clone(state.Messages[slices.IndexFunc(state.Messages, func(message MessageState) bool { return message.UID == uid })].Flags)
				return withFlags(states, "INBOX", uid, append(flags, `\Seen`)...)
			},
		},
		{
			name:       "uid_move",
			command:    "UID MOVE",
			completion: " OK MOVE completed",
			run: func(client *imapclient.Client, uid imap.UID) error {
				_, err := client.Move(imap.UIDSetNum(uid), "Archive").Wait()
				return err
			},
			applied: func(states []MailboxState, uid uint32) []MailboxState {
				return withMoved(states, "INBOX", "Archive", uid)
			},
		},
	}
}

func TestStatefulFaults(t *testing.T) {
	t.Parallel()

	t.Run("lost_completion", func(t *testing.T) {
		t.Parallel()

		for _, target := range faultTargets() {
			t.Run(target.name, func(t *testing.T) {
				t.Parallel()

				server := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: triageMailboxes()})
				baseline := server.Snapshot()
				fault := injectFault(t, server, MutationFault{Command: target.command, UIDs: "2", Boundary: AfterApplication, Action: DropConnection})

				client := dialStateful(t, server, ImplicitTLS)
				selectMailbox(t, client, "INBOX", false)

				var imapError *imap.Error
				if err := target.run(client, 2); err == nil || errors.As(err, &imapError) {
					t.Fatalf("%s error = %v, want lost completion", target.command, err)
				}
				awaitSignal(t, fault.Triggered(), "fault trigger")
				awaitSignal(t, fault.Finished(), "faulted connection close")
				if completion := fault.Completion(); !strings.HasSuffix(completion, target.completion) {
					t.Fatalf("withheld completion = %q, want suffix %q", completion, target.completion)
				}

				want := target.applied(baseline, 2)
				requireSnapshot(t, server, want)
				requireDispatched(t, server, target.command, fault)

				reader := dialStateful(t, server, ImplicitTLS)
				for _, state := range want[:3] {
					selectMailbox(t, reader, state.Name, true)
					requireFetchMatches(t, reader, state)
				}
				if err := server.AssertNoInsecureAuthentication(); err != nil {
					t.Fatal(err)
				}

				// The fault is spent: a replay on a new connection completes
				// normally and cannot apply the change a second time.
				replay := dialStateful(t, server, ImplicitTLS)
				selectMailbox(t, replay, "INBOX", false)
				if err := target.run(replay, 2); err != nil {
					t.Fatalf("replayed %s: %v", target.command, err)
				}
				requireSnapshot(t, server, want)
			})
		}
	})

	t.Run("application_boundary_timeouts", func(t *testing.T) {
		t.Parallel()

		for _, target := range faultTargets() {
			for _, boundary := range []FaultBoundary{BeforeApplication, AfterApplication} {
				t.Run(target.name+"_"+boundary.String(), func(t *testing.T) {
					t.Parallel()

					server := startStateful(t, ImplicitTLS, StatefulOptions{Move: true, Mailboxes: triageMailboxes()})
					want := server.Snapshot()
					apply := func(uid uint32) {
						if boundary == AfterApplication {
							want = target.applied(want, uid)
						}
					}

					unrelated := dialStateful(t, server, ImplicitTLS)
					fault := injectFault(t, server, MutationFault{Command: target.command, UIDs: "1", Boundary: boundary, Action: HoldResponse})
					client := dialStateful(t, server, ImplicitTLS)
					selectMailbox(t, client, "INBOX", false)

					result := startCall(func() error { return target.run(client, 1) })
					awaitSignal(t, fault.Triggered(), "held target")
					apply(1)

					// The held response blocks neither shared state nor other connections.
					requireUsable(t, unrelated, want)
					requireSnapshot(t, server, want)

					select {
					case err := <-result:
						t.Fatalf("held %s completed: %v", target.command, err)
					case <-time.After(faultClientDeadline):
					}
					_ = client.Close()
					if err := awaitResult(t, result, "timed-out target"); err == nil {
						t.Fatalf("timed-out %s reported success", target.command)
					}
					awaitSignal(t, fault.Finished(), "held connection close")

					if completion := fault.Completion(); boundary == BeforeApplication && completion != "" || boundary == AfterApplication && !strings.HasSuffix(completion, target.completion) {
						t.Fatalf("withheld completion = %q", completion)
					}
					requireSnapshot(t, server, want)
					requireUsable(t, unrelated, want)
					requireUsable(t, dialStateful(t, server, ImplicitTLS), want)
					requireDispatched(t, server, target.command, fault)

					// Close interrupts a response that is still held and never
					// applies a pre-application target afterwards.
					interrupted := injectFault(t, server, MutationFault{Command: target.command, UIDs: "3", Boundary: boundary, Action: HoldResponse})
					held := dialStateful(t, server, ImplicitTLS)
					selectMailbox(t, held, "INBOX", false)

					result = startCall(func() error { return target.run(held, 3) })
					awaitSignal(t, interrupted.Triggered(), "second held target")
					apply(3)

					if err := awaitResult(t, startCall(server.Close), "fixture Close"); err != nil {
						t.Fatalf("close fixture: %v", err)
					}
					if err := awaitResult(t, result, "interrupted target"); err == nil {
						t.Fatalf("interrupted %s reported success", target.command)
					}
					awaitSignal(t, interrupted.Finished(), "interrupted connection close")
					requireSnapshot(t, server, want)
				})
			}
		}
	})

	t.Run("selector_validation", func(t *testing.T) {
		t.Parallel()

		server := startStateful(t, ImplicitTLS, StatefulOptions{Mailboxes: triageMailboxes()})
		valid := MutationFault{Command: "UID STORE", UIDs: "1:3", Boundary: BeforeApplication, Action: DropConnection}
		for _, change := range []func(*MutationFault){
			func(fault *MutationFault) { fault.Command = "UID COPY" },
			func(fault *MutationFault) { fault.Command = "STORE" },
			func(fault *MutationFault) { fault.Command = "uid store" },
			func(fault *MutationFault) { fault.UIDs = "" },
			func(fault *MutationFault) { fault.UIDs = "0" },
			func(fault *MutationFault) { fault.UIDs = "1 2" },
			func(fault *MutationFault) { fault.Boundary = 0 },
			func(fault *MutationFault) { fault.Action = 0 },
		} {
			fault := valid
			change(&fault)
			if _, err := server.InjectFault(fault); err == nil {
				t.Fatalf("invalid selector %+v was armed", fault)
			}
		}

		legacy, err := Start(Options{})
		if err != nil {
			t.Fatalf("start legacy server: %v", err)
		}
		t.Cleanup(func() { _ = legacy.Close() })
		if _, err := legacy.InjectFault(valid); err == nil {
			t.Fatal("legacy server armed a mutation fault")
		}
	})
}

func injectFault(t *testing.T, server *Server, selector MutationFault) *FaultHandle {
	t.Helper()

	fault, err := server.InjectFault(selector)
	if err != nil {
		t.Fatalf("inject fault: %v", err)
	}

	return fault
}

func startCall(call func() error) <-chan error {
	result := make(chan error, 1)
	go func() { result <- call() }()

	return result
}

func awaitResult(t *testing.T, result <-chan error, what string) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(statefulNetworkTimeout):
		t.Fatalf("%s did not return within %s", what, statefulNetworkTimeout)
		return nil
	}
}

func awaitSignal(t *testing.T, signal <-chan struct{}, what string) {
	t.Helper()

	select {
	case <-signal:
	case <-time.After(statefulNetworkTimeout):
		t.Fatalf("%s did not happen within %s", what, statefulNetworkTimeout)
	}
}

// requireUsable checks that a connection reads the expected INBOX and Archive.
func requireUsable(t *testing.T, client *imapclient.Client, want []MailboxState) {
	t.Helper()

	for _, name := range []string{"INBOX", "Archive"} {
		state, _ := findState(want, name)
		selectMailbox(t, client, name, true)
		requireFetchMatches(t, client, state)
	}
}

// requireDispatched asserts that the transcript holds exactly one command
// with this verb and that it is the fault's target on its connection.
func requireDispatched(t *testing.T, server *Server, verb string, fault *FaultHandle) {
	t.Helper()

	commands, err := ParseTranscript(server.Commands())
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}

	faulted, ok := fault.Command()
	if !ok {
		t.Fatal("fault has no recorded command")
	}

	var dispatched []TranscriptCommand
	for _, command := range commands {
		if command.Verb == verb {
			dispatched = append(dispatched, command)
		}
	}

	if len(dispatched) != 1 || dispatched[0].Sequence != faulted.Sequence || dispatched[0].ConnectionID != faulted.ConnectionID ||
		dispatched[0].Set != fault.selector.UIDs || !dispatched[0].TLS {
		t.Fatalf("%s transcript = %+v, fault command = %+v", verb, dispatched, faulted)
	}
}

func findState(states []MailboxState, name string) (MailboxState, bool) {
	for _, state := range states {
		if state.Name == name {
			return state, true
		}
	}

	return MailboxState{}, false
}

// withMoved returns a deep copy of states with one message moved to the
// destination's UIDNEXT.
func withMoved(states []MailboxState, source, destination string, uid uint32) []MailboxState {
	copied := withFlags(states, "", 0) // deep copy without flag changes

	var moved MessageState
	for index := range copied {
		if copied[index].Name == source {
			position := slices.IndexFunc(copied[index].Messages, func(message MessageState) bool { return message.UID == uid })
			moved = copied[index].Messages[position]
			copied[index].Messages = slices.Delete(copied[index].Messages, position, position+1)
		}
	}
	for index := range copied {
		if copied[index].Name == destination {
			moved.UID = copied[index].UIDNext
			copied[index].Messages = append(copied[index].Messages, moved)
			copied[index].UIDNext++
		}
	}

	return copied
}

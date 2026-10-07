package testkit

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"sync"
)

// FaultBoundary places a mutation fault relative to fixture state application.
type FaultBoundary uint8

const (
	// BeforeApplication fires once the target is read, before the fixture
	// checks the selection or changes state. The target is never applied.
	BeforeApplication FaultBoundary = iota + 1
	// AfterApplication fires after the fixture has dispatched the target and
	// released its state lock, before any response line is written.
	AfterApplication
)

// String returns the stable name of a fault boundary.
func (boundary FaultBoundary) String() string {
	switch boundary {
	case BeforeApplication:
		return "before_application"
	case AfterApplication:
		return "after_application"
	default:
		return fmt.Sprintf("unknown-%d", boundary)
	}
}

// FaultAction selects what the fixture does with the faulted connection.
type FaultAction uint8

const (
	// DropConnection closes the connection without writing any response line.
	DropConnection FaultAction = iota + 1
	// HoldResponse writes nothing and discards further input until the client
	// closes the connection or the fixture is closed. There is no release.
	// A label fault records that input instead; see InjectLabelFault.
	HoldResponse
	// RejectCommand writes a definitive tagged NO instead of applying the
	// target and keeps the connection open. Only a BeforeApplication label
	// fault on UID EXPUNGE accepts it.
	RejectCommand
)

// MutationFault selects one stateful mutation for a one-shot fault.
type MutationFault struct {
	// Command is "UID STORE" or "UID MOVE".
	Command string
	// UIDs is the exact UID set operand as sent, for example "2" or "1:3".
	UIDs     string
	Boundary FaultBoundary
	Action   FaultAction
}

// FaultHandle observes one armed fault.
type FaultHandle struct {
	selector  MutationFault
	label     LabelFault
	labelKey  labelFaultKey
	triggered chan struct{}
	finished  chan struct{}

	mu         sync.Mutex
	command    Command
	completion string
}

// InjectFault arms a one-shot fault on a stateful server. The first
// authenticated command whose parsed verb and UID set equal the selector
// consumes it, whatever the outcome of dispatch. Armed faults are matched in
// the order they were injected.
func (server *Server) InjectFault(fault MutationFault) (*FaultHandle, error) {
	if server.stateful == nil {
		return nil, errNotStateful
	}

	if fault.Command != "UID STORE" && fault.Command != "UID MOVE" {
		return nil, fmt.Errorf("testkit: fault command %q must be UID STORE or UID MOVE", fault.Command)
	}

	if _, err := parseSequenceSet(fault.UIDs); err != nil {
		return nil, fmt.Errorf("testkit: fault UID set %q: %w", fault.UIDs, err)
	}

	if fault.Boundary != BeforeApplication && fault.Boundary != AfterApplication {
		return nil, errors.New("testkit: fault needs an explicit application boundary")
	}

	if fault.Action != DropConnection && fault.Action != HoldResponse {
		return nil, errors.New("testkit: fault needs an explicit action")
	}

	handle := &FaultHandle{selector: fault, triggered: make(chan struct{}), finished: make(chan struct{})}

	server.mu.Lock()
	server.faults = append(server.faults, handle)
	server.mu.Unlock()

	return handle, nil
}

// Triggered is closed once the target reached its boundary.
func (handle *FaultHandle) Triggered() <-chan struct{} {
	return handle.triggered
}

// Finished is closed after the fixture has closed the faulted connection.
// After a RejectCommand label fault, that is when the connection later ends.
func (handle *FaultHandle) Finished() <-chan struct{} {
	return handle.finished
}

// Command returns the recorded target command once the fault has triggered.
func (handle *FaultHandle) Command() (Command, bool) {
	handle.mu.Lock()
	defer handle.mu.Unlock()

	return handle.command, handle.command.Sequence != 0
}

// Completion returns the tagged completion the fixture withheld after
// application. It is empty before application or before the fault triggers.
func (handle *FaultHandle) Completion() string {
	handle.mu.Lock()
	defer handle.mu.Unlock()

	return handle.completion
}

// claimFault removes and returns the first armed fault matching command.
func (server *Server) claimFault(command Command, authenticated bool) *FaultHandle {
	if !authenticated {
		return nil
	}

	parsed, err := ParseTranscriptCommand(command)
	if err != nil {
		return nil
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	for index, handle := range server.faults {
		if handle.selector.Command == parsed.Verb && handle.selector.UIDs == parsed.Set {
			server.faults = append(server.faults[:index], server.faults[index+1:]...)
			return handle
		}
	}

	return nil
}

// runFault applies the target when the boundary requires it, withholds every
// response line, and holds the connection when asked. The caller closes the
// connection and then calls finish.
func (server *Server) runFault(handle *FaultHandle, session *statefulSession, reader *bufio.Reader, command Command, tag string) {
	completion := ""
	if handle.selector.Boundary == AfterApplication {
		lines := []string{tagged(tag, "BAD malformed arguments")}
		if arguments, err := tokenizeArguments(command.Raw); err == nil {
			lines = server.statefulUID(session, tag, arguments)
		}
		completion = lines[len(lines)-1]
	}

	handle.mu.Lock()
	handle.command = command
	handle.completion = completion
	handle.mu.Unlock()
	close(handle.triggered)

	if handle.selector.Action == HoldResponse {
		// Nothing read here is recorded or dispatched. Server.Close closes the
		// underlying connection, which ends the copy.
		_, _ = io.Copy(io.Discard, reader)
	}
}

func (handle *FaultHandle) finish() {
	close(handle.finished)
}

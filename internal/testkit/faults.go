package testkit

import "fmt"

// MutationFault selects one stateful mutation for a one-shot fault. The
// fixture applies the target, then drops the connection without writing any
// response line.
type MutationFault struct {
	// Command is "UID STORE" or "UID MOVE".
	Command string
	// UIDs is the exact UID set operand as sent, for example "2" or "1:3".
	UIDs string
}

// FaultHandle observes one armed fault.
type FaultHandle struct {
	selector MutationFault
	finished chan struct{}
}

// InjectFault arms a one-shot fault on a stateful server. The first
// authenticated command whose verb and UID set equal the selector consumes
// it, whatever the outcome of dispatch. Armed faults are matched in the order
// they were injected.
func (server *Server) InjectFault(fault MutationFault) (*FaultHandle, error) {
	if server.stateful == nil {
		return nil, errNotStateful
	}

	if fault.Command != "UID STORE" && fault.Command != "UID MOVE" {
		return nil, fmt.Errorf("testkit: fault command %q must be UID STORE or UID MOVE", fault.Command)
	}

	handle := &FaultHandle{selector: fault, finished: make(chan struct{})}

	server.mu.Lock()
	server.faults = append(server.faults, handle)
	server.mu.Unlock()

	return handle, nil
}

// Finished is closed after the fixture has closed the faulted connection.
func (handle *FaultHandle) Finished() <-chan struct{} {
	return handle.finished
}

// claimFault removes and returns the first armed fault matching command.
func (server *Server) claimFault(command Command, authenticated bool) *FaultHandle {
	if !authenticated {
		return nil
	}

	parsed := ParseTranscriptCommand(command)

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

// runFault applies the target and withholds every response line. The caller
// closes the connection and then calls finish.
func (server *Server) runFault(session *statefulSession, command Command, tag string) {
	if arguments, err := tokenizeArguments(command.Raw); err == nil {
		server.statefulUID(session, tag, arguments)
	}
}

func (handle *FaultHandle) finish() {
	close(handle.finished)
}

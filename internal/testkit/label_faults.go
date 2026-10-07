package testkit

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"strings"
)

// labelFaultRefusal is the fixed tagged refusal a RejectCommand fault writes.
const labelFaultRefusal = "NO [UNAVAILABLE] UID EXPUNGE refused by fixture fault"

// LabelFault selects one label-mode mutation for a one-shot fault. Unlike
// MutationFault it is bound to the selected view and exact operands.
type LabelFault struct {
	// Command is "UID COPY", "UID STORE" or "UID EXPUNGE". UID STORE matches
	// only +FLAGS.SILENT (\Deleted).
	Command string
	// View is the existing view the target must be issued in.
	View string
	// UID is the exact single-UID operand as sent, for example "501".
	UID string
	// Destination is the existing UID COPY target. It is required for UID COPY
	// and must be empty otherwise.
	Destination string
	Boundary    FaultBoundary
	Action      FaultAction
}

// labelFaultKey is a target in canonical view names, so equal keys address
// the same selected view, UID and destination.
type labelFaultKey struct {
	command, view, uid, destination string
}

// InjectLabelFault arms a one-shot fault on a label-mode server. The first
// authenticated command that the label dispatcher would apply, in the
// selector's read-write selected view with the same verb, UID and
// destination, consumes it. Armed faults are matched in the order they were
// injected.
func (server *Server) InjectLabelFault(fault LabelFault) (*FaultHandle, error) {
	if server.labels == nil {
		return nil, errors.New("testkit: server is not in label mode")
	}

	switch fault.Command {
	case "UID COPY":
		if fault.Destination == "" {
			return nil, errors.New("testkit: UID COPY label fault needs a destination")
		}
	case "UID STORE", "UID EXPUNGE":
		if fault.Destination != "" {
			return nil, fmt.Errorf("testkit: %s label fault takes no destination", fault.Command)
		}
	default:
		return nil, fmt.Errorf("testkit: label fault command %q must be UID COPY, UID STORE or UID EXPUNGE", fault.Command)
	}

	if strings.ContainsAny(fault.UID, ",:*") {
		return nil, errors.New("testkit: label fault needs exactly one UID")
	}

	if _, err := parseSequenceNumber(fault.UID); err != nil {
		return nil, fmt.Errorf("testkit: label fault UID: %w", err)
	}

	if fault.Boundary != BeforeApplication && fault.Boundary != AfterApplication {
		return nil, errors.New("testkit: fault needs an explicit application boundary")
	}

	switch fault.Action {
	case DropConnection, HoldResponse:
	case RejectCommand:
		if fault.Command != "UID EXPUNGE" || fault.Boundary != BeforeApplication {
			return nil, errors.New("testkit: a definitive refusal needs UID EXPUNGE before application")
		}
	default:
		return nil, errors.New("testkit: fault needs an explicit action")
	}

	key := labelFaultKey{command: fault.Command, uid: fault.UID}

	server.labels.mu.Lock()
	view, destination := server.labels.find(fault.View), server.labels.find(fault.Destination)
	server.labels.mu.Unlock()

	if view == nil {
		return nil, errors.New("testkit: label fault view does not exist")
	}
	key.view = view.name

	if fault.Command == "UID COPY" {
		if destination == nil {
			return nil, errors.New("testkit: label fault destination does not exist")
		}
		key.destination = destination.name
	}

	handle := &FaultHandle{label: fault, labelKey: key, triggered: make(chan struct{}), finished: make(chan struct{})}

	server.mu.Lock()
	server.labelFaults = append(server.labelFaults, handle)
	server.mu.Unlock()

	return handle, nil
}

// claimLabelFault removes and returns the first armed label fault whose target
// is command. Commands the label dispatcher would refuse never match.
func (server *Server) claimLabelFault(session *statefulSession, command Command, authenticated bool) *FaultHandle {
	if server.labels == nil || !authenticated {
		return nil
	}

	server.mu.Lock()
	armed := len(server.labelFaults) > 0
	server.mu.Unlock()

	if !armed {
		return nil
	}

	key, ok := server.labelDispatchable(session, command)
	if !ok {
		return nil
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	for index, handle := range server.labelFaults {
		if handle.labelKey == key {
			server.labelFaults = append(server.labelFaults[:index], server.labelFaults[index+1:]...)
			return handle
		}
	}

	return nil
}

// labelDispatchable returns the canonical target of a command that the label
// dispatcher would apply: an exact single-UID COPY from a read-write folder to
// a selectable label view, or an exact \Deleted STORE or, with UIDPLUS, UID
// EXPUNGE in a read-write label view.
func (server *Server) labelDispatchable(session *statefulSession, command Command) (labelFaultKey, bool) {
	parsed, err := ParseTranscriptCommand(command)
	if err != nil || session.selected == "" || session.readOnly {
		return labelFaultKey{}, false
	}

	if strings.ContainsAny(parsed.Set, ",:*") {
		return labelFaultKey{}, false
	}

	switch parsed.Verb {
	case "UID COPY":
	case "UID STORE":
		if parsed.Store != "+FLAGS" || !parsed.Silent || len(parsed.Flags) != 1 || !isDeletedFlag(parsed.Flags[0]) {
			return labelFaultKey{}, false
		}
	case "UID EXPUNGE":
		if !server.labels.uidPlus {
			return labelFaultKey{}, false
		}
	default:
		return labelFaultKey{}, false
	}

	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	view := server.labels.find(session.selected)
	if view == nil {
		return labelFaultKey{}, false
	}

	key := labelFaultKey{command: parsed.Verb, view: view.name, uid: parsed.Set}
	if parsed.Verb != "UID COPY" {
		return key, view.role == LabelRole
	}

	destination := server.labels.find(parsed.Destination)
	if view.role != FolderRole || destination == nil || destination.role != LabelRole || !selectableAttributes(destination.attributes) {
		return labelFaultKey{}, false
	}
	key.destination = destination.name

	return key, true
}

// runLabelFault applies the target through the label dispatcher when the
// boundary requires it and withholds its response, then drops, holds or
// refuses. It reports whether the connection stays open, which only a
// refusal allows.
func (server *Server) runLabelFault(handle *FaultHandle, session *statefulSession, reader *bufio.Reader, writer *bufio.Writer, command Command, tag string) bool {
	completion := ""
	if handle.label.Boundary == AfterApplication {
		var withheld bytes.Buffer
		server.serveLabels(session, bufio.NewWriter(&withheld), tag, command.Name, command.Raw, command.TLS, true)

		lines := strings.Split(strings.TrimSuffix(withheld.String(), "\r\n"), "\r\n")
		completion = lines[len(lines)-1]
	}

	handle.mu.Lock()
	handle.command = command
	handle.completion = completion
	handle.mu.Unlock()
	close(handle.triggered)

	switch handle.label.Action {
	case RejectCommand:
		return server.writeLine(writer, tagged(tag, labelFaultRefusal)) == nil
	case HoldResponse:
		server.recordHeld(reader, command)
	}

	return false
}

// recordHeld records every complete line read on a held connection without
// dispatching it, until the client or Server.Close ends the connection.
func (server *Server) recordHeld(reader *bufio.Reader, target Command) {
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		raw := strings.TrimRight(line, "\r\n")
		_, name := parseCommand(raw)
		server.record(raw, name, target.TLS, target.ConnectionID)
	}
}

package mcpserver

import (
	"encoding/json"
	"io"
	"sync"
)

// Auditor emits allowlisted, metadata-only audit events as JSON lines. A nil
// Auditor is valid and records nothing.
type Auditor struct {
	mu     sync.Mutex
	writer io.Writer
}

// NewAuditor creates an auditor writing to the given diagnostic stream,
// normally stderr. A nil writer records nothing.
func NewAuditor(writer io.Writer) *Auditor {
	if writer == nil {
		return nil
	}

	return &Auditor{writer: writer}
}

// auditEvent is the complete allowlisted tool audit vocabulary. Every field is
// server-chosen metadata; no caller input, mailbox data, or wrapped error
// detail may reach it.
type auditEvent struct {
	Event     string `json:"event"`
	Tool      string `json:"tool"`
	Outcome   string `json:"outcome"`
	Code      string `json:"code,omitempty"`
	Truncated bool   `json:"truncated,omitempty"`
}

// ToolCall records one tool invocation outcome. Callers pass only the static
// tool name, outcome and error code constants, never caller input or error
// detail.
func (auditor *Auditor) ToolCall(tool, outcome, code string, truncated bool) {
	if auditor == nil {
		return
	}

	event := auditEvent{
		Event:     "tool_call",
		Tool:      tool,
		Outcome:   outcome,
		Code:      code,
		Truncated: truncated,
	}

	auditor.writeEvent(event)
}

func (auditor *Auditor) writeEvent(event any) {
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}

	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	_, _ = auditor.writer.Write(append(encoded, '\n'))
}

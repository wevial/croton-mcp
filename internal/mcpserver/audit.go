package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// ToolCall records one tool invocation outcome. Metadata-only: the tool name,
// outcome, and error code are each re-validated against fixed allowlists so
// even an unexpected upstream value cannot smuggle content into the log.
func (auditor *Auditor) ToolCall(tool, outcome, code string, truncated bool) {
	if auditor == nil {
		return
	}

	event := auditEvent{
		Event:     "tool_call",
		Tool:      sanitizeToolName(tool),
		Outcome:   sanitizeOutcome(outcome),
		Code:      sanitizeErrorCode(code),
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

func sanitizeToolName(tool string) string {
	switch tool {
	case "list_folders", "search_mail", "get_message", "get_thread", "list_attachments", "select_digest_candidates":
		return tool
	default:
		return "unknown_tool"
	}
}

func sanitizeOutcome(outcome string) string {
	if outcome == "ok" {
		return "ok"
	}

	return "error"
}

func sanitizeErrorCode(code string) string {
	switch code {
	case "", errInvalidArgument, errNotFound, errStaleID, errBoundsExceeded, errTimedOut, errCanceled, errUnavailable, errInternal:
		return code
	default:
		return errInternal
	}
}

// transportEndEvent is independent of tool audit metadata and contains no
// transport error detail.
type transportEndEvent struct {
	Event    string `json:"event"`
	Category string `json:"category"`
}

func (auditor *Auditor) transportEnd(err error) {
	if auditor == nil {
		return
	}

	category := "transport_failure"
	switch {
	case err == nil, errors.Is(err, io.EOF), errors.Is(err, mcp.ErrConnectionClosed):
		category = "normal_close"
	case errors.Is(err, context.Canceled):
		category = "canceled"
	case errors.Is(err, syscall.EPIPE):
		category = "client_disconnected"
	}

	auditor.writeEvent(transportEndEvent{Event: "transport_end", Category: category})
}

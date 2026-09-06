// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drivemcp

import (
	"encoding/json"
	"io"
	"sync"
)

// Auditor emits allowlisted, metadata-only Drive audit events as JSON lines.
// A nil Auditor is valid and records nothing.
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

// auditEvent is the complete allowlisted audit vocabulary. Every field is
// server-chosen metadata; no caller input, Drive path, node name, CLI output,
// or wrapped error detail may reach it.
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
	case "list_drive_entries", "get_drive_metadata", "get_drive_sharing_status":
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
	case "", errInvalidArgument, errBoundsExceeded, errTimedOut, errCanceled, errUnavailable, errInternal:
		return code
	default:
		return errInternal
	}
}

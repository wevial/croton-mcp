package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/wevial/croton-mcp/bridge"
)

// Triage is the opt-in Seen and native-move mutation surface. *bridge.Adapter
// satisfies it. The tools that use it are registered only when Options.Triage
// is non-nil.
type Triage interface {
	SetSeen(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, seen bool) ([]bridge.TriageResult, error)
	Move(ctx context.Context, mailbox string, uidValidity uint32, uids []uint32, destination string) ([]bridge.TriageResult, error)
}

// uidSchema advertises a positive 32-bit IMAP UID or UIDVALIDITY. It is a
// literal because the maximum does not fit int on 32-bit platforms.
const uidSchema = `{"type":"integer","minimum":1,"maximum":4294967295}`

// triageInput is the exact source identity of one approved payload. There is
// deliberately no approval, ordinal or opaque message-id field.
type triageInput struct {
	Mailbox     string   `json:"mailbox"`
	UIDValidity uint32   `json:"uidvalidity"`
	UIDs        []uint32 `json:"uids"`
}

// triageMoveInput adds the one exact destination an approval names.
type triageMoveInput struct {
	Mailbox     string   `json:"mailbox"`
	UIDValidity uint32   `json:"uidvalidity"`
	UIDs        []uint32 `json:"uids"`
	Destination string   `json:"destination"`
}

type triageResultRow struct {
	UID     uint32 `json:"uid"`
	Outcome string `json:"outcome"`
	Code    string `json:"code,omitempty"`
}

type triageResult struct {
	Results []triageResultRow `json:"results"`
}

// triageTool is a mutation tool definition with its client-facing hints.
type triageTool struct {
	toolDefinition
	destructive bool
	idempotent  bool
}

func triageToolDefinitions() []triageTool {
	return []triageTool{
		{
			toolDefinition: toolDefinition{
				name:        "mark_read",
				description: "Set the Seen flag on the given UIDs of one mailbox generation. Call only for an exact payload the user approved.",
				schema:      triageSchema(false),
				run:         runMarkRead,
			},
			idempotent: true,
		},
		{
			toolDefinition: toolDefinition{
				name:        "mark_unread",
				description: "Clear the Seen flag on the given UIDs of one mailbox generation. Call only for an exact payload the user approved.",
				schema:      triageSchema(false),
				run:         runMarkUnread,
			},
			idempotent: true,
		},
		{
			toolDefinition: toolDefinition{
				name:        "move_mail",
				description: "Natively move the given UIDs of one mailbox generation to one exact existing destination: INBOX, a Folders/ folder, or a root Sent, Drafts, Junk, Archive or Trash mailbox. Call only for an exact payload the user approved.",
				schema:      triageSchema(true),
				run:         runMoveMail,
			},
			destructive: true,
		},
	}
}

func triageSchema(destination bool) json.RawMessage {
	properties := map[string]json.RawMessage{
		"mailbox":     stringSchema(maxMailboxArgumentBytes),
		"uidvalidity": json.RawMessage(uidSchema),
		"uids":        json.RawMessage(`{"type":"array","minItems":1,"maxItems":` + itoa(bridge.MaxTriageUIDs) + `,"uniqueItems":true,"items":` + uidSchema + `}`),
	}
	required := []string{"mailbox", "uidvalidity", "uids"}
	if destination {
		properties["destination"] = stringSchema(maxMailboxArgumentBytes)
		required = append(required, "destination")
	}

	return objectSchema(properties, required)
}

func runMarkRead(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	return runSetSeen(ctx, deps, arguments, true)
}

func runMarkUnread(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	return runSetSeen(ctx, deps, arguments, false)
}

func runSetSeen(ctx context.Context, deps Options, arguments json.RawMessage, seen bool) (any, string) {
	var input triageInput
	if !decodeArguments(arguments, &input) || !validTriageInput(input) {
		return nil, errInvalidArgument
	}
	if deps.Triage == nil {
		return nil, errUnavailable
	}

	outcomes, err := deps.Triage.SetSeen(ctx, input.Mailbox, input.UIDValidity, input.UIDs, seen)
	if err != nil {
		return nil, mapAdapterError(err)
	}

	return triageResults(input.UIDs, outcomes)
}

func runMoveMail(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input triageMoveInput
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}
	if !validTriageInput(triageInput{Mailbox: input.Mailbox, UIDValidity: input.UIDValidity, UIDs: input.UIDs}) || !validMailboxArgument(input.Destination) {
		return nil, errInvalidArgument
	}
	if deps.Triage == nil {
		return nil, errUnavailable
	}

	outcomes, err := deps.Triage.Move(ctx, input.Mailbox, input.UIDValidity, input.UIDs, input.Destination)
	if err != nil {
		return nil, mapAdapterError(err)
	}

	return triageResults(input.UIDs, outcomes)
}

// triageResults pairs adapter outcomes with the requested UIDs in input order
// and reduces every code to the stable tool vocabulary.
func triageResults(uids []uint32, outcomes []bridge.TriageResult) (any, string) {
	if len(outcomes) != len(uids) {
		return nil, errInternal
	}

	result := triageResult{Results: make([]triageResultRow, 0, len(outcomes))}
	for index, outcome := range outcomes {
		if outcome.UID != uids[index] {
			return nil, errInternal
		}

		row := triageResultRow{UID: outcome.UID, Outcome: sanitizeTriageOutcome(outcome.Outcome)}
		if outcome.Code != "" {
			row.Code = mapAdapterError(&bridge.Error{Code: outcome.Code})
		}
		result.Results = append(result.Results, row)
	}

	return &result, ""
}

// validTriageInput applies the server-authoritative identity bounds: a named
// mailbox, a nonzero generation and 1 to 50 distinct nonzero UIDs.
func validTriageInput(input triageInput) bool {
	if !validMailboxArgument(input.Mailbox) || input.UIDValidity == 0 {
		return false
	}
	if len(input.UIDs) == 0 || len(input.UIDs) > bridge.MaxTriageUIDs {
		return false
	}

	seen := make(map[uint32]struct{}, len(input.UIDs))
	for _, uid := range input.UIDs {
		if _, duplicate := seen[uid]; uid == 0 || duplicate {
			return false
		}
		seen[uid] = struct{}{}
	}

	return true
}

// sanitizeTriageOutcome keeps the result vocabulary closed. Anything
// unexpected is reported as unknown, never as applied.
func sanitizeTriageOutcome(outcome string) string {
	switch outcome {
	case bridge.OutcomeApplied, bridge.OutcomeRefused, bridge.OutcomeNotAttempted:
		return outcome
	default:
		return bridge.OutcomeUnknown
	}
}

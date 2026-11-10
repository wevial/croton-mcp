package mcpserver

import (
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// maxToolResultBytes caps every serialized tool result. It matches the bridge
// layer's default output budget.
const maxToolResultBytes = 100000

// shrinkable results reduce their own content structurally, one step at a
// time, so truncation always re-marshals complete JSON values.
type shrinkable interface {
	shrinkForOutput() bool
}

// errOutputExceeded reports that a result still exceeds the budget after
// every structural shrink; it maps to bounds_exceeded.
var errOutputExceeded = errors.New("mcpserver: result exceeds the output budget")

// encodeBounded serializes a tool result within maxToolResultBytes. Oversize
// results are shrunk structurally and re-marshaled; serialized JSON is never
// byte-sliced, so output remains syntactically valid. A result that still
// exceeds the budget once it cannot shrink further fails with
// errOutputExceeded and nothing is written.
func encodeBounded(result any) ([]byte, bool, error) {
	truncated := false
	for {
		encoded, err := json.Marshal(result)
		if err != nil {
			return nil, false, err
		}
		if len(encoded) <= maxToolResultBytes {
			return encoded, truncated, nil
		}

		shrinker, ok := result.(shrinkable)
		if !ok || !shrinker.shrinkForOutput() {
			return nil, false, errOutputExceeded
		}

		truncated = true
	}
}

// halveRunes drops the trailing half of a string on a rune boundary.
func halveRunes(value string) string {
	if value == "" {
		return ""
	}

	half := len(value) / 2
	for half > 0 && !utf8.RuneStart(value[half]) {
		half--
	}

	return value[:half]
}

func (result *listFoldersResult) shrinkForOutput() bool {
	if len(result.Folders) == 0 {
		return false
	}

	result.Folders = result.Folders[:len(result.Folders)/2]
	result.Truncated = true
	return true
}

func (result *searchMailResult) shrinkForOutput() bool {
	if len(result.Results) == 0 {
		return false
	}

	result.Results = result.Results[:len(result.Results)/2]
	result.Truncated = true
	result.NextCursor = ""
	return true
}

func (result *getMessageResult) shrinkForOutput() bool {
	switch {
	case result.Text != "":
		result.Text = halveRunes(result.Text)
	case len(result.Attachments) > 0:
		result.Attachments = result.Attachments[:len(result.Attachments)/2]
	default:
		return false
	}

	result.Truncation.Output = true
	result.Truncated = true
	return true
}

// Thread shrinking drops whole unavailable-member records before whole
// nodes, so every retained node keeps its own identity and truncation flags
// and no record or node is ever manufactured.
func (result *getThreadResult) shrinkForOutput() bool {
	switch {
	case len(result.UnavailableMembers) > 0:
		result.UnavailableMembers = result.UnavailableMembers[:len(result.UnavailableMembers)/2]
	case len(result.Nodes) > 1:
		result.Nodes = result.Nodes[:(len(result.Nodes)+1)/2]
	default:
		return false
	}

	result.Truncated = true
	return true
}

func (result *listAttachmentsResult) shrinkForOutput() bool {
	if len(result.Attachments) == 0 {
		return false
	}

	result.Attachments = result.Attachments[:len(result.Attachments)/2]
	result.Truncation.Output = true
	result.Truncated = true
	return true
}

func (result *selectDigestResult) shrinkForOutput() bool {
	if len(result.Candidates) == 0 {
		return false
	}

	result.Candidates = result.Candidates[:len(result.Candidates)/2]
	result.Truncated = true
	return true
}

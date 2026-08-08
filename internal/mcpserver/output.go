package mcpserver

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/wevial/croton-mcp/bridge"
)

// maxToolResultBytes caps every serialized tool result. It matches the bridge
// layer's default output budget.
const maxToolResultBytes = 100000

// shrinkable results reduce their own content structurally, one step at a
// time, so truncation always re-marshals complete JSON values.
type shrinkable interface {
	shrinkForOutput() bool
}

type toolShapedFallback interface {
	fallbackForOutput() any
}

// encodeBounded serializes a tool result within maxToolResultBytes. Oversize
// results are shrunk structurally and re-marshaled; serialized JSON is never
// byte-sliced, so output remains syntactically valid.
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
			if fallback, ok := result.(toolShapedFallback); ok {
				encoded, err := json.Marshal(fallback.fallbackForOutput())
				if err != nil {
					return nil, false, err
				}
				if len(encoded) <= maxToolResultBytes {
					return encoded, true, nil
				}
			}
			return []byte(`{"truncated":true}`), true, nil
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

	result.Truncated = true
	return true
}

func (result *getThreadResult) shrinkForOutput() bool {
	if len(result.Nodes) <= 1 {
		return false
	}

	remaining := (len(result.Nodes) + 1) / 2
	result.Nodes = result.Nodes[:remaining]
	result.Truncated = true
	return true
}

func (result *listAttachmentsResult) shrinkForOutput() bool {
	if len(result.Attachments) == 0 {
		return false
	}

	result.Attachments = result.Attachments[:len(result.Attachments)/2]
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

func fallbackString(value string) string {
	const maximum = 1024
	if len(value) <= maximum {
		return value
	}

	boundary := maximum
	for boundary > 0 && !utf8.RuneStart(value[boundary]) {
		boundary--
	}
	return value[:boundary]
}

func (result *listFoldersResult) fallbackForOutput() any {
	return &listFoldersResult{Folders: []folderResult{}, Truncated: true}
}

func (result *searchMailResult) fallbackForOutput() any {
	return &searchMailResult{Results: []messageResult{}, Truncated: true}
}

func (result *getMessageResult) fallbackForOutput() any {
	return &getMessageResult{
		ID:          fallbackString(result.ID),
		Mailbox:     fallbackString(result.Mailbox),
		Headers:     bridge.CanonicalHeaders{},
		TextSource:  fallbackString(result.TextSource),
		Attachments: []attachmentResult{},
		Truncated:   true,
	}
}

func (result *getThreadResult) fallbackForOutput() any {
	return &getThreadResult{
		ID:        fallbackString(result.ID),
		Mailbox:   fallbackString(result.Mailbox),
		Nodes:     []threadNodeResult{},
		Truncated: true,
	}
}

func (result *listAttachmentsResult) fallbackForOutput() any {
	return &listAttachmentsResult{
		ID:          fallbackString(result.ID),
		Mailbox:     fallbackString(result.Mailbox),
		Attachments: []attachmentResult{},
		Truncated:   true,
	}
}

func (result *selectDigestResult) fallbackForOutput() any {
	return &selectDigestResult{
		Mailbox:       fallbackString(result.Mailbox),
		Candidates:    []messageResult{},
		TotalMessages: result.TotalMessages,
		UnseenCount:   result.UnseenCount,
		Truncated:     true,
	}
}

package bridge

import (
	"cmp"
	"slices"
	"strconv"
	"time"
)

// ThreadMessage is an already-fetched, header-only message available for local thread resolution.
type ThreadMessage struct {
	Key        string           `json:"key,omitempty"`
	Headers    CanonicalHeaders `json:"headers"`
	ReceivedAt time.Time        `json:"receivedAt,omitempty"`
}

// ThreadOptions configures bounded, deterministic local thread resolution.
type ThreadOptions struct {
	Limits NormalizeLimits
}

// ThreadNode records a message and its resolved direct parent without performing mailbox lookups.
type ThreadNode struct {
	Key             string        `json:"key"`
	MessageID       string        `json:"messageId,omitempty"`
	ParentKey       string        `json:"parentKey,omitempty"`
	MissingParentID string        `json:"missingParentId,omitempty"`
	Depth           int           `json:"depth"`
	Message         ThreadMessage `json:"message"`
}

// Thread is a bounded, date-then-key ordered local reference graph.
type Thread struct {
	Nodes      []ThreadNode `json:"nodes"`
	Truncation Truncation   `json:"truncation"`
}

// BuildThread links supplied headers using References and In-Reply-To without network or transport state.
func BuildThread(messages []ThreadMessage, options ThreadOptions) Thread {
	limits := normalizedLimits(options.Limits)
	ordered := slices.Clone(messages)
	slices.SortStableFunc(ordered, compareThreadMessages)
	ordered = ordered[:min(len(ordered), limits.MaxThreadMessages)]

	thread := Thread{}
	thread.Truncation.ThreadMessages = len(ordered) < len(messages)

	keys := make(map[string]bool, len(ordered))
	identifiers := make(map[string]int, len(ordered))
	for index := range ordered {
		ordered[index].Key = stableThreadKey(ordered[index], keys)
		keys[ordered[index].Key] = true

		if identifier := ordered[index].Headers.MessageID; identifier != "" {
			if _, exists := identifiers[identifier]; !exists {
				identifiers[identifier] = index
			}
		}
	}

	parents := slices.Repeat([]int{-1}, len(ordered))
	thread.Nodes = make([]ThreadNode, len(ordered))
	for index, message := range ordered {
		parent, missing, truncated := parentFor(message.Headers, identifiers, limits.MaxReferenceCount)
		thread.Nodes[index] = ThreadNode{
			Key:             message.Key,
			MessageID:       message.Headers.MessageID,
			MissingParentID: missing,
			Message:         message,
		}
		thread.Truncation.References = thread.Truncation.References || truncated
		if wouldCycle(index, parent, parents) {
			thread.Truncation.ThreadCycle = true
			continue
		}

		parents[index] = parent
	}

	for index := range thread.Nodes {
		node := &thread.Nodes[index]
		depth := resolveThreadDepth(index, parents, thread.Nodes)
		if depth > limits.MaxThreadDepth {
			parents[index] = -1
			thread.Truncation.ThreadDepth = true
			continue
		}

		node.Depth = depth
		if parent := parents[index]; parent >= 0 {
			node.ParentKey = ordered[parent].Key
		}
	}

	return thread
}

func compareThreadMessages(left, right ThreadMessage) int {
	if order := left.ReceivedAt.Compare(right.ReceivedAt); order != 0 {
		return order
	}

	return cmp.Or(cmp.Compare(left.Key, right.Key), cmp.Compare(left.Headers.MessageID, right.Headers.MessageID))
}

func stableThreadKey(message ThreadMessage, keys map[string]bool) string {
	base := cmp.Or(message.Key, message.Headers.MessageID, "message")
	key := base
	for suffix := 2; keys[key]; suffix++ {
		key = base + "#" + strconv.Itoa(suffix)
	}

	return key
}

func parentFor(headers CanonicalHeaders, identifiers map[string]int, maxReferences int) (int, string, bool) {
	references := headers.References
	truncated := len(references) > maxReferences
	if truncated {
		references = references[len(references)-maxReferences:]
	}
	if len(references) == 0 {
		references = []string{headers.InReplyTo}
	}

	for _, identifier := range slices.Backward(references) {
		if parent, exists := identifiers[identifier]; exists {
			return parent, "", truncated
		}
	}

	return -1, references[len(references)-1], truncated
}

func wouldCycle(child, parent int, parents []int) bool {
	for parent >= 0 {
		if parent == child {
			return true
		}

		parent = parents[parent]
	}

	return false
}

func resolveThreadDepth(index int, parents []int, nodes []ThreadNode) int {
	parent := parents[index]
	if parent < 0 {
		return 0
	}

	if nodes[parent].Depth > 0 {
		return nodes[parent].Depth + 1
	}

	return resolveThreadDepth(parent, parents, nodes) + 1
}

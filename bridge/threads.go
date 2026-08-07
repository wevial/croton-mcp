package bridge

import (
	"sort"
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
	ordered := append([]ThreadMessage(nil), messages...)
	sort.SliceStable(ordered, func(left, right int) bool {
		if !ordered[left].ReceivedAt.Equal(ordered[right].ReceivedAt) {
			return ordered[left].ReceivedAt.Before(ordered[right].ReceivedAt)
		}

		if ordered[left].Key != ordered[right].Key {
			return ordered[left].Key < ordered[right].Key
		}

		return ordered[left].Headers.MessageID < ordered[right].Headers.MessageID
	})

	thread := Thread{}
	if len(ordered) > limits.MaxThreadMessages {
		ordered = ordered[:limits.MaxThreadMessages]
		thread.Truncation.ThreadMessages = true
	}

	keys := make(map[string]int, len(ordered))
	identifiers := make(map[string]int, len(ordered))
	for index := range ordered {
		ordered[index].Key = stableThreadKey(ordered[index], keys)
		keys[ordered[index].Key] = index

		if identifier := ordered[index].Headers.MessageID; identifier != "" {
			if _, exists := identifiers[identifier]; !exists {
				identifiers[identifier] = index
			}
		}
	}

	parents := make([]int, len(ordered))
	for index := range parents {
		parents[index] = -1
	}

	missingParents := make([]string, len(ordered))
	for index := range ordered {
		parents[index], missingParents[index], thread.Truncation.References = parentFor(ordered[index], identifiers, limits.MaxReferenceCount, thread.Truncation.References)
		if parents[index] >= 0 && wouldCycle(index, parents[index], parents) {
			parents[index] = -1
			thread.Truncation.ThreadCycle = true
		}
	}

	depths := make([]int, len(ordered))
	for index := range ordered {
		depths[index] = resolveThreadDepth(index, parents, depths)
		if depths[index] > limits.MaxThreadDepth {
			parents[index] = -1
			depths[index] = 0
			thread.Truncation.ThreadDepth = true
		}
	}

	thread.Nodes = make([]ThreadNode, len(ordered))
	for index, message := range ordered {
		node := ThreadNode{
			Key:             message.Key,
			MessageID:       message.Headers.MessageID,
			MissingParentID: missingParents[index],
			Depth:           depths[index],
			Message:         message,
		}
		if parent := parents[index]; parent >= 0 {
			node.ParentKey = ordered[parent].Key
		}

		thread.Nodes[index] = node
	}

	return thread
}

func stableThreadKey(message ThreadMessage, keys map[string]int) string {
	key := message.Key
	if key == "" {
		key = message.Headers.MessageID
	}
	if key == "" {
		key = "message"
	}

	base := key
	for suffix := 2; ; suffix++ {
		if _, exists := keys[key]; !exists {
			return key
		}

		key = base + "#" + strconv.Itoa(suffix)
	}
}

func parentFor(message ThreadMessage, identifiers map[string]int, maxReferences int, alreadyTruncated bool) (int, string, bool) {
	references := message.Headers.References
	truncated := alreadyTruncated
	if len(references) > maxReferences {
		references = references[len(references)-maxReferences:]
		truncated = true
	}

	for index := len(references) - 1; index >= 0; index-- {
		identifier := references[index]
		if parent, exists := identifiers[identifier]; exists {
			return parent, "", truncated
		}
	}

	if len(references) > 0 {
		return -1, references[len(references)-1], truncated
	}

	if identifier := message.Headers.InReplyTo; identifier != "" {
		if parent, exists := identifiers[identifier]; exists {
			return parent, "", truncated
		}

		return -1, identifier, truncated
	}

	return -1, "", truncated
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

func resolveThreadDepth(index int, parents, depths []int) int {
	parent := parents[index]
	if parent < 0 {
		return 0
	}

	if depths[parent] > 0 {
		return depths[parent] + 1
	}

	return resolveThreadDepth(parent, parents, depths) + 1
}

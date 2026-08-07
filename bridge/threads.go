package bridge

import (
	"container/heap"
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
	ordered := earliestThreadMessages(messages, limits.MaxThreadMessages)

	thread := Thread{}
	if len(messages) > limits.MaxThreadMessages {
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

type indexedThreadMessage struct {
	message    ThreadMessage
	inputIndex int
}

type latestThreadMessageHeap []indexedThreadMessage

func (messages latestThreadMessageHeap) Len() int {
	return len(messages)
}

func (messages latestThreadMessageHeap) Less(left, right int) bool {
	return threadMessageBefore(messages[right], messages[left])
}

func (messages latestThreadMessageHeap) Swap(left, right int) {
	messages[left], messages[right] = messages[right], messages[left]
}

func (messages *latestThreadMessageHeap) Push(value any) {
	*messages = append(*messages, value.(indexedThreadMessage))
}

func (messages *latestThreadMessageHeap) Pop() any {
	old := *messages
	last := len(old) - 1
	value := old[last]
	*messages = old[:last]

	return value
}

func earliestThreadMessages(messages []ThreadMessage, limit int) []ThreadMessage {
	selected := make(latestThreadMessageHeap, 0, min(len(messages), limit))
	for inputIndex, message := range messages {
		candidate := indexedThreadMessage{message: message, inputIndex: inputIndex}
		if len(selected) < limit {
			heap.Push(&selected, candidate)

			continue
		}

		if threadMessageBefore(candidate, selected[0]) {
			selected[0] = candidate
			heap.Fix(&selected, 0)
		}
	}

	sort.Slice(selected, func(left, right int) bool {
		return threadMessageBefore(selected[left], selected[right])
	})

	ordered := make([]ThreadMessage, len(selected))
	for index := range selected {
		ordered[index] = selected[index].message
	}

	return ordered
}

func threadMessageBefore(left, right indexedThreadMessage) bool {
	if !left.message.ReceivedAt.Equal(right.message.ReceivedAt) {
		return left.message.ReceivedAt.Before(right.message.ReceivedAt)
	}

	if left.message.Key != right.message.Key {
		return left.message.Key < right.message.Key
	}

	if left.message.Headers.MessageID != right.message.Headers.MessageID {
		return left.message.Headers.MessageID < right.message.Headers.MessageID
	}

	return left.inputIndex < right.inputIndex
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

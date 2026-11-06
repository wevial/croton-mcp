package bridge

import (
	"bytes"
)

// threadFieldNames are the only fields retained independently of the generic
// header count bound, in their fixed slot order.
var threadFieldNames = [...][]byte{
	[]byte("Subject"),
	[]byte("Message-ID"),
	[]byte("In-Reply-To"),
	[]byte("References"),
}

// threadFieldBlock copies the first occurrence of each threading field, with
// its folded continuation lines, from a complete header block into a new
// header block. The scan is a single pass over the already byte-bounded
// header and keeps at most one field per fixed slot, so the result is never
// larger than the input. Other fields are skipped without being decoded.
func threadFieldBlock(header []byte) []byte {
	var slots [len(threadFieldNames)][]byte
	current := -1
	for lineStart := 0; lineStart < len(header); {
		lineLength := bytes.IndexByte(header[lineStart:], '\n')
		nextLineStart := len(header)
		if lineLength >= 0 {
			nextLineStart = lineStart + lineLength + 1
		}

		raw := header[lineStart:nextLineStart]
		line := bytes.TrimRight(raw, "\r\n")
		if len(line) == 0 {
			break
		}

		if line[0] == ' ' || line[0] == '\t' {
			if current >= 0 {
				slots[current] = append(slots[current], raw...)
			}
		} else {
			current = threadFieldSlot(line)
			if current >= 0 && slots[current] != nil {
				current = -1
			}
			if current >= 0 {
				slots[current] = append(make([]byte, 0, len(raw)), raw...)
			}
		}

		lineStart = nextLineStart
	}

	block := make([]byte, 0, len(header))
	for _, slot := range slots {
		block = append(block, slot...)
	}

	return append(block, "\r\n"...)
}

// threadFieldSlot returns the slot of a field line whose name matches a
// threading field case-insensitively, as net/mail does, or -1.
func threadFieldSlot(line []byte) int {
	name, _, found := bytes.Cut(line, []byte{':'})
	if !found {
		return -1
	}

	for slot, candidate := range threadFieldNames {
		if bytes.EqualFold(name, candidate) {
			return slot
		}
	}

	return -1
}

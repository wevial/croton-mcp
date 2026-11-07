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
// header block ended by the input's own blank line. The scan is a single pass
// over the already byte-bounded header and keeps at most one field per fixed
// slot, so the result is never larger than the input. Other fields are
// skipped without being decoded.
func threadFieldBlock(header []byte) []byte {
	var slots [len(threadFieldNames)][]byte
	terminator := []byte("\r\n")
	current := -1
	for lineStart := 0; lineStart < len(header); {
		lineLength := bytes.IndexByte(header[lineStart:], '\n')
		nextLineStart := len(header)
		if lineLength >= 0 {
			nextLineStart = lineStart + lineLength + 1
		}

		raw := header[lineStart:nextLineStart]
		line := bytes.TrimSuffix(bytes.TrimSuffix(raw, []byte{'\n'}), []byte{'\r'})
		if len(line) == 0 {
			terminator = raw
			break
		}

		if line[0] != ' ' && line[0] != '\t' {
			current = threadFieldSlot(line)
			if current >= 0 && slots[current] != nil {
				current = -1
			}
		}
		if current >= 0 {
			slots[current] = append(slots[current], raw...)
		}

		lineStart = nextLineStart
	}

	return append(bytes.Join(slots[:], nil), terminator...)
}

// threadFieldSlot returns the slot of a field line whose name matches a
// threading field under ASCII case folding, as net/mail's key
// canonicalization does, or -1. Unicode folding would let a non-ASCII name
// such as "ſubject" take the Subject slot that mail.Header.Get never maps.
func threadFieldSlot(line []byte) int {
	name, _, found := bytes.Cut(line, []byte{':'})
	if !found {
		return -1
	}

	for slot, candidate := range threadFieldNames {
		if asciiEqualFold(name, candidate) {
			return slot
		}
	}

	return -1
}

func asciiEqualFold(left, right []byte) bool {
	if len(left) != len(right) {
		return false
	}

	for index := range left {
		if asciiLower(left[index]) != asciiLower(right[index]) {
			return false
		}
	}

	return true
}

func asciiLower(value byte) byte {
	if 'A' <= value && value <= 'Z' {
		return value + 'a' - 'A'
	}

	return value
}

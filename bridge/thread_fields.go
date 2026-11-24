package bridge

import "bytes"

// threadFieldNames are the only fields retained independently of the generic
// header count bound, in their fixed slot order.
var threadFieldNames = [...][]byte{
	[]byte("Subject"),
	[]byte("Message-ID"),
	[]byte("In-Reply-To"),
	[]byte("References"),
}

// threadFieldBlock scans a bounded, complete header once, keeping the first
// copy of each threading field with its continuations and skipping other fields
// without decoding. Each line, including the terminator, appears at most once
// in the result, so the output fits within the input size.
func threadFieldBlock(header []byte) []byte {
	var slots [len(threadFieldNames)][]byte
	terminator := []byte("\r\n")
	current := -1
	for raw := range bytes.Lines(header) {
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
	}

	return append(bytes.Join(slots[:], nil), terminator...)
}

func threadFieldSlot(line []byte) int {
	name, _, found := bytes.Cut(line, []byte{':'})
	if !found {
		return -1
	}

	for slot, candidate := range threadFieldNames {
		// net/mail folds only ASCII names. Equal byte lengths keep a
		// multibyte fold such as "ſubject" from claiming the Subject slot.
		if len(name) == len(candidate) && bytes.EqualFold(name, candidate) {
			return slot
		}
	}

	return -1
}

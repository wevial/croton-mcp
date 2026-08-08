package bridge

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func TestDecodeMessageIDEnforcesEncodedCeilingBeforeDecoding(t *testing.T) {
	adapter := &Adapter{idKey: [sha256.Size]byte{1}}

	exact := signedMessageID(t, adapter.idKey, maxMessageIDBytes)
	if _, err := adapter.decodeMessageID(exact); err != nil {
		t.Fatalf("decode exact ceiling: %v", err)
	}

	for _, value := range []string{
		signedMessageID(t, adapter.idKey, maxMessageIDBytes+1),
		"%%%",
		strings.Repeat("A", 1<<20),
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		if _, err := adapter.decodeMessageID(value); CodeOf(err) != CodeStaleMessageID {
			t.Fatalf("decode %d-byte identifier error = %v, want %q", len(value), err, CodeStaleMessageID)
		}

		runtime.ReadMemStats(&after)
		if allocated := after.TotalAlloc - before.TotalAlloc; allocated > uint64(maxMessageIDBytes*4) {
			t.Fatalf("decode %d-byte identifier allocated %d bytes after the encoded ceiling", len(value), allocated)
		}
	}
}

func signedMessageID(t *testing.T, key [sha256.Size]byte, decodedBytes int) string {
	t.Helper()

	const prefix = `{"m":"INBOX","v":1,"u":1,"x":"`
	const suffix = `"}`
	payloadBytes := decodedBytes - sha256.Size
	paddingBytes := payloadBytes - len(prefix) - len(suffix)
	if paddingBytes < 0 {
		t.Fatalf("decoded length %d cannot contain message identifier payload", decodedBytes)
	}

	payload := []byte(fmt.Sprintf("%s%s%s", prefix, strings.Repeat("x", paddingBytes), suffix))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(payload)

	return base64.RawURLEncoding.EncodeToString(append(payload, mac.Sum(nil)...))
}

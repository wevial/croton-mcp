package bridge

import (
	"errors"
	"os"
	"testing"
)

func TestLoadTrustAnchorRejectsUnsupportedFileBeforeOpen(t *testing.T) {
	opened := false
	_, err := loadTrustAnchorWith("unsupported-anchor.pem", false, func(string) (*os.File, error) {
		opened = true
		return nil, errors.New("unexpected trust-anchor open")
	})
	if CodeOf(err) != CodeInvalidConfig {
		t.Fatalf("unsupported trust-anchor file error = %v, want %q", err, CodeInvalidConfig)
	}
	if opened {
		t.Fatal("unsupported trust-anchor file attempted to open")
	}
}

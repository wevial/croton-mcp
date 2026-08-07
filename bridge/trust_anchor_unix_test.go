//go:build linux || darwin || freebsd

package bridge_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/wevial/croton-mcp/bridge"
)

func TestNewTLSConfigRejectsSymlinkAndFIFOTrustAnchorsWithoutBlocking(t *testing.T) {
	directory := t.TempDir()
	regular := filepath.Join(directory, "regular.pem")
	if err := os.WriteFile(regular, []byte("not a certificate"), 0o600); err != nil {
		t.Fatalf("write regular fixture: %v", err)
	}

	replaced := filepath.Join(directory, "replaced.pem")
	if err := os.Rename(regular, replaced); err != nil {
		t.Fatalf("rename regular fixture: %v", err)
	}
	if err := os.Symlink(replaced, regular); err != nil {
		t.Fatalf("replace fixture with symlink: %v", err)
	}
	if _, err := bridge.NewTLSConfig(bridge.TLSConfig{TrustAnchorFile: regular}); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("symlink trust anchor error = %v, want %q", err, bridge.CodeInvalidConfig)
	}

	fifo := filepath.Join(directory, "anchor.fifo")
	if err := unix.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("create FIFO fixture: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		_, err := bridge.NewTLSConfig(bridge.TLSConfig{TrustAnchorFile: fifo})
		result <- err
	}()

	select {
	case err := <-result:
		if bridge.CodeOf(err) != bridge.CodeInvalidConfig {
			t.Fatalf("FIFO trust anchor error = %v, want %q", err, bridge.CodeInvalidConfig)
		}
	case <-time.After(time.Second):
		t.Fatal("FIFO trust anchor open blocked")
	}
}

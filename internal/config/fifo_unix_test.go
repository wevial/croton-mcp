//go:build linux || darwin

package config

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestLoadRejectsFIFOWithoutBlocking(t *testing.T) {
	// The FIFO must live on a symlink-free path, or the loader rejects it for
	// the parent directory rather than for the FIFO this test is about.
	path := filepath.Join(canonicalTempDir(t), "croton.json")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}

	result := make(chan error, 1)
	go func() {
		_, err := Load(path)
		result <- err
	}()

	select {
	case err := <-result:
		if !errors.Is(err, ErrConfigUnreadable) {
			t.Fatalf("Load FIFO error = %v, want ErrConfigUnreadable", err)
		}
	case <-time.After(250 * time.Millisecond):
		// Unblock the pre-fix reader so the regression test does not leak a
		// goroutine when it records RED.
		writer, err := syscall.Open(path, syscall.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			_ = syscall.Close(writer)
		}
		select {
		case <-result:
		case <-time.After(time.Second):
		}
		t.Fatal("Load blocked on FIFO")
	}
}

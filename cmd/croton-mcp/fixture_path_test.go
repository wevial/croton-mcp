//go:build linux || darwin

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wevial/croton-mcp/internal/config"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// TestServerConfigFixtureIsReachableThroughSecureLoader pins the fixture the
// Hermes catalog smoke and the server tests hand to a real Croton process. The
// secure loader refuses any path whose parents are reached through a symlink,
// and on macOS t.TempDir returns a path under /var, itself a symlink to
// /private/var, so an unresolved fixture path fails on the native runner
// before the smoke can start. TMPDIR is pointed at a symlink here so the same
// shape is reproduced on any supported platform.
func TestServerConfigFixtureIsReachableThroughSecureLoader(t *testing.T) {
	// The roots are built before any fixture runs, because t.TempDir creates
	// one base directory per test on its first call and caches it.
	base := t.TempDir()
	realRoot := filepath.Join(base, "real-tmp")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatalf("create real temporary root: %v", err)
	}
	linkedRoot := filepath.Join(base, "linked-tmp")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatalf("create temporary root symlink: %v", err)
	}
	t.Setenv("TMPDIR", linkedRoot)

	t.Run("fixture loads", func(t *testing.T) {
		server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
		if err != nil {
			t.Fatalf("start fake server: %v", err)
		}
		t.Cleanup(func() { _ = server.Close() })

		path := writeServerConfig(t, server)
		if resolved, evalErr := filepath.EvalSymlinks(path); evalErr != nil || resolved != path {
			t.Fatalf("fixture path %q is not symlink-free (resolves to %q, err %v)", path, resolved, evalErr)
		}
		if _, err := config.Load(path); err != nil {
			t.Fatalf("secure loader could not read the server config fixture: %v", err)
		}
	})
}

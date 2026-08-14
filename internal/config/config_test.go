package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// canonicalTempDir returns a temporary directory with every symlink in it
// resolved. The secure loader refuses to traverse a symlinked parent, and on
// macOS t.TempDir hands back a path under /var, which is a symlink to
// /private/var, so an unresolved fixture path is rejected before the behavior
// under test is ever reached. Resolving here is a test-fixture concern only:
// the loader's policy is unchanged, and TestLoadRejectsSymlinkedParentDirectory
// plus TestLoadAcceptsCanonicalTemporaryPathAndRejectsSymlinkedParent still
// prove a symlinked parent stays rejected. On Linux, where the temporary
// directory is already canonical, this is a no-op.
func canonicalTempDir(t *testing.T) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}

	return resolved
}

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(canonicalTempDir(t), "croton.json")
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadParsesBridgeConfig(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"imap": {
			"host": "127.0.0.1",
			"port": 2143,
			"tlsMode": "implicit",
			"credentialCommand": ["/usr/bin/false"],
			"tls": {"spkiSha256": "0000000000000000000000000000000000000000000000000000000000000000"}
		},
		"bounds": {"maxSearchResults": 10},
		"audit": {"enabled": true}
	}`)

	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.IMAP.Host != "127.0.0.1" || loaded.IMAP.Port != 2143 || loaded.IMAP.TLSMode != "implicit" {
		t.Fatalf("unexpected IMAP config: %+v", loaded.IMAP)
	}
	if loaded.Bounds.MaxSearchResults == nil || *loaded.Bounds.MaxSearchResults != 10 {
		t.Fatalf("unexpected bounds: %+v", loaded.Bounds)
	}
	if !loaded.Audit.Enabled {
		t.Fatalf("unexpected audit config: %+v", loaded.Audit)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		path func(t *testing.T) string
	}{
		{"missing file", func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent.json") }},
		{"malformed json", func(t *testing.T) string { return writeConfig(t, `{"imap":`) }},
		{"null document", func(t *testing.T) string { return writeConfig(t, `null`) }},
		{"duplicate field", func(t *testing.T) string { return writeConfig(t, `{"imap":{},"imap":{}}`) }},
		{"nested duplicate field", func(t *testing.T) string { return writeConfig(t, `{"imap":{"host":"127.0.0.1","host":"localhost"}}`) }},
		{"case-folded field alias", func(t *testing.T) string { return writeConfig(t, `{"imap":{"host":"first"},"IMAP":{"host":"second"}}`) }},
		{"world-readable file", func(t *testing.T) string {
			path := writeConfig(t, `{"imap":{}}`)
			if err := os.Chmod(path, 0o644); err != nil {
				t.Fatalf("chmod config: %v", err)
			}
			return path
		}},
		{"unknown field", func(t *testing.T) string { return writeConfig(t, `{"imap":{}, "prompts": true}`) }},
		{"trailing data", func(t *testing.T) string { return writeConfig(t, `{"imap":{}} {"more": 1}`) }},
		{"oversize file", func(t *testing.T) string { return writeConfig(t, `{"pad":"`+strings.Repeat("x", 70*1024)+`"}`) }},
		{"directory", func(t *testing.T) string { return t.TempDir() }},
		{"relative path", func(t *testing.T) string { return "croton.json" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			_, err := Load(testCase.path(t))
			if err == nil {
				t.Fatal("Load unexpectedly succeeded")
			}
			if strings.Contains(err.Error(), "xxxx") || strings.Contains(err.Error(), "prompts") {
				t.Fatalf("error leaks file contents: %v", err)
			}
		})
	}
}

func TestLoadRejectsSymlinkedParentDirectory(t *testing.T) {
	t.Parallel()

	root := canonicalTempDir(t)
	realDirectory := filepath.Join(root, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatalf("create real directory: %v", err)
	}
	configPath := filepath.Join(realDirectory, "croton.json")
	if err := os.WriteFile(configPath, []byte(`{"imap":{}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	linkedDirectory := filepath.Join(root, "linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatalf("create symlink: %v", err)
	}

	if _, err := Load(filepath.Join(linkedDirectory, "croton.json")); err == nil {
		t.Fatal("Load accepted a config through a symlinked parent")
	}
}

// TestLoadAcceptsCanonicalTemporaryPathAndRejectsSymlinkedParent pins both
// halves of the fixture contract that macOS exposed. On Darwin t.TempDir hands
// back a path under /var, which is a symlink to /private/var, so every fixture
// that expects Load to reach its file must canonicalize the directory first.
// This test reproduces that shape on any platform by pointing TMPDIR at a
// symlink, and it keeps the production policy honest in the same breath: the
// canonicalized path must load, while a parent that is still reached through a
// symlink must stay rejected.
func TestLoadAcceptsCanonicalTemporaryPathAndRejectsSymlinkedParent(t *testing.T) {
	// The roots are built before any fixture runs, because t.TempDir creates
	// one base directory per test on its first call and caches it.
	t.Setenv("TMPDIR", symlinkedTemporaryRoot(t))

	t.Run("canonicalized path loads", func(t *testing.T) {
		path := writeConfig(t, `{"imap":{"host":"127.0.0.1"}}`)
		if resolved, err := filepath.EvalSymlinks(path); err != nil || resolved != path {
			t.Fatalf("fixture path %q is not symlink-free (resolves to %q, err %v)", path, resolved, err)
		}

		loaded, err := Load(path)
		if err != nil {
			t.Fatalf("Load through a canonicalized temporary path: %v", err)
		}
		if loaded.IMAP.Host != "127.0.0.1" {
			t.Fatalf("unexpected config through canonicalized path: %+v", loaded.IMAP)
		}
	})

	t.Run("symlinked parent stays rejected", func(t *testing.T) {
		path := writeConfig(t, `{"imap":{"host":"127.0.0.1"}}`)
		linkedParent := filepath.Join(canonicalTempDir(t), "linked-parent")
		if err := os.Symlink(filepath.Dir(path), linkedParent); err != nil {
			t.Fatalf("create parent symlink: %v", err)
		}

		if _, err := Load(filepath.Join(linkedParent, filepath.Base(path))); err == nil {
			t.Fatal("Load accepted a config through a symlinked parent")
		}
	})
}

// symlinkedTemporaryRoot returns a directory that is itself a symlink to a
// real temporary directory, reproducing the macOS /var -> /private/var shape
// that the secure loader correctly refuses to traverse.
func symlinkedTemporaryRoot(t *testing.T) string {
	t.Helper()

	base := t.TempDir()
	realRoot := filepath.Join(base, "real-tmp")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatalf("create real temporary root: %v", err)
	}
	linkedRoot := filepath.Join(base, "linked-tmp")
	if err := os.Symlink(realRoot, linkedRoot); err != nil {
		t.Fatalf("create temporary root symlink: %v", err)
	}

	return linkedRoot
}

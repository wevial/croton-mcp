package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, contents string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "croton.json")
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

	root := t.TempDir()
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

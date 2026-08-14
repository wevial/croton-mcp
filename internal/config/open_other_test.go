//go:build !linux && !darwin

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenSecureFailsClosedOnUnsupportedPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "croton.json")
	if err := os.WriteFile(path, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openSecure(path)
	if file != nil {
		_ = file.Close()
		t.Fatal("unsupported platform returned an open configuration file")
	}
	if err == nil {
		t.Fatal("unsupported platform accepted a configuration path")
	}
}

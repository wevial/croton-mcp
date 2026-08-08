package config

import (
	"errors"
	"io"
	"path/filepath"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

// maxConfigBytes bounds the configuration file read. Larger files fail closed.
const maxConfigBytes = 64 * 1024

// Errors are static so that no file content or partial parse detail can leak
// into diagnostics.
var (
	ErrConfigUnreadable = errors.New("configuration file is unreadable")
	ErrConfigInvalid    = errors.New("configuration file is invalid")
)

// Load reads one absolute-path JSON configuration file into an untrusted
// bridge.Config. Validation and clamping remain the bridge's responsibility.
func Load(path string) (bridge.Config, error) {
	if !filepath.IsAbs(path) {
		return bridge.Config{}, ErrConfigUnreadable
	}

	file, err := openSecure(path)
	if err != nil {
		return bridge.Config{}, ErrConfigUnreadable
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) ||
		info.Size() > maxConfigBytes || info.Mode().Perm()&0o077 != 0 {
		return bridge.Config{}, ErrConfigUnreadable
	}

	contents, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(contents) > maxConfigBytes {
		return bridge.Config{}, ErrConfigUnreadable
	}

	var loaded bridge.Config
	if !strictjson.DecodeObject(contents, maxConfigBytes, &loaded) {
		return bridge.Config{}, ErrConfigInvalid
	}

	return loaded, nil
}

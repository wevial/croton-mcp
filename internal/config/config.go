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
	var loaded bridge.Config
	if err := load(path, &loaded); err != nil {
		return bridge.Config{}, err
	}

	return loaded, nil
}

// load applies Croton's platform-specific secure-file policy before strictly
// decoding one JSON object into target. Configuration schemas share this path
// so no server can bypass the descriptor-relative, no-follow loader.
func load(path string, target any) error {
	if !filepath.IsAbs(path) {
		return ErrConfigUnreadable
	}

	file, err := openSecure(path)
	if err != nil {
		return ErrConfigUnreadable
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || !ownedByCurrentUser(info) ||
		info.Size() > maxConfigBytes || info.Mode().Perm()&0o077 != 0 {
		return ErrConfigUnreadable
	}

	contents, err := io.ReadAll(io.LimitReader(file, maxConfigBytes+1))
	if err != nil || len(contents) > maxConfigBytes {
		return ErrConfigUnreadable
	}

	if !strictjson.DecodeObject(contents, maxConfigBytes, target) {
		return ErrConfigInvalid
	}

	return nil
}

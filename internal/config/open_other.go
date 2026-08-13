//go:build !linux && !darwin

package config

import (
	"errors"
	"os"
)

var errUnsafeConfigPath = errors.New("secure configuration loading is unavailable on this platform")

// openSecure fails closed on platforms outside Linux and Darwin, where Croton
// does not yet have a descriptor-relative, no-symlink implementation. This
// avoids path-based Lstat/Open races and incorrect Windows volume or UNC
// traversal.
func openSecure(string) (*os.File, error) {
	return nil, errUnsafeConfigPath
}

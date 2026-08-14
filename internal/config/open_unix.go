//go:build linux || darwin

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

var errUnsafeConfigPath = errors.New("unsafe configuration path")

// openSecure resolves every path component through a pinned directory file
// descriptor. O_NOFOLLOW on each open rejects symlinks without a check/use
// window between path validation and the final file open. Linux and Darwin
// share this implementation: openat, O_DIRECTORY, and O_NOFOLLOW are native
// on both, so both platforms get the identical race-free contract. The calls
// go through golang.org/x/sys/unix because the standard syscall package
// exposes Openat only on Linux.
func openSecure(path string) (*os.File, error) {
	cleaned := filepath.Clean(path)
	components := strings.Split(strings.TrimPrefix(cleaned, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[0] == "" {
		return nil, errUnsafeConfigPath
	}

	directory, err := unix.Open(string(filepath.Separator), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errUnsafeConfigPath
	}

	for _, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(directory, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		_ = unix.Close(directory)
		if openErr != nil {
			return nil, errUnsafeConfigPath
		}
		directory = next
	}

	// O_NONBLOCK prevents special files such as FIFOs from stalling startup;
	// Load rejects them after descriptor-based Stat confirms the file type.
	fileDescriptor, openErr := unix.Openat(
		directory,
		components[len(components)-1],
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	_ = unix.Close(directory)
	if openErr != nil {
		return nil, errUnsafeConfigPath
	}

	file := os.NewFile(uintptr(fileDescriptor), cleaned)
	if file == nil {
		_ = unix.Close(fileDescriptor)
		return nil, errUnsafeConfigPath
	}
	return file, nil
}

//go:build linux

package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

var errUnsafeConfigPath = errors.New("unsafe configuration path")

// openSecure resolves every path component through a pinned directory file
// descriptor. O_NOFOLLOW on each open rejects symlinks without a check/use
// window between path validation and the final file open.
func openSecure(path string) (*os.File, error) {
	cleaned := filepath.Clean(path)
	components := strings.Split(strings.TrimPrefix(cleaned, string(filepath.Separator)), string(filepath.Separator))
	if len(components) == 0 || components[0] == "" {
		return nil, errUnsafeConfigPath
	}

	directory, err := syscall.Open(string(filepath.Separator), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, errUnsafeConfigPath
	}

	for _, component := range components[:len(components)-1] {
		next, openErr := syscall.Openat(directory, component, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0)
		_ = syscall.Close(directory)
		if openErr != nil {
			return nil, errUnsafeConfigPath
		}
		directory = next
	}

	// O_NONBLOCK prevents special files such as FIFOs from stalling startup;
	// Load rejects them after descriptor-based Stat confirms the file type.
	fileDescriptor, openErr := syscall.Openat(
		directory,
		components[len(components)-1],
		syscall.O_RDONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC|syscall.O_NOFOLLOW,
		0,
	)
	_ = syscall.Close(directory)
	if openErr != nil {
		return nil, errUnsafeConfigPath
	}

	file := os.NewFile(uintptr(fileDescriptor), cleaned)
	if file == nil {
		_ = syscall.Close(fileDescriptor)
		return nil, errUnsafeConfigPath
	}
	return file, nil
}

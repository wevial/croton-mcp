//go:build linux || darwin

// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package drivefs contains an unregistered, descriptor-bound output primitive.
// It provides resolution-time confinement, not lifetime pathname confinement.
package drivefs

import (
	"errors"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// WriteFresh resolves a fresh destination under an explicit absolute root and
// lends its file descriptor to write. Parent directories must already exist.
// The callback must not close or retain the file. All descriptors are closed on
// return, including callback errors; partial output is not published or removed.
// Existing destinations are refused solely to keep this prerequisite primitive
// independent of the product's reserved overwrite and publication policies.
// No CLI or MCP production path calls this primitive.
func WriteFresh(root, destination string, write func(*os.File) error) error {
	return writeFresh(root, destination, write, confinedOps{open: platformOpen})
}

// Hooks are per invocation and private: tests can stop exactly after selection
// or after resolution without global syscall replacement or timing assumptions.
type confinedOps struct {
	open     func(int, string, int, uint32) (int, error)
	selected func()
	resolved func()
}

func components(path string) ([]string, error) {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return nil, errors.New("invalid output path component")
		}
	}

	return parts, nil
}

func writeFresh(root, destination string, write func(*os.File) error, ops confinedOps) (err error) {
	if !strings.HasPrefix(root, "/") || strings.HasPrefix(destination, "/") || write == nil {
		return errors.New("explicit absolute root, relative destination and writer required")
	}

	var roots []string
	if root != "/" {
		roots, err = components(strings.TrimPrefix(root, "/"))
		if err != nil {
			return err
		}
	}
	targets, err := components(destination)
	if err != nil {
		return err
	}

	if ops.selected != nil {
		ops.selected()
	}

	var held []*os.File
	defer func() {
		for i := len(held) - 1; i >= 0; i-- {
			err = errors.Join(err, held[i].Close())
		}
	}()
	retain := func(fd int) int {
		// Never give the writer a validated pathname to reopen.
		held = append(held, os.NewFile(uintptr(fd), "confined-output"))

		return fd
	}

	const directoryFlags = unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_DIRECTORY
	fd, err := ops.open(unix.AT_FDCWD, "/", directoryFlags, 0)
	if err != nil {
		return err
	}

	parent := retain(fd)
	for _, part := range append(roots, targets[:len(targets)-1]...) {
		fd, err = ops.open(parent, part, directoryFlags, 0)
		if err != nil {
			return err
		}

		parent = retain(fd)
	}

	fd, err = ops.open(parent, targets[len(targets)-1], unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}

	retain(fd)
	if ops.resolved != nil {
		ops.resolved()
	}

	return write(held[len(held)-1])
}

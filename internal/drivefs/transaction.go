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

package drivefs

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// WriteTransactional writes privately and atomically publishes a complete file
// at a fresh destination under an explicit absolute root. Caller destination
// directories must already exist. The writer borrows a descriptor and must not
// close or retain it. The primitive closes all owned descriptors on exit.
//
// Existing destinations (including symlinks) are refused before writing and
// atomically at publication. A writer error, including cancellation, aborts and
// removes temporary output; an arbitrary blocked writer is not interrupted.
// Cleanup failures are joined with the original error and can leave temporary
// artifacts. An error after publication can leave the complete final file;
// rollback is not promised. No crash recovery or fsync durability is provided.
//
// All operations retain the resolved directory authority, with the same
// resolution-time confinement and root-relocation boundary as WriteFresh.
// Publication requires descriptor-relative hard links on the filesystem;
// unsupported operations fail closed without an overwrite fallback.
// No CLI or MCP production path calls this primitive.
func WriteTransactional(root, destination string, write func(*os.File) error) error {
	return writeTransactional(root, destination, write, transactionOps{
		confinedOps: confinedOps{open: platformOpen},
		publish:     publishLink,
		remove:      unix.Unlinkat,
	})
}

// Fault and barrier hooks are private and scoped to one invocation.
type transactionOps struct {
	confinedOps
	publish func(int, string, int, string) error
	remove  func(int, string, int) error
}

func publishLink(from int, source string, to int, destination string) error {
	// linkat is atomic and never replaces an existing destination on either
	// Linux or macOS. Both names are single components under held descriptors;
	// flags=0 does not follow a source symlink. There is no pathname fallback.
	return unix.Linkat(from, source, to, destination, 0)
}

func writeTransactional(root, destination string, write func(*os.File) error, ops transactionOps) error {
	if write == nil {
		return errors.New("writer required")
	}

	return withConfinedParent(root, destination, ops.confinedOps, func(parent int, name string) (err error) {
		var stat unix.Stat_t
		if err := unix.Fstatat(parent, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err == nil {
			return os.ErrExist
		} else if !errors.Is(err, unix.ENOENT) {
			return err
		}

		// A private same-filesystem directory protects the source name. Neither
		// the writer nor publication reopens the checked destination pathname.
		stage := ".croton-output-" + rand.Text()
		if err := unix.Mkdirat(parent, stage, 0700); err != nil {
			return err
		}

		defer func() {
			if cleanupErr := ops.remove(parent, stage, unix.AT_REMOVEDIR); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove temporary output directory: %w", cleanupErr))
			}
		}()

		fd, err := ops.open(parent, stage, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW|unix.O_DIRECTORY, 0)
		if err != nil {
			return err
		}

		directory := os.NewFile(uintptr(fd), "transaction-directory")
		defer func() { err = errors.Join(err, directory.Close()) }()
		const source = "output"
		outputFD, err := ops.open(fd, source, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}

		defer func() {
			if cleanupErr := ops.remove(fd, source, 0); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("remove temporary output file: %w", cleanupErr))
			}
		}()
		file := os.NewFile(uintptr(outputFD), "transaction-output")
		// Close on abort (and panic), but also check close before publication so
		// delayed write errors cannot result in a successful publication.
		defer func() {
			if file != nil {
				err = errors.Join(err, file.Close())
			}
		}()
		if ops.resolved != nil {
			ops.resolved()
		}

		if err := write(file); err != nil {
			return err
		}

		err = file.Close()
		file = nil
		if err != nil {
			return err
		}

		return ops.publish(fd, source, parent, name)
	})
}

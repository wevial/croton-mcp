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
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

func transactionTracked(t *testing.T) (transactionOps, func()) {
	t.Helper()

	ops, closed := tracked(t)

	return transactionOps{
		confinedOps: ops,
		publish:     publishLink,
		remove:      unix.Unlinkat,
	}, closed
}

func absent(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected absent output, got %v", err)
	}
}

// Pause either the writer after partial output or publication after the writer
// finishes. Channels order filesystem observations and mutations without sleeps.
func transactionBarrier(t *testing.T, root, destination string, result error, beforePublication bool, mutate func()) error {
	t.Helper()

	reached, resume := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	ops, closed := transactionTracked(t)
	pause := func() {
		close(reached)

		<-resume
	}
	if beforePublication {
		ops.publish = func(from int, source string, to int, destination string) error {
			pause()

			return publishLink(from, source, to, destination)
		}
	}

	go func() {
		done <- writeTransactional(root, destination, func(file *os.File) error {
			if _, err := file.WriteString("partial.test"); err != nil {
				return err
			}

			if !beforePublication {
				pause()
			}

			if result != nil {
				return result
			}

			_, err := file.WriteString(" complete.test")

			return err
		}, ops)
	}()

	select {
	case <-reached:
	case err := <-done:
		closed()
		t.Fatalf("writer failed before barrier: %v", err)
	}
	var err error
	func() {
		// Also drain the worker if mutate aborts the test with Fatal.
		defer func() {
			close(resume)
			err = <-done
			closed()
		}()

		mutate()
	}()

	return err
}

func TestTransactionalOutputPublication(t *testing.T) {
	root := fixture(t)
	destination := filepath.Join(root, "output.test")
	err := transactionBarrier(t, root, "output.test", nil, false, func() {
		absent(t, destination)
		// Partial data is private, including while the writer is paused.
		stage, err := os.ReadDir(root)
		must(t, err)
		if len(stage) != 1 || !stage[0].IsDir() {
			t.Fatal("expected one private staging directory")
		}
		info, err := stage[0].Info()
		must(t, err)
		if info.Mode().Perm() != 0700 {
			t.Fatalf("staging directory permissions = %o", info.Mode().Perm())
		}
	})
	must(t, err)
	content(t, destination, "partial.test complete.test")
	entries(t, root, 1)
	info, err := os.Stat(destination)
	must(t, err)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("output permissions = %o", info.Mode().Perm())
	}

	// Exercise the exported entry point as well as the instrumented path.
	must(t, WriteTransactional(root, "public.test", syntheticWrite))
	content(t, filepath.Join(root, "public.test"), "synthetic.test\n")
	entries(t, root, 2)
}

func TestTransactionalOutputCollision(t *testing.T) {
	for _, kind := range []string{"file", "directory", "symlink"} {
		for _, race := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/race=%v", kind, race), func(t *testing.T) {
				root, outside := fixture(t), fixture(t)
				final := filepath.Join(root, "output.test")
				sentinel := filepath.Join(outside, "sentinel.test")
				must(t, os.WriteFile(sentinel, []byte("competitor.test"), 0600))
				create := func() {
					switch kind {
					case "file":
						must(t, os.WriteFile(final, []byte("competitor.test"), 0600))
					case "directory":
						must(t, os.Mkdir(final, 0700))
					case "symlink":
						must(t, os.Symlink(sentinel, final))
					}
				}

				var err error
				if race {
					err = transactionBarrier(t, root, "output.test", nil, true, create)
				} else {
					create()
					ops, closed := transactionTracked(t)
					called := false
					err = writeTransactional(root, "output.test", func(file *os.File) error {
						called = true

						return syntheticWrite(file)
					}, ops)
					closed()
					if called {
						t.Fatal("writer called for existing destination")
					}
				}
				if !errors.Is(err, os.ErrExist) {
					t.Fatalf("collision error = %v", err)
				}

				entries(t, root, 1)
				content(t, sentinel, "competitor.test")
				if kind == "directory" {
					entries(t, final, 0)
				} else {
					content(t, final, "competitor.test")
				}
			})
		}
	}
}

func TestTransactionalOutputAbort(t *testing.T) {
	for _, result := range []error{nil, errors.New("synthetic.test writer failure"), context.Canceled} {
		t.Run(fmt.Sprint(result), func(t *testing.T) {
			root := fixture(t)
			ops, closed := transactionTracked(t)
			var borrowed *os.File
			err := writeTransactional(root, "output.test", func(file *os.File) error {
				borrowed = file
				if err := syntheticWrite(file); err != nil {
					return err
				}
				if _, err := file.Stat(); err != nil {
					return err
				}

				return result
			}, ops)
			closed()
			if !errors.Is(err, result) {
				t.Fatalf("error = %v, want %v", err, result)
			}
			if borrowed == nil {
				t.Fatal("writer was not called")
			}
			// Only the ownership test keeps this reference: callers may not retain it.
			if _, err := borrowed.WriteString("late.test"); !errors.Is(err, os.ErrClosed) {
				t.Fatalf("borrowed descriptor remains usable: %v", err)
			}

			if result == nil {
				content(t, filepath.Join(root, "output.test"), "synthetic.test\n")
				entries(t, root, 1)
			} else {
				absent(t, filepath.Join(root, "output.test"))
				entries(t, root, 0)
			}
		})
	}
}

func TestTransactionalOutputRename(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		for _, abort := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/abort=%v", replacement, abort), func(t *testing.T) {
				root, outside := fixture(t), fixture(t)
				original, moved := filepath.Join(root, "nested.test"), filepath.Join(root, "moved.test")
				must(t, os.Mkdir(original, 0700))
				must(t, os.WriteFile(filepath.Join(outside, "output.test"), []byte("outside.test"), 0600))
				var result error
				if abort {
					result = context.Canceled
				}

				err := transactionBarrier(t, root, "nested.test/output.test", result, false, func() {
					must(t, os.Rename(original, moved))
					if replacement == "directory" {
						must(t, os.Mkdir(original, 0700))
						must(t, os.WriteFile(filepath.Join(original, "output.test"), []byte("replacement.test"), 0600))
					} else {
						must(t, os.Symlink(outside, original))
					}
				})
				if !errors.Is(err, result) {
					t.Fatalf("error = %v, want %v", err, result)
				}

				content(t, filepath.Join(outside, "output.test"), "outside.test")
				entries(t, outside, 1)
				entries(t, root, 2)
				if replacement == "directory" {
					content(t, filepath.Join(original, "output.test"), "replacement.test")
					entries(t, original, 1)
				}
				if abort {
					entries(t, moved, 0)
				} else {
					content(t, filepath.Join(moved, "output.test"), "partial.test complete.test")
					entries(t, moved, 1)
				}
			})
		}
	}
}

func TestTransactionalOutputFailures(t *testing.T) {
	for _, stage := range []string{"directory", "file"} {
		t.Run("open/"+stage, func(t *testing.T) {
			root := fixture(t)
			ops, closed := transactionTracked(t)
			realOpen := ops.open
			ops.open = func(parent int, name string, flags int, mode uint32) (int, error) {
				if (stage == "directory" && strings.HasPrefix(name, ".croton-output-")) || (stage == "file" && name == "output") {
					return -1, unix.EIO
				}

				return realOpen(parent, name, flags, mode)
			}
			called := false
			err := writeTransactional(root, "output.test", func(file *os.File) error {
				called = true

				return syntheticWrite(file)
			}, ops)
			closed()
			if !errors.Is(err, unix.EIO) || called {
				t.Fatalf("open error = %v, writer called = %v", err, called)
			}

			entries(t, root, 0)
		})
	}
	for _, unavailable := range []error{unix.ENOSYS, unix.ENOTSUP, unix.EINVAL} {
		t.Run("publication/"+unavailable.Error(), func(t *testing.T) {
			root := fixture(t)
			ops, closed := transactionTracked(t)
			attempts := 0
			ops.publish = func(int, string, int, string) error {
				attempts++

				return unavailable
			}

			err := writeTransactional(root, "output.test", syntheticWrite, ops)
			closed()
			if !errors.Is(err, unavailable) || attempts != 1 {
				t.Fatalf("error = %v, attempts = %d", err, attempts)
			}
			entries(t, root, 0)
		})
	}
	for _, abort := range []bool{false, true} {
		for _, directory := range []bool{false, true} {
			t.Run(fmt.Sprintf("cleanup/abort=%v/directory=%v", abort, directory), func(t *testing.T) {
				root := fixture(t)
				ops, closed := transactionTracked(t)
				cleanupError := errors.New("synthetic.test cleanup failure")
				ops.remove = func(parent int, name string, flags int) error {
					if (flags == unix.AT_REMOVEDIR) == directory {
						return cleanupError
					}

					return unix.Unlinkat(parent, name, flags)
				}
				err := writeTransactional(root, "output.test", func(file *os.File) error {
					if err := syntheticWrite(file); err != nil {
						return err
					}
					if abort {
						return context.Canceled
					}

					return nil
				}, ops)
				closed()
				if !errors.Is(err, cleanupError) || (abort && !errors.Is(err, context.Canceled)) {
					t.Fatalf("cleanup failure lost: %v", err)
				}
				if abort {
					absent(t, filepath.Join(root, "output.test"))
					entries(t, root, 1)
				} else {
					content(t, filepath.Join(root, "output.test"), "synthetic.test\n")
					entries(t, root, 2)
				}
			})
		}
	}
}

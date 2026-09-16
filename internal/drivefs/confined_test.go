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
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"golang.org/x/sys/unix"
)

func fixture(t *testing.T) string {
	t.Helper()

	base := "/tmp"
	if runtime.GOOS == "darwin" {
		// /tmp and /var are symlinks on macOS; supply the actual allowed root.
		base = "/private/tmp"
	}
	root, err := os.MkdirTemp(base, "drivefs.test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { must(t, os.RemoveAll(root)) })

	return root
}

func must(t *testing.T, err error) {
	t.Helper()

	if err != nil {
		t.Fatal(err)
	}
}

func content(t *testing.T, path, want string) {
	t.Helper()

	got, err := os.ReadFile(path)
	must(t, err)
	if string(got) != want {
		t.Fatalf("content = %q, want %q", got, want)
	}
}

func entries(t *testing.T, path string, want int) {
	t.Helper()

	got, err := os.ReadDir(path)
	must(t, err)
	if len(got) != want {
		t.Fatalf("directory has %d entries, want %d", len(got), want)
	}
}

func syntheticWrite(file *os.File) error {
	_, err := file.WriteString("synthetic.test\n")

	return err
}

// Track every owned descriptor, including the filesystem-root and output
// descriptors. Check the actual OS state before any new opens can reuse them.
func tracked(t *testing.T) (confinedOps, func()) {
	t.Helper()

	var descriptors []int
	ops := confinedOps{open: func(parent int, name string, flags int, mode uint32) (int, error) {
		fd, err := platformOpen(parent, name, flags, mode)
		if err == nil {
			descriptors = append(descriptors, fd)
		}

		return fd, err
	}}

	return ops, func() {
		for _, fd := range descriptors {
			if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); !errors.Is(err, unix.EBADF) {
				t.Fatalf("descriptor %d not closed: %v", fd, err)
			}
		}
	}
}

func TestConfinedOutputBasic(t *testing.T) {
	for _, name := range []string{"success", "empty root", "absolute", "parent", "missing parent", "writer error", "root symlink"} {
		t.Run(name, func(t *testing.T) {
			root := fixture(t)
			must(t, os.Mkdir(filepath.Join(root, "nested"), 0700))
			selectedRoot, destination := root, "nested/output.test"
			writerError := errors.New("synthetic writer failure")
			switch name {
			case "empty root":
				selectedRoot = ""
			case "absolute":
				destination = filepath.Join(root, "nested/output.test")
			case "parent":
				destination = "nested/../output.test"
			case "missing parent":
				destination = "nested/missing/output.test"
			case "root symlink":
				alias := filepath.Join(fixture(t), "alias")
				must(t, os.Symlink(root, alias))
				selectedRoot = alias
			}

			ops, closed := tracked(t)
			called := false
			err := writeFresh(selectedRoot, destination, func(file *os.File) error {
				called = true
				if name == "writer error" {
					return writerError
				}

				return syntheticWrite(file)
			}, ops)
			closed()

			if name == "success" {
				must(t, err)
				if !called {
					t.Fatal("writer was not called")
				}
				content(t, filepath.Join(root, destination), "synthetic.test\n")
			} else if name == "writer error" {
				if !called || !errors.Is(err, writerError) {
					t.Fatalf("writer error = %v, called = %v", err, called)
				}
			} else {
				if err == nil || called {
					t.Fatalf("unsafe output: error = %v, called = %v", err, called)
				}
				entries(t, filepath.Join(root, "nested"), 0)
				entries(t, root, 1)
			}
		})
	}
}

// The opener blocks at an explicit hook until the test completes substitution.
// There are no sleeps and no races between mutation and syscall execution.
func atBarrier(t *testing.T, root, destination string, afterResolution bool, mutate func()) (error, bool) {
	t.Helper()

	reached, resume := make(chan struct{}), make(chan struct{})
	hook := func() {
		close(reached)

		<-resume
	}
	ops := confinedOps{open: platformOpen}
	if afterResolution {
		ops.resolved = hook
	} else {
		ops.selected = hook
	}
	done := make(chan error, 1)
	called := false
	go func() {
		done <- writeFresh(root, destination, func(file *os.File) error {
			called = true

			return syntheticWrite(file)
		}, ops)
	}()

	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("opener failed before synchronization barrier: %v", err)
	}
	// Also release the worker if a fixture assertion aborts this test.
	func() {
		defer close(resume)
		mutate()
	}()
	err := <-done

	return err, called
}

func TestConfinedOutputSymlinks(t *testing.T) {
	for _, intermediate := range []bool{false, true} {
		for _, substituted := range []bool{false, true} {
			name := "final"
			if intermediate {
				name = "intermediate"
			}
			if substituted {
				name += "/substituted"
			} else {
				name += "/initial"
			}
			t.Run(name, func(t *testing.T) {
				root, outside := fixture(t), fixture(t)
				sentinel := filepath.Join(outside, "sentinel.test")
				must(t, os.WriteFile(sentinel, []byte("unchanged.test"), 0600))
				must(t, os.Mkdir(filepath.Join(root, "nested"), 0700))
				link, target := filepath.Join(root, "nested/output.test"), sentinel
				if intermediate {
					link, target = filepath.Join(root, "nested"), outside
				}
				substitute := func() {
					if intermediate {
						must(t, os.Remove(link))
					}
					must(t, os.Symlink(target, link))
				}

				var err error
				called := false
				if substituted {
					err, called = atBarrier(t, root, "nested/output.test", false, substitute)
				} else {
					substitute()
					err = WriteFresh(root, "nested/output.test", func(file *os.File) error {
						called = true

						return syntheticWrite(file)
					})
				}

				if err == nil || called {
					t.Fatalf("symlink accepted: %v, writer = %v", err, called)
				}
				content(t, sentinel, "unchanged.test")
				entries(t, outside, 1)
				entries(t, root, 1)
				if !intermediate {
					entries(t, filepath.Join(root, "nested"), 1)
				}
			})
		}
	}
}

func TestConfinedOutputRename(t *testing.T) {
	for _, replacement := range []string{"directory", "symlink"} {
		t.Run(replacement, func(t *testing.T) {
			root, outside := fixture(t), fixture(t)
			original, moved := filepath.Join(root, "nested"), filepath.Join(root, "moved")
			must(t, os.Mkdir(original, 0700))
			must(t, os.WriteFile(filepath.Join(outside, "sentinel.test"), []byte("unchanged.test"), 0600))

			err, called := atBarrier(t, root, "nested/output.test", true, func() {
				must(t, os.Rename(original, moved))
				if replacement == "directory" {
					must(t, os.Mkdir(original, 0700))
				} else {
					must(t, os.Symlink(outside, original))
				}
			})
			must(t, err)
			if !called {
				t.Fatal("writer was not called")
			}

			content(t, filepath.Join(moved, "output.test"), "synthetic.test\n")
			content(t, filepath.Join(outside, "sentinel.test"), "unchanged.test")
			entries(t, outside, 1)
			if replacement == "directory" {
				entries(t, original, 0)
			}
		})
	}
}

func TestConfinedOutputUnavailable(t *testing.T) {
	for _, failure := range []struct {
		name string
		err  error
	}{
		{"unavailable syscall", unix.ENOSYS},
		{"unsupported required flag", unix.EINVAL},
		{"unresolved resolution race", unix.EAGAIN},
	} {
		for _, stage := range []string{"directory", "output"} {
			t.Run(failure.name+"/"+stage, func(t *testing.T) {
				root := fixture(t)
				must(t, os.Mkdir(filepath.Join(root, "nested"), 0700))
				ops, closed := tracked(t)
				realOpen := ops.open
				refused, called := 0, false
				ops.open = func(parent int, name string, flags int, mode uint32) (int, error) {
					if (stage == "directory" && name == "nested") || (stage == "output" && name == "output.test") {
						refused++

						return -1, failure.err
					}

					return realOpen(parent, name, flags, mode)
				}

				err := writeFresh(root, "nested/output.test", func(file *os.File) error {
					called = true

					return syntheticWrite(file)
				}, ops)
				closed()
				if !errors.Is(err, failure.err) || called || refused != 1 {
					t.Fatalf("error = %v, writer = %v, attempts = %d", err, called, refused)
				}
				entries(t, filepath.Join(root, "nested"), 0)
				entries(t, root, 1)
			})
		}
	}
}

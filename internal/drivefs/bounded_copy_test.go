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
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

type copyReaderFunc func([]byte) (int, error)

func (f copyReaderFunc) Read(p []byte) (int, error) { return f(p) }

type copyWriterFunc func([]byte) (int, error)

func (f copyWriterFunc) Write(p []byte) (int, error) { return f(p) }

func copyTransaction(ops transactionOps) func(string, string, func(*os.File) error) error {
	return func(root, destination string, write func(*os.File) error) error {
		return writeTransactional(root, destination, write, ops)
	}
}

func TestBoundedTransactionalCopySuccess(t *testing.T) {
	for _, limit := range []int64{14, 40} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			root := fixture(t)
			must(t, BoundedTransactionalCopy(context.Background(), root, "output.test", strings.NewReader("synthetic.test"), limit))
			content(t, filepath.Join(root, "output.test"), "synthetic.test")
			entries(t, root, 1)
		})
	}
	t.Run("post-publication-cleanup-error", func(t *testing.T) {
		root := fixture(t)
		ops, closed := transactionTracked(t)
		defer closed()
		cleanupErr := errors.New("synthetic.test cleanup failure")
		ops.remove = func(parent int, name string, flags int) error {
			if flags == unix.AT_REMOVEDIR {
				return cleanupErr
			}

			return unix.Unlinkat(parent, name, flags)
		}

		err := boundedTransactionalCopy(context.Background(), root, "output.test", strings.NewReader("synthetic.test"), 14, copyTransaction(ops))
		if !errors.Is(err, cleanupErr) {
			t.Fatalf("cleanup error = %v", err)
		}

		content(t, filepath.Join(root, "output.test"), "synthetic.test")
		entries(t, root, 2)
	})
}

func TestBoundedTransactionalCopyLimit(t *testing.T) {
	for _, limit := range []int64{1, 14, 65536} {
		t.Run(fmt.Sprint(limit), func(t *testing.T) {
			root := fixture(t)
			ops, closed := transactionTracked(t)
			defer closed()
			consumed, written := int64(0), int64(0)
			source := copyReaderFunc(func(p []byte) (int, error) {
				for i := range p {
					p[i] = 'x'
				}
				consumed += int64(len(p))

				return len(p), nil
			})
			// Instrument the writer inside the real transaction, keeping its cleanup.
			err := writeTransactional(root, "output.test", func(file *os.File) error {
				writer := copyWriterFunc(func(p []byte) (int, error) {
					written += int64(len(p))
					if written > limit {
						t.Fatalf("destination writes exceeded cap: %d", written)
					}

					return file.Write(p)
				})

				return copyBounded(context.Background(), writer, source, limit)
			}, ops)
			if !errors.Is(err, ErrCopyLimit) || consumed != limit+1 || written != limit {
				t.Fatalf("error = %v, consumed = %d, written = %d", err, consumed, written)
			}

			absent(t, filepath.Join(root, "output.test"))
			entries(t, root, 0)

			// Exercise the adapter's real callback as well as the writer seam.
			consumed = 0
			err = BoundedTransactionalCopy(context.Background(), root, "output.test", source, limit)
			if !errors.Is(err, ErrCopyLimit) || consumed != limit+1 {
				t.Fatalf("adapter error = %v, consumed = %d", err, consumed)
			}

			absent(t, filepath.Join(root, "output.test"))
			entries(t, root, 0)
		})
	}
}

func TestBoundedTransactionalCopyAbort(t *testing.T) {
	for _, kind := range []string{"entry", "between-chunks", "read-error", "read-with-data-error", "eof-cancellation", "probe-cancellation", "deadline", "no-progress"} {
		t.Run(kind, func(t *testing.T) {
			root := fixture(t)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			expected := context.Canceled
			calls := 0
			limit := int64(100)
			ops, closed := transactionTracked(t)
			defer closed()
			if kind == "entry" {
				cancel()
				ops.open = func(int, string, int, uint32) (int, error) {
					t.Fatal("entry cancellation invoked transaction")

					return -1, unix.EIO
				}
			}
			if kind == "deadline" {
				var stop context.CancelFunc
				ctx, stop = context.WithDeadline(context.Background(), time.Unix(0, 0))
				defer stop()
				expected = context.DeadlineExceeded
			}
			if strings.Contains(kind, "error") || kind == "no-progress" {
				expected = ErrCopyRead
			}
			if kind == "probe-cancellation" {
				limit = 1
			}
			source := copyReaderFunc(func(p []byte) (int, error) {
				calls++
				if kind == "no-progress" {
					return 0, nil
				}
				if calls == 1 {
					p[0] = 'x'
					if kind == "read-with-data-error" {
						return 1, errors.New("private synthetic.test reader diagnostic")
					}
					if kind == "eof-cancellation" {
						cancel()

						return 1, io.EOF
					}

					return 1, nil
				}

				// This deterministic between-chunk barrier observes partial output before
				// cancellation; no sleeps or scheduler timing are involved.
				stages, err := os.ReadDir(root)
				must(t, err)
				if len(stages) != 1 {
					t.Fatalf("stages = %d", len(stages))
				}
				content(t, filepath.Join(root, stages[0].Name(), "output"), "x")
				absent(t, filepath.Join(root, "output.test"))
				if kind == "read-error" {
					return 0, errors.New("private synthetic.test reader diagnostic")
				}

				cancel()

				return 0, io.EOF
			})

			err := boundedTransactionalCopy(ctx, root, "output.test", source, limit, copyTransaction(ops))
			if !errors.Is(err, expected) || strings.Contains(err.Error(), "private synthetic") {
				t.Fatalf("abort error = %v", err)
			}

			absent(t, filepath.Join(root, "output.test"))
			entries(t, root, 0)
		})
	}
	t.Run("cancel-after-write", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source := copyReaderFunc(func(p []byte) (int, error) {
			p[0] = 'x'

			return 1, io.EOF
		})
		writer := copyWriterFunc(func(p []byte) (int, error) {
			cancel()

			return len(p), nil
		})

		if err := copyBounded(ctx, writer, source, 1); !errors.Is(err, context.Canceled) {
			t.Fatalf("completion cancellation = %v", err)
		}
	})
}

func TestBoundedTransactionalCopyValidation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ctx    context.Context
		source io.Reader
		limit  int64
	}{
		{"nil-context", nil, strings.NewReader("test"), 4},
		{"nil-reader", context.Background(), nil, 4},
		{"zero", context.Background(), strings.NewReader("test"), 0},
		{"negative", context.Background(), strings.NewReader("test"), -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := fixture(t)
			transaction := func(string, string, func(*os.File) error) error {
				t.Fatal("invalid input invoked transaction")

				return nil
			}

			err := boundedTransactionalCopy(tc.ctx, root, "output.test", tc.source, tc.limit, transaction)
			if !errors.Is(err, ErrInvalidCopyInput) {
				t.Fatalf("validation error = %v", err)
			}

			entries(t, root, 0)
		})
	}
	t.Run("maximum-limit", func(t *testing.T) {
		root := fixture(t)
		must(t, BoundedTransactionalCopy(context.Background(), root, "output.test", bytes.NewBufferString("synthetic.test"), math.MaxInt64))
		content(t, filepath.Join(root, "output.test"), "synthetic.test")
		entries(t, root, 1)
	})
}

func TestBoundedTransactionalCopyCollision(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			root := fixture(t)
			final := filepath.Join(root, "output.test")
			create := func() {
				must(t, os.WriteFile(final, []byte("competitor.test"), 0600))
			}
			ops, closed := transactionTracked(t)
			defer closed()
			if race {
				ops.publish = func(from int, source string, to int, destination string) error {
					create()

					return publishLink(from, source, to, destination)
				}
			} else {
				create()
			}
			reads := 0
			source := copyReaderFunc(func(p []byte) (int, error) {
				reads++

				return copy(p, "synthetic.test"), io.EOF
			})

			err := boundedTransactionalCopy(context.Background(), root, "output.test", source, 14, copyTransaction(ops))
			if !errors.Is(err, os.ErrExist) || (!race && reads != 0) {
				t.Fatalf("collision error = %v, reads = %d", err, reads)
			}

			content(t, final, "competitor.test")
			entries(t, root, 1)
		})
	}
}

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
	"io"
	"os"
)

var (
	// ErrInvalidCopyInput identifies a missing context/reader or nonpositive cap.
	ErrInvalidCopyInput = errors.New("invalid bounded copy input")
	// ErrCopyLimit identifies input exceeding the destination byte cap.
	ErrCopyLimit = errors.New("bounded copy byte limit exceeded")
	// ErrCopyRead identifies a source failure without exposing source diagnostics.
	ErrCopyRead = errors.New("bounded copy source read failed")
)

// BoundedTransactionalCopy copies source into a fresh transactional output with
// a positive byte limit. It writes at most limit bytes and consumes at most one
// additional byte to distinguish exact-limit EOF from oversized input. Invalid
// arguments and entry cancellation are rejected before invoking the transaction.
// Cancellation returns context.Canceled or context.DeadlineExceeded; source
// failures return ErrCopyRead without retaining the source error text.
//
// Cancellation is cooperative: checks precede reads/writes and successful return
// to publication, but cannot interrupt an arbitrary blocked Read or Write. Future
// CLI integration must provide an interruptible source and terminate/reap its
// child. The cap bounds destination bytes, not a pathname-only CLI's upstream
// staging writes. Cancellation racing after the final check may not prevent
// publication, and cancellation cannot roll back a published file.
//
// WriteTransactional's filesystem trust, cleanup and publication semantics are
// unchanged: cleanup errors may leave temporary output, and an error after
// publication may leave a complete final file. No stronger rollback or durability
// is promised. This internal prerequisite is not registered as an MCP tool and
// does not enable CLI downloads.
func BoundedTransactionalCopy(ctx context.Context, root, destination string, source io.Reader, limit int64) error {
	return boundedTransactionalCopy(ctx, root, destination, source, limit, WriteTransactional)
}

func boundedTransactionalCopy(ctx context.Context, root, destination string, source io.Reader, limit int64, transaction func(string, string, func(*os.File) error) error) error {
	if ctx == nil || source == nil || limit <= 0 {
		return ErrInvalidCopyInput
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	return transaction(root, destination, func(file *os.File) error {
		return copyBounded(ctx, file, source, limit)
	})
}

// copyBounded also supplies a narrow writer seam for byte-bound tests. Its
// arguments have already been validated by boundedTransactionalCopy.
func copyBounded(ctx context.Context, destination io.Writer, source io.Reader, remaining int64) error {
	var buffer [32 * 1024]byte
	emptyReads := 0

	for {
		if err := ctx.Err(); err != nil {
			return err
		}

		size := int64(len(buffer))
		if remaining < size {
			size = remaining
		}
		if remaining == 0 {
			size = 1 // Probe without adding one to a possibly maximum-int64 cap.
		}

		n, readErr := source.Read(buffer[:int(size)])

		if err := ctx.Err(); err != nil {
			return err
		}
		if n < 0 || n > int(size) {
			return ErrCopyRead
		}
		if remaining == 0 && n > 0 {
			return ErrCopyLimit
		}

		if n > 0 {
			written, err := destination.Write(buffer[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}

			remaining -= int64(n)
			emptyReads = 0
		} else {
			emptyReads++
		}

		if readErr == io.EOF {
			return ctx.Err()
		}
		if readErr != nil || emptyReads >= 100 {
			return ErrCopyRead
		}
	}
}

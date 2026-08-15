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

package drivemcp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/drivecli"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func newGateTestClient(t *testing.T, scenario string, timeout time.Duration) (*drivecli.Client, string) {
	t.Helper()

	binary := testkit.FakeDrive(t, scenario, nil)
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary, Timeout: timeout})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client, binary
}

func TestGateLatchesOneSuccessfulHandshakeAcrossConcurrentRequests(t *testing.T) {
	t.Parallel()

	client, binary := newGateTestClient(t, "", 0)
	gate := newHandshakeGate()

	var group sync.WaitGroup
	results := make([]error, 8)
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			results[index] = gate.ensure(context.Background(), client)
		}()
	}
	group.Wait()
	for index, err := range results {
		if err != nil {
			t.Fatalf("concurrent ensure %d: %v", index, err)
		}
	}

	// A latched gate never negotiates again: removing the binary proves that a
	// further ensure cannot be re-executing the CLI.
	if err := os.Remove(binary); err != nil {
		t.Fatalf("remove fake binary: %v", err)
	}
	if err := gate.ensure(context.Background(), client); err != nil {
		t.Fatalf("ensure after latch: %v", err)
	}
}

func TestGateFailsClosedOnMismatchAndRetriesOnTheNextRequest(t *testing.T) {
	t.Parallel()

	client, binary := newGateTestClient(t, "version-mismatch", 0)
	gate := newHandshakeGate()

	err := gate.ensure(context.Background(), client)
	if drivecli.CodeOf(err) != drivecli.CodeVersionMismatch {
		t.Fatalf("mismatch ensure error = %v, want %q", err, drivecli.CodeVersionMismatch)
	}

	// Failure must not latch: once the operator repairs the CLI, the next
	// request performs a fresh handshake and succeeds.
	if err := os.Remove(filepath.Join(filepath.Dir(binary), "scenario")); err != nil {
		t.Fatalf("repair fake scenario: %v", err)
	}
	if err := gate.ensure(context.Background(), client); err != nil {
		t.Fatalf("ensure after repair: %v", err)
	}
}

func TestGateFailsClosedOnMalformedBannerAndTimeout(t *testing.T) {
	t.Parallel()

	malformedClient, _ := newGateTestClient(t, "malformed-version", 0)
	if err := newHandshakeGate().ensure(context.Background(), malformedClient); drivecli.CodeOf(err) != drivecli.CodeMalformedOutput {
		t.Fatalf("malformed banner ensure error = %v, want %q", err, drivecli.CodeMalformedOutput)
	}

	hangingClient, _ := newGateTestClient(t, "hang", 100*time.Millisecond)
	if err := newHandshakeGate().ensure(context.Background(), hangingClient); drivecli.CodeOf(err) != drivecli.CodeTimedOut {
		t.Fatalf("hanging ensure error = %v, want %q", err, drivecli.CodeTimedOut)
	}
}

func TestGateWaitersFailClosedWhenTheirContextEndsDuringNegotiation(t *testing.T) {
	t.Parallel()

	client, _ := newGateTestClient(t, "", 0)
	gate := newHandshakeGate()

	// Occupy the negotiation slot to model an in-flight handshake.
	gate.slot <- struct{}{}
	defer func() { <-gate.slot }()

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := gate.ensure(canceled, client); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled waiter error = %v, want context.Canceled", err)
	}

	expired, expire := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer expire()
	if err := gate.ensure(expired, client); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired waiter error = %v, want context.DeadlineExceeded", err)
	}
}

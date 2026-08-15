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
	"sync/atomic"

	"github.com/wevial/croton-mcp/internal/drivecli"
)

// handshakeGate orders every data command after one successful exact-version
// CLI handshake. Negotiation is single-flight: concurrent requests wait,
// context-aware, on the one in-flight attempt instead of racing past it.
type handshakeGate struct {
	slot     chan struct{}
	verified atomic.Bool
}

func newHandshakeGate() *handshakeGate {
	return &handshakeGate{slot: make(chan struct{}, 1)}
}

// ensure returns nil only after the pinned-version handshake has succeeded.
// Success latches for the process lifetime. Failure latches nothing: it fails
// only the requests that observed it, and the next request negotiates again,
// so each data request performs at most one handshake attempt.
func (gate *handshakeGate) ensure(ctx context.Context, client *drivecli.Client) error {
	if gate.verified.Load() {
		return nil
	}

	select {
	case gate.slot <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-gate.slot }()

	if gate.verified.Load() {
		return nil
	}
	if err := client.Handshake(ctx); err != nil {
		return err
	}

	gate.verified.Store(true)
	return nil
}

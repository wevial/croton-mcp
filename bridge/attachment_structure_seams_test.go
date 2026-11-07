package bridge

import (
	"context"
	"net"
	"sync"
)

// This file is the only test code that names the private dedicated-reader
// seam. It registers attachmentStructureHooks, so the attachment structure
// tests compile against a baseline without the seam and fail there at the
// first hook use instead.
func init() {
	attachmentStructureHooks.scripted = func(adapter *Adapter, answer func(attempt int) (selectedStructure, error)) func() []scriptedAttempt {
		var mu sync.Mutex
		var attempts []scriptedAttempt
		adapter.reader = func(ctx context.Context, _ ValidatedConfig, request structureRequest) (selectedStructure, error) {
			mu.Lock()
			index := len(attempts)
			attempts = append(attempts, scriptedAttempt{ctx: ctx, mailbox: request.Mailbox, uid: request.UID, uidValidity: request.UIDValidity})
			mu.Unlock()

			return answer(index)
		}

		return func() []scriptedAttempt {
			mu.Lock()
			defer mu.Unlock()

			return append([]scriptedAttempt(nil), attempts...)
		}
	}

	attachmentStructureHooks.transport = func(adapter *Adapter, wrap func(attempt int, transport net.Conn) net.Conn) {
		var mu sync.Mutex
		attempt := 0
		adapter.reader = func(ctx context.Context, config ValidatedConfig, request structureRequest) (selectedStructure, error) {
			mu.Lock()
			index := attempt
			attempt++
			mu.Unlock()

			seams := structureSeams{transport: func(transport net.Conn) net.Conn { return wrap(index, transport) }}
			return readStructure(ctx, config, request, seams)
		}
	}
}

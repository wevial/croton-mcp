package bridge

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestAdapterCancelsQueuedCallerBeforeSessionUse(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	firstResult := make(chan error, 1)
	session := &oversizedListSession{}
	adapter := &Adapter{
		config: ValidatedConfig{IMAP: IMAPConfig{CommandTimeoutMs: 1000}},
		factory: func(context.Context) (readSession, error) {
			return session, nil
		},
		gate: make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}

	go func() {
		firstResult <- adapter.execute(context.Background(), func(context.Context, readSession) error {
			close(started)
			<-release
			return nil
		})
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first operation did not acquire the adapter gate")
	}

	queuedContext, cancel := context.WithCancel(context.Background())
	cancel()
	if err := adapter.execute(queuedContext, func(context.Context, readSession) error {
		t.Fatal("cancelled caller acquired the adapter gate")
		return nil
	}); CodeOf(err) != CodeOperationCanceled {
		t.Fatalf("queued operation error = %v, want %q", err, CodeOperationCanceled)
	}

	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("first operation: %v", err)
	}
}

func TestAdapterDoesNotReplayNonTransportFailures(t *testing.T) {
	for _, code := range []string{
		CodeOperationCanceled,
		CodeCommandTimedOut,
		CodeIMAPCommand,
		CodeStaleMessageID,
		CodeBoundsExceeded,
		CodeIMAPProtocol,
	} {
		t.Run(code, func(t *testing.T) {
			var mu sync.Mutex
			factoryCalls := 0
			adapter := &Adapter{
				config: ValidatedConfig{IMAP: IMAPConfig{CommandTimeoutMs: 1000}},
				factory: func(context.Context) (readSession, error) {
					mu.Lock()
					defer mu.Unlock()

					factoryCalls++
					return &oversizedListSession{}, nil
				},
				gate: make(chan struct{}, 1),
			}
			adapter.gate <- struct{}{}

			err := adapter.execute(context.Background(), func(context.Context, readSession) error {
				return errorCode(code)
			})
			if CodeOf(err) != code {
				t.Fatalf("operation error = %v, want %q", err, code)
			}

			mu.Lock()
			defer mu.Unlock()
			if factoryCalls != 1 {
				t.Fatalf("session factory calls = %d, want one", factoryCalls)
			}
		})
	}
}

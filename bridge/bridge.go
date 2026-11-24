package bridge

import (
	"context"
	"sync"
	"time"
)

type sessionFactory func(context.Context) (bridgeSession, error)

// Adapter serializes a single authenticated IMAP session. Its only writes are
// the opt-in, Seen-only SetSeen and native Move; every other operation is
// read-only.
type Adapter struct {
	config  ValidatedConfig
	factory sessionFactory
	gate    chan struct{}
	idKey   [32]byte

	mu      sync.Mutex
	session bridgeSession
	closed  bool
}

// NewAdapter validates configuration and creates a read-only adapter. It does
// not dial the Bridge or execute the credential command until an operation.
func NewAdapter(input Config) (*Adapter, error) {
	config, err := ValidateConfig(input)
	if err != nil {
		return nil, err
	}

	idKey, err := newAdapterIDKey()
	if err != nil {
		return nil, errorCode(CodeBridgeUnreachable)
	}

	adapter := &Adapter{
		config: config,
		gate:   make(chan struct{}, 1),
		idKey:  idKey,
	}
	adapter.gate <- struct{}{}
	adapter.factory = adapter.openSession

	return adapter, nil
}

func (adapter *Adapter) openSession(ctx context.Context) (bridgeSession, error) {
	connection, err := Dial(ctx, Config{IMAP: adapter.config.IMAP})
	if err != nil {
		return nil, err
	}

	session, err := newIMAPSession(ctx, connection)
	if err != nil {
		_ = connection.Close()
		return nil, err
	}

	credentials, err := LoadCredentials(ctx, adapter.config.IMAP.CredentialCommand, time.Duration(adapter.config.IMAP.CommandTimeoutMs)*time.Millisecond)
	if err != nil {
		_ = session.Abort()
		return nil, err
	}
	if err := session.Authenticate(ctx, credentials); err != nil {
		_ = session.Abort()
		return nil, err
	}

	return session, nil
}

func (adapter *Adapter) execute(ctx context.Context, operation func(context.Context, bridgeSession) error) error {
	if err := adapter.acquire(ctx); err != nil {
		return err
	}
	defer func() { adapter.gate <- struct{}{} }()

	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	for attempt := 0; ; attempt++ {
		session, err := adapter.sessionForOperation(operationContext)
		if err != nil {
			err = mapIMAPError(operationContext, err)
		} else {
			err = operation(operationContext, session)
			if err == nil {
				return nil
			}
		}

		retry := attempt == 0 && adapter.canReplay(operationContext, err)
		if session != nil && adapter.invalidatesSession(err) {
			adapter.invalidate(session)
		}
		if !retry {
			return err
		}
	}
}

func (adapter *Adapter) sessionForOperation(ctx context.Context) (bridgeSession, error) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()

	if adapter.closed {
		return nil, errorCode(CodeAdapterClosed)
	}
	if adapter.session == nil {
		session, err := adapter.factory(ctx)
		if err != nil {
			return nil, err
		}

		adapter.session = session
	}

	return adapter.session, nil
}

func (adapter *Adapter) canReplay(ctx context.Context, err error) bool {
	return ctx.Err() == nil && CodeOf(err) == CodeBridgeUnreachable
}

func (adapter *Adapter) invalidatesSession(err error) bool {
	switch CodeOf(err) {
	case CodeBridgeUnreachable, CodeCommandTimedOut, CodeOperationCanceled, CodeBoundsExceeded, CodeIMAPProtocol:
		return true
	default:
		return false
	}
}

func (adapter *Adapter) acquire(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return mapIMAPError(ctx, ctx.Err())
	case <-adapter.gate:
		return nil
	}
}

func (adapter *Adapter) operationContext(ctx context.Context) (context.Context, context.CancelFunc) {
	timeout := time.Duration(adapter.config.IMAP.CommandTimeoutMs) * time.Millisecond
	return context.WithTimeout(ctx, timeout)
}

func (adapter *Adapter) invalidate(session bridgeSession) {
	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.session == session {
		_ = session.Abort()
		adapter.session = nil
	}
}

// Close logs out the retained session, then closes its transport. It is safe to
// call repeatedly.
func (adapter *Adapter) Close() error {
	if adapter == nil {
		return nil
	}
	<-adapter.gate
	defer func() { adapter.gate <- struct{}{} }()

	adapter.mu.Lock()
	defer adapter.mu.Unlock()
	if adapter.closed {
		return nil
	}
	adapter.closed = true
	if adapter.session == nil {
		return nil
	}

	logoutContext, cancel := adapter.operationContext(context.Background())
	defer cancel()
	err := adapter.session.Logout(logoutContext)
	adapter.session = nil
	return err
}

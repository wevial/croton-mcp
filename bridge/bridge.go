package bridge

import (
	"context"
	"sync"
	"time"
)

type sessionFactory func(context.Context) (readSession, error)

// Adapter serializes a single authenticated read-only IMAP session.
type Adapter struct {
	config  ValidatedConfig
	factory sessionFactory
	gate    chan struct{}
	idKey   [32]byte

	mu      sync.Mutex
	session readSession
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

func (adapter *Adapter) openSession(ctx context.Context) (readSession, error) {
	connection, err := Dial(ctx, Config{IMAP: adapter.config.IMAP, Bounds: boundsPatch(adapter.config.Bounds), Audit: adapter.config.Audit})
	if err != nil {
		return nil, err
	}

	session, err := newIMAPSession(connection)
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

func (adapter *Adapter) execute(ctx context.Context, operation func(context.Context, readSession) error) error {
	if err := adapter.acquire(ctx); err != nil {
		return err
	}
	defer func() { adapter.gate <- struct{}{} }()

	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	adapter.mu.Lock()
	if adapter.closed {
		adapter.mu.Unlock()
		return errorCode(CodeAdapterClosed)
	}
	if adapter.session == nil {
		session, err := adapter.factory(operationContext)
		if err != nil {
			adapter.mu.Unlock()
			return err
		}
		adapter.session = session
	}
	session := adapter.session
	adapter.mu.Unlock()

	if err := operation(operationContext, session); err != nil {
		if CodeOf(err) == CodeBridgeUnreachable || CodeOf(err) == CodeCommandTimedOut || CodeOf(err) == CodeOperationCanceled || CodeOf(err) == CodeBoundsExceeded || CodeOf(err) == CodeIMAPProtocol {
			adapter.invalidate(session)
		}
		return err
	}

	return nil
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
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < timeout {
			timeout = remaining
		}
	}
	return context.WithTimeout(ctx, timeout)
}

func (adapter *Adapter) invalidate(session readSession) {
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
	if err := adapter.acquire(context.Background()); err != nil {
		return err
	}
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

func boundsPatch(bounds Bounds) BoundsPatch {
	return BoundsPatch{
		MaxSearchResults:   Int(bounds.MaxSearchResults),
		MaxFolderResults:   Int(bounds.MaxFolderResults),
		MaxBodyBytes:       Int(bounds.MaxBodyBytes),
		MaxHeaderBytes:     Int(bounds.MaxHeaderBytes),
		MaxMimeParts:       Int(bounds.MaxMimeParts),
		MaxAttachmentCount: Int(bounds.MaxAttachmentCount),
		MaxThreadMessages:  Int(bounds.MaxThreadMessages),
		MaxThreadFetches:   Int(bounds.MaxThreadFetches),
		MaxPreviewChars:    Int(bounds.MaxPreviewChars),
		MaxOutputBytes:     Int(bounds.MaxOutputBytes),
	}
}

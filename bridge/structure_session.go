package bridge

import (
	"context"
	"net"
	"sync"
	"time"
)

type structureSessionState int

const (
	structureConnected structureSessionState = iota
	structureAuthenticated
	structureSelected
	structureDispatched
	structureClosed
)

// selectedStructure is the extended BODYSTRUCTURE of one selected UID and the
// mailbox generation it was read under.
type selectedStructure struct {
	Snapshot  mailboxSnapshot
	Structure bodyStructure
}

// structureSession is a dedicated, never pooled session that reads the
// extended BODYSTRUCTURE of one selected UID. Its structureQuarantine is the
// only reader of the decrypted transport from before the client is
// constructed until the transport is closed, so no server byte reaches the
// client unframed and there is no arm, disarm or pool-return interval. The
// session dispatches at most one UID FETCH, of UID and BODYSTRUCTURE, and is
// closed after it without fallback. docs/design/0010-structure-quarantine.md
// records the stack.
type structureSession struct {
	session    *imapSession
	quarantine *structureQuarantine
	uid        uint32
	stop       func()

	mu    sync.Mutex
	state structureSessionState
}

// quarantineConn presents the quarantine as the read side of the transport.
// Writes and Close reach the transport unchanged.
type quarantineConn struct {
	net.Conn
	quarantine *structureQuarantine
}

func (connection *quarantineConn) Read(buffer []byte) (int, error) {
	return connection.quarantine.Read(buffer)
}

// readSelectedStructure runs one dedicated session under a single finite
// operation deadline that covers connect, the credential helper,
// authentication, selection, the existence check, the structure fetch and
// close. tap is a private test seam between the quarantine and the
// decoder-facing layers; production callers pass nil.
func readSelectedStructure(parent context.Context, config ValidatedConfig, mailbox string, uid uint32, tap func(net.Conn) net.Conn) (selectedStructure, error) {
	if uid == 0 {
		return selectedStructure{}, errorCode(CodeInvalidRequest)
	}

	timeout := time.Duration(config.IMAP.CommandTimeoutMs) * time.Millisecond
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()

	connection, err := Dial(ctx, Config{IMAP: config.IMAP, Bounds: boundsPatch(config.Bounds), Audit: config.Audit})
	if err != nil {
		return selectedStructure{}, err
	}
	transport, startTLS, err := connection.takeConn()
	if err != nil {
		return selectedStructure{}, err
	}

	session, err := newStructureSessionOver(ctx, transport, startTLS, uid, tap)
	if err != nil {
		return selectedStructure{}, mapIMAPError(ctx, err)
	}

	result, err := session.run(ctx, config, mailbox)
	if err != nil {
		session.Abort(err)
		return selectedStructure{}, mapIMAPError(ctx, err)
	}

	if err := session.Close(ctx); err != nil {
		return selectedStructure{}, err
	}
	return result, nil
}

func (session *structureSession) run(ctx context.Context, config ValidatedConfig, mailbox string) (selectedStructure, error) {
	credentials, err := LoadCredentials(ctx, config.IMAP.CredentialCommand, time.Duration(config.IMAP.CommandTimeoutMs)*time.Millisecond)
	if err != nil {
		return selectedStructure{}, err
	}
	if err := session.Authenticate(ctx, credentials); err != nil {
		return selectedStructure{}, err
	}

	snapshot, err := session.Examine(ctx, mailbox)
	if err != nil {
		return selectedStructure{}, err
	}

	structure, err := session.FetchStructure(ctx)
	if err != nil {
		return selectedStructure{}, err
	}

	return selectedStructure{Snapshot: snapshot, Structure: structure}, nil
}

// newStructureSessionOver installs the quarantine directly on the established
// decrypted transport and only then constructs the client, whose reader
// starts at once. ctx must carry the deadline of the whole operation; it
// bounds the session until Close or Abort.
func newStructureSessionOver(ctx context.Context, transport net.Conn, startTLS bool, uid uint32, tap func(net.Conn) net.Conn) (*structureSession, error) {
	deadline, hasDeadline := ctx.Deadline()
	if transport == nil {
		return nil, errorCode(CodeInvalidRequest)
	}
	if !hasDeadline || uid == 0 {
		_ = transport.Close()
		return nil, errorCode(CodeInvalidRequest)
	}

	wrapped := &deadlineConn{Conn: transport}
	quarantine, err := newStructureQuarantine(wrapped, uid)
	if err != nil {
		_ = transport.Close()
		return nil, err
	}
	reader := net.Conn(&quarantineConn{Conn: wrapped, quarantine: quarantine})
	if tap != nil {
		reader = tap(reader)
	}
	budget := newReadBudgetConn(reader, maxControlResponseBytes)
	session := &structureSession{
		session:    &imapSession{conn: wrapped, budget: budget},
		quarantine: quarantine,
		uid:        uid,
	}

	wrapped.setLimit(deadline)
	session.stop = closeOnContext(ctx, wrapped)
	if err := session.session.startClient(ctx, budget, startTLS); err != nil {
		session.stop()
		return nil, session.refusal(ctx, err)
	}

	return session, nil
}

// Authenticate logs in once. Credentials are zeroed on every path.
func (session *structureSession) Authenticate(ctx context.Context, credentials Credentials) error {
	if err := session.advance(structureConnected, structureAuthenticated); err != nil {
		credentials.Zero()
		return err
	}

	return session.refusal(ctx, session.session.Authenticate(ctx, credentials))
}

// Examine examines mailbox read-only and requires the selected UID to exist
// with one exact UID SEARCH before any structure dispatch.
func (session *structureSession) Examine(ctx context.Context, mailbox string) (mailboxSnapshot, error) {
	if err := session.advance(structureAuthenticated, structureSelected); err != nil {
		return mailboxSnapshot{}, err
	}

	snapshot, err := session.session.Examine(ctx, mailbox)
	if err != nil {
		return mailboxSnapshot{}, session.refusal(ctx, err)
	}
	if err := requireMessageUID(ctx, session.session, session.uid); err != nil {
		return mailboxSnapshot{}, session.refusal(ctx, err)
	}

	return snapshot, nil
}

// FetchStructure dispatches the session's only FETCH: UID FETCH of the
// selected UID for UID and extended BODYSTRUCTURE. The response must carry
// that UID and one extended structure, and may carry FLAGS. A second call
// never dispatches.
func (session *structureSession) FetchStructure(ctx context.Context) (bodyStructure, error) {
	if err := session.advance(structureSelected, structureDispatched); err != nil {
		return nil, err
	}
	if err := session.quarantine.beginDispatch(); err != nil {
		return nil, err
	}

	structure, err := session.session.fetchUIDStructure(ctx, session.uid)
	if err != nil {
		return nil, session.refusal(ctx, err)
	}

	return structure, nil
}

// Close logs out and closes the transport. It is bounded by the operation
// deadline and never retries.
func (session *structureSession) Close(ctx context.Context) error {
	if !session.finish() {
		return nil
	}
	defer session.stop()

	return session.refusal(ctx, session.session.Logout(ctx))
}

// Abort ends the quarantine with cause unless it was already refused, closes
// the transport without draining it and waits for the client reader.
func (session *structureSession) Abort(cause error) {
	if !session.finish() {
		return
	}
	defer session.stop()

	_ = session.quarantine.abort(cause)

	_ = session.session.conn.Close()
	_ = session.session.client.Close()
}

// advance moves the session from one state to the next or refuses the call.
// A refused call leaves the state unchanged and dispatches nothing.
func (session *structureSession) advance(from, to structureSessionState) error {
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.state != from {
		return errorCode(CodeIMAPProtocol)
	}

	session.state = to
	return nil
}

// finish reports whether this call is the one that ends the session.
func (session *structureSession) finish() bool {
	session.mu.Lock()
	defer session.mu.Unlock()

	if session.state == structureClosed {
		return false
	}

	session.state = structureClosed
	return true
}

// refusal prefers the quarantine's stable refusal over the client error it
// caused, unless the operation context already ended.
func (session *structureSession) refusal(ctx context.Context, err error) error {
	if err == nil || ctx.Err() != nil {
		return err
	}
	if terminal := session.quarantine.terminalError(); terminal != nil {
		return terminal
	}

	return err
}

func (quarantine *structureQuarantine) terminalError() error {
	quarantine.mu.Lock()
	defer quarantine.mu.Unlock()

	return quarantine.terminal
}

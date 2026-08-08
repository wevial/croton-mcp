package mcpserver

import (
	"context"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Connect creates one independently cancelable server session. Closing the
// session context closes its transport connection, which makes the SDK cancel
// every in-flight JSON-RPC handler before waiting for session shutdown.
func (server *Server) Connect(ctx context.Context, transport mcp.Transport, options *mcp.ServerSessionOptions) (*mcp.ServerSession, error) {
	return server.sdk.Connect(ctx, contextClosingTransport{Transport: transport}, options)
}

// Run preserves the SDK's one-session convenience behavior while applying the
// same per-connection cancellation used by direct Connect callers.
func (server *Server) Run(ctx context.Context, transport mcp.Transport) error {
	return server.sdk.Run(ctx, contextClosingTransport{Transport: transport})
}

type contextClosingTransport struct {
	mcp.Transport
}

func (transport contextClosingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := transport.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	wrapped := &contextClosingConnection{Connection: connection}
	stop := context.AfterFunc(ctx, func() { _ = wrapped.closeUnderlying() })
	wrapped.stopMu.Lock()
	wrapped.stop = stop
	wrapped.stopMu.Unlock()
	return wrapped, nil
}

type contextClosingConnection struct {
	mcp.Connection

	closeOnce sync.Once
	closeErr  error
	stopMu    sync.Mutex
	stop      func() bool
}

func (connection *contextClosingConnection) Close() error {
	connection.stopMu.Lock()
	stop := connection.stop
	connection.stop = nil
	connection.stopMu.Unlock()
	if stop != nil {
		stop()
	}
	return connection.closeUnderlying()
}

func (connection *contextClosingConnection) closeUnderlying() error {
	connection.closeOnce.Do(func() {
		connection.closeErr = connection.Connection.Close()
	})
	return connection.closeErr
}

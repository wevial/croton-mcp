package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

func TestServerNegotiatesCurrentAndLegacyProtocols(t *testing.T) {
	t.Parallel()

	if got := connectTestClient(t, Options{}).InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol version = %q, want 2026-07-28", got)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).sdk.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	connection, err := clientTransport.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect legacy transport: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	request, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy","version":"0"}}}`))
	if err != nil {
		t.Fatalf("decode legacy initialize: %v", err)
	}
	if err := connection.Write(context.Background(), request); err != nil {
		t.Fatalf("write legacy initialize: %v", err)
	}
	response, err := connection.Read(context.Background())
	if err != nil {
		t.Fatalf("read legacy initialize: %v", err)
	}
	wire, err := jsonrpc.EncodeMessage(response)
	if err != nil {
		t.Fatalf("encode legacy response: %v", err)
	}
	var envelope struct {
		Result *mcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil || envelope.Result == nil || envelope.Result.ProtocolVersion != "2025-11-25" {
		t.Fatalf("legacy initialize = %s (%v)", wire, err)
	}
}

// blockingMail parks ListFolders until its handler context ends.
type blockingMail struct {
	fakeMail
	started chan context.Context
}

func (mail *blockingMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	mail.started <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

func waitFor[T any](t *testing.T, channel <-chan T, label string) T {
	t.Helper()

	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatalf("%s did not happen", label)
		panic("unreachable")
	}
}

func TestServeCancellationUnwindsActiveHandler(t *testing.T) {
	t.Parallel()

	mail := &blockingMail{started: make(chan context.Context, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	served := make(chan error, 1)
	go func() { served <- Serve(ctx, New(Options{Mail: mail}), serverTransport) }()

	client, err := mcp.NewClient(&mcp.Implementation{Name: "cancel-probe", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	called := make(chan struct{})
	go func() {
		_, _ = client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(called)
	}()
	handler := waitFor(t, mail.started, "handler start")

	cancel()

	if err := waitFor(t, served, "Serve unwind"); err != nil {
		t.Fatalf("Serve cancellation: %v", err)
	}
	waitFor(t, handler.Done(), "handler unwind")
	waitFor(t, called, "call unwind")
}

func TestServeReturnsNilOnCloseAndServerIsReusable(t *testing.T) {
	t.Parallel()

	server := New(Options{Mail: &fakeMail{folders: []bridge.Folder{{Name: "INBOX"}}}})
	for range 2 {
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		done := make(chan error, 1)
		go func() { done <- Serve(context.Background(), server, serverTransport) }()

		session, err := mcp.NewClient(&mcp.Implementation{Name: "reuse-probe", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
		if err != nil {
			t.Fatalf("connect client: %v", err)
		}
		if result := callTool(t, session, "list_folders", map[string]any{}); result.IsError {
			t.Fatalf("reused server failed: %s", resultText(t, result))
		}
		if err := session.Close(); err != nil {
			t.Fatalf("close client: %v", err)
		}
		if err := waitFor(t, done, "Serve stop"); err != nil {
			t.Fatalf("Serve after client close: %v", err)
		}
	}
}

type failingTransport struct {
	err error
}

func (transport failingTransport) Connect(context.Context) (mcp.Connection, error) {
	return nil, transport.err
}

func TestServeHidesTransportErrors(t *testing.T) {
	t.Parallel()

	const sentinel = "credential-sentinel person@privacy.test mailbox-sentinel"
	for _, tc := range []struct {
		err     error
		wantErr error
	}{
		{io.EOF, nil},
		{mcp.ErrConnectionClosed, nil},
		{context.Canceled, nil},
		{syscall.EPIPE, errServerUnavailable},
		{errors.New("EOF context canceled broken pipe"), errServerUnavailable},
	} {
		err := Serve(context.Background(), New(Options{}), failingTransport{err: fmt.Errorf("%s: %w", sentinel, tc.err)})
		if err != tc.wantErr {
			t.Fatalf("%v: Serve error = %v, want %v", tc.err, err, tc.wantErr)
		}
	}
}

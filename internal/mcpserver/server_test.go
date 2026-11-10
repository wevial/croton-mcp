package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

func TestServerNegotiatesCurrentAndLegacyProtocols(t *testing.T) {
	t.Parallel()

	if got := connectTestClient(t, Options{}).InitializeResult().ProtocolVersion; got != currentProtocolVersion {
		t.Fatalf("negotiated protocol version = %q, want %q", got, currentProtocolVersion)
	}

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).Connect(context.Background(), serverTransport, nil)
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
	if err := json.Unmarshal(wire, &envelope); err != nil || envelope.Result == nil || envelope.Result.ProtocolVersion != legacyProtocolVersion {
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

func startCall(session *mcp.ClientSession) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(done)
	}()
	return done
}

func TestCancellationUnwindsOnlyItsOwnSession(t *testing.T) {
	t.Parallel()

	mail := &blockingMail{started: make(chan context.Context, 3)}
	server := New(Options{Mail: mail})
	connect := func(serve bool) (context.CancelFunc, <-chan error, *mcp.ClientSession) {
		ctx, cancel := context.WithCancel(context.Background())
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		done := make(chan error, 1)
		if serve {
			go func() { done <- Serve(ctx, server, serverTransport) }()
		} else {
			session, err := server.Connect(ctx, serverTransport, nil)
			if err != nil {
				t.Fatalf("connect server: %v", err)
			}
			go func() { done <- session.Wait() }()
		}
		client, err := mcp.NewClient(&mcp.Implementation{Name: "cancel-probe", Version: "1"}, nil).Connect(context.Background(), clientTransport, nil)
		if err != nil {
			t.Fatalf("connect client: %v", err)
		}
		return cancel, done, client
	}

	cancelServed, served, servedClient := connect(true)
	cancelFirst, first, firstClient := connect(false)
	cancelSecond, second, secondClient := connect(false)
	servedCall, firstCall, secondCall := startCall(servedClient), startCall(firstClient), startCall(secondClient)
	handlers := []context.Context{waitFor(t, mail.started, "handler start"), waitFor(t, mail.started, "handler start"), waitFor(t, mail.started, "handler start")}

	cancelServed()
	if err := waitFor(t, served, "Serve unwind"); err != nil {
		t.Fatalf("Serve cancellation: %v", err)
	}
	cancelFirst()
	waitFor(t, first, "direct session unwind")
	waitFor(t, servedCall, "served call unwind")
	waitFor(t, firstCall, "first call unwind")
	for deadline := time.Now().Add(2 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		live := 0
		for _, handler := range handlers {
			if handler.Err() == nil {
				live++
			}
		}
		if live == 1 {
			break
		}
		if live == 0 || time.Now().After(deadline) {
			t.Fatalf("live handlers after two cancellations = %d, want 1", live)
		}
	}

	cancelSecond()
	waitFor(t, second, "second session unwind")
	waitFor(t, secondCall, "second call unwind")
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

type diagnosticTestWriter struct {
	io.Writer
	closer io.Closer
}

func (writer diagnosticTestWriter) Close() error {
	if writer.closer != nil {
		return writer.closer.Close()
	}
	return nil
}

func requireTransportEnd(t *testing.T, diagnostics *bytes.Buffer, category string) {
	t.Helper()

	if got := strings.TrimSpace(diagnostics.String()); got != `{"event":"transport_end","category":"`+category+`"}` {
		t.Fatalf("lifecycle diagnostics = %q, want only %s", got, category)
	}
}

func TestTransportDiagnostics(t *testing.T) {
	t.Parallel()

	const sentinel = "credential-sentinel person@privacy.test mailbox-sentinel"
	for _, tc := range []struct {
		err      error
		category string
		wantErr  error
	}{
		{nil, "normal_close", nil},
		{io.EOF, "normal_close", nil},
		{mcp.ErrConnectionClosed, "normal_close", nil},
		{context.Canceled, "canceled", nil},
		{syscall.EPIPE, "client_disconnected", errServerUnavailable},
		{errors.New("EOF context canceled broken pipe"), "transport_failure", errServerUnavailable},
	} {
		for _, enabled := range []bool{true, false} {
			var diagnostics bytes.Buffer
			var auditor *Auditor
			if enabled {
				auditor = NewAuditor(&diagnostics)
			}
			var transport mcp.Transport = failingTransport{err: fmt.Errorf("%s: %w", sentinel, tc.err)}
			if tc.err == nil {
				transport = &mcp.IOTransport{Reader: io.NopCloser(strings.NewReader("")), Writer: diagnosticTestWriter{Writer: io.Discard}}
			}

			err := Serve(context.Background(), New(Options{Audit: auditor}), transport)
			if err != tc.wantErr || strings.Contains(diagnostics.String(), "sentinel") {
				t.Fatalf("%v: Serve error = %v, diagnostics %q", tc.err, err, diagnostics.String())
			}
			if enabled {
				requireTransportEnd(t, &diagnostics, tc.category)
			} else if diagnostics.Len() != 0 {
				t.Fatalf("disabled diagnostics = %q", diagnostics.String())
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientRead, serverWrite := io.Pipe()
	serverRead, clientWrite := io.Pipe()
	defer clientRead.Close()
	defer serverRead.Close()

	var protocol, diagnostics bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, New(Options{Audit: NewAuditor(&diagnostics)}), &mcp.IOTransport{
			Reader: serverRead,
			Writer: diagnosticTestWriter{Writer: io.MultiWriter(serverWrite, &protocol), closer: serverWrite},
		})
	}()
	session, err := mcp.NewClient(&mcp.Implementation{Name: "diagnostic-test", Version: "1"}, nil).Connect(ctx, &mcp.IOTransport{Reader: clientRead, Writer: clientWrite}, nil)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if _, err := session.ListTools(ctx, nil); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := waitFor(t, done, "Serve finish"); err != nil {
		t.Fatalf("Serve: %v", err)
	}

	requireTransportEnd(t, &diagnostics, "normal_close")
	for _, line := range strings.Split(strings.TrimSpace(protocol.String()), "\n") {
		var message map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &message); err != nil || string(message["jsonrpc"]) != `"2.0"` || message["result"] == nil || message["event"] != nil {
			t.Fatalf("stdout carries a non-protocol line: %s", line)
		}
	}
}

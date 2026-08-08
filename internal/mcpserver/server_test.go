package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

func TestNewNegotiatesCurrentProtocol(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "croton-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect current-protocol client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result := clientSession.InitializeResult()
	if result == nil {
		t.Fatal("connect returned no discovery result")
	}
	if got := result.ProtocolVersion; got != currentProtocolVersion {
		t.Fatalf("negotiated protocol version = %q, want %q", got, currentProtocolVersion)
	}
	capabilities, err := json.Marshal(result.Capabilities)
	if err != nil {
		t.Fatalf("encode server capabilities: %v", err)
	}
	var advertised map[string]json.RawMessage
	if err := json.Unmarshal(capabilities, &advertised); err != nil {
		t.Fatalf("decode server capabilities: %v", err)
	}
	if _, ok := advertised["logging"]; ok {
		t.Fatal("server advertised the deprecated logging capability")
	}
}

func TestNewSupportsLegacyInitialize(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	clientConnection, err := clientTransport.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect legacy transport: %v", err)
	}
	t.Cleanup(func() { _ = clientConnection.Close() })

	request, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"croton-legacy-test-client","version":"0.0.0"}}}`))
	if err != nil {
		t.Fatalf("decode legacy initialize request: %v", err)
	}
	if err := clientConnection.Write(context.Background(), request); err != nil {
		t.Fatalf("write legacy initialize request: %v", err)
	}

	response, err := clientConnection.Read(context.Background())
	if err != nil {
		t.Fatalf("read legacy initialize response: %v", err)
	}
	wire, err := jsonrpc.EncodeMessage(response)
	if err != nil {
		t.Fatalf("encode legacy initialize response: %v", err)
	}
	var envelope struct {
		Result *mcp.InitializeResult `json:"result"`
		Error  *jsonrpc.Error        `json:"error"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("decode legacy initialize response: %v", err)
	}
	if envelope.Error != nil {
		t.Fatalf("legacy initialize failed: %v", envelope.Error)
	}
	if envelope.Result == nil {
		t.Fatal("legacy initialize returned no result")
	}
	if got := envelope.Result.ProtocolVersion; got != legacyProtocolVersion {
		t.Fatalf("negotiated protocol version = %q, want %q", got, legacyProtocolVersion)
	}
}

func TestServeReturnsNilWhenClientCloses(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverDone := make(chan error, 1)
	go func() { serverDone <- Serve(context.Background(), New(Options{}), serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "croton-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("initialize exchange: %v", err)
	}
	if err := clientSession.Close(); err != nil {
		t.Fatalf("close client session: %v", err)
	}

	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("serve after client close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop after client close")
	}
}

type cancellationProbeMail struct {
	started chan struct{}
	once    sync.Once
}

func (mail *cancellationProbeMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	mail.once.Do(func() { close(mail.started) })
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*cancellationProbeMail) Status(context.Context, string) (bridge.MailboxStatus, error) {
	return bridge.MailboxStatus{}, errors.New("unused")
}

func (*cancellationProbeMail) SearchMailPage(context.Context, bridge.SearchQuery) (bridge.SearchPage, error) {
	return bridge.SearchPage{}, errors.New("unused")
}

func (*cancellationProbeMail) GetMessageMetadata(context.Context, string) (bridge.MessageMetadata, error) {
	return bridge.MessageMetadata{}, errors.New("unused")
}

func (*cancellationProbeMail) GetMessageBody(context.Context, string) ([]byte, error) {
	return nil, errors.New("unused")
}

func TestServeCancellationUnwindsActiveHandler(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	mail := &cancellationProbeMail{started: make(chan struct{})}
	serveDone := make(chan error, 1)
	go func() { serveDone <- Serve(ctx, New(Options{Mail: mail}), serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "cancel-probe", Version: "1"}, nil)
	session, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	callDone := make(chan struct{})
	go func() {
		_, _ = session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(callDone)
	}()

	select {
	case <-mail.started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("Serve cancellation: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Serve did not unwind active handler")
	}
	select {
	case <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("client call did not unwind")
	}
}

func TestDirectConnectCancellationUnwindsActiveHandler(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	mail := &cancellationProbeMail{started: make(chan struct{})}
	serverSession, err := New(Options{Mail: mail}).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	waitDone := make(chan error, 1)
	go func() { waitDone <- serverSession.Wait() }()

	client := mcp.NewClient(&mcp.Implementation{Name: "direct-cancel-probe", Version: "1"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	callDone := make(chan struct{})
	go func() {
		_, _ = clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(callDone)
	}()

	select {
	case <-mail.started:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	cancel()
	select {
	case <-waitDone:
	case <-time.After(2 * time.Second):
		t.Fatal("direct Connect session did not unwind")
	}
	select {
	case <-callDone:
	case <-time.After(2 * time.Second):
		t.Fatal("direct Connect client call did not unwind")
	}
}

type concurrentCancellationMail struct {
	started chan context.Context
}

func (mail *concurrentCancellationMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	mail.started <- ctx
	<-ctx.Done()
	return nil, ctx.Err()
}

func (*concurrentCancellationMail) Status(context.Context, string) (bridge.MailboxStatus, error) {
	return bridge.MailboxStatus{}, errors.New("unused")
}

func (*concurrentCancellationMail) SearchMailPage(context.Context, bridge.SearchQuery) (bridge.SearchPage, error) {
	return bridge.SearchPage{}, errors.New("unused")
}

func (*concurrentCancellationMail) GetMessageMetadata(context.Context, string) (bridge.MessageMetadata, error) {
	return bridge.MessageMetadata{}, errors.New("unused")
}

func (*concurrentCancellationMail) GetMessageBody(context.Context, string) ([]byte, error) {
	return nil, errors.New("unused")
}

func TestConcurrentSessionCancellationIsIndependent(t *testing.T) {
	t.Parallel()

	mail := &concurrentCancellationMail{started: make(chan context.Context, 2)}
	server := New(Options{Mail: mail})
	type connectedSession struct {
		cancel     context.CancelFunc
		serverDone chan error
		client     *mcp.ClientSession
	}
	connect := func(name string) connectedSession {
		ctx, cancel := context.WithCancel(context.Background())
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Fatalf("connect server %s: %v", name, err)
		}
		serverDone := make(chan error, 1)
		go func() { serverDone <- serverSession.Wait() }()
		client := mcp.NewClient(&mcp.Implementation{Name: name, Version: "1"}, nil)
		clientSession, err := client.Connect(context.Background(), clientTransport, nil)
		if err != nil {
			t.Fatalf("connect client %s: %v", name, err)
		}
		return connectedSession{cancel: cancel, serverDone: serverDone, client: clientSession}
	}

	first := connect("concurrent-one")
	second := connect("concurrent-two")
	firstCallDone := make(chan struct{})
	go func() {
		_, _ = first.client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(firstCallDone)
	}()
	firstHandler := <-mail.started
	secondCallDone := make(chan struct{})
	go func() {
		_, _ = second.client.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
		close(secondCallDone)
	}()
	secondHandler := <-mail.started

	first.cancel()
	select {
	case <-firstHandler.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("first handler was not canceled")
	}
	select {
	case <-secondHandler.Done():
		t.Fatal("canceling first session canceled the second handler")
	case <-time.After(50 * time.Millisecond):
	}
	select {
	case <-firstCallDone:
	case <-time.After(2 * time.Second):
		t.Fatal("first client call did not unwind")
	}

	second.cancel()
	select {
	case <-secondHandler.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("second handler was not canceled")
	}
	select {
	case <-secondCallDone:
	case <-time.After(2 * time.Second):
		t.Fatal("second client call did not unwind")
	}
	for index, done := range []chan error{first.serverDone, second.serverDone} {
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("server session %d did not unwind", index+1)
		}
	}
}

type contextCheckingMail struct{}

func (*contextCheckingMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-time.After(25 * time.Millisecond):
		return []bridge.Folder{{Name: "INBOX"}}, nil
	}
}

func (*contextCheckingMail) Status(context.Context, string) (bridge.MailboxStatus, error) {
	return bridge.MailboxStatus{}, errors.New("unused")
}

func (*contextCheckingMail) SearchMailPage(context.Context, bridge.SearchQuery) (bridge.SearchPage, error) {
	return bridge.SearchPage{}, errors.New("unused")
}

func (*contextCheckingMail) GetMessageMetadata(context.Context, string) (bridge.MessageMetadata, error) {
	return bridge.MessageMetadata{}, errors.New("unused")
}

func (*contextCheckingMail) GetMessageBody(context.Context, string) ([]byte, error) {
	return nil, errors.New("unused")
}

func TestServerCanBeReusedAfterNormalServeCompletion(t *testing.T) {
	t.Parallel()

	server := New(Options{Mail: &contextCheckingMail{}})
	runSession := func(callTool bool) {
		clientTransport, serverTransport := mcp.NewInMemoryTransports()
		serverDone := make(chan error, 1)
		go func() { serverDone <- Serve(context.Background(), server, serverTransport) }()

		client := mcp.NewClient(&mcp.Implementation{Name: "reuse-probe", Version: "1"}, nil)
		session, err := client.Connect(context.Background(), clientTransport, nil)
		if err != nil {
			t.Fatalf("connect client: %v", err)
		}
		if callTool {
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
			if err != nil {
				t.Fatalf("call on reused server: %v", err)
			}
			if result.IsError {
				t.Fatal("reused server inherited a canceled handler lifetime")
			}
		}
		if err := session.Close(); err != nil {
			t.Fatalf("close client: %v", err)
		}
		select {
		case err := <-serverDone:
			if err != nil {
				t.Fatalf("Serve: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("Serve did not stop")
		}
	}

	runSession(false)
	runSession(true)
}

type failingTransport struct {
	err error
}

func (transport failingTransport) Connect(context.Context) (mcp.Connection, error) {
	return nil, transport.err
}

func TestServeSanitizesTransportFailures(t *testing.T) {
	t.Parallel()

	const secret = "secret-user@mail.test hunter2 127.0.0.1"
	err := Serve(context.Background(), New(Options{}), failingTransport{err: errors.New(secret)})
	if err == nil {
		t.Fatal("Serve unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("Serve leaked transport error: %v", err)
	}
}

func TestImmutableToolCatalogDoesNotAdvertiseListChanges(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "capabilities-test", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	capabilities, err := json.Marshal(clientSession.InitializeResult().Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	if bytes.Contains(capabilities, []byte(`"listChanged":true`)) {
		t.Fatalf("immutable catalog advertises changes: %s", capabilities)
	}
}

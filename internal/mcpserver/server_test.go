package mcpserver

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewNegotiatesCurrentProtocol(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New().Connect(context.Background(), serverTransport, nil)
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
	serverSession, err := New().Connect(context.Background(), serverTransport, nil)
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
	go func() { serverDone <- Serve(context.Background(), serverTransport) }()

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

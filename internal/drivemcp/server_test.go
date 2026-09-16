// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package drivemcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestNewNegotiatesCurrentProtocolWithTheReadOnlyDriveCatalog(t *testing.T) {
	t.Parallel()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(Options{}).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "croton-drive-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect current-protocol client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	result := clientSession.InitializeResult()
	if result == nil || result.ProtocolVersion != currentProtocolVersion {
		t.Fatalf("initialize result = %+v, want protocol %q", result, currentProtocolVersion)
	}
	if result.ServerInfo.Name != "croton-drive-mcp" || result.ServerInfo.Title != "Croton Drive MCP (Unofficial)" {
		t.Fatalf("server identity = %+v", result.ServerInfo)
	}
	if result.ServerInfo.Description != unofficialDescription {
		t.Fatalf("server description = %q", result.ServerInfo.Description)
	}

	listed, err := clientSession.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != 3 {
		t.Fatalf("Drive tools = %d, want 3", len(listed.Tools))
	}
	names := []string{listed.Tools[0].Name, listed.Tools[1].Name, listed.Tools[2].Name}
	if names[0] != "get_drive_metadata" || names[1] != "get_drive_sharing_status" || names[2] != "list_drive_entries" {
		t.Fatalf("Drive tool names = %v", names)
	}

	// The confined output prerequisite must never register a download tool.
	rejected, err := clientSession.CallTool(context.Background(), &mcp.CallToolParams{Name: "download_drive_file", Arguments: map[string]any{}})
	if err == nil && (rejected == nil || !rejected.IsError) {
		t.Fatal("unregistered download tool was accepted")
	}

	for _, tool := range listed.Tools {
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Fatalf("tool %q is not marked read-only", tool.Name)
		}
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

	connection, err := clientTransport.Connect(context.Background())
	if err != nil {
		t.Fatalf("connect legacy transport: %v", err)
	}
	t.Cleanup(func() { _ = connection.Close() })

	request, err := jsonrpc.DecodeMessage([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"legacy-test-client","version":"0.0.0"}}}`))
	if err != nil {
		t.Fatalf("decode legacy initialize request: %v", err)
	}
	if err := connection.Write(context.Background(), request); err != nil {
		t.Fatalf("write legacy initialize request: %v", err)
	}
	response, err := connection.Read(context.Background())
	if err != nil {
		t.Fatalf("read legacy initialize response: %v", err)
	}
	wire, err := jsonrpc.EncodeMessage(response)
	if err != nil {
		t.Fatalf("encode legacy response: %v", err)
	}
	var envelope struct {
		Result *mcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(wire, &envelope); err != nil {
		t.Fatalf("decode legacy response: %v", err)
	}
	if envelope.Result == nil || envelope.Result.ProtocolVersion != legacyProtocolVersion {
		t.Fatalf("legacy initialize result = %+v", envelope.Result)
	}
}

// connectDriveStdioFrames uses the executable's bounded newline framing while
// allowing tests to register handlers on their own server instance.
func connectDriveStdioFrames(t *testing.T, server *Server) (func(string) json.RawMessage, func()) {
	t.Helper()

	client, peer := net.Pipe()
	t.Cleanup(func() { _ = client.Close() })
	t.Cleanup(func() { _ = peer.Close() })
	if err := client.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatalf("set stdio deadline: %v", err)
	}

	session, err := server.Connect(context.Background(), NewStdioTransport(peer, peer), nil)
	if err != nil {
		t.Fatalf("connect stdio server: %v", err)
	}
	closeSession := func() { _ = session.Close() }
	t.Cleanup(closeSession)

	reader := bufio.NewReader(client)
	exchange := func(request string) json.RawMessage {
		t.Helper()

		if _, err := io.WriteString(client, request+"\n"); err != nil {
			t.Fatalf("write stdio request: %v", err)
		}
		frame, err := reader.ReadBytes('\n')
		if err != nil {
			t.Fatalf("read stdio response: %v", err)
		}
		if !json.Valid(frame) {
			t.Fatalf("stdout contains a non-JSON frame: %q", frame)
		}

		return frame
	}

	frame := exchange(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"drive.test","version":"0.0.0"}}}`)
	var initialized struct {
		Result *mcp.InitializeResult `json:"result"`
	}
	if err := json.Unmarshal(frame, &initialized); err != nil || initialized.Result == nil || initialized.Result.ProtocolVersion != legacyProtocolVersion {
		t.Fatalf("initialize response = %s, error = %v", frame, err)
	}
	if _, err := io.WriteString(client, `{"jsonrpc":"2.0","method":"notifications/initialized"}`+"\n"); err != nil {
		t.Fatalf("write initialized notification: %v", err)
	}

	return exchange, closeSession
}

func TestAllowlistMiddlewareAnswersMethodNotFound(t *testing.T) {
	t.Parallel()

	cli, binary := newToolTestClient(t, "", nil)
	server := New(Options{CLI: cli})
	// Enable the SDK resource method so the rejection witnesses Drive's
	// middleware rather than the SDK's absent-capability fallback.
	server.sdk.AddResource(&mcp.Resource{
		URI:  "https://drive.test/resource",
		Name: "synthetic resource",
	}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	exchange, closeSession := connectDriveStdioFrames(t, server)

	// resources/list is an SDK method, but is outside Drive's allowlist.
	frame := exchange(`{"jsonrpc":"2.0","id":2,"method":"resources/list","params":{}}`)
	closeSession()

	var response struct {
		JSONRPC string         `json:"jsonrpc"`
		ID      int            `json:"id"`
		Error   *jsonrpc.Error `json:"error"`
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatalf("decode rejection: %v", err)
	}
	if response.JSONRPC != "2.0" || response.ID != 2 || response.Error == nil || response.Error.Code != jsonrpc.CodeMethodNotFound {
		t.Fatalf("allowlist response = %s, want method not found from middleware", frame)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "argv")); !os.IsNotExist(err) {
		t.Fatalf("unlisted method reached the CLI: stat argv err = %v", err)
	}
}

func TestRunToolRecoversAPanicAsInternal(t *testing.T) {
	t.Parallel()

	cli, _ := newToolTestClient(t, "", nil)
	var stderr bytes.Buffer
	server := New(Options{CLI: cli, Audit: NewAuditor(&stderr)})
	definition := toolDefinition{
		name: "panic_test",
		run: func(context.Context, *Server, json.RawMessage) (any, string) {
			panic("synthetic panic at drive.test")
		},
	}
	server.sdk.AddTool(&mcp.Tool{
		Name:        definition.name,
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, makeHandler(server, definition))
	exchange, closeSession := connectDriveStdioFrames(t, server)

	frame := exchange(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"panic_test","arguments":{}}}`)
	closeSession()

	var response struct {
		JSONRPC string              `json:"jsonrpc"`
		ID      int                 `json:"id"`
		Error   *jsonrpc.Error      `json:"error"`
		Result  *mcp.CallToolResult `json:"result"`
	}
	if err := json.Unmarshal(frame, &response); err != nil {
		t.Fatalf("decode panic response: %v", err)
	}
	if response.JSONRPC != "2.0" || response.ID != 2 || response.Error != nil || response.Result == nil {
		t.Fatalf("panic response = %s, want tool result", frame)
	}
	requireDriveToolError(t, response.Result, "internal")

	// Pin the entire wire payload: neither the panic value nor any stack frame
	// may appear in content, metadata, or another response field.
	var compact bytes.Buffer
	if err := json.Compact(&compact, frame); err != nil {
		t.Fatalf("compact panic response: %v", err)
	}
	want := `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"{\"error\":{\"code\":\"internal\"}}"}],"isError":true}}`
	if compact.String() != want {
		t.Fatalf("panic response = %s, want only the stable internal error", frame)
	}
	if got := stderr.String(); got != "{\"event\":\"tool_call\",\"tool\":\"unknown_tool\",\"outcome\":\"error\",\"code\":\"internal\"}\n" {
		t.Fatalf("stderr = %q, want sanitized internal diagnostic", got)
	}
}

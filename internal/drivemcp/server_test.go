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
	"context"
	"encoding/json"
	"testing"

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
	if len(listed.Tools) != 2 {
		t.Fatalf("Drive tools = %d, want 2", len(listed.Tools))
	}
	names := []string{listed.Tools[0].Name, listed.Tools[1].Name}
	if names[0] != "get_drive_metadata" || names[1] != "list_drive_entries" {
		t.Fatalf("Drive tool names = %v", names)
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

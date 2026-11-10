package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

// fakeMail is a deterministic Mail implementation for protocol-level tests;
// err fails every call.
type fakeMail struct {
	folders     []bridge.Folder
	searched    []bridge.MessageMetadata
	partial     bool
	metadata    bridge.MessageMetadata
	body        []byte
	headers     map[string]bridge.MessageHeaders
	headerErr   map[string]error
	headerReads int
	structure   bridge.MessageAttachmentStructure
	err         error
}

func (mail *fakeMail) ListFolders(context.Context) ([]bridge.Folder, error) {
	return mail.folders, mail.err
}

func (mail *fakeMail) Status(context.Context, string) (bridge.MailboxStatus, error) {
	return bridge.MailboxStatus{}, mail.err
}

func (mail *fakeMail) SearchMailPage(context.Context, bridge.SearchQuery) (bridge.SearchPage, error) {
	return bridge.SearchPage{Messages: mail.searched, Truncated: mail.partial}, mail.err
}

func (mail *fakeMail) GetMessageMetadata(context.Context, string) (bridge.MessageMetadata, error) {
	return mail.metadata, mail.err
}

func (mail *fakeMail) GetMessageBody(context.Context, string) ([]byte, error) {
	return mail.body, mail.err
}

func (mail *fakeMail) GetMessageHeaders(_ context.Context, identifier string) (bridge.MessageHeaders, error) {
	mail.headerReads++
	if mail.err != nil {
		return bridge.MessageHeaders{}, mail.err
	}
	if err := mail.headerErr[identifier]; err != nil {
		return bridge.MessageHeaders{}, err
	}

	return mail.headers[identifier], nil
}

func (mail *fakeMail) GetMessageAttachmentStructure(context.Context, string) (bridge.MessageAttachmentStructure, error) {
	return mail.structure, mail.err
}

func connectTestClient(t *testing.T, options Options) *mcp.ClientSession {
	t.Helper()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(options).sdk.Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "croton-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) != 1 {
		t.Fatalf("content items = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	return text.Text
}

func decodeResult(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()

	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, result))
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), target); err != nil {
		t.Fatalf("tool result is not valid JSON: %v: %s", err, resultText(t, result))
	}
}

func requireToolError(t *testing.T, result *mcp.CallToolResult, wantCode string) {
	t.Helper()

	text := resultText(t, result)
	if !result.IsError || text != `{"error":{"code":"`+wantCode+`"}}` {
		t.Fatalf("result = %s (isError %v), want tool error %q", text, result.IsError, wantCode)
	}
}

func TestToolCatalogIsExactlySixReadOnlyTools(t *testing.T) {
	t.Parallel()

	session := connectTestClient(t, Options{Mail: &fakeMail{}})

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := map[string]bool{"get_message": true, "get_thread": true, "list_attachments": true, "list_folders": true, "search_mail": true, "select_digest_candidates": true}
	if len(listed.Tools) != len(want) {
		t.Fatalf("tool count = %d, want %d", len(listed.Tools), len(want))
	}
	for _, tool := range listed.Tools {
		if !want[tool.Name] {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint || tool.Annotations.OpenWorldHint == nil || *tool.Annotations.OpenWorldHint {
			t.Errorf("tool %q is not annotated read-only and closed-world", tool.Name)
		}

		schemaJSON, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", tool.Name, err)
		}
		var schema struct {
			Type                 string `json:"type"`
			AdditionalProperties *bool  `json:"additionalProperties"`
			Properties           map[string]struct {
				Type      string `json:"type"`
				MaxLength *int   `json:"maxLength"`
				MaxBytes  *int   `json:"x-maxBytes"`
				Maximum   *int   `json:"maximum"`
				Minimum   *int   `json:"minimum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schemaJSON, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", tool.Name, err)
		}
		if schema.Type != "object" || schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("tool %q schema is not a closed object", tool.Name)
		}
		for name, property := range schema.Properties {
			switch {
			case property.Type == "string" && property.MaxLength != nil && property.MaxBytes != nil && *property.MaxLength > 0:
			case property.Type == "integer" && property.Minimum != nil && property.Maximum != nil:
			case property.Type == "boolean":
			default:
				t.Errorf("%s.%s is not a bounded property: %+v", tool.Name, name, property)
			}
		}
	}

	capabilities, err := json.Marshal(session.InitializeResult().Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	if string(capabilities) != `{"tools":{}}` {
		t.Fatalf("capabilities = %s, want only a fixed tool catalog", capabilities)
	}
	if _, err := session.ListResources(context.Background(), &mcp.ListResourcesParams{}); err == nil {
		t.Error("resources/list unexpectedly succeeded")
	}
	if _, err := session.ListPrompts(context.Background(), &mcp.ListPromptsParams{}); err == nil {
		t.Error("prompts/list unexpectedly succeeded")
	}
}

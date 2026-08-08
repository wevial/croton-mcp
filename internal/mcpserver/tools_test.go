package mcpserver

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

// fakeMail is a deterministic Mail implementation for protocol-level tests.
type fakeMail struct {
	folders         []bridge.Folder
	status          bridge.MailboxStatus
	searched        []bridge.MessageMetadata
	searchTruncated bool
	metadata        bridge.MessageMetadata
	body            []byte
	metadataByID    map[string]bridge.MessageMetadata
	bodyByID        map[string][]byte
	err             error
	searchCalls     []bridge.SearchQuery
	bodyCalls       []string
	metaCalls       []string
}

func (mail *fakeMail) ListFolders(ctx context.Context) ([]bridge.Folder, error) {
	if mail.err != nil {
		return nil, mail.err
	}
	return mail.folders, nil
}

func (mail *fakeMail) Status(ctx context.Context, mailbox string) (bridge.MailboxStatus, error) {
	if mail.err != nil {
		return bridge.MailboxStatus{}, mail.err
	}
	return mail.status, nil
}

func (mail *fakeMail) SearchMail(ctx context.Context, query bridge.SearchQuery) ([]bridge.MessageMetadata, error) {
	mail.searchCalls = append(mail.searchCalls, query)
	if mail.err != nil {
		return nil, mail.err
	}
	return mail.searched, nil
}

func (mail *fakeMail) SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error) {
	mail.searchCalls = append(mail.searchCalls, query)
	if mail.err != nil {
		return bridge.SearchPage{}, mail.err
	}
	return bridge.SearchPage{Messages: mail.searched, Truncated: mail.searchTruncated}, nil
}

func (mail *fakeMail) GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error) {
	mail.metaCalls = append(mail.metaCalls, identifier)
	if mail.err != nil {
		return bridge.MessageMetadata{}, mail.err
	}
	if mail.metadataByID != nil {
		metadata, ok := mail.metadataByID[identifier]
		if !ok {
			return bridge.MessageMetadata{}, &bridge.Error{Code: bridge.CodeStaleMessageID}
		}
		return metadata, nil
	}
	return mail.metadata, nil
}

func (mail *fakeMail) GetMessageBody(ctx context.Context, identifier string) ([]byte, error) {
	mail.bodyCalls = append(mail.bodyCalls, identifier)
	if mail.err != nil {
		return nil, mail.err
	}
	if mail.bodyByID != nil {
		body, ok := mail.bodyByID[identifier]
		if !ok {
			return nil, &bridge.Error{Code: bridge.CodeStaleMessageID}
		}
		return body, nil
	}
	return mail.body, nil
}

func connectTestClient(t *testing.T, options Options) *mcp.ClientSession {
	t.Helper()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(options).Connect(context.Background(), serverTransport, nil)
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

func TestToolCatalogIsExactlySixReadOnlyTools(t *testing.T) {
	t.Parallel()

	session := connectTestClient(t, Options{Mail: &fakeMail{}})

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	want := []string{
		"get_message",
		"get_thread",
		"list_attachments",
		"list_folders",
		"search_mail",
		"select_digest_candidates",
	}
	var got []string
	byName := make(map[string]*mcp.Tool)
	for _, tool := range listed.Tools {
		got = append(got, tool.Name)
		byName[tool.Name] = tool
	}
	if len(got) != len(want) {
		t.Fatalf("tool count = %d (%v), want %d", len(got), got, len(want))
	}
	for _, name := range want {
		tool, ok := byName[name]
		if !ok {
			t.Fatalf("tool %q missing from catalog %v", name, got)
		}
		if tool.Annotations == nil || !tool.Annotations.ReadOnlyHint {
			t.Errorf("tool %q does not carry readOnlyHint=true", name)
		}
		if tool.Annotations != nil && tool.Annotations.OpenWorldHint != nil && *tool.Annotations.OpenWorldHint {
			t.Errorf("tool %q advertises an open world", name)
		}

		schemaJSON, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", name, err)
		}
		var schema struct {
			Type                 string          `json:"type"`
			AdditionalProperties *bool           `json:"additionalProperties"`
			Properties           json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(schemaJSON, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", name, err)
		}
		if schema.Type != "object" {
			t.Errorf("tool %q schema type = %q, want object", name, schema.Type)
		}
		if schema.AdditionalProperties == nil || *schema.AdditionalProperties {
			t.Errorf("tool %q schema does not reject additional properties", name)
		}
	}
}

func TestSchemaStringInputsCarryBoundedMaxLength(t *testing.T) {
	t.Parallel()

	session := connectTestClient(t, Options{Mail: &fakeMail{}})

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}

	for _, tool := range listed.Tools {
		schemaJSON, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %q input schema: %v", tool.Name, err)
		}
		var schema struct {
			Properties map[string]struct {
				Type      string   `json:"type"`
				MaxLength *int     `json:"maxLength"`
				MaxBytes  *int     `json:"x-maxBytes"`
				Maximum   *float64 `json:"maximum"`
				Minimum   *float64 `json:"minimum"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(schemaJSON, &schema); err != nil {
			t.Fatalf("decode %q input schema: %v", tool.Name, err)
		}
		for propertyName, property := range schema.Properties {
			switch property.Type {
			case "string":
				if property.MaxLength == nil || *property.MaxLength <= 0 {
					t.Errorf("%s.%s: string property lacks positive maxLength", tool.Name, propertyName)
				}
				if property.MaxBytes == nil || *property.MaxBytes <= 0 {
					t.Errorf("%s.%s: string property lacks positive x-maxBytes", tool.Name, propertyName)
				}
			case "integer":
				if property.Maximum == nil || property.Minimum == nil {
					t.Errorf("%s.%s: integer property lacks minimum/maximum", tool.Name, propertyName)
				}
			case "boolean":
			default:
				t.Errorf("%s.%s: unexpected schema property type %q", tool.Name, propertyName, property.Type)
			}
		}
	}
}

func TestServerAdvertisesOnlyToolCapability(t *testing.T) {
	t.Parallel()

	session := connectTestClient(t, Options{Mail: &fakeMail{}})

	result := session.InitializeResult()
	if result == nil {
		t.Fatal("no initialize result")
	}
	capabilitiesJSON, err := json.Marshal(result.Capabilities)
	if err != nil {
		t.Fatalf("marshal capabilities: %v", err)
	}
	var advertised map[string]json.RawMessage
	if err := json.Unmarshal(capabilitiesJSON, &advertised); err != nil {
		t.Fatalf("decode capabilities: %v", err)
	}
	if _, ok := advertised["tools"]; !ok {
		t.Errorf("capabilities missing tools: %s", capabilitiesJSON)
	}
	for name := range advertised {
		if name != "tools" && name != "completions" {
			t.Errorf("unexpected capability %q advertised: %s", name, capabilitiesJSON)
		}
	}
	if _, ok := advertised["completions"]; ok {
		t.Errorf("completions capability advertised: %s", capabilitiesJSON)
	}

	if _, err := session.ListResources(context.Background(), &mcp.ListResourcesParams{}); err == nil {
		t.Error("resources/list unexpectedly succeeded")
	}
	if _, err := session.ListPrompts(context.Background(), &mcp.ListPromptsParams{}); err == nil {
		t.Error("prompts/list unexpectedly succeeded")
	}
}

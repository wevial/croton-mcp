package mcpserver

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Input ceilings mirror the bridge layer's authoritative limits. The server
// enforces them itself; the published schemas merely advertise them.
const (
	maxMailboxArgumentBytes   = 512
	maxSearchTermBytes        = 1024
	maxMessageIDArgumentBytes = 1366
	maxTimestampArgumentBytes = 64

	maxSearchLimit        = 250
	defaultSearchLimit    = 50
	maxThreadMessages     = 20
	defaultThreadMessages = 10
	maxDigestLimit        = 50
	defaultDigestLimit    = 20
	maxDigestSinceHours   = 168
	defaultDigestHours    = 24
)

// Stable, secret-free tool error codes.
const (
	errInvalidArgument = "invalid_argument"
	errNotFound        = "not_found"
	errStaleID         = "stale_id"
	errBoundsExceeded  = "bounds_exceeded"
	errTimedOut        = "timed_out"
	errCanceled        = "canceled"
	errUnavailable     = "unavailable"
	errInternal        = "internal"
)

// toolFunc validates raw arguments authoritatively and returns either a
// JSON-marshalable result or a stable error code.
type toolFunc func(ctx context.Context, deps Options, arguments json.RawMessage) (any, string)

type toolDefinition struct {
	name        string
	description string
	schema      json.RawMessage
	run         toolFunc
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			name:        "list_folders",
			description: "List mailbox folders available through the local Proton Mail Bridge.",
			schema:      objectSchema(nil, nil),
			run:         runListFolders,
		},
		{
			name:        "search_mail",
			description: "Search one mailbox with bounded structured criteria and return message metadata.",
			schema: objectSchema(map[string]json.RawMessage{
				"mailbox":    stringSchema(maxMailboxArgumentBytes),
				"since":      stringSchema(maxTimestampArgumentBytes),
				"before":     stringSchema(maxTimestampArgumentBytes),
				"sender":     stringSchema(maxSearchTermBytes),
				"subject":    stringSchema(maxSearchTermBytes),
				"unreadOnly": booleanSchema(),
				"limit":      integerSchema(1, maxSearchLimit),
			}, []string{"mailbox"}),
			run: runSearchMail,
		},
		{
			name:        "get_message",
			description: "Fetch one message by opaque id: bounded headers, normalized text, and attachment metadata.",
			schema: objectSchema(map[string]json.RawMessage{
				"messageId": stringSchema(maxMessageIDArgumentBytes),
			}, []string{"messageId"}),
			run: runGetMessage,
		},
		{
			name:        "get_thread",
			description: "Resolve the local conversation thread around one message using bounded read-only fetches.",
			schema: objectSchema(map[string]json.RawMessage{
				"messageId":   stringSchema(maxMessageIDArgumentBytes),
				"maxMessages": integerSchema(1, maxThreadMessages),
			}, []string{"messageId"}),
			run: runGetThread,
		},
		{
			name:        "list_attachments",
			description: "List attachment metadata for one message without returning attachment bytes.",
			schema: objectSchema(map[string]json.RawMessage{
				"messageId": stringSchema(maxMessageIDArgumentBytes),
			}, []string{"messageId"}),
			run: runListAttachments,
		},
		{
			name:        "select_digest_candidates",
			description: "Select bounded, metadata-first digest candidates from recent mail in one mailbox.",
			schema: objectSchema(map[string]json.RawMessage{
				"mailbox":    stringSchema(maxMailboxArgumentBytes),
				"sinceHours": integerSchema(1, maxDigestSinceHours),
				"limit":      integerSchema(1, maxDigestLimit),
				"unreadOnly": booleanSchema(),
			}, []string{"mailbox"}),
			run: runSelectDigestCandidates,
		},
	}
}

func registerTools(server *mcp.Server, options Options) {
	server.AddReceivingMiddleware(allowlistMiddleware())

	falseHint := false
	for _, definition := range toolDefinitions() {
		server.AddTool(&mcp.Tool{
			Name:        definition.name,
			Description: definition.description,
			InputSchema: definition.schema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:  true,
				OpenWorldHint: &falseHint,
			},
		}, makeHandler(definition, options))
	}
}

// allowlistMiddleware fails closed: only the negotiation, discovery, and tool
// methods Croton intentionally serves are reachable.
func allowlistMiddleware() mcp.Middleware {
	allowed := map[string]bool{
		"initialize":                true,
		"ping":                      true,
		"server/discover":           true,
		"tools/list":                true,
		"tools/call":                true,
		"notifications/initialized": true,
		"notifications/cancelled":   true,
		"notifications/progress":    true,
	}

	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if !allowed[method] {
				return nil, &jsonrpc.Error{Code: jsonrpc.CodeMethodNotFound, Message: "method not supported"}
			}

			return next(ctx, method, req)
		}
	}
}

func makeHandler(definition toolDefinition, options Options) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments json.RawMessage
		if request != nil && request.Params != nil {
			arguments = request.Params.Arguments
		}

		result, errCode := runTool(ctx, definition, options, arguments)
		if errCode != "" {
			options.Audit.ToolCall(definition.name, "error", errCode, false)
			return errorResult(errCode), nil
		}

		encoded, truncated, encodeErr := encodeBounded(result)
		if encodeErr != nil {
			options.Audit.ToolCall(definition.name, "error", errInternal, false)
			return errorResult(errInternal), nil
		}

		options.Audit.ToolCall(definition.name, "ok", "", truncated)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		}, nil
	}
}

// runTool isolates tool execution so that a panicking handler can never leak
// stack or state details onto the protocol stream.
func runTool(ctx context.Context, definition toolDefinition, options Options, arguments json.RawMessage) (result any, errCode string) {
	defer func() {
		if recover() != nil {
			result, errCode = nil, errInternal
		}
	}()

	if options.Mail == nil {
		return nil, errUnavailable
	}

	return definition.run(ctx, options, arguments)
}

func errorResult(code string) *mcp.CallToolResult {
	encoded, err := json.Marshal(map[string]any{"error": map[string]string{"code": code}})
	if err != nil {
		encoded = []byte(`{"error":{"code":"internal"}}`)
	}

	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
	}
}

func objectSchema(properties map[string]json.RawMessage, required []string) json.RawMessage {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
	}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	if len(required) > 0 {
		schema["required"] = required
	}

	encoded, err := json.Marshal(schema)
	if err != nil {
		panic("mcpserver: static schema must marshal: " + err.Error())
	}

	return encoded
}

func stringSchema(maxLength int) json.RawMessage {
	maximum := itoa(maxLength)
	return json.RawMessage([]byte(`{"type":"string","maxLength":` + maximum + `,"x-maxBytes":` + maximum + `}`))
}

func integerSchema(minimum, maximum int) json.RawMessage {
	return json.RawMessage([]byte(`{"type":"integer","minimum":` + itoa(minimum) + `,"maximum":` + itoa(maximum) + `}`))
}

func booleanSchema() json.RawMessage {
	return json.RawMessage(`{"type":"boolean"}`)
}

func itoa(value int) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("mcpserver: static integer must marshal")
	}

	return string(encoded)
}

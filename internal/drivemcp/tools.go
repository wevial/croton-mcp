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
	"errors"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/drivecli"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

// Listing bounds. The server enforces them itself; the published schemas
// merely advertise them.
const (
	maxListEntries     = 200
	defaultListEntries = 100
	maxSharingMembers  = 100

	maxToolArgumentsBytes = 24 * 1024
)

// Stable, secret-free tool error codes.
const (
	errInvalidArgument = "invalid_argument"
	errBoundsExceeded  = "bounds_exceeded"
	errTimedOut        = "timed_out"
	errCanceled        = "canceled"
	errUnavailable     = "unavailable"
	errInternal        = "internal"
)

// toolFunc validates raw arguments authoritatively and returns either a
// JSON-marshalable result or a stable error code.
type toolFunc func(ctx context.Context, server *Server, arguments json.RawMessage) (any, string)

// truncationReporter lets bounded result types propagate count-based
// truncation into metadata-only audit events. Byte-based truncation remains
// reported by encodeBounded.
type truncationReporter interface {
	wasTruncated() bool
}

type toolDefinition struct {
	name        string
	description string
	schema      json.RawMessage
	run         toolFunc
}

func toolDefinitions() []toolDefinition {
	return []toolDefinition{
		{
			name:        "get_drive_sharing_status",
			description: "Fetch bounded frozen sharing status for one absolute Proton Drive path.",
			schema: objectSchema(map[string]json.RawMessage{
				"path": stringSchema(maxDrivePathBytes),
			}, []string{"path"}),
			run: runGetDriveSharingStatus,
		},
		{
			name:        "list_drive_entries",
			description: "List one absolute Proton Drive path: root sections, synced devices, or bounded folder entries.",
			schema: objectSchema(map[string]json.RawMessage{
				"path":  stringSchema(maxDrivePathBytes),
				"type":  enumSchema("file", "folder"),
				"limit": integerSchema(1, maxListEntries),
			}, []string{"path"}),
			run: runListDriveEntries,
		},
		{
			name:        "get_drive_metadata",
			description: "Fetch the frozen file or folder metadata object for one absolute Proton Drive path.",
			schema: objectSchema(map[string]json.RawMessage{
				"path": stringSchema(maxDrivePathBytes),
			}, []string{"path"}),
			run: runGetDriveMetadata,
		},
	}
}

func registerTools(server *Server) {
	server.sdk.AddReceivingMiddleware(allowlistMiddleware())

	falseHint := false
	for _, definition := range toolDefinitions() {
		server.sdk.AddTool(&mcp.Tool{
			Name:        definition.name,
			Description: definition.description,
			InputSchema: definition.schema,
			Annotations: &mcp.ToolAnnotations{
				ReadOnlyHint:  true,
				OpenWorldHint: &falseHint,
			},
		}, makeHandler(server, definition))
	}
}

// allowlistMiddleware fails closed: only the negotiation, discovery, and tool
// methods Croton Drive intentionally serves are reachable.
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

func makeHandler(server *Server, definition toolDefinition) mcp.ToolHandler {
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arguments json.RawMessage
		if request != nil && request.Params != nil {
			arguments = request.Params.Arguments
		}

		result, errCode := runTool(ctx, server, definition, arguments)
		if errCode != "" {
			server.audit.ToolCall(definition.name, "error", errCode, false)
			return errorResult(errCode), nil
		}

		encoded, truncated, encodeErr := encodeBounded(result)
		if encodeErr != nil {
			server.audit.ToolCall(definition.name, "error", errInternal, false)
			return errorResult(errInternal), nil
		}
		if reporter, ok := result.(truncationReporter); ok {
			truncated = truncated || reporter.wasTruncated()
		}

		server.audit.ToolCall(definition.name, "ok", "", truncated)
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: string(encoded)}},
		}, nil
	}
}

// runTool isolates tool execution so that a panicking handler can never leak
// stack or state details onto the protocol stream.
func runTool(ctx context.Context, server *Server, definition toolDefinition, arguments json.RawMessage) (result any, errCode string) {
	defer func() {
		if recover() != nil {
			result, errCode = nil, errInternal
		}
	}()

	if server.cli == nil {
		return nil, errUnavailable
	}

	return definition.run(ctx, server, arguments)
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

// decodeArguments strictly decodes one bounded JSON object into target,
// rejecting unknown or duplicate fields and trailing values regardless of any
// client-side schema behavior.
func decodeArguments(arguments json.RawMessage, target any) bool {
	return strictjson.DecodeObject(arguments, maxToolArgumentsBytes, target)
}

// mapDriveError converts any adapter or context failure to one stable,
// secret-free tool error code. Unknown errors collapse to errInternal so that
// no wrapped detail can ever cross the protocol boundary.
func mapDriveError(err error) string {
	if errors.Is(err, context.Canceled) {
		return errCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return errTimedOut
	}

	switch drivecli.CodeOf(err) {
	case drivecli.CodeTimedOut:
		return errTimedOut
	case drivecli.CodeCanceled:
		return errCanceled
	case drivecli.CodeOutputOverflow:
		return errBoundsExceeded
	case drivecli.CodeInvalidConfig, drivecli.CodeVersionMismatch, drivecli.CodeMalformedOutput,
		drivecli.CodeTruncatedOutput, drivecli.CodeCommandFailed, drivecli.CodeAuthRequired:
		return errUnavailable
	default:
		return errInternal
	}
}

// listDriveResult holds exactly one frozen listing shape for the requested
// path; the populated field stays present (as an empty array if needed) while
// the other shapes are omitted.
type listDriveResult struct {
	Path      string                 `json:"path"`
	Sections  []drivecli.RootSection `json:"sections,omitzero"`
	Devices   []drivecli.Device      `json:"devices,omitzero"`
	Entries   []drivecli.NodeEntity  `json:"entries,omitzero"`
	Truncated bool                   `json:"truncated,omitempty"`
}

func (result *listDriveResult) wasTruncated() bool { return result.Truncated }

func runListDriveEntries(ctx context.Context, server *Server, arguments json.RawMessage) (any, string) {
	var input struct {
		Path  string `json:"path"`
		Type  string `json:"type"`
		Limit *int   `json:"limit"`
	}
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}
	if !validDrivePath(input.Path) {
		return nil, errInvalidArgument
	}
	if input.Type != "" && input.Type != "file" && input.Type != "folder" {
		return nil, errInvalidArgument
	}
	limit, ok := clampLimit(input.Limit, defaultListEntries, maxListEntries)
	if !ok {
		return nil, errInvalidArgument
	}

	if err := server.gate.ensure(ctx, server.cli); err != nil {
		return nil, mapDriveError(err)
	}

	listing, err := server.cli.List(ctx, input.Path, input.Type)
	if err != nil {
		return nil, mapDriveError(err)
	}

	result := listDriveResult{Path: input.Path}
	switch input.Path {
	case "/":
		result.Sections, result.Truncated = boundEntries(listing.Sections, limit)
	case "/devices":
		result.Devices, result.Truncated = boundEntries(listing.Devices, limit)
	default:
		result.Entries, result.Truncated = boundEntries(listing.Nodes, limit)
	}

	return &result, ""
}

func runGetDriveMetadata(ctx context.Context, server *Server, arguments json.RawMessage) (any, string) {
	var input struct {
		Path string `json:"path"`
	}
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}
	if !validDrivePath(input.Path) {
		return nil, errInvalidArgument
	}

	if err := server.gate.ensure(ctx, server.cli); err != nil {
		return nil, mapDriveError(err)
	}

	node, err := server.cli.Info(ctx, input.Path)
	if err != nil {
		return nil, mapDriveError(err)
	}

	return &node, ""
}

// sharingStatusResult is the bounded MCP mapping of the frozen ShareResult.
// Member identifiers and link URLs can occur only in this tool result, never
// in audit records or diagnostics.
type sharingStatusResult struct {
	Shared               bool              `json:"shared"`
	ProtonInvitations    []drivecli.Member `json:"protonInvitations"`
	NonProtonInvitations []drivecli.Member `json:"nonProtonInvitations"`
	Members              []drivecli.Member `json:"members"`
	URLAccess            *sharingURLAccess `json:"urlAccess,omitempty"`
	EditorsCanShare      bool              `json:"editorsCanShare"`
	Truncated            bool              `json:"truncated,omitempty"`
}

func (result *sharingStatusResult) wasTruncated() bool { return result.Truncated }

// sharingURLAccess is the safe MCP subset of the frozen CLI's public-link
// object. The adapter type includes CustomPassword for decoding, but a link
// password is a credential and must remain structurally unreachable to MCP.
type sharingURLAccess struct {
	UID                          string `json:"uid"`
	CreationTime                 string `json:"creationTime"`
	Role                         string `json:"role"`
	URL                          string `json:"url"`
	ExpirationTime               string `json:"expirationTime,omitempty"`
	NumberOfInitializedDownloads int64  `json:"numberOfInitializedDownloads"`
}

func mapSharingURLAccess(access *drivecli.URLAccess) *sharingURLAccess {
	if access == nil {
		return nil
	}

	return &sharingURLAccess{
		UID:                          access.UID,
		CreationTime:                 access.CreationTime,
		Role:                         access.Role,
		URL:                          access.URL,
		ExpirationTime:               access.ExpirationTime,
		NumberOfInitializedDownloads: access.NumberOfInitializedDownloads,
	}
}

func runGetDriveSharingStatus(ctx context.Context, server *Server, arguments json.RawMessage) (any, string) {
	var input struct {
		Path string `json:"path"`
	}
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}
	if !validDrivePath(input.Path) {
		return nil, errInvalidArgument
	}

	if err := server.gate.ensure(ctx, server.cli); err != nil {
		return nil, mapDriveError(err)
	}

	status, err := server.cli.SharingStatus(ctx, input.Path)
	if err != nil {
		return nil, mapDriveError(err)
	}

	result := &sharingStatusResult{Shared: status.Shared}
	if status.Info == nil {
		return result, ""
	}

	var truncated bool
	result.ProtonInvitations, truncated = boundEntries(status.Info.ProtonInvitations, maxSharingMembers)
	result.Truncated = result.Truncated || truncated
	result.NonProtonInvitations, truncated = boundEntries(status.Info.NonProtonInvitations, maxSharingMembers)
	result.Truncated = result.Truncated || truncated
	result.Members, truncated = boundEntries(status.Info.Members, maxSharingMembers)
	result.Truncated = result.Truncated || truncated
	result.URLAccess = mapSharingURLAccess(status.Info.URLAccess)
	result.EditorsCanShare = status.Info.EditorsCanShare

	return result, ""
}

// boundEntries keeps the listed shape present even when empty and reports
// explicitly whether entries were dropped to honor the count bound.
func boundEntries[Entry any](entries []Entry, limit int) ([]Entry, bool) {
	if entries == nil {
		entries = []Entry{}
	}
	if len(entries) > limit {
		return entries[:limit], true
	}

	return entries, false
}

func clampLimit(value *int, fallback, ceiling int) (int, bool) {
	if value == nil {
		return fallback, true
	}
	if *value <= 0 {
		return 0, false
	}
	if *value > ceiling {
		return ceiling, true
	}

	return *value, true
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
		panic("drivemcp: static schema must marshal: " + err.Error())
	}

	return encoded
}

func stringSchema(maxLength int) json.RawMessage {
	maximum := itoa(maxLength)
	return json.RawMessage([]byte(`{"type":"string","maxLength":` + maximum + `,"x-maxBytes":` + maximum + `}`))
}

func enumSchema(values ...string) json.RawMessage {
	encoded, err := json.Marshal(map[string]any{"type": "string", "enum": values})
	if err != nil {
		panic("drivemcp: static enum schema must marshal")
	}

	return encoded
}

func integerSchema(minimum, maximum int) json.RawMessage {
	return json.RawMessage([]byte(`{"type":"integer","minimum":` + itoa(minimum) + `,"maximum":` + itoa(maximum) + `}`))
}

func itoa(value int) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		panic("drivemcp: static integer must marshal")
	}

	return string(encoded)
}

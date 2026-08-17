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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/drivecli"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func newToolTestClient(t *testing.T, scenario string, stdout []byte) (*drivecli.Client, string) {
	t.Helper()

	binary := testkit.FakeDrive(t, scenario, stdout)
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	return client, binary
}

func connectDriveTestClient(t *testing.T, options Options) *mcp.ClientSession {
	t.Helper()

	clientTransport, serverTransport := mcp.NewInMemoryTransports()
	serverSession, err := New(options).Connect(context.Background(), serverTransport, nil)
	if err != nil {
		t.Fatalf("connect server: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })

	client := mcp.NewClient(&mcp.Implementation{Name: "croton-drive-test-client", Version: "0.0.0"}, nil)
	clientSession, err := client.Connect(context.Background(), clientTransport, nil)
	if err != nil {
		t.Fatalf("connect client: %v", err)
	}
	t.Cleanup(func() { _ = clientSession.Close() })

	return clientSession
}

func callDriveTool(t *testing.T, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}

	return result
}

func driveResultText(t *testing.T, result *mcp.CallToolResult) string {
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

func decodeDriveResult(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()

	if result.IsError {
		t.Fatalf("unexpected tool error: %s", driveResultText(t, result))
	}
	if err := json.Unmarshal([]byte(driveResultText(t, result)), target); err != nil {
		t.Fatalf("tool result is not valid JSON: %v: %s", err, driveResultText(t, result))
	}
}

func requireDriveToolError(t *testing.T, result *mcp.CallToolResult, wantCode string) {
	t.Helper()

	if !result.IsError {
		t.Fatalf("expected tool error %q, got success: %s", wantCode, driveResultText(t, result))
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(driveResultText(t, result)), &envelope); err != nil {
		t.Fatalf("error result is not valid JSON: %v: %s", err, driveResultText(t, result))
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q", envelope.Error.Code, wantCode)
	}
}

func minimalNodeJSON(index int) string {
	return fmt.Sprintf(`{"uid":"node:%d","name":{"ok":true,"value":"entry-%d.txt"},"keyAuthor":{"ok":true,"value":null},"nameAuthor":{"ok":true,"value":null},"directRole":"admin","ownedBy":{},"type":"file","isShared":false,"isSharedByUrl":false,"creationTime":"2026-01-01T00:00:00.000Z","modificationTime":"2026-01-01T00:00:00.000Z","treeEventScopeId":"scope:1"}`, index, index)
}

func nodeListJSON(count int) []byte {
	nodes := make([]string, 0, count)
	for index := range count {
		nodes = append(nodes, minimalNodeJSON(index))
	}

	return []byte("[" + strings.Join(nodes, ",") + "]")
}

func TestListDriveEntriesReturnsFrozenNodeShapesAfterNegotiation(t *testing.T) {
	t.Parallel()

	client, binary := newToolTestClient(t, "", testkit.DriveFixture(t, "list-my-files.json"))
	session := connectDriveTestClient(t, Options{CLI: client})

	var decoded listDriveResult
	decodeDriveResult(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files"}), &decoded)
	if decoded.Path != "/my-files" || len(decoded.Entries) != 1 || decoded.Truncated {
		t.Fatalf("list result = %+v", decoded)
	}
	if decoded.Entries[0].UID != "node:file-1" || decoded.Entries[0].Type != "file" || decoded.Entries[0].Name.Value != "notes.txt" {
		t.Fatalf("frozen node entry = %+v", decoded.Entries[0])
	}
	if got := testkit.RecordedArgv(t, binary); got != "filesystem\nlist\n/my-files\n--json\n" {
		t.Fatalf("recorded argv = %q", got)
	}
}

func TestListDriveEntriesSupportsRootSectionsDevicesAndTypeFilter(t *testing.T) {
	t.Parallel()

	rootClient, _ := newToolTestClient(t, "", testkit.DriveFixture(t, "list-root.json"))
	rootSession := connectDriveTestClient(t, Options{CLI: rootClient})

	rootResult := callDriveTool(t, rootSession, "list_drive_entries", map[string]any{"path": "/"})
	var root listDriveResult
	decodeDriveResult(t, rootResult, &root)
	if root.Path != "/" || len(root.Sections) != 5 || root.Sections[0].Path != "/my-files" || len(root.Entries) != 0 {
		t.Fatalf("root result = %+v", root)
	}
	if raw := driveResultText(t, rootResult); !strings.Contains(raw, `"sections"`) || strings.Contains(raw, `"entries"`) || strings.Contains(raw, `"devices"`) {
		t.Fatalf("root result must serialize exactly one listing shape: %s", raw)
	}

	devicesClient, _ := newToolTestClient(t, "", testkit.DriveFixture(t, "list-devices.json"))
	devicesSession := connectDriveTestClient(t, Options{CLI: devicesClient})

	var devices listDriveResult
	decodeDriveResult(t, callDriveTool(t, devicesSession, "list_drive_entries", map[string]any{"path": "/devices"}), &devices)
	if len(devices.Devices) != 1 || devices.Devices[0].Type != "Linux" {
		t.Fatalf("devices result = %+v", devices)
	}

	typedClient, typedBinary := newToolTestClient(t, "", testkit.DriveFixture(t, "list-my-files.json"))
	typedSession := connectDriveTestClient(t, Options{CLI: typedClient})

	var typed listDriveResult
	decodeDriveResult(t, callDriveTool(t, typedSession, "list_drive_entries", map[string]any{"path": "/my-files", "type": "file"}), &typed)
	if len(typed.Entries) != 1 {
		t.Fatalf("typed result = %+v", typed)
	}
	if got := testkit.RecordedArgv(t, typedBinary); got != "filesystem\nlist\n/my-files\n--type\nfile\n--json\n" {
		t.Fatalf("typed argv = %q", got)
	}
}

func TestListDriveEntriesEnforcesEntryLimitAndSignalsTruncation(t *testing.T) {
	t.Parallel()

	client, _ := newToolTestClient(t, "", nodeListJSON(3))
	session := connectDriveTestClient(t, Options{CLI: client})

	var capped listDriveResult
	decodeDriveResult(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files", "limit": 2}), &capped)
	if len(capped.Entries) != 2 || !capped.Truncated {
		t.Fatalf("capped result = %+v", capped)
	}

	var whole listDriveResult
	decodeDriveResult(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files", "limit": 3}), &whole)
	if len(whole.Entries) != 3 || whole.Truncated {
		t.Fatalf("whole result = %+v", whole)
	}
}

func TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI(t *testing.T) {
	t.Parallel()

	client, binary := newToolTestClient(t, "", nil)
	session := connectDriveTestClient(t, Options{CLI: client})

	invalidArguments := []map[string]any{
		{"path": "my-files"},
		{"path": "/my-files/../shared-by-me"},
		{"path": "/my-files//x"},
		{"path": "/my-files/"},
		{"path": "/my-files/a\nb"},
		{"path": ""},
		{"path": "/my-files", "type": "device"},
		{"path": "/my-files", "limit": 0},
		{"path": "/my-files", "limit": -1},
		{"path": "/my-files", "surprise": true},
		{},
	}
	for _, arguments := range invalidArguments {
		requireDriveToolError(t, callDriveTool(t, session, "list_drive_entries", arguments), "invalid_argument")
	}
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_metadata", map[string]any{"path": "../etc"}), "invalid_argument")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_metadata", map[string]any{}), "invalid_argument")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_sharing_status", map[string]any{"path": "../etc"}), "invalid_argument")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_sharing_status", map[string]any{}), "invalid_argument")

	if _, err := os.Stat(filepath.Join(filepath.Dir(binary), "argv")); !os.IsNotExist(err) {
		t.Fatalf("invalid arguments reached the CLI: stat argv err = %v", err)
	}
}

func TestDriveToolsFailClosedWhenNegotiationFails(t *testing.T) {
	t.Parallel()

	client, binary := newToolTestClient(t, "version-mismatch", testkit.DriveFixture(t, "list-my-files.json"))
	session := connectDriveTestClient(t, Options{CLI: client})

	requireDriveToolError(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files"}), "unavailable")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_metadata", map[string]any{"path": "/my-files"}), "unavailable")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_sharing_status", map[string]any{"path": "/my-files"}), "unavailable")

	// The fake records every invocation: after three refused tool calls the last
	// (and only) command must still be the version handshake, proving no data
	// command raced past failed negotiation.
	if got := testkit.RecordedArgv(t, binary); got != "version\n" {
		t.Fatalf("recorded argv after failed negotiation = %q", got)
	}
}

func TestDriveToolsFailClosedWithoutConfiguredCLI(t *testing.T) {
	t.Parallel()

	session := connectDriveTestClient(t, Options{})

	requireDriveToolError(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files"}), "unavailable")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_metadata", map[string]any{"path": "/my-files"}), "unavailable")
	requireDriveToolError(t, callDriveTool(t, session, "get_drive_sharing_status", map[string]any{"path": "/my-files"}), "unavailable")
}

func TestConcurrentDriveCallsShareOneNegotiationAndAllSucceed(t *testing.T) {
	t.Parallel()

	client, _ := newToolTestClient(t, "", testkit.DriveFixture(t, "list-my-files.json"))
	session := connectDriveTestClient(t, Options{CLI: client})

	var group sync.WaitGroup
	results := make([]*mcp.CallToolResult, 8)
	for index := range results {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := session.CallTool(context.Background(), &mcp.CallToolParams{
				Name:      "list_drive_entries",
				Arguments: map[string]any{"path": "/my-files"},
			})
			if err != nil {
				t.Errorf("concurrent call %d: %v", index, err)
				return
			}
			results[index] = result
		}()
	}
	group.Wait()

	for index, result := range results {
		if result == nil {
			continue
		}
		var decoded listDriveResult
		decodeDriveResult(t, result, &decoded)
		if len(decoded.Entries) != 1 {
			t.Fatalf("concurrent result %d = %+v", index, decoded)
		}
	}
}

func TestGetDriveMetadataReturnsTheFrozenNodeObject(t *testing.T) {
	t.Parallel()

	client, binary := newToolTestClient(t, "", testkit.DriveFixture(t, "info-folder.json"))
	session := connectDriveTestClient(t, Options{CLI: client})

	var node drivecli.NodeEntity
	decodeDriveResult(t, callDriveTool(t, session, "get_drive_metadata", map[string]any{"path": "/my-files/Reports"}), &node)
	if node.Type != "folder" || node.Name.Value != "Reports" || node.UID != "node:folder-1" {
		t.Fatalf("metadata node = %+v", node)
	}
	if node.NameAuthor.OK || node.NameAuthor.Error.Error != "signature mismatch" {
		t.Fatalf("frozen author result = %+v", node.NameAuthor)
	}
	if got := testkit.RecordedArgv(t, binary); got != "filesystem\ninfo\n/my-files/Reports\n--json\n" {
		t.Fatalf("metadata argv = %q", got)
	}
}

func TestGetDriveSharingStatusReportsSharedUnsharedAndCommandErrors(t *testing.T) {
	t.Parallel()

	sharedClient, sharedBinary := newToolTestClient(t, "", testkit.DriveFixture(t, "sharing-status.json"))
	sharedSession := connectDriveTestClient(t, Options{CLI: sharedClient})

	var shared struct {
		Shared            bool                `json:"shared"`
		ProtonInvitations []drivecli.Member   `json:"protonInvitations"`
		URLAccess         *drivecli.URLAccess `json:"urlAccess"`
		EditorsCanShare   bool                `json:"editorsCanShare"`
	}
	decodeDriveResult(t, callDriveTool(t, sharedSession, "get_drive_sharing_status", map[string]any{"path": "/my-files/Reports"}), &shared)
	if !shared.Shared || len(shared.ProtonInvitations) != 1 || shared.ProtonInvitations[0].InviteeEmail != "reader@example.test" || shared.URLAccess == nil || shared.URLAccess.URL != "https://drive.proton.test/urls/fixture" || shared.EditorsCanShare {
		t.Fatalf("shared status = %+v", shared)
	}
	if got := testkit.RecordedArgv(t, sharedBinary); got != "sharing\nstatus\n/my-files/Reports\n--json\n" {
		t.Fatalf("shared status argv = %q", got)
	}

	unsharedClient, _ := newToolTestClient(t, "unshared", nil)
	unsharedSession := connectDriveTestClient(t, Options{CLI: unsharedClient})
	var unshared struct {
		Shared bool `json:"shared"`
	}
	decodeDriveResult(t, callDriveTool(t, unsharedSession, "get_drive_sharing_status", map[string]any{"path": "/my-files/notes.txt"}), &unshared)
	if unshared.Shared {
		t.Fatalf("unshared status = %+v", unshared)
	}

	failingClient, _ := newToolTestClient(t, "nonzero-secret", nil)
	failingSession := connectDriveTestClient(t, Options{CLI: failingClient})
	requireDriveToolError(t, callDriveTool(t, failingSession, "get_drive_sharing_status", map[string]any{"path": "/my-files/Reports"}), "unavailable")
}

func TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree(t *testing.T) {
	t.Parallel()

	members := make([]drivecli.Member, maxSharingMembers+1)
	for index := range members {
		members[index] = drivecli.Member{
			UID:          fmt.Sprintf("member:%d", index),
			InviteeEmail: fmt.Sprintf("member-%d@example.test", index),
			Role:         "viewer",
		}
	}
	fixture, err := json.Marshal(drivecli.ShareResult{Members: members})
	if err != nil {
		t.Fatalf("marshal sharing fixture: %v", err)
	}

	var audit bytes.Buffer
	client, _ := newToolTestClient(t, "", fixture)
	session := connectDriveTestClient(t, Options{CLI: client, Audit: NewAuditor(&audit)})

	var result sharingStatusResult
	decodeDriveResult(t, callDriveTool(t, session, "get_drive_sharing_status", map[string]any{"path": "/my-files/confidential"}), &result)
	if !result.Shared || len(result.Members) != maxSharingMembers || !result.Truncated {
		t.Fatalf("bounded sharing result = %+v", result)
	}
	if got := audit.String(); got != `{"event":"tool_call","tool":"get_drive_sharing_status","outcome":"ok","truncated":true}`+"\n" {
		t.Fatalf("sharing audit = %q", got)
	}
	for _, leak := range []string{"confidential", "member-0@example.test", "member:0"} {
		if strings.Contains(audit.String(), leak) {
			t.Fatalf("sharing audit leaks %q: %q", leak, audit.String())
		}
	}
}

func TestEncodeBoundedPreservesSharingStateWhenURLAccessOverflows(t *testing.T) {
	t.Parallel()

	oversize := &sharingStatusResult{
		Shared: true,
		URLAccess: &sharingURLAccess{
			URL: strings.Repeat("<", maxToolResultBytes),
		},
	}

	encoded, truncated, err := encodeBounded(oversize)
	if err != nil {
		t.Fatalf("encodeBounded: %v", err)
	}
	if !truncated || len(encoded) > maxToolResultBytes {
		t.Fatalf("truncated = %v, bytes = %d", truncated, len(encoded))
	}

	var decoded struct {
		Shared    bool `json:"shared"`
		Truncated bool `json:"truncated"`
		URLAccess any  `json:"urlAccess"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("shrunken sharing result is not valid JSON: %v", err)
	}
	if !decoded.Shared || !decoded.Truncated || decoded.URLAccess != nil {
		t.Fatalf("shrunken sharing result = %s", encoded)
	}
}

func TestDriveToolsMapAdapterFailuresToStableCodes(t *testing.T) {
	t.Parallel()

	authClient, _ := newToolTestClient(t, "auth-required", nil)
	authSession := connectDriveTestClient(t, Options{CLI: authClient})
	requireDriveToolError(t, callDriveTool(t, authSession, "get_drive_metadata", map[string]any{"path": "/my-files"}), "unavailable")

	failingClient, _ := newToolTestClient(t, "", nil)
	failingSession := connectDriveTestClient(t, Options{CLI: failingClient})
	requireDriveToolError(t, callDriveTool(t, failingSession, "list_drive_entries", map[string]any{"path": "/absent"}), "unavailable")
}

func TestMapDriveErrorCoversEveryAdapterCode(t *testing.T) {
	t.Parallel()

	cases := map[string]string{
		drivecli.CodeTimedOut:        errTimedOut,
		drivecli.CodeCanceled:        errCanceled,
		drivecli.CodeOutputOverflow:  errBoundsExceeded,
		drivecli.CodeInvalidConfig:   errUnavailable,
		drivecli.CodeVersionMismatch: errUnavailable,
		drivecli.CodeMalformedOutput: errUnavailable,
		drivecli.CodeTruncatedOutput: errUnavailable,
		drivecli.CodeCommandFailed:   errUnavailable,
		drivecli.CodeAuthRequired:    errUnavailable,
	}
	for adapterCode, wantCode := range cases {
		if got := mapDriveError(&drivecli.Error{Code: adapterCode}); got != wantCode {
			t.Errorf("mapDriveError(%q) = %q, want %q", adapterCode, got, wantCode)
		}
	}

	if got := mapDriveError(context.Canceled); got != errCanceled {
		t.Errorf("mapDriveError(context.Canceled) = %q, want %q", got, errCanceled)
	}
	if got := mapDriveError(context.DeadlineExceeded); got != errTimedOut {
		t.Errorf("mapDriveError(context.DeadlineExceeded) = %q, want %q", got, errTimedOut)
	}
	if got := mapDriveError(fmt.Errorf("wrapped: %w", os.ErrPermission)); got != errInternal {
		t.Errorf("mapDriveError(unknown) = %q, want %q", got, errInternal)
	}
}

func TestDriveAuditRecordsOnlyToolNameAndOutcome(t *testing.T) {
	t.Parallel()

	var audit bytes.Buffer
	client, _ := newToolTestClient(t, "", testkit.DriveFixture(t, "list-my-files.json"))
	session := connectDriveTestClient(t, Options{CLI: client, Audit: NewAuditor(&audit)})

	decodeDriveResult(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "/my-files"}), &listDriveResult{})
	requireDriveToolError(t, callDriveTool(t, session, "list_drive_entries", map[string]any{"path": "not-absolute"}), "invalid_argument")

	lines := strings.Split(strings.TrimSpace(audit.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("audit lines = %d: %q", len(lines), audit.String())
	}
	if lines[0] != `{"event":"tool_call","tool":"list_drive_entries","outcome":"ok"}` {
		t.Fatalf("ok audit line = %q", lines[0])
	}
	if lines[1] != `{"event":"tool_call","tool":"list_drive_entries","outcome":"error","code":"invalid_argument"}` {
		t.Fatalf("error audit line = %q", lines[1])
	}
	for _, leak := range []string{"my-files", "notes.txt", "node:", "not-absolute", "owner@example.test"} {
		if strings.Contains(audit.String(), leak) {
			t.Fatalf("audit output leaks %q: %q", leak, audit.String())
		}
	}
}

func TestEncodeBoundedShrinksOversizeListResultsIntoValidJSON(t *testing.T) {
	t.Parallel()

	oversize := &listDriveResult{Path: "/my-files"}
	for index := range 400 {
		oversize.Entries = append(oversize.Entries, drivecli.NodeEntity{
			UID:  fmt.Sprintf("node:%d", index),
			Name: drivecli.NameResult{OK: true, Value: strings.Repeat("n", 400)},
		})
	}

	encoded, truncated, err := encodeBounded(oversize)
	if err != nil {
		t.Fatalf("encodeBounded: %v", err)
	}
	if !truncated || len(encoded) > maxToolResultBytes {
		t.Fatalf("truncated = %v, bytes = %d", truncated, len(encoded))
	}
	var decoded listDriveResult
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("shrunken output is not valid JSON: %v", err)
	}
	if !decoded.Truncated || len(decoded.Entries) == 0 || len(decoded.Entries) >= 400 {
		t.Fatalf("shrunken result = truncated %v with %d entries", decoded.Truncated, len(decoded.Entries))
	}

	unshrinkable := struct{ Filler string }{Filler: strings.Repeat("x", maxToolResultBytes+1)}
	encoded, truncated, err = encodeBounded(&unshrinkable)
	if err != nil || !truncated || string(encoded) != `{"truncated":true}` {
		t.Fatalf("fallback output = %q, truncated %v, err %v", encoded, truncated, err)
	}
}

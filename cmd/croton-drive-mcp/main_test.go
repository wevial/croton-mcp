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

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const runDriveMainEnv = "CROTON_DRIVE_MCP_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runDriveMainEnv) == "1" {
		main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}

// startDriveStdioSession spawns the production executable over stdio against
// one CLI binary path and returns the client session plus captured stderr.
func startDriveStdioSession(t *testing.T, binaryPath string) (*mcp.ClientSession, *bytes.Buffer) {
	t.Helper()

	configPath := filepath.Join(canonicalTempDir(t), "croton-drive.json")
	encoded, err := json.Marshal(map[string]any{"cli": map[string]string{"binaryPath": binaryPath}})
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	if err := os.WriteFile(configPath, encoded, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	stderr := &bytes.Buffer{}
	command := exec.Command(executable, "--config", configPath)
	command.Env = append(os.Environ(), runDriveMainEnv+"=1")
	command.Stderr = stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "croton-drive-stdio-test", Version: "0.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect Drive server over stdio: %v (stderr: %s)", err, stderr.String())
	}

	return session, stderr
}

func callDriveStdioTool(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}

	return result
}

func driveStdioResultText(t *testing.T, result *mcp.CallToolResult) string {
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

func TestStdioInitializesAnIndependentDriveServerWithThreeReadOnlyTools(t *testing.T) {
	session, stderr := startDriveStdioSession(t, "/opt/proton-drive/proton-drive")

	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol = %q, want 2026-07-28", got)
	}
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list Drive tools: %v", err)
	}
	if len(listed.Tools) != 3 {
		t.Fatalf("Drive tools = %d, want 3", len(listed.Tools))
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close Drive session: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("clean protocol startup wrote diagnostics: %q", stderr.String())
	}
}

func TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation(t *testing.T) {
	listBinary := testkit.FakeDrive(t, "", testkit.DriveFixture(t, "list-my-files.json"))
	listSession, listStderr := startDriveStdioSession(t, listBinary)

	var listing struct {
		Path      string            `json:"path"`
		Entries   []json.RawMessage `json:"entries"`
		Truncated bool              `json:"truncated"`
	}
	listResult := callDriveStdioTool(t, listSession, "list_drive_entries", map[string]any{"path": "/my-files"})
	if listResult.IsError {
		t.Fatalf("list over stdio failed: %s", driveStdioResultText(t, listResult))
	}
	if err := json.Unmarshal([]byte(driveStdioResultText(t, listResult)), &listing); err != nil {
		t.Fatalf("decode list result: %v", err)
	}
	if listing.Path != "/my-files" || len(listing.Entries) != 1 || listing.Truncated {
		t.Fatalf("stdio list result = %+v", listing)
	}
	if got := testkit.RecordedArgv(t, listBinary); got != "filesystem\nlist\n/my-files\n--json\n" {
		t.Fatalf("stdio list argv = %q", got)
	}
	if err := listSession.Close(); err != nil {
		t.Fatalf("close list session: %v", err)
	}
	if audit := listStderr.String(); !strings.Contains(audit, `"tool":"list_drive_entries","outcome":"ok"`) || strings.Contains(audit, "my-files") {
		t.Fatalf("stdio list audit = %q", audit)
	}

	infoBinary := testkit.FakeDrive(t, "", testkit.DriveFixture(t, "info-folder.json"))
	infoSession, _ := startDriveStdioSession(t, infoBinary)
	defer func() { _ = infoSession.Close() }()

	var node struct {
		UID  string `json:"uid"`
		Type string `json:"type"`
		Name struct {
			Value string `json:"value"`
		} `json:"name"`
	}
	infoResult := callDriveStdioTool(t, infoSession, "get_drive_metadata", map[string]any{"path": "/my-files/Reports"})
	if infoResult.IsError {
		t.Fatalf("metadata over stdio failed: %s", driveStdioResultText(t, infoResult))
	}
	if err := json.Unmarshal([]byte(driveStdioResultText(t, infoResult)), &node); err != nil {
		t.Fatalf("decode metadata result: %v", err)
	}
	if node.UID != "node:folder-1" || node.Type != "folder" || node.Name.Value != "Reports" {
		t.Fatalf("stdio metadata node = %+v", node)
	}
	if got := testkit.RecordedArgv(t, infoBinary); got != "filesystem\ninfo\n/my-files/Reports\n--json\n" {
		t.Fatalf("stdio metadata argv = %q", got)
	}

	sharingBinary := testkit.FakeDrive(t, "", testkit.DriveFixture(t, "sharing-status.json"))
	sharingSession, sharingStderr := startDriveStdioSession(t, sharingBinary)

	var sharing struct {
		Shared            bool `json:"shared"`
		ProtonInvitations []struct {
			InviteeEmail string `json:"inviteeEmail"`
		} `json:"protonInvitations"`
	}
	sharingResult := callDriveStdioTool(t, sharingSession, "get_drive_sharing_status", map[string]any{"path": "/my-files/Reports"})
	if sharingResult.IsError {
		t.Fatalf("sharing status over stdio failed: %s", driveStdioResultText(t, sharingResult))
	}
	sharingText := driveStdioResultText(t, sharingResult)
	if err := json.Unmarshal([]byte(sharingText), &sharing); err != nil {
		t.Fatalf("decode sharing status result: %v", err)
	}
	if !sharing.Shared || len(sharing.ProtonInvitations) != 1 || sharing.ProtonInvitations[0].InviteeEmail != "reader@example.test" {
		t.Fatalf("stdio sharing status = %+v", sharing)
	}
	if strings.Contains(sharingText, "fixture-password") {
		t.Fatalf("stdio sharing result leaks custom password: %q", sharingText)
	}
	if got := testkit.RecordedArgv(t, sharingBinary); got != "sharing\nstatus\n/my-files/Reports\n--json\n" {
		t.Fatalf("stdio sharing argv = %q", got)
	}
	if err := sharingSession.Close(); err != nil {
		t.Fatalf("close sharing session: %v", err)
	}
	if audit := sharingStderr.String(); !strings.Contains(audit, `"tool":"get_drive_sharing_status","outcome":"ok"`) || strings.Contains(audit, "reader@example.test") || strings.Contains(audit, "my-files") || strings.Contains(audit, "fixture-password") {
		t.Fatalf("stdio sharing audit = %q", audit)
	}
}

func TestStdioDriveToolsFailClosedWhenNegotiationFails(t *testing.T) {
	binary := testkit.FakeDrive(t, "version-mismatch", testkit.DriveFixture(t, "list-my-files.json"))
	session, stderr := startDriveStdioSession(t, binary)

	for _, call := range []struct {
		tool      string
		arguments map[string]any
	}{
		{"list_drive_entries", map[string]any{"path": "/my-files"}},
		{"get_drive_metadata", map[string]any{"path": "/my-files"}},
	} {
		result := callDriveStdioTool(t, session, call.tool, call.arguments)
		if !result.IsError {
			t.Fatalf("%s succeeded despite failed negotiation: %s", call.tool, driveStdioResultText(t, result))
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(driveStdioResultText(t, result)), &envelope); err != nil {
			t.Fatalf("decode %s error: %v", call.tool, err)
		}
		if envelope.Error.Code != "unavailable" {
			t.Fatalf("%s error code = %q, want unavailable", call.tool, envelope.Error.Code)
		}
	}

	if got := testkit.RecordedArgv(t, binary); got != "version\n" {
		t.Fatalf("argv after failed negotiation = %q, want only the version handshake", got)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close session: %v", err)
	}
	if audit := stderr.String(); strings.Contains(audit, "my-files") || strings.Contains(audit, binary) {
		t.Fatalf("stderr leaks request or CLI details: %q", audit)
	}
}

func canonicalTempDir(t *testing.T) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}

	return resolved
}

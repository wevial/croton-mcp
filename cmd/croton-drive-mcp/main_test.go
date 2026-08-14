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
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const runDriveMainEnv = "CROTON_DRIVE_MCP_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runDriveMainEnv) == "1" {
		main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func TestStdioInitializesAnIndependentEmptyDriveServer(t *testing.T) {
	configPath := filepath.Join(canonicalTempDir(t), "croton-drive.json")
	if err := os.WriteFile(configPath, []byte(`{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	var stderr bytes.Buffer
	command := exec.Command(executable, "--config", configPath)
	command.Env = append(os.Environ(), runDriveMainEnv+"=1")
	command.Stderr = &stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "croton-drive-stdio-test", Version: "0.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect Drive server over stdio: %v (stderr: %s)", err, stderr.String())
	}

	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol = %q, want 2026-07-28", got)
	}
	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list Drive tools: %v", err)
	}
	if len(listed.Tools) != 0 {
		t.Fatalf("Drive tools = %d, want 0", len(listed.Tools))
	}
	if err := session.Close(); err != nil {
		t.Fatalf("close Drive session: %v", err)
	}
	if stderr.Len() != 0 {
		t.Fatalf("clean protocol startup wrote diagnostics: %q", stderr.String())
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

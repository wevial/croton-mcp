//go:build linux || darwin

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
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestServerInfoVersionIsStamped builds the real binary so a renamed version
// variable cannot silently fall back to "dev" in a release build.
func TestServerInfoVersionIsStamped(t *testing.T) {
	for _, tc := range []struct {
		name  string
		flags []string
		want  string
	}{
		{name: "unstamped", want: "dev"},
		{name: "stamped", flags: []string{"-ldflags", "-X github.com/wevial/croton-mcp/internal/mcpserver.version=v9.9.9-test"}, want: "v9.9.9-test"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			binary := buildCrotonBinary(t, tc.flags...)

			config, err := json.Marshal(map[string]any{
				"imap": map[string]any{
					"host": "127.0.0.1", "port": 1, "tlsMode": "implicit",
					"credentialCommand": []string{"/nonexistent/croton-credentials"},
					"tls":               map[string]any{"spkiSha256": strings.Repeat("0", 64)},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(canonicalTempDir(t), "croton.json")
			if err := os.WriteFile(configPath, config, 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			var stderr bytes.Buffer
			command := exec.CommandContext(ctx, binary, "--config", configPath)
			command.Stderr = &stderr
			client := mcp.NewClient(&mcp.Implementation{Name: "croton-version-client", Version: "0.0.0"}, nil)
			session, err := client.Connect(ctx, &mcp.CommandTransport{Command: command}, nil)
			if err != nil {
				t.Fatalf("connect over stdio: %v (stderr: %s)", err, stderr.String())
			}
			defer session.Close()

			if got := session.InitializeResult().ServerInfo.Version; got != tc.want {
				t.Fatalf("serverInfo.version = %q, want %q", got, tc.want)
			}
		})
	}
}

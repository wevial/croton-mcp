//go:build linux || darwin

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// This helper fails closed before any connection if catalog discovery ever
// requests credentials. Its only side effect is a test-owned marker.
func TestIndependentCatalogCredentialHelper(t *testing.T) {
	for i, arg := range os.Args {
		if arg == "--catalog-marker" && i+1 < len(os.Args) {
			if err := os.WriteFile(os.Args[i+1], []byte("invoked"), 0o600); err != nil {
				os.Exit(2)
			}

			os.Exit(1)
		}
	}
}

func TestIndependentStdioCatalog(t *testing.T) {
	binary := buildCrotonBinary(t)
	var baseline any

	for _, split := range []bool{false, true} {
		name := "one_frame_per_write"
		if split {
			name = "split_frame"
		}

		t.Run(name, func(t *testing.T) {
			directory := canonicalTempDir(t)
			marker := filepath.Join(directory, "helper-invoked")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}

			config, err := json.Marshal(map[string]any{
				"imap": map[string]any{
					"host": "127.0.0.1", "port": 1, "tlsMode": "implicit",
					"credentialCommand": []string{executable, "-test.run=^TestIndependentCatalogCredentialHelper$", "--", "--catalog-marker", marker},
					"tls":               map[string]any{"spkiSha256": strings.Repeat("0", 64)},
				},
				"audit": map[string]any{"enabled": true},
			})
			if err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(directory, "croton.json")
			if err := os.WriteFile(configPath, config, 0o600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			// Parent-owned pipes permit deadlines and draining stdout through EOF
			// even when Wait has already reaped the child.
			input, stdin, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer input.Close()
			defer stdin.Close()

			stdout, output, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdout.Close()
			defer output.Close()

			deadline, _ := ctx.Deadline()
			if err := stdin.SetWriteDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if err := stdout.SetReadDeadline(deadline); err != nil {
				t.Fatal(err)
			}

			var stderr bytes.Buffer
			command := exec.CommandContext(ctx, binary, "--config", configPath)
			command.Stdin, command.Stdout, command.Stderr = input, output, &stderr
			command.WaitDelay = time.Second
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			_ = input.Close()
			_ = output.Close()

			done := make(chan struct{})
			var waitErr error
			go func() {
				waitErr = command.Wait()
				close(done)
			}()
			defer func() {
				cancel()
				_ = command.Process.Kill()
				<-done
			}()

			writeFrame := func(frame string) {
				t.Helper()

				parts := []string{frame + "\n"}
				if split {
					// Split inside the JSON-RPC version string, always before the
					// newline. No sleeps or assumptions about pipe read boundaries.
					const boundary = 14
					parts = []string{frame[:boundary], frame[boundary:] + "\n"}
				}

				for _, part := range parts {
					if n, err := io.WriteString(stdin, part); err != nil || n != len(part) {
						t.Fatalf("write protocol frame: bytes=%d, err=%v", n, err)
					}
				}
			}
			reader := bufio.NewReader(stdout)
			readResponse := func(id string, result any) {
				t.Helper()

				frame, err := reader.ReadBytes('\n')
				if err != nil {
					t.Fatalf("read response %s: %v", id, err)
				}

				var envelope map[string]json.RawMessage
				if err := json.Unmarshal(frame, &envelope); err != nil {
					t.Fatalf("malformed stdout frame: %v", err)
				}
				if string(envelope["jsonrpc"]) != `"2.0"` || string(envelope["id"]) != id || envelope["result"] == nil || envelope["error"] != nil || envelope["method"] != nil {
					t.Fatalf("expected JSON-RPC success response for ID %s", id)
				}
				if err := json.Unmarshal(envelope["result"], result); err != nil {
					t.Fatalf("decode result for ID %s: %v", id, err)
				}
			}

			writeFrame(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"independent.test","version":"0.0.0"}}}`)
			var initialized struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			readResponse("1", &initialized)
			if initialized.ProtocolVersion != "2025-11-25" {
				t.Fatalf("protocolVersion = %q, want 2025-11-25", initialized.ProtocolVersion)
			}

			writeFrame(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)
			writeFrame(`{"jsonrpc":"2.0","id":2,"method":"tools/list","params":{}}`)
			var catalog json.RawMessage
			readResponse("2", &catalog)
			var listed struct {
				Tools []struct {
					Name        string `json:"name"`
					Annotations struct {
						ReadOnlyHint bool `json:"readOnlyHint"`
					} `json:"annotations"`
				} `json:"tools"`
				NextCursor string `json:"nextCursor"`
			}
			if err := json.Unmarshal(catalog, &listed); err != nil {
				t.Fatal(err)
			}
			want := []string{"get_message", "get_thread", "list_attachments", "list_folders", "search_mail", "select_digest_candidates"}
			var names []string
			for _, tool := range listed.Tools {
				names = append(names, tool.Name)
				if !tool.Annotations.ReadOnlyHint {
					t.Errorf("tool %q lacks readOnlyHint=true", tool.Name)
				}
			}
			slices.Sort(names)
			if len(listed.Tools) != 6 || !slices.Equal(names, want) || listed.NextCursor != "" {
				t.Fatalf("catalog = %v, cursor=%q; want exactly %v", names, listed.NextCursor, want)
			}

			var decoded any
			if err := json.Unmarshal(catalog, &decoded); err != nil {
				t.Fatal(err)
			}
			if !split {
				baseline = decoded
			} else if !reflect.DeepEqual(decoded, baseline) {
				t.Error("catalog differs between framing cases")
			}

			if err := stdin.Close(); err != nil {
				t.Fatal(err)
			}
			// Exactly two responses total: reject duplicates, notification replies,
			// audit output, blank lines, and even unterminated trailing bytes.
			if extra, err := reader.ReadBytes('\n'); len(extra) != 0 || err != io.EOF {
				t.Fatalf("expected stdout EOF after two responses; extra bytes=%d, err=%v", len(extra), err)
			}
			select {
			case <-done:
				if waitErr != nil {
					t.Fatalf("child did not exit successfully: %v", waitErr)
				}
			case <-ctx.Done():
				t.Fatal("child did not exit within deadline after stdin EOF")
			}

			if _, err := os.Stat(marker); !os.IsNotExist(err) {
				t.Fatalf("credential helper marker must not exist: %v", err)
			}
		})
	}
}

func TestIndependentStdioCatalogDocumentation(t *testing.T) {
	content, err := os.ReadFile(filepath.Join("..", "..", "docs", "MCP.md"))
	if err != nil {
		t.Fatal(err)
	}

	for _, statement := range []string{
		"`TestIndependentStdioCatalog`", "independent legacy protocol coverage",
		"`2025-11-25`", "one frame per write", "split across two writes",
		"not validated Claude Code or Codex compatibility",
	} {
		if !strings.Contains(string(content), statement) {
			t.Errorf("MCP.md lacks coverage statement %q", statement)
		}
	}
}

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const runMainEnv = "CROTON_MCP_RUN_MAIN"

func TestMain(m *testing.M) {
	if os.Getenv(runMainEnv) == "1" {
		main()
		os.Exit(0)
	}

	os.Exit(m.Run())
}

func TestCmdCredentialHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestCmdCredentialHelperProcess") {
		return
	}
	for index, value := range os.Args {
		if value == "--" && index+1 < len(os.Args) && os.Args[index+1] == "valid" {
			fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"}`)
			// Exit before the test framework can print PASS to stdout: the
			// credential parser strictly rejects trailing output.
			os.Exit(0)
		}
	}
}

func writeServerConfig(t *testing.T, server *testkit.Server) string {
	t.Helper()

	host, portText, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	configured := map[string]any{
		"imap": map[string]any{
			"host":              host,
			"port":              port,
			"tlsMode":           "implicit",
			"credentialCommand": []string{executable, "-test.run=TestCmdCredentialHelperProcess", "--", "valid"},
			"tls":               map[string]any{"spkiSha256": server.SPKISHA256()},
			"connectTimeoutMs":  2000,
		},
		"audit": map[string]any{"enabled": true},
	}
	encoded, err := json.Marshal(configured)
	if err != nil {
		t.Fatalf("encode config: %v", err)
	}
	path := filepath.Join(canonicalTempDir(t), "croton.json")
	if err := os.WriteFile(path, encoded, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// canonicalTempDir returns a temporary directory with every symlink in it
// resolved. Croton's secure loader refuses to traverse a symlinked parent, and
// on macOS t.TempDir hands back a path under /var, which is a symlink to
// /private/var, so a Croton process handed an unresolved fixture path exits
// before serving a single request. Resolving belongs to the fixture, not to
// the loader: production policy is untouched. On Linux, where the temporary
// directory is already canonical, this is a no-op.
func canonicalTempDir(t *testing.T) string {
	t.Helper()

	resolved, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temporary directory: %v", err)
	}

	return resolved
}

func serverCommand(t *testing.T, configPath string, stderr *bytes.Buffer) *exec.Cmd {
	t.Helper()

	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := exec.Command(executable, "--config", configPath)
	command.Env = append(os.Environ(), runMainEnv+"=1")
	command.Stderr = stderr
	return command
}

func TestStdioServesToolsOverOfficialSDKAndAuditsSafely(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	var stderr bytes.Buffer
	command := serverCommand(t, writeServerConfig(t, server), &stderr)
	client := mcp.NewClient(&mcp.Implementation{Name: "croton-e2e-client", Version: "0.0.0"}, nil)
	session, err := client.Connect(context.Background(), &mcp.CommandTransport{Command: command}, nil)
	if err != nil {
		t.Fatalf("connect over stdio: %v (stderr: %s)", err, stderr.String())
	}

	if got := session.InitializeResult().ProtocolVersion; got != "2026-07-28" {
		t.Fatalf("negotiated protocol = %q, want 2026-07-28", got)
	}

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools over stdio: %v", err)
	}
	if len(listed.Tools) != 6 {
		t.Fatalf("tools = %d, want 6", len(listed.Tools))
	}

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "list_folders", Arguments: map[string]any{}})
	if err != nil {
		t.Fatalf("call list_folders over stdio: %v", err)
	}
	if result.IsError {
		t.Fatalf("list_folders failed: %+v (stderr: %s)", result, stderr.String())
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok || !strings.Contains(text.Text, "INBOX") {
		t.Fatalf("unexpected list_folders content: %+v", result.Content)
	}

	if err := session.Close(); err != nil {
		t.Fatalf("close stdio session: %v (stderr: %s)", err, stderr.String())
	}

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}

	// Stderr must contain only allowlisted audit metadata: never folder
	// names, credentials, endpoints, or protocol data.
	sawAudit := false
	for _, line := range strings.Split(strings.TrimSpace(stderr.String()), "\n") {
		if line == "" {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("stderr line is not audit JSON: %q", line)
		}
		for key := range event {
			switch key {
			case "event", "tool", "outcome", "code", "truncated":
			default:
				t.Fatalf("stderr audit key %q not allowlisted: %q", key, line)
			}
		}
		sawAudit = true
	}
	if !sawAudit {
		t.Fatalf("no audit events on stderr: %q", stderr.String())
	}
	for _, fragment := range []string{"INBOX", "fixture-user", "fixture-password", "127.0.0.1", "jsonrpc"} {
		if strings.Contains(stderr.String(), fragment) {
			t.Fatalf("stderr leaks %q: %s", fragment, stderr.String())
		}
	}
}

func TestSIGTERMShutsDownCleanlyWithProtocolOnlyStdout(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	var stderr bytes.Buffer
	command := serverCommand(t, writeServerConfig(t, server), &stderr)
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	t.Cleanup(func() { _ = command.Process.Kill() })

	// One real initialize exchange proves stdout carries protocol frames.
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"croton-sigterm-client","version":"0.0.0"}}}` + "\n"
	if _, err := stdin.Write([]byte(initialize)); err != nil {
		t.Fatalf("write initialize: %v", err)
	}
	reader := bufio.NewReader(stdout)
	line, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read initialize response: %v (stderr: %s)", err, stderr.String())
	}
	var response struct {
		JSONRPC string `json:"jsonrpc"`
		ID      any    `json:"id"`
		Result  any    `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		t.Fatalf("stdout frame is not JSON-RPC: %v: %q", err, line)
	}
	if response.JSONRPC != "2.0" || response.Result == nil {
		t.Fatalf("unexpected initialize response: %q", line)
	}

	// Drain any remaining stdout concurrently; every byte must stay JSON.
	remainder := make(chan []byte, 1)
	go func() {
		var drained bytes.Buffer
		_, _ = drained.ReadFrom(reader)
		remainder <- drained.Bytes()
	}()

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("send SIGTERM: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("server did not exit cleanly after SIGTERM: %v (stderr: %s)", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("server did not exit within 5s of SIGTERM")
	}

	leftover := <-remainder
	for _, line := range bytes.Split(bytes.TrimSpace(leftover), []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if !json.Valid(line) {
			t.Fatalf("non-protocol bytes on stdout after SIGTERM: %q", line)
		}
	}
}

func TestStartupFailsClosedWithoutConfig(t *testing.T) {
	var stderr bytes.Buffer
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := exec.Command(executable)
	command.Env = append(os.Environ(), runMainEnv+"=1")
	command.Stderr = &stderr
	var stdout bytes.Buffer
	command.Stdout = &stdout

	err = command.Run()
	var exitError *exec.ExitError
	if err == nil {
		t.Fatal("server started without configuration")
	} else if !isExitCode(err, 1, &exitError) {
		t.Fatalf("exit status = %v, want exit code 1", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("startup failure wrote to stdout: %q", stdout.String())
	}
}

func TestStartupArgumentErrorsDoNotEchoAdversarialInput(t *testing.T) {
	const secret = "secret-user@mail.test-hunter2"

	var stderr bytes.Buffer
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}
	command := exec.Command(executable, "--"+secret)
	command.Env = append(os.Environ(), runMainEnv+"=1")
	command.Stderr = &stderr
	var stdout bytes.Buffer
	command.Stdout = &stdout

	err = command.Run()
	var exitError *exec.ExitError
	if err == nil {
		t.Fatal("server accepted an unknown argument")
	} else if !isExitCode(err, 1, &exitError) {
		t.Fatalf("exit status = %v, want exit code 1", err)
	}
	if stdout.Len() != 0 {
		t.Fatalf("argument failure wrote to stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) {
		t.Fatalf("argument failure leaked adversarial input: %q", stderr.String())
	}
}

func TestOversizeStdioFrameFailsClosedWithoutContentLeak(t *testing.T) {
	const secret = "secret-user@mail.test-hunter2"

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	var stderr bytes.Buffer
	command := serverCommand(t, writeServerConfig(t, server), &stderr)
	var stdout bytes.Buffer
	command.Stdout = &stdout
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	if err := command.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}

	frame := strings.Repeat("S", 64*1024-len(secret)) + secret + "\n"
	_, writeErr := io.WriteString(stdin, frame)
	_ = stdin.Close()
	if writeErr != nil {
		t.Fatalf("write oversize frame: %v", writeErr)
	}

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		var exitError *exec.ExitError
		if err == nil || !isExitCode(err, 1, &exitError) {
			t.Fatalf("exit status = %v, want exit code 1", err)
		}
	case <-time.After(5 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("server did not reject oversize frame within 5s")
	}

	if stdout.Len() != 0 {
		t.Fatalf("oversize frame produced stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), secret) || strings.Contains(stderr.String(), "mail.test") {
		t.Fatalf("oversize frame leaked content: %q", stderr.String())
	}
	if got := strings.TrimSpace(stderr.String()); got != "croton-mcp: server unavailable" {
		t.Fatalf("stderr = %q, want static server failure", got)
	}
}

func isExitCode(err error, want int, target **exec.ExitError) bool {
	if exitError, ok := err.(*exec.ExitError); ok {
		*target = exitError
		return exitError.ExitCode() == want
	}
	return false
}

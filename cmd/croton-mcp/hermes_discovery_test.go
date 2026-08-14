//go:build linux || darwin

// Hermes catalog discovery is an integration test against the installed
// Hermes CLI, which ships on Linux and macOS. Croton supports both, so the
// smoke runs natively on both. It is isolated from the first command onwards:
// every invocation runs against a throwaway HERMES_HOME holding nothing but
// this test's own registration, and the registered server is a freshly built
// Croton pinned to a synthetic fixture. No real Hermes profile, credential, or
// mailbox is read, written, or inspected.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// hermesServerName is the MCP server key Croton registers under. Hermes
// derives every wire tool name from it.
const hermesServerName = "croton"

// hermesCommandTimeout bounds every Hermes CLI invocation: discovery spawns
// the Croton binary, which only ever talks to the synthetic fixture.
const hermesCommandTimeout = 90 * time.Second

// hermesRequiredEnvironmentVariable turns a missing Hermes CLI from a skip
// into a failure. CI sets it on the runners whose entire purpose is to observe
// catalog discovery natively.
const hermesRequiredEnvironmentVariable = "CROTON_REQUIRE_HERMES"

// hermesSmokeRequired reports whether this host must actually observe Hermes
// rather than skip. Only an explicit "1" opts in, so an unrelated exported
// variable cannot fail an ordinary developer's test run.
func hermesSmokeRequired() bool {
	return strings.TrimSpace(os.Getenv(hermesRequiredEnvironmentVariable)) == "1"
}

// hermesRawToolNames are the tool names Croton itself advertises over MCP.
var hermesRawToolNames = []string{
	"get_message",
	"get_thread",
	"list_attachments",
	"list_folders",
	"search_mail",
	"select_digest_candidates",
}

// hermesPrefixedToolNames are the registry names Hermes must expose after a
// real runtime registration. Hermes builds them as mcp__<server>__<tool>.
var hermesPrefixedToolNames = []string{
	"mcp__croton__get_message",
	"mcp__croton__get_thread",
	"mcp__croton__list_attachments",
	"mcp__croton__list_folders",
	"mcp__croton__search_mail",
	"mcp__croton__select_digest_candidates",
}

// hermesForbiddenUtilityTools are the resource and prompt utilities Hermes
// synthesizes only for servers that advertise those capabilities. Croton
// advertises neither, so none of them may reach the catalog.
var hermesForbiddenUtilityTools = []string{
	"mcp__croton__list_resources",
	"mcp__croton__list_resource",
	"mcp__croton__read_resource",
	"mcp__croton__list_prompts",
	"mcp__croton__get_prompt",
}

// hermesDiscoveryScript prints the tool names Hermes registers at runtime.
// File descriptor 1 is parked on stderr for the duration of discovery so that
// logging, banners, and any inherited writer cannot corrupt the JSON result.
const hermesDiscoveryScript = `import json, os, sys

saved = os.dup(1)
os.dup2(2, 1)
try:
    from tools.mcp_tool import discover_mcp_tools, shutdown_mcp_servers

    names = list(discover_mcp_tools())
    try:
        shutdown_mcp_servers()
    except Exception:
        pass
finally:
    sys.stdout.flush()
    os.dup2(saved, 1)
    os.close(saved)

os.write(1, json.dumps(names).encode("utf-8"))
`

// TestHermesDiscoversExactCrotonCatalogFromProductionBinary drives the
// installed Hermes CLI through add, test, list, runtime registration, and
// remove inside a throwaway profile, against a production Croton binary that
// is pinned to a synthetic fixture, and proves Hermes registers exactly the
// six prefixed Croton tools and no utilities.
func TestHermesDiscoversExactCrotonCatalogFromProductionBinary(t *testing.T) {
	hermesPath, err := exec.LookPath("hermes")
	if err != nil {
		if hermesSmokeRequired() {
			t.Fatalf("%s=1 but the Hermes CLI is not installed: %v", hermesRequiredEnvironmentVariable, err)
		}
		t.Skip("hermes CLI is not installed; Croton catalog discovery cannot be observed")
	}

	// The throwaway profile comes first: nothing below can reach a real
	// Hermes profile, because every invocation is pinned to this directory.
	hermesHome := t.TempDir()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	configPath := writeServerConfig(t, server)
	binaryPath := buildCrotonBinary(t)

	added := runHermes(t, hermesPath, hermesHome, "\n",
		"mcp", "add", hermesServerName,
		"--connect-timeout", "60",
		"--command", binaryPath,
		"--args", "--config", configPath)
	if !strings.Contains(added, fmt.Sprintf("Found 6 tool(s) from '%s':", hermesServerName)) {
		t.Fatalf("add did not discover six tools:\n%s", added)
	}
	if !strings.Contains(added, "(6/6 tools enabled)") {
		t.Fatalf("add did not enable all six tools:\n%s", added)
	}
	assertHermesToolNames(t, "mcp add", hermesToolNamesAfter(added, "tool(s) from"), hermesRawToolNames)

	tested := runHermes(t, hermesPath, hermesHome, "", "mcp", "test", hermesServerName)
	if !strings.Contains(tested, "Tools discovered: 6") {
		t.Fatalf("test did not discover six tools:\n%s", tested)
	}
	assertHermesToolNames(t, "mcp test", hermesToolNamesAfter(tested, "Tools discovered:"), hermesRawToolNames)

	listed := runHermes(t, hermesPath, hermesHome, "", "mcp", "list")
	if !strings.Contains(listed, hermesServerName) || !strings.Contains(listed, "enabled") {
		t.Fatalf("list did not report %q as enabled:\n%s", hermesServerName, listed)
	}

	registered := runHermesDiscovery(t, hermesPath, hermesHome)
	assertHermesToolNames(t, "runtime registration", registered, hermesPrefixedToolNames)
	for _, utility := range hermesForbiddenUtilityTools {
		if slices.Contains(registered, utility) {
			t.Fatalf("Hermes registered resource/prompt utility %q: %v", utility, registered)
		}
	}

	removed := runHermes(t, hermesPath, hermesHome, "y\n", "mcp", "remove", hermesServerName)
	if !strings.Contains(removed, fmt.Sprintf("Removed '%s' from config", hermesServerName)) {
		t.Fatalf("remove did not drop %q:\n%s", hermesServerName, removed)
	}

	empty := runHermes(t, hermesPath, hermesHome, "", "mcp", "list")
	if !strings.Contains(empty, "No MCP servers configured.") {
		t.Fatalf("list still reports servers after removal:\n%s", empty)
	}
	if strings.Contains(empty, hermesServerName) {
		t.Fatalf("removed registration is still visible:\n%s", empty)
	}
}

// hermesEnvironment builds the child environment for one Hermes invocation.
// An inherited HERMES_HOME is removed rather than shadowed, so no command can
// fall back to a developer's real profile, and inherited Python paths are
// dropped so runtime discovery loads only the installed Hermes package.
func hermesEnvironment(base []string, home string) []string {
	environment := slices.DeleteFunc(slices.Clone(base), func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")

		return name == "HERMES_HOME" || name == "PYTHONPATH" || name == "PYTHONHOME"
	})

	return append(environment, "HERMES_HOME="+home, "NO_COLOR=1")
}

// buildCrotonBinary builds the production command into a test-owned directory.
func buildCrotonBinary(t *testing.T) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "croton-mcp")
	build := exec.Command("go", "build", "-o", path, ".")

	var stderr bytes.Buffer
	build.Stderr = &stderr
	if err := build.Run(); err != nil {
		t.Fatalf("build croton-mcp: %v: %s", err, stderr.String())
	}

	return path
}

// runHermes invokes the installed CLI against an isolated HERMES_HOME and
// fails the test on any nonzero exit.
func runHermes(t *testing.T, hermesPath, hermesHome, stdin string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), hermesCommandTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, hermesPath, args...)
	command.Env = hermesEnvironment(os.Environ(), hermesHome)
	command.Stdin = strings.NewReader(stdin)

	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("hermes %s failed: %v\n%s", strings.Join(args, " "), err, output)
	}

	return string(output)
}

// runHermesDiscovery registers the isolated catalog through the same runtime
// entry point a Hermes session uses, and returns the registered tool names.
func runHermesDiscovery(t *testing.T, hermesPath, hermesHome string) []string {
	t.Helper()

	scriptPath := filepath.Join(t.TempDir(), "discover.py")
	if err := os.WriteFile(scriptPath, []byte(hermesDiscoveryScript), 0o600); err != nil {
		t.Fatalf("write discovery script: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), hermesCommandTimeout)
	defer cancel()

	command := exec.CommandContext(ctx, hermesPythonInterpreter(t, hermesPath), scriptPath)
	command.Env = hermesEnvironment(os.Environ(), hermesHome)

	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("hermes runtime discovery failed: %v\n%s", err, stderr.String())
	}

	var names []string
	if err := json.Unmarshal(bytes.TrimSpace(stdout.Bytes()), &names); err != nil {
		t.Fatalf("discovery stdout is not JSON: %v: %q (stderr: %s)", err, stdout.String(), stderr.String())
	}

	return names
}

// hermesPythonInterpreter follows the installed launcher chain to the
// interpreter Hermes itself runs under: shell wrappers are traced through
// their exec target, and the first Python shebang wins.
func hermesPythonInterpreter(t *testing.T, hermesPath string) string {
	t.Helper()

	const maxHops = 8

	current := hermesPath
	for hop := 0; hop < maxHops; hop++ {
		interpreter, next, err := hermesLauncherHop(current)
		if err != nil {
			t.Fatalf("resolve Hermes interpreter from %s: %v", current, err)
		}
		if interpreter != "" {
			return interpreter
		}
		if next == "" {
			t.Fatalf("launcher %s names no Python interpreter or exec target", current)
		}
		if strings.Contains(filepath.Base(next), "python") {
			return next
		}

		current = next
	}

	t.Fatalf("Hermes launcher chain from %s did not terminate", hermesPath)

	return ""
}

// hermesLauncherHop reads one launcher script. It returns a Python
// interpreter when the shebang names one, or the next launcher an exec line
// hands off to.
func hermesLauncherHop(path string) (string, string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return "", "", errors.New("launcher is empty")
	}

	shebang := scanner.Text()
	if !strings.HasPrefix(shebang, "#!") {
		return "", "", errors.New("launcher is not a script")
	}
	fields := strings.Fields(strings.TrimPrefix(shebang, "#!"))
	if len(fields) == 0 {
		return "", "", errors.New("launcher shebang is empty")
	}

	for _, field := range fields {
		if strings.Contains(filepath.Base(field), "python") {
			return field, "", nil
		}
	}

	for scanner.Scan() {
		target, ok := hermesExecTarget(scanner.Text())
		if ok {
			return "", target, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", "", err
	}

	return "", "", nil
}

// hermesExecTarget extracts the absolute path a shell launcher execs.
func hermesExecTarget(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "exec ") {
		return "", false
	}

	for _, field := range strings.Fields(trimmed)[1:] {
		candidate := strings.Trim(field, `"'`)
		if filepath.IsAbs(candidate) {
			return candidate, true
		}
	}

	return "", false
}

// TestHermesPythonInterpreterResolvesNativeInterpreter proves the launcher
// chain resolver accepts a native (non-script) Python interpreter as its
// terminal hop, which is what macOS virtual environments install, while a
// shell wrapper leading to it is still traced through its exec target. The
// synthetic interpreter is only resolved, never executed.
func TestHermesPythonInterpreterResolvesNativeInterpreter(t *testing.T) {
	directory := t.TempDir()

	interpreterPath := filepath.Join(directory, "python3")
	nativeMagic := []byte{0x7f, 'E', 'L', 'F', 0x02, 0x01, 0x01, 0x00}
	if err := os.WriteFile(interpreterPath, nativeMagic, 0o700); err != nil {
		t.Fatalf("write native interpreter: %v", err)
	}

	launcherPath := filepath.Join(directory, "hermes")
	launcher := "#!/bin/sh\nexec \"" + interpreterPath + "\" -m hermes \"$@\"\n"
	if err := os.WriteFile(launcherPath, []byte(launcher), 0o700); err != nil {
		t.Fatalf("write launcher: %v", err)
	}

	if got := hermesPythonInterpreter(t, launcherPath); got != interpreterPath {
		t.Fatalf("resolved interpreter %q, want %q", got, interpreterPath)
	}
}

// hermesToolNamesAfter collects the tool names Hermes prints in the indented
// block that follows a marker line, stopping at the blank line that closes it.
func hermesToolNamesAfter(output, marker string) []string {
	_, rest, found := strings.Cut(output, marker)
	if !found {
		return nil
	}

	var names []string
	started := false
	for _, line := range strings.Split(rest, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasPrefix(line, "    ") {
			if started {
				break
			}
			continue
		}

		started = true
		names = append(names, fields[0])
	}

	return names
}

// assertHermesToolNames requires an exact set match, so an extra or renamed
// tool fails as loudly as a missing one.
func assertHermesToolNames(t *testing.T, stage string, got, want []string) {
	t.Helper()

	sorted := slices.Clone(got)
	slices.Sort(sorted)
	expected := slices.Clone(want)
	slices.Sort(expected)

	if !slices.Equal(sorted, expected) {
		t.Fatalf("%s catalog mismatch:\n got: %v\nwant: %v", stage, sorted, expected)
	}
}

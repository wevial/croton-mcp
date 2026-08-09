//go:build linux

// Hermes catalog discovery is an integration test against the installed
// Hermes CLI, which ships on Linux. The non-mutating fingerprint below also
// depends on Linux-only open flags and stat fields.
package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
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

// hermesFileFingerprint is a byte-and-metadata identity for one live Hermes
// file. Contents are reduced to a digest so the live profile is never logged.
type hermesFileFingerprint struct {
	exists     bool
	digest     string
	size       int64
	mode       fs.FileMode
	modTime    time.Time
	accessTime time.Time
	uid        uint32
	gid        uint32
}

// TestHermesDiscoversExactCrotonCatalogFromProductionBinary drives the
// installed Hermes CLI through add, test, list, and remove against a
// production Croton binary that is pinned to a synthetic fixture, and proves
// Hermes registers exactly the six prefixed Croton tools and no utilities.
func TestHermesDiscoversExactCrotonCatalogFromProductionBinary(t *testing.T) {
	hermesPath, err := exec.LookPath("hermes")
	if err != nil {
		t.Skip("hermes CLI is not installed; Croton catalog discovery cannot be observed")
	}

	liveConfigPath := hermesLiveConfigPath(t)
	before := hermesFingerprint(t, liveConfigPath)

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	configPath := writeServerConfig(t, server)
	binaryPath := buildCrotonBinary(t)
	hermesHome := t.TempDir()

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

	assertHermesLiveConfigUnchanged(t, liveConfigPath, before)
}

// TestHermesFingerprintPreservesLiveConfigAccessTime pins the live profile
// down to its access time: reading the file to fingerprint it must not itself
// be the mutation the fingerprint is meant to detect. A deliberately stale
// atime is what a relatime mount would refresh on the first ordinary read.
func TestHermesFingerprintPreservesLiveConfigAccessTime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := []byte("mcp_servers: {}\n")
	if err := os.WriteFile(path, contents, 0o600); err != nil {
		t.Fatalf("write fixture config: %v", err)
	}

	stale := time.Now().Add(-90 * 24 * time.Hour).Truncate(time.Second)
	modified := time.Now().Add(-30 * 24 * time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, stale, modified); err != nil {
		t.Fatalf("age fixture config: %v", err)
	}

	before := hermesAccessTime(t, path)

	fingerprint := hermesFingerprint(t, path)

	digest := sha256.Sum256(contents)
	switch {
	case !fingerprint.exists:
		t.Fatal("fingerprint reports the fixture config missing")
	case fingerprint.digest != hex.EncodeToString(digest[:]):
		t.Fatal("fingerprint digest does not match the fixture bytes")
	case fingerprint.size != int64(len(contents)):
		t.Fatalf("fingerprint size = %d, want %d", fingerprint.size, len(contents))
	case fingerprint.mode.Perm() != 0o600:
		t.Fatalf("fingerprint mode = %v, want -rw-------", fingerprint.mode.Perm())
	case !fingerprint.modTime.Equal(modified):
		t.Fatalf("fingerprint mtime = %s, want %s", fingerprint.modTime, modified)
	case !fingerprint.accessTime.Equal(stale):
		t.Fatalf("fingerprint atime = %s, want %s", fingerprint.accessTime, stale)
	}

	if after := hermesAccessTime(t, path); !after.Equal(before) {
		t.Fatalf("fingerprinting updated access time: %s -> %s", before, after)
	}
}

// hermesAccessTime reads one file's access time without opening it.
func hermesAccessTime(t *testing.T, path string) time.Time {
	t.Helper()

	var stat syscall.Stat_t
	if err := syscall.Stat(path, &stat); err != nil {
		t.Fatalf("stat access time: %v", err)
	}

	return time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
}

// hermesLiveConfigPath resolves the profile configuration the test must never
// touch: the active HERMES_HOME when one is set, and ~/.hermes otherwise.
func hermesLiveConfigPath(t *testing.T) string {
	t.Helper()

	if home := strings.TrimSpace(os.Getenv("HERMES_HOME")); home != "" {
		return filepath.Join(home, "config.yaml")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve home directory: %v", err)
	}

	return filepath.Join(home, ".hermes", "config.yaml")
}

// hermesFingerprint digests one live file. A missing file is a valid state and
// must still be missing afterwards.
func hermesFingerprint(t *testing.T, path string) hermesFileFingerprint {
	t.Helper()

	// O_NOATIME keeps the fingerprinting read from becoming the very
	// mutation the fingerprint exists to detect. Fail closed: anything short
	// of a non-mutating open aborts rather than quietly degrading to a read
	// that touches the live profile.
	descriptor, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOATIME, 0)
	if errors.Is(err, fs.ErrNotExist) {
		return hermesFileFingerprint{}
	}
	if err != nil {
		t.Fatalf("open live Hermes config without access-time updates: %v", err)
	}

	file := os.NewFile(uintptr(descriptor), path)
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		t.Fatalf("stat live Hermes config: %v", err)
	}

	contents, err := io.ReadAll(file)
	if err != nil {
		t.Fatalf("read live Hermes config: %v", err)
	}
	sum := sha256.Sum256(contents)

	fingerprint := hermesFileFingerprint{
		exists:  true,
		digest:  hex.EncodeToString(sum[:]),
		size:    info.Size(),
		mode:    info.Mode(),
		modTime: info.ModTime(),
	}

	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("live Hermes config carries no stat metadata")
	}
	fingerprint.accessTime = time.Unix(stat.Atim.Sec, stat.Atim.Nsec)
	fingerprint.uid, fingerprint.gid = stat.Uid, stat.Gid

	return fingerprint
}

// assertHermesLiveConfigUnchanged reports drift without ever echoing the live
// profile: only the differing field is named.
func assertHermesLiveConfigUnchanged(t *testing.T, path string, before hermesFileFingerprint) {
	t.Helper()

	after := hermesFingerprint(t, path)

	switch {
	case before.exists != after.exists:
		t.Fatalf("live Hermes config existence changed: before=%t after=%t", before.exists, after.exists)
	case before.digest != after.digest:
		t.Fatal("live Hermes config contents changed")
	case before.size != after.size:
		t.Fatalf("live Hermes config size changed: %d -> %d", before.size, after.size)
	case before.mode != after.mode:
		t.Fatalf("live Hermes config mode changed: %v -> %v", before.mode, after.mode)
	case !before.modTime.Equal(after.modTime):
		t.Fatalf("live Hermes config mtime changed: %s -> %s", before.modTime, after.modTime)
	case !before.accessTime.Equal(after.accessTime):
		t.Fatalf("live Hermes config atime changed: %s -> %s", before.accessTime, after.accessTime)
	case before.uid != after.uid || before.gid != after.gid:
		t.Fatalf("live Hermes config ownership changed: %d:%d -> %d:%d", before.uid, before.gid, after.uid, after.gid)
	}
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
	command.Env = append(os.Environ(), "HERMES_HOME="+hermesHome, "NO_COLOR=1")
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
	command.Env = append(os.Environ(), "HERMES_HOME="+hermesHome, "NO_COLOR=1")
	command.Env = slices.DeleteFunc(command.Env, func(entry string) bool {
		return strings.HasPrefix(entry, "PYTHONPATH=") || strings.HasPrefix(entry, "PYTHONHOME=")
	})

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

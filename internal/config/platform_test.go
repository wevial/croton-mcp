package config

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secureSources are the package files that implement and exercise the
// descriptor-relative, no-symlink configuration loader. Every one of them must
// be selected for Linux and Darwin, and for no other target.
var secureSources = []string{
	"open_unix.go",
	"ownership_unix.go",
	"ownership_unix_test.go",
	"fifo_unix_test.go",
}

// failClosedSources are the package files that refuse to load configuration at
// all. They are the exact complement of secureSources: selected everywhere the
// secure implementation is not, so no target is left without one of the two.
var failClosedSources = []string{
	"open_other.go",
	"ownership_other.go",
	"open_other_test.go",
}

// publicPlatformDocs are the documents a user consults before trusting Croton
// with a configuration file on their machine.
var publicPlatformDocs = []string{"README.md", "docs/MCP.md"}

// requiredDocPhrases are the guarantees the public documentation must state:
// which platforms are supported, what the secure loader actually promises on
// them, and that every other platform refuses to load configuration.
var requiredDocPhrases = []string{
	"Linux",
	"macOS",
	"descriptor-relative",
	"no-follow",
	"owned by the current user",
	"fail closed",
	"Windows",
}

// requiredMCPClientDocPhrases keep the client-neutral support statement honest:
// Croton's protocol contract is required, Hermes has a separate exercised
// consumer smoke, and other standards-compatible client targets are not
// represented as independently verified harnesses.
var requiredMCPClientDocPhrases = []string{
	"standards-compatible MCP clients",
	"Hermes, Claude Code, and Codex",
	"Hermes is the only client-specific compatibility",
}

// forbiddenDocPhrases are the stale Linux-only claims that must not survive
// macOS support, because they would tell a macOS user the opposite of the
// truth about their own configuration file.
var forbiddenDocPhrases = []string{"Linux-only", "currently Linux"}

// pinnedUVVersion is the uv release the macOS job pre-provisions, and
// pinnedUVDarwinSHA256 are the SHA-256 digests GitHub publishes alongside that
// release's two macOS archives. Both digests are required, because the digest
// the job actually checks is chosen at runtime from the runner's architecture.
const pinnedUVVersion = `UV_VERSION: "0.12.4"`

var pinnedUVDarwinSHA256 = []string{
	"99a913b606194867b43086404412c1afe079547fee72ecfb6af7e7b0dd54b0c6",
	"e603f1eb634ca97a2a125539b983891f53235e901511ed10c32c08c86e253ecd",
}

// requiredHermesInstallPhrases pin the Hermes CLI onto one immutable installer
// revision, run the way a clean GitHub runner needs it: the script is fetched
// from the commit-addressed raw URL to a file, checksum-verified with the
// shasum binary macOS ships, and only then executed from that verified file
// with no prompts, no setup wizard, and no browser, computer-use, or
// bundled-skill downloads. Pinning the installer alone is not enough, because
// on a fresh runner it installs its own dependencies: the job therefore
// pre-provisions a pinned, checksum-verified uv at $HERMES_HOME/bin/uv, which
// is exactly where the installer looks before it would otherwise fetch a
// mutable installer from astral.sh. Installation state stays confined to a CI
// temporary directory. The non-gating Hermes consumer-compatibility job alone
// sets CROTON_REQUIRE_HERMES=1, so a broken external installer cannot turn a
// client-neutral Croton quality failure into a required core failure.
var requiredHermesInstallPhrases = append([]string{
	"https://raw.githubusercontent.com/NousResearch/hermes-agent/f80f453ae0679347e38abc917c7f94f717bf96c5/scripts/install.sh",
	"shasum -a 256",
	"458ed1873bec1766ccd723b8a86338fbdf1caff5d43eae45065bc448cafa2dca",
	"--commit f80f453ae0679347e38abc917c7f94f717bf96c5",
	`bash "$installer"`,
	"--non-interactive",
	"--skip-setup",
	"--skip-browser",
	"--skip-computer-use",
	"--no-skills",
	"HERMES_HOME",
	"hermes --version",
	pinnedUVVersion,
	"https://github.com/astral-sh/uv/releases/download/",
	"aarch64-apple-darwin",
	"x86_64-apple-darwin",
	`"$HERMES_HOME/bin/uv" --version`,
}, pinnedUVDarwinSHA256...)

// requiredMacOSJobPhrases are the checks the native macOS core CI job must
// run. Cross-compilation cannot observe Darwin syscall behavior, so this job
// independently exercises the secure loader without installing any MCP client.
var requiredMacOSJobPhrases = []string{
	"runs-on: macos-latest",
	"go-version: 1.26.6",
	"go build ./...",
	"go vet ./...",
	"go test -race ./...",
	"staticcheck",
	"govulncheck",
}

// requiredLinuxJobPhrases keep the pre-existing Linux quality job intact and
// add the Darwin typecheck that catches a Linux-only syscall reference hiding
// behind a broadened build tag without executing a Darwin binary.
var requiredLinuxJobPhrases = []string{
	"runs-on: ubuntu-latest",
	"go build ./...",
	"go vet ./...",
	"go test -race ./...",
	"staticcheck",
	"govulncheck",
	"go mod tidy -diff",
	"go mod verify",
	"GOOS=darwin",
}

// forbiddenLinuxJobPhrases keep macOS support from changing what the Linux job
// does. The native smoke belongs to the Darwin runner; requiring or installing
// Hermes on Linux would make an unrelated third-party download a prerequisite
// for the pre-existing quality gates, whose Hermes test already skips itself
// when the CLI is absent.
var forbiddenLinuxJobPhrases = append([]string{
	"CROTON_REQUIRE_HERMES",
	"hermes",
}, requiredHermesInstallPhrases...)

// TestPlatformSelectsSecureOrFailClosedSources pins the build-constraint
// matrix itself: Darwin and Linux must compile the descriptor-relative
// implementation together with the behavior tests that prove it, and every
// other target must compile the fail-closed stub and its test. A build tag
// edit that quietly broadens platform support, or that ships the Darwin
// implementation without the tests that cover it, fails here before it can
// reach a release.
func TestPlatformSelectsSecureOrFailClosedSources(t *testing.T) {
	t.Parallel()

	cases := []struct {
		goos   string
		goarch string
		secure bool
	}{
		{goos: "darwin", goarch: "arm64", secure: true},
		{goos: "darwin", goarch: "amd64", secure: true},
		{goos: "linux", goarch: "amd64", secure: true},
		{goos: "windows", goarch: "amd64", secure: false},
		{goos: "freebsd", goarch: "amd64", secure: false},
		{goos: "js", goarch: "wasm", secure: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.goos+"/"+testCase.goarch, func(t *testing.T) {
			t.Parallel()

			context := build.Default
			context.GOOS, context.GOARCH = testCase.goos, testCase.goarch

			for _, name := range secureSources {
				if selected := matchesTarget(t, context, name); selected != testCase.secure {
					t.Errorf("%s selected = %t, want %t", name, selected, testCase.secure)
				}
			}
			for _, name := range failClosedSources {
				if selected := matchesTarget(t, context, name); selected == testCase.secure {
					t.Errorf("%s selected = %t, want %t", name, selected, !testCase.secure)
				}
			}
		})
	}
}

// TestPublicDocsDeclareMacOSSupportAndFailClosedPlatforms holds the public
// documentation to the same platform matrix as the build tags. A macOS user
// reading a stale "Linux-only" claim would either avoid a supported platform
// or, worse, assume the loader is unverified there.
func TestPublicDocsDeclareMacOSSupportAndFailClosedPlatforms(t *testing.T) {
	t.Parallel()

	combined := strings.Join(readRepositoryFiles(t, publicPlatformDocs), "\n")

	for _, phrase := range requiredDocPhrases {
		if !strings.Contains(combined, phrase) {
			t.Errorf("public docs never state %q", phrase)
		}
	}
	for _, phrase := range requiredMCPClientDocPhrases {
		if !strings.Contains(combined, phrase) {
			t.Errorf("public docs never state %q", phrase)
		}
	}
	for _, phrase := range forbiddenDocPhrases {
		if strings.Contains(combined, phrase) {
			t.Errorf("public docs still carry the stale claim %q", phrase)
		}
	}
}

// TestContinuousIntegrationSeparatesCoreAndConsumerCompatibility pins the
// client-neutral quality boundary. Linux and native macOS core jobs retain all
// required Croton build, security, and race coverage without installing
// Hermes. The Hermes catalog smoke remains visible in an isolated, non-gating
// consumer-compatibility job, so an upstream installer failure cannot block a
// valid Croton change.
func TestContinuousIntegrationSeparatesCoreAndConsumerCompatibility(t *testing.T) {
	t.Parallel()

	jobs := workflowJobs(t)

	macOS := jobs["verify-macos"]
	if macOS == "" {
		t.Fatal("CI defines no native macOS core job")
	}
	for _, phrase := range requiredMacOSJobPhrases {
		if !strings.Contains(macOS, phrase) {
			t.Errorf("macOS core CI job never runs %q", phrase)
		}
	}
	if strings.Contains(strings.ToLower(macOS), "hermes") {
		t.Error("macOS core CI job still depends on Hermes")
	}

	linux := jobs["verify"]
	if linux == "" {
		t.Fatal("CI no longer defines the Linux quality and security job")
	}
	for _, phrase := range requiredLinuxJobPhrases {
		if !strings.Contains(linux, phrase) {
			t.Errorf("Linux CI job never runs %q", phrase)
		}
	}
	for _, phrase := range forbiddenLinuxJobPhrases {
		if strings.Contains(linux, phrase) {
			t.Errorf("Linux CI job still carries the Hermes requirement %q", phrase)
		}
	}

	compatibility := jobs["hermes-compatibility"]
	if compatibility == "" {
		t.Fatal("CI defines no Hermes consumer-compatibility job")
	}
	for _, phrase := range []string{
		"name: Hermes consumer compatibility (non-gating)",
		"continue-on-error: true",
		"runs-on: macos-latest",
		"CROTON_REQUIRE_HERMES: \"1\"",
		"HERMES_HOME",
		"go test -race ./cmd/croton-mcp -run '^TestHermes'",
	} {
		if !strings.Contains(compatibility, phrase) {
			t.Errorf("Hermes compatibility CI job omits %q", phrase)
		}
	}
}

// TestContinuousIntegrationPinsHermesInstallerDependencies proves the two
// things a substring check alone cannot. First, that the pinned uv is already
// in place before the Hermes installer runs: the installer skips its own
// mutable astral.sh download only when $HERMES_HOME/bin/uv exists, so
// provisioning it afterwards would verify a checksum that no longer decides
// what executes. Second, that the flag suppressing the mutable trycua driver
// fetch sits on the installer's own command line, not merely somewhere in the
// job where a comment would satisfy the phrase check.
func TestContinuousIntegrationPinsHermesInstallerDependencies(t *testing.T) {
	t.Parallel()

	compatibility := workflowJobs(t)["hermes-compatibility"]
	if compatibility == "" {
		t.Fatal("CI defines no Hermes consumer-compatibility job")
	}

	invocation := strings.Index(compatibility, `bash "$installer"`)
	if invocation < 0 {
		t.Fatal("Hermes compatibility CI job never runs the verified Hermes installer")
	}

	provisioned := strings.Index(compatibility, `"$HERMES_HOME/bin/uv" --version`)
	if provisioned < 0 || provisioned > invocation {
		t.Error("Hermes compatibility CI job never proves a pinned uv at $HERMES_HOME/bin/uv before running the installer")
	}
	for _, digest := range pinnedUVDarwinSHA256 {
		if index := strings.Index(compatibility, digest); index < 0 || index > provisioned {
			t.Errorf("Hermes compatibility CI job never pins the uv archive digest %q before provisioning uv", digest)
		}
	}

	command := installerCommand(compatibility[invocation:])
	for _, flag := range []string{
		"--non-interactive",
		"--skip-setup",
		"--skip-browser",
		"--skip-computer-use",
		"--no-skills",
		"--commit f80f453ae0679347e38abc917c7f94f717bf96c5",
	} {
		if !strings.Contains(command, flag) {
			t.Errorf("Hermes installer command omits %q:\n%s", flag, command)
		}
	}
}

// installerCommand returns the one shell command opening the text, following
// backslash line continuations so a flag on a later line still counts as part
// of the same invocation.
func installerCommand(text string) string {
	var command strings.Builder
	for _, line := range strings.Split(text, "\n") {
		command.WriteString(line)
		command.WriteString("\n")
		if !strings.HasSuffix(strings.TrimRight(line, " \t"), `\`) {
			break
		}
	}

	return command.String()
}

// matchesTarget reports whether one package source compiles for the target
// described by the context. A source that does not exist is not selected,
// which is exactly how a rename that drops platform support should read.
func matchesTarget(t *testing.T, context build.Context, name string) bool {
	t.Helper()

	if _, err := os.Stat(name); err != nil {
		return false
	}

	matched, err := context.MatchFile(".", name)
	if err != nil {
		t.Fatalf("match %s for %s/%s: %v", name, context.GOOS, context.GOARCH, err)
	}

	return matched
}

// readRepositoryFiles reads paths relative to the repository root, which sits
// two directories above this package.
func readRepositoryFiles(t *testing.T, paths []string) []string {
	t.Helper()

	contents := make([]string, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join("..", "..", filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		contents = append(contents, string(data))
	}

	return contents
}

// workflowJobs splits the CI workflow into its job blocks, keyed by job name.
// Jobs are the two-space-indented mapping keys under "jobs:", so a block runs
// until the next such key.
func workflowJobs(t *testing.T) map[string]string {
	t.Helper()

	workflow := readRepositoryFiles(t, []string{".github/workflows/ci.yml"})[0]

	_, jobSection, found := strings.Cut(workflow, "\njobs:\n")
	if !found {
		t.Fatal("CI workflow declares no jobs")
	}

	jobs := make(map[string]string)
	current := ""
	var block strings.Builder
	for _, line := range strings.Split(jobSection, "\n") {
		if name, isHeader := workflowJobHeader(line); isHeader {
			if current != "" {
				jobs[current] = block.String()
			}
			current, block = name, strings.Builder{}
			continue
		}
		if current != "" {
			block.WriteString(line)
			block.WriteString("\n")
		}
	}
	if current != "" {
		jobs[current] = block.String()
	}

	return jobs
}

// workflowJobHeader reports whether one line opens a job block.
func workflowJobHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "  ") || strings.HasPrefix(line, "   ") {
		return "", false
	}

	name, _, found := strings.Cut(strings.TrimSpace(line), ":")
	if !found || name == "" || strings.Contains(name, " ") {
		return "", false
	}

	return name, true
}

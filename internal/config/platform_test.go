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

// forbiddenDocPhrases are the stale Linux-only claims that must not survive
// macOS support, because they would tell a macOS user the opposite of the
// truth about their own configuration file.
var forbiddenDocPhrases = []string{"Linux-only", "currently Linux"}

// requiredHermesInstallPhrases pin the Hermes CLI onto one immutable installer
// revision, run the way a clean GitHub runner needs it: the script is fetched
// from the commit-addressed raw URL to a file, checksum-verified with the
// shasum binary macOS ships, and only then executed from that verified file
// with no prompts, no setup wizard, and no browser or bundled-skill downloads.
// Installation state stays confined to a CI temporary directory. Only the
// native macOS job sets CROTON_REQUIRE_HERMES=1, so only that job must install
// this way, or the smoke it demands can never start.
var requiredHermesInstallPhrases = []string{
	"https://raw.githubusercontent.com/NousResearch/hermes-agent/f80f453ae0679347e38abc917c7f94f717bf96c5/scripts/install.sh",
	"shasum -a 256",
	"458ed1873bec1766ccd723b8a86338fbdf1caff5d43eae45065bc448cafa2dca",
	"--commit f80f453ae0679347e38abc917c7f94f717bf96c5",
	`bash "$installer"`,
	"--non-interactive",
	"--skip-setup",
	"--skip-browser",
	"--no-skills",
	"HERMES_HOME",
	"hermes --version",
}

// forbiddenJobPhrases are install routes a fresh clone cannot take. An
// undefined repository variable makes the very first CI run fail before the
// native smoke executes, a host package manager reintroduces the same unpinned
// dependency a pinned installer removes, and a mutable redirector piped
// straight into a shell executes whatever that endpoint serves at the moment
// CI runs, with no revision or checksum to verify.
var forbiddenJobPhrases = []string{
	"HERMES_INSTALL_SPEC",
	"pipx",
	"brew install",
	"hermes-agent.nousresearch.com/install.sh",
	"| bash",
	"bash -s",
}

// requiredMacOSJobPhrases are the checks the native macOS CI job must run.
// Cross-compilation cannot observe Darwin syscall behavior, so this job is the
// only place the secure loader and the Hermes smoke actually execute on macOS.
var requiredMacOSJobPhrases = append([]string{
	"runs-on: macos-latest",
	"go-version: 1.26.6",
	"go build ./...",
	"go vet ./...",
	"go test -race ./...",
	"CROTON_REQUIRE_HERMES",
	"staticcheck",
	"govulncheck",
	"hermes",
}, requiredHermesInstallPhrases...)

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
	for _, phrase := range forbiddenDocPhrases {
		if strings.Contains(combined, phrase) {
			t.Errorf("public docs still carry the stale claim %q", phrase)
		}
	}
}

// TestContinuousIntegrationRunsNativeMacOSChecks pins the one thing no test on
// a Linux host can prove for itself: that the supported-platform claim is
// backed by macOS builds, vet, race tests, and a required Hermes smoke on a
// real Darwin runner.
func TestContinuousIntegrationRunsNativeMacOSChecks(t *testing.T) {
	t.Parallel()

	jobs := workflowJobs(t)

	macOS := jobContaining(jobs, "runs-on: macos-latest")
	if macOS == "" {
		t.Fatal("CI defines no native macos-latest job")
	}
	for _, phrase := range requiredMacOSJobPhrases {
		if !strings.Contains(macOS, phrase) {
			t.Errorf("macOS CI job never runs %q", phrase)
		}
	}
	for _, phrase := range forbiddenJobPhrases {
		if strings.Contains(macOS, phrase) {
			t.Errorf("macOS CI job still installs Hermes via %q", phrase)
		}
	}

	linux := jobContaining(jobs, "runs-on: ubuntu-latest")
	if linux == "" {
		t.Fatal("CI no longer defines the Linux quality and security job")
	}
	for _, phrase := range requiredLinuxJobPhrases {
		if !strings.Contains(linux, phrase) {
			t.Errorf("Linux CI job never runs %q", phrase)
		}
	}
	for _, phrase := range forbiddenJobPhrases {
		if strings.Contains(linux, phrase) {
			t.Errorf("Linux CI job still installs Hermes via %q", phrase)
		}
	}
	for _, phrase := range forbiddenLinuxJobPhrases {
		if strings.Contains(linux, phrase) {
			t.Errorf("Linux CI job still carries the Hermes requirement %q", phrase)
		}
	}
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

// jobContaining returns the single job block carrying a marker, or the empty
// string when no job does.
func jobContaining(jobs map[string]string, marker string) string {
	for _, block := range jobs {
		if strings.Contains(block, marker) {
			return block
		}
	}

	return ""
}

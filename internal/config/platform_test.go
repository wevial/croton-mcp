package config

import (
	"go/build"
	"os"
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

package main

import (
	"go/build"
	"os"
	"testing"
)

// hermesSmokeSources are the package files carrying the Hermes catalog smoke.
// They must be selected wherever Croton can be smoke-tested against a real
// Hermes installation: Linux and macOS, and nowhere else.
var hermesSmokeSources = []string{
	"hermes_discovery_test.go",
	"hermes_isolation_test.go",
}

// TestHermesSmokeSourcesSelectLinuxAndDarwin pins the Hermes smoke test to the
// two platforms Croton supports. A regression that quietly re-narrows the
// catalog smoke to Linux, and so stops proving anything about macOS, fails
// here rather than passing silently as a no-op on a macOS runner.
func TestHermesSmokeSourcesSelectLinuxAndDarwin(t *testing.T) {
	t.Parallel()

	cases := []struct {
		goos      string
		goarch    string
		supported bool
	}{
		{goos: "darwin", goarch: "arm64", supported: true},
		{goos: "darwin", goarch: "amd64", supported: true},
		{goos: "linux", goarch: "amd64", supported: true},
		{goos: "windows", goarch: "amd64", supported: false},
		{goos: "freebsd", goarch: "amd64", supported: false},
	}
	for _, testCase := range cases {
		t.Run(testCase.goos+"/"+testCase.goarch, func(t *testing.T) {
			t.Parallel()

			context := build.Default
			context.GOOS, context.GOARCH = testCase.goos, testCase.goarch

			for _, name := range hermesSmokeSources {
				if selected := matchesTarget(t, context, name); selected != testCase.supported {
					t.Errorf("%s selected = %t, want %t", name, selected, testCase.supported)
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

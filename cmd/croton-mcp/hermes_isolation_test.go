//go:build linux || darwin

package main

import (
	"slices"
	"strings"
	"testing"
)

// TestHermesSmokeRequiredReadsEnvironment pins the one switch that decides
// whether a missing Hermes CLI is an acceptable skip or a CI failure. A native
// macOS runner that is supposed to prove catalog discovery must fail loudly
// when Hermes is absent, because a skipped test looks exactly like a passing
// one in a CI summary.
func TestHermesSmokeRequiredReadsEnvironment(t *testing.T) {
	cases := []struct {
		value    string
		required bool
	}{
		{value: "", required: false},
		{value: "0", required: false},
		{value: "false", required: false},
		{value: "1", required: true},
		{value: "  1  ", required: true},
	}
	for _, testCase := range cases {
		t.Run("value="+testCase.value, func(t *testing.T) {
			t.Setenv(hermesRequiredEnvironmentVariable, testCase.value)

			if required := hermesSmokeRequired(); required != testCase.required {
				t.Fatalf("hermesSmokeRequired() = %t for %q, want %t", required, testCase.value, testCase.required)
			}
		})
	}
}

// TestHermesEnvironmentIsolatesFromInheritedProfile is the guarantee the whole
// smoke rests on: every Hermes invocation must be pinned to the test's own
// throwaway profile. An inherited HERMES_HOME that survived into the child
// environment would point the add/test/remove sequence at a developer's real
// profile, so the override must replace it rather than sit beside it.
func TestHermesEnvironmentIsolatesFromInheritedProfile(t *testing.T) {
	t.Parallel()

	const isolated = "/tmp/croton-isolated-profile"

	base := []string{
		"PATH=/usr/bin",
		"HERMES_HOME=/home/someone/.hermes",
		"PYTHONPATH=/opt/injected",
		"PYTHONHOME=/opt/injected",
	}

	environment := hermesEnvironment(base, isolated)

	var homes []string
	for _, entry := range environment {
		if name, value, _ := strings.Cut(entry, "="); name == "HERMES_HOME" {
			homes = append(homes, value)
		}
	}
	if !slices.Equal(homes, []string{isolated}) {
		t.Fatalf("HERMES_HOME entries = %v, want exactly [%s]", homes, isolated)
	}

	for _, name := range []string{"PYTHONPATH=", "PYTHONHOME="} {
		if slices.ContainsFunc(environment, func(entry string) bool { return strings.HasPrefix(entry, name) }) {
			t.Errorf("inherited %s reached the Hermes child environment: %v", name, environment)
		}
	}
	if !slices.Contains(environment, "PATH=/usr/bin") {
		t.Fatalf("environment dropped unrelated inherited variables: %v", environment)
	}
}

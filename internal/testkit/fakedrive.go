// Copyright 2026 Ko
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package testkit

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
)

var fakeDriveBuild struct {
	sync.Mutex
}

// FakeDrive installs a credential-free proton-drive stand-in. Scenario selects a
// deterministic failure mode; stdout is optional golden JSON written beside the
// executable. The returned path is always absolute.
func FakeDrive(t *testing.T, scenario string, stdout []byte) string {
	t.Helper()

	directory := t.TempDir()
	path := filepath.Join(directory, "proton-drive")

	fakeDriveBuild.Lock()
	command := exec.Command("go", "build", "-o", path, "./internal/testkit/fakedrive")
	command.Dir = moduleRoot(t)
	command.Stderr = os.Stderr
	err := command.Run()
	fakeDriveBuild.Unlock()
	if err != nil {
		t.Fatalf("build fake proton-drive: %v", err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatalf("chmod fake proton-drive: %v", err)
	}

	if scenario != "" {
		if err := os.WriteFile(filepath.Join(directory, "scenario"), []byte(scenario+"\n"), 0o600); err != nil {
			t.Fatalf("write fake-drive scenario: %v", err)
		}
	}
	if len(stdout) > 0 {
		if err := os.WriteFile(filepath.Join(directory, "stdout.json"), stdout, 0o600); err != nil {
			t.Fatalf("write fake-drive stdout: %v", err)
		}
	}

	return path
}

// RecordedArgv returns the argv the fake CLI last observed, one argument per line.
func RecordedArgv(t *testing.T, binaryPath string) string {
	t.Helper()

	contents, err := os.ReadFile(filepath.Join(filepath.Dir(binaryPath), "argv"))
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}

	return string(contents)
}

func moduleRoot(t *testing.T) string {
	t.Helper()

	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("working directory: %v", err)
	}

	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory
		}

		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("module root not found")
		}

		directory = parent
	}
}

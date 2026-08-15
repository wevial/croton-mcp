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

package drivecli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

func fakeDriveBinary(t *testing.T) string {
	t.Helper()

	return installFakeDrive(t, "")
}

func installFakeDrive(t *testing.T, scenario string) string {
	t.Helper()

	return installFakeDriveFixture(t, scenario, "")
}

func installFakeDriveFixture(t *testing.T, scenario, fixture string) string {
	t.Helper()

	var stdout []byte
	if fixture != "" {
		contents, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatalf("read golden fixture %s: %v", fixture, err)
		}

		stdout = contents
	}

	return testkit.FakeDrive(t, scenario, stdout)
}

func recordedArgv(t *testing.T, binaryPath string) string {
	t.Helper()

	return testkit.RecordedArgv(t, binaryPath)
}

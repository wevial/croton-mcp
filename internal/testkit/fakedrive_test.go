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

package testkit_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestFakeDriveRecordsExactArgvBesideTheInstalledBinary(t *testing.T) {
	t.Parallel()

	path := testkit.FakeDrive(t, "", []byte("[\n\n]\n"))
	if !filepath.IsAbs(path) {
		t.Fatalf("FakeDrive path %q is not absolute", path)
	}

	command := exec.Command(path, "filesystem", "list", "/my-files/report 2026", "--json")
	command.Env = []string{"PATH=/usr/bin:/bin"}
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("run fake drive: %v (%s)", err, output)
	}

	if got := testkit.RecordedArgv(t, path); got != "filesystem\nlist\n/my-files/report 2026\n--json\n" {
		t.Fatalf("recorded argv = %q", got)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(path), "argv")); err != nil {
		t.Fatalf("argv sidecar: %v", err)
	}
}

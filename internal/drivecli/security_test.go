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
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/drivecli"
)

func TestInvokeDoesNotInheritParentSecrets(t *testing.T) {
	t.Setenv("CROTON_PARENT_SECRET", "session-material-must-not-leak@account.test")
	t.Setenv("PROTON_DRIVE_CREDENTIALS_STORE", "unsafe_file")

	binary := installFakeDrive(t, "environment")
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Handshake(context.Background()); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "env-leak"))
	if err != nil {
		t.Fatalf("read env-leak: %v", err)
	}
	if leaked := strings.TrimSpace(string(contents)); leaked != "absent" {
		t.Fatalf("parent secret reached subprocess: %q", leaked)
	}
}

func TestSubprocessRunsInExplicitNeutralWorkingDirectory(t *testing.T) {
	t.Parallel()

	binary := installFakeDrive(t, "working-directory")
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := client.Handshake(context.Background()); err != nil {
		t.Fatalf("Handshake: %v", err)
	}

	contents, err := os.ReadFile(filepath.Join(filepath.Dir(binary), "cwd"))
	if err != nil {
		t.Fatalf("read recorded cwd: %v", err)
	}

	recorded := strings.TrimSpace(string(contents))
	serverDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if recorded == serverDirectory {
		t.Fatalf("subprocess inherited server working directory %q", recorded)
	}
	if recorded != "/" {
		t.Fatalf("subprocess working directory = %q, want %q", recorded, "/")
	}
}

func TestNewRejectsUnsafeCredentialStore(t *testing.T) {
	t.Parallel()

	_, err := drivecli.New(drivecli.Options{
		BinaryPath:       "/opt/proton-drive/proton-drive",
		CredentialsStore: "unsafe_file",
	})
	if drivecli.CodeOf(err) != drivecli.CodeInvalidConfig {
		t.Fatalf("unsafe store error = %v, want %q", err, drivecli.CodeInvalidConfig)
	}
}

func TestListKeepsPathAsSingleArgvElement(t *testing.T) {
	t.Parallel()

	binary := installFakeDriveFixture(t, "", "list-my-files.json")
	client, err := drivecli.New(drivecli.Options{BinaryPath: binary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	path := "/my-files/report 2026; rm -rf /"
	if _, err := client.List(context.Background(), path, ""); err != nil {
		t.Fatalf("List: %v", err)
	}

	argv := recordedArgv(t, binary)
	if argv != "filesystem\nlist\n"+path+"\n--json\n" {
		t.Fatalf("argv = %q", argv)
	}
}

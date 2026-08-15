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
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/internal/drivecli"
)

func TestListDecodesFrozenRootAndNodeSchemas(t *testing.T) {
	t.Parallel()

	rootBinary := installFakeDriveFixture(t, "", "list-root.json")
	client, err := drivecli.New(drivecli.Options{BinaryPath: rootBinary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	root, err := client.List(context.Background(), "/", "")
	if err != nil {
		t.Fatalf("List /: %v", err)
	}
	if len(root.Sections) != 5 || root.Sections[0].Path != "/my-files" {
		t.Fatalf("root list = %#v", root)
	}
	if got := recordedArgv(t, rootBinary); got != "filesystem\nlist\n/\n--json\n" {
		t.Fatalf("root argv = %q", got)
	}

	filesBinary := installFakeDriveFixture(t, "", "list-my-files.json")
	filesClient, err := drivecli.New(drivecli.Options{BinaryPath: filesBinary})
	if err != nil {
		t.Fatalf("New files client: %v", err)
	}

	files, err := filesClient.List(context.Background(), "/my-files", "file")
	if err != nil {
		t.Fatalf("List /my-files: %v", err)
	}
	if len(files.Nodes) != 1 || files.Nodes[0].UID != "node:file-1" || files.Nodes[0].Type != "file" {
		t.Fatalf("node list = %#v", files)
	}
	if got := recordedArgv(t, filesBinary); got != "filesystem\nlist\n/my-files\n--type\nfile\n--json\n" {
		t.Fatalf("typed list argv = %q", got)
	}

	devicesBinary := installFakeDriveFixture(t, "", "list-devices.json")
	devicesClient, err := drivecli.New(drivecli.Options{BinaryPath: devicesBinary})
	if err != nil {
		t.Fatalf("New devices client: %v", err)
	}

	devices, err := devicesClient.List(context.Background(), "/devices", "")
	if err != nil {
		t.Fatalf("List /devices: %v", err)
	}
	if len(devices.Devices) != 1 || devices.Devices[0].Type != "Linux" {
		t.Fatalf("device list = %#v", devices)
	}

	emptyBinary := installFakeDriveFixture(t, "", "list-empty.json")
	emptyClient, err := drivecli.New(drivecli.Options{BinaryPath: emptyBinary})
	if err != nil {
		t.Fatalf("New empty client: %v", err)
	}

	empty, err := emptyClient.List(context.Background(), "/my-files", "")
	if err != nil {
		t.Fatalf("empty List: %v", err)
	}
	if len(empty.Nodes) != 0 || empty.Sections != nil || empty.Devices != nil {
		t.Fatalf("empty list = %#v", empty)
	}
}

func TestInfoSharingAndDownloadDecodeFrozenObjects(t *testing.T) {
	t.Parallel()

	infoBinary := installFakeDriveFixture(t, "", "info-folder.json")
	client, err := drivecli.New(drivecli.Options{BinaryPath: infoBinary})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	node, err := client.Info(context.Background(), "/my-files/Reports")
	if err != nil {
		t.Fatalf("Info: %v", err)
	}
	if node.Type != "folder" || node.Name.Value != "Reports" || node.KeyAuthor.Value != nil {
		t.Fatalf("info node = %#v", node)
	}

	shareBinary := installFakeDriveFixture(t, "", "sharing-status.json")
	shareClient, err := drivecli.New(drivecli.Options{BinaryPath: shareBinary})
	if err != nil {
		t.Fatalf("New share client: %v", err)
	}

	status, err := shareClient.SharingStatus(context.Background(), "/my-files/Reports")
	if err != nil {
		t.Fatalf("SharingStatus: %v", err)
	}
	if !status.Shared || status.Info == nil || status.Info.URLAccess == nil {
		t.Fatalf("sharing status = %#v", status)
	}

	unsharedBinary := installFakeDrive(t, "unshared")
	unsharedClient, err := drivecli.New(drivecli.Options{BinaryPath: unsharedBinary})
	if err != nil {
		t.Fatalf("New unshared client: %v", err)
	}

	unshared, err := unsharedClient.SharingStatus(context.Background(), "/my-files/notes.txt")
	if err != nil {
		t.Fatalf("unshared SharingStatus: %v", err)
	}
	if unshared.Shared || unshared.Info != nil {
		t.Fatalf("unshared status = %#v", unshared)
	}

	downloadBinary := installFakeDriveFixture(t, "", "download-summary.json")
	downloadClient, err := drivecli.New(drivecli.Options{BinaryPath: downloadBinary})
	if err != nil {
		t.Fatalf("New download client: %v", err)
	}

	summary, err := downloadClient.Download(context.Background(), []string{"/my-files/notes.txt"}, "/tmp/croton-fixture")
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if summary.TransferredItems != 1 || summary.FailedItems != 0 {
		t.Fatalf("download summary = %#v", summary)
	}
	if got := recordedArgv(t, downloadBinary); !strings.Contains(got, "--file-conflict-strategy\nskip\n--folder-conflict-strategy\nskip\n--json\n") {
		t.Fatalf("download argv = %q", got)
	}
}

func TestDownloadRequiresAbsoluteLocalFolder(t *testing.T) {
	t.Parallel()

	client, err := drivecli.New(drivecli.Options{BinaryPath: "/nonexistent/proton-drive"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, localFolder := range []string{"", "downloads", "./downloads", "../downloads", "downloads/2026"} {
		_, err := client.Download(context.Background(), []string{"/my-files/notes.txt"}, localFolder)
		if drivecli.CodeOf(err) != drivecli.CodeInvalidConfig {
			t.Fatalf("Download(localFolder=%q) error = %v, want %q", localFolder, err, drivecli.CodeInvalidConfig)
		}
	}
}

func TestListRejectsUnknownFieldsFromFrozenSchema(t *testing.T) {
	t.Parallel()

	client, err := drivecli.New(drivecli.Options{BinaryPath: installFakeDrive(t, "unknown-field")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	_, err = client.List(context.Background(), "/", "")
	if drivecli.CodeOf(err) != drivecli.CodeMalformedOutput {
		t.Fatalf("unknown-field list error = %v, want %q", err, drivecli.CodeMalformedOutput)
	}
}

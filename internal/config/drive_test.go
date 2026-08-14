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

package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDriveParsesSeparateReadOnlyConfig(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"cli": {"binaryPath": "/opt/proton-drive/proton-drive"},
		"allowedDownloadDirectories": ["/srv/downloads"]
	}`)

	loaded, err := LoadDrive(path)
	if err != nil {
		t.Fatalf("LoadDrive: %v", err)
	}
	if loaded.CLI.BinaryPath != "/opt/proton-drive/proton-drive" {
		t.Fatalf("binary path = %q", loaded.CLI.BinaryPath)
	}
	if len(loaded.AllowedDownloadDirectories) != 1 || loaded.AllowedDownloadDirectories[0] != "/srv/downloads" {
		t.Fatalf("download allowlist = %#v", loaded.AllowedDownloadDirectories)
	}
	if loaded.Writes.Enabled {
		t.Fatal("omitted writes policy must default to disabled")
	}
}

func TestLoadDriveRejectsNonAbsoluteDecodedPaths(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name   string
		config string
	}{
		{"empty binary path", `{"cli":{"binaryPath":""}}`},
		{"relative binary path", `{"cli":{"binaryPath":"proton-drive"}}`},
		{"empty download directory", `{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"},"allowedDownloadDirectories":[""]}`},
		{"relative download directory", `{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"},"allowedDownloadDirectories":["downloads"]}`},
		{"empty later download directory", `{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"},"allowedDownloadDirectories":["/srv/downloads",""]}`},
		{"relative later download directory", `{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"},"allowedDownloadDirectories":["/srv/downloads","downloads"]}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if _, err := LoadDrive(writeConfig(t, testCase.config)); err != ErrConfigInvalid {
				t.Fatalf("LoadDrive error = %v, want %v", err, ErrConfigInvalid)
			}
		})
	}
}

func TestLoadDriveAcceptsEmptyDownloadAllowlist(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{
		"cli": {"binaryPath": "/opt/proton-drive/proton-drive"},
		"allowedDownloadDirectories": [],
		"writes": {"enabled": true}
	}`)

	loaded, err := LoadDrive(path)
	if err != nil {
		t.Fatalf("LoadDrive: %v", err)
	}
	if len(loaded.AllowedDownloadDirectories) != 0 {
		t.Fatalf("download allowlist = %#v, want empty", loaded.AllowedDownloadDirectories)
	}
	if !loaded.Writes.Enabled {
		t.Fatal("writes policy was not preserved")
	}
}

func TestLoadDriveUsesTheSecureLoaderAndStrictSchema(t *testing.T) {
	t.Parallel()

	path := writeConfig(t, `{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"},"unexpected":true}`)
	if _, err := LoadDrive(path); err != ErrConfigInvalid {
		t.Fatalf("unknown-field error = %v, want %v", err, ErrConfigInvalid)
	}

	if _, err := LoadDrive("croton-drive.json"); err != ErrConfigUnreadable {
		t.Fatalf("relative-path error = %v, want %v", err, ErrConfigUnreadable)
	}

	root := canonicalTempDir(t)
	realDirectory := filepath.Join(root, "drive-real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatalf("create real directory: %v", err)
	}
	configPath := filepath.Join(realDirectory, "croton-drive.json")
	if err := os.WriteFile(configPath, []byte(`{"cli":{"binaryPath":"/opt/proton-drive/proton-drive"}}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	linkedDirectory := filepath.Join(root, "drive-linked")
	if err := os.Symlink(realDirectory, linkedDirectory); err != nil {
		t.Fatalf("create symlink: %v", err)
	}
	if _, err := LoadDrive(filepath.Join(linkedDirectory, "croton-drive.json")); err != ErrConfigUnreadable {
		t.Fatalf("symlink-parent error = %v, want %v", err, ErrConfigUnreadable)
	}
}

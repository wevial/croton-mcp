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
	"strings"
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

func TestDriveDownloadPolicyDefaults(t *testing.T) {
	t.Parallel()

	for name, suffix := range map[string]string{
		"omitted":        "",
		"empty":          `,"download":{}`,
		"explicit-false": `,"download":{"enabled":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			loaded, err := LoadDrive(writeConfig(t, `{"cli":{"binaryPath":"/opt/drive.test"}`+suffix+`}`))
			if err != nil {
				t.Fatalf("LoadDrive: %v", err)
			}
			if got := loaded.Download; got.Enabled || got.MaxBytes != 256*1024*1024 || got.TimeoutSeconds != 120 {
				t.Fatalf("download policy = %+v, want disabled with default limits", got)
			}
		})
	}
}

func TestDriveDownloadPolicyLimits(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, policy      string
		maxBytes, timeout int64
	}{
		{"positive", `{"maxBytes":4096,"timeoutSeconds":30}`, 4096, 30},
		{"maximum", `{"maxBytes":9223372036854775807,"timeoutSeconds":9223372036}`, 9223372036854775807, 9223372036},
		{"bytes-only", `{"maxBytes":1}`, 1, 120},
		{"timeout-only", `{"timeoutSeconds":1}`, 256 * 1024 * 1024, 1},
		{"bytes-zero", `{"maxBytes":0}`, 0, 0},
		{"bytes-negative", `{"maxBytes":-1}`, 0, 0},
		{"bytes-fractional", `{"maxBytes":1.5}`, 0, 0},
		{"bytes-int64-overflow", `{"maxBytes":9223372036854775808}`, 0, 0},
		{"timeout-zero", `{"timeoutSeconds":0}`, 0, 0},
		{"timeout-negative", `{"timeoutSeconds":-1}`, 0, 0},
		{"timeout-fractional", `{"timeoutSeconds":1.5}`, 0, 0},
		{"timeout-int64-overflow", `{"timeoutSeconds":9223372036854775808}`, 0, 0},
		{"duration-conversion-overflow", `{"timeoutSeconds":9223372037}`, 0, 0},
		{"bytes-string", `{"maxBytes":"1"}`, 0, 0},
		{"timeout-string", `{"timeoutSeconds":"1"}`, 0, 0},
		{"bytes-null", `{"maxBytes":null}`, 0, 0},
		{"timeout-null", `{"timeoutSeconds":null}`, 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			loaded, err := LoadDrive(writeConfig(t, `{"cli":{"binaryPath":"/opt/drive.test"},"download":`+tc.policy+`}`))
			if tc.maxBytes == 0 {
				if err != ErrConfigInvalid {
					t.Fatalf("error = %v, want ErrConfigInvalid", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadDrive: %v", err)
			}
			if got := loaded.Download; got.Enabled || got.MaxBytes != tc.maxBytes || got.TimeoutSeconds != tc.timeout {
				t.Fatalf("download policy = %+v, want disabled, %d bytes, %d seconds", got, tc.maxBytes, tc.timeout)
			}
		})
	}
}

func TestDriveDownloadPolicyDisabledUntilRegistration(t *testing.T) {
	t.Parallel()

	const secret = "synthetic-secret-user@download.test-token"
	for name, enabled := range map[string]string{"enabled": "true", "wrong-type": `"` + secret + `"`} {
		for rootsName, roots := range map[string]string{"empty-roots": `[]`, "allowed-root": `["/srv/download.test"]`} {
			t.Run(name+"/"+rootsName, func(t *testing.T) {
				t.Parallel()

				_, err := LoadDrive(writeConfig(t, `{"cli":{"binaryPath":"/opt/`+secret+`"},"allowedDownloadDirectories":`+roots+`,"download":{"enabled":`+enabled+`}}`))
				if err != ErrConfigInvalid {
					t.Fatal("expected static ErrConfigInvalid without input text")
				}
				if strings.Contains(err.Error(), secret) {
					t.Fatal("error exposed synthetic secret")
				}
			})
		}
	}
}

func TestDriveDownloadPolicyStrictKeys(t *testing.T) {
	t.Parallel()

	for name, policy := range map[string]string{
		"unknown":                    `{"unexpected":1}`,
		"duplicate-enabled":          `{"enabled":false,"enabled":false}`,
		"duplicate-maxBytes":         `{"maxBytes":1,"maxBytes":2}`,
		"duplicate-timeoutSeconds":   `{"timeoutSeconds":1,"timeoutSeconds":2}`,
		"case-folded-alias":          `{"enabled":false,"Enabled":false}`,
		"case-folded-enabled":        `{"Enabled":false}`,
		"case-folded-maxBytes":       `{"MaxBytes":1}`,
		"case-folded-timeoutSeconds": `{"TimeoutSeconds":1}`,
		"null":                       `null`,
		"array":                      `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			if _, err := LoadDrive(writeConfig(t, `{"cli":{"binaryPath":"/opt/drive.test"},"download":`+policy+`}`)); err != ErrConfigInvalid {
				t.Fatalf("error = %v, want ErrConfigInvalid", err)
			}
		})
	}
}

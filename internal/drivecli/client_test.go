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
	"testing"

	"github.com/wevial/croton-mcp/internal/drivecli"
)

func TestNewRejectsNonAbsoluteBinaryPath(t *testing.T) {
	t.Parallel()

	for _, path := range []string{"", "proton-drive", "./proton-drive", "usr/bin/proton-drive"} {
		t.Run(path, func(t *testing.T) {
			t.Parallel()

			_, err := drivecli.New(drivecli.Options{BinaryPath: path})
			if drivecli.CodeOf(err) != drivecli.CodeInvalidConfig {
				t.Fatalf("New(%q) error = %v, want %q", path, err, drivecli.CodeInvalidConfig)
			}
		})
	}
}

func TestHandshakeAcceptsPinnedOfficialVersion(t *testing.T) {
	t.Parallel()

	client, err := drivecli.New(drivecli.Options{BinaryPath: fakeDriveBinary(t)})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if err := client.Handshake(context.Background()); err != nil {
		t.Fatalf("Handshake: %v", err)
	}
}

func TestHandshakeRejectsNonExactVersionBanners(t *testing.T) {
	t.Parallel()

	for _, scenario := range []string{
		"prerelease-version",
		"fork-version",
		"extra-component-version",
		"embedded-version",
		"trailing-garbage-version",
	} {
		t.Run(scenario, func(t *testing.T) {
			t.Parallel()

			client, err := drivecli.New(drivecli.Options{BinaryPath: installFakeDrive(t, scenario)})
			if err != nil {
				t.Fatalf("New: %v", err)
			}

			err = client.Handshake(context.Background())
			if drivecli.CodeOf(err) != drivecli.CodeMalformedOutput {
				t.Fatalf("Handshake error = %v, want %q", err, drivecli.CodeMalformedOutput)
			}
		})
	}
}

func TestHandshakeRefusesVersionMismatch(t *testing.T) {
	t.Parallel()

	client, err := drivecli.New(drivecli.Options{BinaryPath: installFakeDrive(t, "version-mismatch")})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	err = client.Handshake(context.Background())
	if drivecli.CodeOf(err) != drivecli.CodeVersionMismatch {
		t.Fatalf("Handshake error = %v, want %q", err, drivecli.CodeVersionMismatch)
	}
	if err != nil && err.Error() != drivecli.CodeVersionMismatch {
		t.Fatalf("version mismatch error leaked detail: %q", err)
	}
}

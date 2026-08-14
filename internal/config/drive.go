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

import "path/filepath"

// DriveConfig is Croton Drive's untrusted, process-local configuration. The
// Drive executable has no relationship to the Mail Bridge configuration.
type DriveConfig struct {
	CLI                        DriveCLIConfig `json:"cli"`
	AllowedDownloadDirectories []string       `json:"allowedDownloadDirectories"`
	Writes                     DriveWrites    `json:"writes"`
}

// DriveCLIConfig reserves the operator-selected absolute CLI path for the
// subprocess adapter. This scaffold never executes it.
type DriveCLIConfig struct {
	BinaryPath string `json:"binaryPath"`
}

// DriveWrites retains an explicit future policy boundary. Writes are disabled
// by Go's zero value and no write-capable operation is registered in this server.
type DriveWrites struct {
	Enabled bool `json:"enabled"`
}

// LoadDrive reads Croton Drive configuration through the same secure loader as
// the Mail executable. Linux and macOS use descriptor-relative no-follow
// traversal; every other platform fails closed in openSecure.
func LoadDrive(path string) (DriveConfig, error) {
	var loaded DriveConfig
	if err := load(path, &loaded); err != nil {
		return DriveConfig{}, err
	}

	if !filepath.IsAbs(loaded.CLI.BinaryPath) {
		return DriveConfig{}, ErrConfigInvalid
	}
	for _, directory := range loaded.AllowedDownloadDirectories {
		if !filepath.IsAbs(directory) {
			return DriveConfig{}, ErrConfigInvalid
		}
	}

	return loaded, nil
}

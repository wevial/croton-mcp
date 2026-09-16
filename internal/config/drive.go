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
	"encoding/json"
	"math"
	"path/filepath"
	"time"

	"github.com/wevial/croton-mcp/internal/strictjson"
)

// DriveConfig is Croton Drive's untrusted, process-local configuration. The
// Drive executable has no relationship to the Mail Bridge configuration.
type DriveConfig struct {
	CLI                        DriveCLIConfig `json:"cli"`
	AllowedDownloadDirectories []string       `json:"allowedDownloadDirectories"`
	Download                   DriveDownload  `json:"download"`
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

// DriveDownload is the reserved download policy. Parsing it does not register
// a download tool or enable runtime enforcement of its limits.
type DriveDownload struct {
	Enabled        bool  `json:"enabled"`
	MaxBytes       int64 `json:"maxBytes"`
	TimeoutSeconds int64 `json:"timeoutSeconds"`
}

// UnmarshalJSON requires exact policy keys, including their case. The secure
// loader also validates the enclosing document before decoding this object.
func (policy *DriveDownload) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if !strictjson.DecodeObject(data, maxConfigBytes, &fields) {
		return ErrConfigInvalid
	}

	for key, value := range fields {
		var target any
		switch key {
		case "enabled":
			target = &policy.Enabled
		case "maxBytes":
			target = &policy.MaxBytes
		case "timeoutSeconds":
			target = &policy.TimeoutSeconds
		default:
			return ErrConfigInvalid
		}

		if err := json.Unmarshal(value, target); err != nil {
			return ErrConfigInvalid
		}
	}

	return nil
}

// LoadDrive reads Croton Drive configuration through the same secure loader as
// the Mail executable. Linux and macOS use descriptor-relative no-follow
// traversal; every other platform fails closed in openSecure.
func LoadDrive(path string) (DriveConfig, error) {
	// Seed defaults before decoding so explicit zero remains invalid.
	loaded := DriveConfig{
		Download: DriveDownload{MaxBytes: 256 * 1024 * 1024, TimeoutSeconds: 120},
	}
	if err := load(path, &loaded); err != nil {
		return DriveConfig{}, err
	}

	if loaded.Download.Enabled || loaded.Download.MaxBytes <= 0 ||
		loaded.Download.TimeoutSeconds <= 0 || loaded.Download.TimeoutSeconds > math.MaxInt64/int64(time.Second) {
		return DriveConfig{}, ErrConfigInvalid
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

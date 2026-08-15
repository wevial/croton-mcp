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

package drivecli

import "errors"

const (
	// CodeInvalidConfig is returned when adapter options are unusable.
	CodeInvalidConfig = "invalid_config"
	// CodeVersionMismatch is returned when the CLI is not the pinned 0.8.0 release.
	CodeVersionMismatch = "version_mismatch"
	// CodeTimedOut is returned when a subprocess exceeds its deadline.
	CodeTimedOut = "command_timed_out"
	// CodeOutputOverflow is returned when stdout or stderr exceeds the byte cap.
	CodeOutputOverflow = "output_too_large"
	// CodeMalformedOutput is returned when JSON or version text cannot be decoded.
	CodeMalformedOutput = "malformed_output"
	// CodeTruncatedOutput is returned when captured output is incomplete.
	CodeTruncatedOutput = "truncated_output"
	// CodeCommandFailed is returned for a non-zero one-shot exit.
	CodeCommandFailed = "command_failed"
	// CodeAuthRequired is returned for the stable unauthenticated literal.
	CodeAuthRequired = "auth_required"
	// CodeCanceled is returned when the caller context is canceled.
	CodeCanceled = "operation_canceled"
)

// Error is a stable, secret-safe adapter failure.
type Error struct {
	Code string
}

// Error returns only the stable code.
func (err *Error) Error() string {
	return err.Code
}

func errorCode(code string) error {
	return &Error{Code: code}
}

// CodeOf returns the stable adapter code, or the empty string.
func CodeOf(err error) string {
	var adapterError *Error
	if errors.As(err, &adapterError) {
		return adapterError.Code
	}

	return ""
}

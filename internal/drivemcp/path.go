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

package drivemcp

import (
	"strings"
	"unicode/utf8"
)

// maxDrivePathBytes bounds every remote path argument before it can reach the
// subprocess adapter as one argv operand.
const maxDrivePathBytes = 1024

// validDrivePath accepts only one canonical absolute Drive path form: "/" or
// "/"-joined non-empty segments. Traversal or self segments, empty segments,
// trailing separators, control characters, and invalid UTF-8 fail closed, so
// an accepted path is never flag-shaped and never renames a different node.
func validDrivePath(path string) bool {
	if path == "" || len(path) > maxDrivePathBytes || !utf8.ValidString(path) {
		return false
	}
	for _, character := range path {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	if path[0] != '/' {
		return false
	}
	if path == "/" {
		return true
	}
	if strings.HasSuffix(path, "/") {
		return false
	}

	for _, segment := range strings.Split(path[1:], "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}

	return true
}

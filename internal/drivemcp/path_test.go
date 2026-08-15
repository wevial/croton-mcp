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
	"testing"
)

func TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths(t *testing.T) {
	t.Parallel()

	longest := "/" + strings.Repeat("a", maxDrivePathBytes-1)
	overlong := "/" + strings.Repeat("a", maxDrivePathBytes)

	cases := []struct {
		name string
		path string
		want bool
	}{
		{"root", "/", true},
		{"top level section", "/my-files", true},
		{"devices section", "/devices", true},
		{"nested folder", "/my-files/Reports", true},
		{"file with spaces", "/my files/2026 Q1 report.txt", true},
		{"unicode name", "/my-files/日本語ノート.txt", true},
		{"dash inside segment", "/my-files/-draft.txt", true},
		{"dot inside segment", "/my-files/notes.v2.txt", true},
		{"longest allowed path", longest, true},

		{"empty", "", false},
		{"relative name", "my-files", false},
		{"relative dot", "./my-files", false},
		{"relative parent", "../my-files", false},
		{"bare dash", "-", false},
		{"flag shaped", "--json", false},
		{"root parent traversal", "/..", false},
		{"root self segment", "/.", false},
		{"trailing parent traversal", "/my-files/..", false},
		{"trailing self segment", "/my-files/.", false},
		{"inner parent traversal", "/my-files/../shared-by-me", false},
		{"inner self segment", "/my-files/./Reports", false},
		{"double slash root", "//", false},
		{"empty inner segment", "/my-files//Reports", false},
		{"trailing slash", "/my-files/", false},
		{"leading space", " /my-files", false},
		{"embedded NUL", "/my-files/a\x00b", false},
		{"embedded newline", "/my-files/a\nb", false},
		{"embedded carriage return", "/my-files/a\rb", false},
		{"embedded tab", "/my-files/a\tb", false},
		{"embedded escape", "/my-files/a\x1bb", false},
		{"embedded delete", "/my-files/a\x7fb", false},
		{"invalid utf-8", "/my-files/a\xffb", false},
		{"overlong path", overlong, false},
	}

	for _, testCase := range cases {
		if got := validDrivePath(testCase.path); got != testCase.want {
			t.Errorf("%s: validDrivePath(%q) = %v, want %v", testCase.name, testCase.path, got, testCase.want)
		}
	}
}

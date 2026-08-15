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

import (
	"context"
	"testing"
)

func TestIsAllowlistedInvocationMatchesExactArgvShapes(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name      string
		arguments []string
		want      bool
	}{
		{name: "version", arguments: []string{"version"}, want: true},
		{name: "list", arguments: []string{"filesystem", "list", "/my-files", "--json"}, want: true},
		{name: "list with file type", arguments: []string{"filesystem", "list", "/my-files", "--type", "file", "--json"}, want: true},
		{name: "list with folder type", arguments: []string{"filesystem", "list", "/my-files", "--type", "folder", "--json"}, want: true},
		{name: "info", arguments: []string{"filesystem", "info", "/my-files/notes.txt", "--json"}, want: true},
		{name: "sharing status", arguments: []string{"sharing", "status", "/my-files/notes.txt", "--json"}, want: true},
		{name: "download one remote path", arguments: []string{"filesystem", "download", "/my-files/notes.txt", "/tmp/dest", "--file-conflict-strategy", "skip", "--folder-conflict-strategy", "skip", "--json"}, want: true},
		{name: "download several remote paths", arguments: []string{"filesystem", "download", "/my-files/a", "/my-files/b", "/tmp/dest", "--file-conflict-strategy", "skip", "--folder-conflict-strategy", "skip", "--json"}, want: true},
		{name: "operand with spaces stays one slot", arguments: []string{"filesystem", "list", "/my-files/report 2026; rm -rf /", "--json"}, want: true},

		{name: "bare invocation", arguments: nil, want: false},
		{name: "empty argv", arguments: []string{}, want: false},
		{name: "version with extra flag", arguments: []string{"version", "--help"}, want: false},
		{name: "list with flag in path slot", arguments: []string{"filesystem", "list", "--type", "--json"}, want: false},
		{name: "list missing json flag", arguments: []string{"filesystem", "list", "/my-files"}, want: false},
		{name: "list with structure smuggled into operand", arguments: []string{"filesystem", "list", "/my-files --json"}, want: false},
		{name: "list with structure smuggled into type", arguments: []string{"filesystem", "list", "/my-files", "--type", "file --json"}, want: false},
		{name: "list with duplicated json flag", arguments: []string{"filesystem", "list", "/my-files", "--json", "--json"}, want: false},
		{name: "info with structure smuggled into operand", arguments: []string{"filesystem", "info", "/my-files --json"}, want: false},
		{name: "info with flag in path slot", arguments: []string{"filesystem", "info", "--json", "--json"}, want: false},
		{name: "info with trailing extra operand", arguments: []string{"filesystem", "info", "/my-files", "--json", "extra"}, want: false},
		{name: "download missing local folder slot", arguments: []string{"filesystem", "download", "/my-files/a", "--file-conflict-strategy", "skip", "--folder-conflict-strategy", "skip", "--json"}, want: false},
		{name: "download with structure smuggled into operand", arguments: []string{"filesystem", "download", "/my-files/a", "/tmp/dest --file-conflict-strategy skip --folder-conflict-strategy skip --json"}, want: false},
		{name: "download with overwrite strategy", arguments: []string{"filesystem", "download", "/my-files/a", "/tmp/dest", "--file-conflict-strategy", "skip", "--folder-conflict-strategy", "overwrite", "--json"}, want: false},
		{name: "download with reordered trailing flags", arguments: []string{"filesystem", "download", "/my-files/a", "/tmp/dest", "--folder-conflict-strategy", "skip", "--file-conflict-strategy", "skip", "--json"}, want: false},
		{name: "write-capable upload", arguments: []string{"filesystem", "upload", "/local/notes.txt", "/my-files", "--json"}, want: false},
		{name: "write-capable trash", arguments: []string{"filesystem", "trash", "/my-files/notes.txt", "--json"}, want: false},
		{name: "write-capable delete", arguments: []string{"filesystem", "delete", "/my-files/notes.txt", "--json"}, want: false},
		{name: "write-capable sharing invite", arguments: []string{"sharing", "invite", "/my-files/notes.txt", "--json"}, want: false},
		{name: "write-capable auth login", arguments: []string{"auth", "login"}, want: false},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			if got := isAllowlistedInvocation(testCase.arguments); got != testCase.want {
				t.Fatalf("isAllowlistedInvocation(%q) = %v, want %v", testCase.arguments, got, testCase.want)
			}
		})
	}
}

func TestInvokeEnforcesAllowlistBeforeExecution(t *testing.T) {
	t.Parallel()

	client, err := New(Options{BinaryPath: "/nonexistent/proton-drive"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, arguments := range [][]string{
		nil,
		{"auth", "login"},
		{"filesystem", "trash", "/my-files/notes.txt", "--json"},
		{"filesystem", "upload", "/local/notes.txt", "/my-files", "--json"},
		{"filesystem", "list", "--type", "--json"},
	} {
		if _, err := client.invoke(context.Background(), arguments, false); CodeOf(err) != CodeInvalidConfig {
			t.Fatalf("invoke(%q) error = %v, want %q before any execution", arguments, err, CodeInvalidConfig)
		}
	}
}

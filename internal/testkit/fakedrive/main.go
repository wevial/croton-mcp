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

// Command fakedrive is a credential-free Proton Drive CLI stand-in for tests.
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const fixtureSecret = "session-material-must-not-leak@account.test"

func main() {
	directory := filepath.Dir(os.Args[0])
	_ = os.WriteFile(filepath.Join(directory, "argv"), []byte(strings.Join(os.Args[1:], "\n")+"\n"), 0o600)

	scenarioBytes, _ := os.ReadFile(filepath.Join(directory, "scenario"))
	scenario := strings.TrimSpace(string(scenarioBytes))

	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "interactive shell is not available")
		os.Exit(2)
	}

	switch scenario {
	case "hang":
		time.Sleep(time.Hour)
	case "flood-stdout":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 1024))
		return
	case "flood-stderr":
		fmt.Fprint(os.Stderr, strings.Repeat("y", 1024))
		os.Exit(1)
	case "malformed-version":
		fmt.Println("not a version banner")
		return
	case "environment":
		leaked := "absent"
		if os.Getenv("CROTON_PARENT_SECRET") != "" || os.Getenv("PROTON_DRIVE_CREDENTIALS_STORE") == "unsafe_file" {
			leaked = "present"
		}
		_ = os.WriteFile(filepath.Join(directory, "env-leak"), []byte(leaked+"\n"), 0o600)
	case "working-directory":
		cwd, err := os.Getwd()
		if err != nil {
			cwd = "unavailable"
		}
		_ = os.WriteFile(filepath.Join(directory, "cwd"), []byte(cwd+"\n"), 0o600)
	}

	if os.Args[1] == "version" {
		switch scenario {
		case "version-mismatch":
			fmt.Println("Proton Drive CLI cli-drive@0.7.9")
			fmt.Println("Proton Drive SDK 0.0.0-fixture")
		case "prerelease-version":
			fmt.Println("Proton Drive CLI cli-drive@0.8.0-rc.1")
		case "fork-version":
			fmt.Println("Proton Drive CLI cli-drive@0.8.0-fork")
		case "extra-component-version":
			fmt.Println("Proton Drive CLI cli-drive@0.8.0.1")
		case "embedded-version":
			fmt.Println("A new update is available")
			fmt.Println("Proton Drive CLI cli-drive@0.8.0")
		case "trailing-garbage-version":
			fmt.Println("Proton Drive CLI cli-drive@0.8.0 and other words")
		default:
			fmt.Println("Proton Drive CLI cli-drive@0.8.0")
			fmt.Println("Proton Drive SDK 0.0.0-fixture")
			fmt.Println("You are running the latest version.")
		}
		return
	}

	switch scenario {
	case "nonzero-secret":
		fmt.Fprintf(os.Stderr, "download failed for %s password=hunter2\n", fixtureSecret)
		os.Exit(1)
	case "auth-required":
		fmt.Fprintln(os.Stderr, "You need to login first")
		os.Exit(1)
	case "truncated-list":
		fmt.Print("[\n{\"uid\":\"node:1\"")
		return
	case "malformed-list":
		fmt.Print("[\n{\"uid\":1}\n]\n")
		return
	case "unknown-field":
		fmt.Print("[\n{\"path\":\"/my-files\",\"surprise\":true}\n]\n")
		return
	case "unshared":
		fmt.Println("undefined")
		return
	}

	fixture := filepath.Join(directory, "stdout.json")
	if contents, err := os.ReadFile(fixture); err == nil {
		_, _ = os.Stdout.Write(contents)
		return
	}

	os.Exit(1)
}

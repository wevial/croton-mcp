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

// Package drivecli invokes one operator-configured Proton Drive CLI binary.
package drivecli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const (
	pinnedCLIVersion    = "0.8.0"
	versionBannerPrefix = "Proton Drive CLI cli-drive@"
	defaultTimeout      = 15 * time.Second
	defaultMaxBytes     = 64 * 1024
	waitDelay           = 100 * time.Millisecond

	// subprocessWorkingDirectory pins the child to a neutral, always-present
	// directory so it never depends on the server's working directory.
	subprocessWorkingDirectory = "/"
)

var exactSemanticVersionPattern = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// Options configures the subprocess adapter. BinaryPath must be absolute.
type Options struct {
	BinaryPath       string
	CacheDir         string
	CredentialsStore string
	Timeout          time.Duration
	MaxBytes         int
}

// Client is the fail-closed subprocess boundary around proton-drive.
type Client struct {
	binaryPath       string
	cacheDir         string
	credentialsStore string
	timeout          time.Duration
	maxBytes         int
}

// New validates adapter options without executing the binary.
func New(options Options) (*Client, error) {
	if !filepath.IsAbs(options.BinaryPath) {
		return nil, errorCode(CodeInvalidConfig)
	}
	if options.CacheDir != "" && !filepath.IsAbs(options.CacheDir) {
		return nil, errorCode(CodeInvalidConfig)
	}
	switch options.CredentialsStore {
	case "", "keychain", "pass":
	default:
		return nil, errorCode(CodeInvalidConfig)
	}
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" {
		return nil, errorCode(CodeInvalidConfig)
	}

	timeout := options.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}

	maxBytes := options.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}

	return &Client{
		binaryPath:       options.BinaryPath,
		cacheDir:         options.CacheDir,
		credentialsStore: options.CredentialsStore,
		timeout:          timeout,
		maxBytes:         maxBytes,
	}, nil
}

// Handshake runs `version` and refuses any CLI other than the pinned 0.8.0 release.
func (client *Client) Handshake(ctx context.Context) error {
	stdout, err := client.invoke(ctx, []string{"version"}, false)
	if err != nil {
		return err
	}

	version, ok := parseVersionBanner(stdout)
	if !ok {
		return errorCode(CodeMalformedOutput)
	}
	if version != pinnedCLIVersion {
		return errorCode(CodeVersionMismatch)
	}

	return nil
}

// parseVersionBanner reads only a complete first banner line of the frozen
// contract; suffixed, forked, extended, or embedded versions fail closed.
func parseVersionBanner(stdout []byte) (string, bool) {
	firstLine, _, _ := strings.Cut(string(stdout), "\n")
	firstLine = strings.TrimSuffix(firstLine, "\r")

	version, found := strings.CutPrefix(firstLine, versionBannerPrefix)
	if !found || !exactSemanticVersionPattern.MatchString(version) {
		return "", false
	}

	return version, true
}

func (client *Client) invoke(ctx context.Context, arguments []string, requireJSON bool) ([]byte, error) {
	if !isAllowlistedInvocation(arguments) {
		return nil, errorCode(CodeInvalidConfig)
	}

	deadline, cancel := context.WithTimeout(ctx, client.timeout)
	defer cancel()

	stdout := &limitedBuffer{limit: client.maxBytes}
	stderr := &limitedBuffer{limit: client.maxBytes}
	process := exec.CommandContext(deadline, client.binaryPath, arguments...)
	configureProcess(process)
	process.Dir = subprocessWorkingDirectory
	process.Stdin = bytes.NewReader(nil)
	process.Stdout = stdout
	process.Stderr = stderr
	process.Env = isolatedEnvironment(client.cacheDir, client.credentialsStore)
	process.WaitDelay = waitDelay

	err := process.Run()

	if stdout.overflow || stderr.overflow {
		return nil, errorCode(CodeOutputOverflow)
	}
	if deadline.Err() != nil {
		if ctx.Err() == context.Canceled {
			return nil, errorCode(CodeCanceled)
		}
		if deadline.Err() == context.DeadlineExceeded {
			return nil, errorCode(CodeTimedOut)
		}
	}
	if err != nil {
		if strings.Contains(string(stderr.bytes), "You need to login first") {
			return nil, errorCode(CodeAuthRequired)
		}

		return nil, errorCode(CodeCommandFailed)
	}
	if requireJSON && looksTruncated(stdout.bytes) {
		return nil, errorCode(CodeTruncatedOutput)
	}

	return append([]byte(nil), stdout.bytes...), nil
}

func looksTruncated(output []byte) bool {
	trimmed := bytes.TrimSpace(output)
	if len(trimmed) == 0 {
		return true
	}
	if trimmed[0] == '[' {
		return !bytes.HasSuffix(trimmed, []byte("]"))
	}
	if trimmed[0] == '{' {
		return !bytes.HasSuffix(trimmed, []byte("}"))
	}

	return false
}

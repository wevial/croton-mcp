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
	"bytes"
	"context"
	"path/filepath"
	"strings"

	"github.com/wevial/croton-mcp/internal/strictjson"
)

// AllowedCommandLines returns the frozen read-only invocation surface.
func AllowedCommandLines() []string {
	return []string{
		"version",
		"filesystem list <path> [--type file|folder] --json",
		"filesystem info <path> --json",
		"filesystem download <remotePath...> <localFolder> --file-conflict-strategy skip --folder-conflict-strategy skip --json",
		"sharing status <path> --json",
	}
}

// List runs `filesystem list` with an explicit path, optional type, and --json.
func (client *Client) List(ctx context.Context, path, nodeType string) (ListResult, error) {
	if path == "" {
		return ListResult{}, errorCode(CodeInvalidConfig)
	}

	arguments := []string{"filesystem", "list", path}
	if nodeType != "" {
		if nodeType != "file" && nodeType != "folder" {
			return ListResult{}, errorCode(CodeInvalidConfig)
		}

		arguments = append(arguments, "--type", nodeType)
	}

	arguments = append(arguments, "--json")

	stdout, err := client.invoke(ctx, arguments, true)
	if err != nil {
		return ListResult{}, err
	}

	return decodeList(path, stdout, client.maxBytes)
}

// Info runs `filesystem info <path> --json`.
func (client *Client) Info(ctx context.Context, path string) (NodeEntity, error) {
	if path == "" {
		return NodeEntity{}, errorCode(CodeInvalidConfig)
	}

	stdout, err := client.invoke(ctx, []string{"filesystem", "info", path, "--json"}, true)
	if err != nil {
		return NodeEntity{}, err
	}

	var node NodeEntity
	if !strictjson.DecodeValue(stdout, client.maxBytes, &node) {
		return NodeEntity{}, errorCode(CodeMalformedOutput)
	}

	return node, nil
}

// SharingStatus runs `sharing status <path> --json` and treats literal undefined as unshared.
func (client *Client) SharingStatus(ctx context.Context, path string) (SharingStatus, error) {
	if path == "" {
		return SharingStatus{}, errorCode(CodeInvalidConfig)
	}

	stdout, err := client.invoke(ctx, []string{"sharing", "status", path, "--json"}, false)
	if err != nil {
		return SharingStatus{}, err
	}

	trimmed := bytes.TrimSpace(stdout)
	if string(trimmed) == "undefined" {
		return SharingStatus{}, nil
	}
	if looksTruncated(stdout) {
		return SharingStatus{}, errorCode(CodeTruncatedOutput)
	}

	var info ShareResult
	if !strictjson.DecodeValue(stdout, client.maxBytes, &info) {
		return SharingStatus{}, errorCode(CodeMalformedOutput)
	}

	return SharingStatus{Shared: true, Info: &info}, nil
}

// Download runs the frozen read-only download summary command. Local confinement
// is a later slice; this method only invokes the allowlisted CLI surface.
func (client *Client) Download(ctx context.Context, remotePaths []string, localFolder string) (DownloadSummary, error) {
	if len(remotePaths) == 0 || !filepath.IsAbs(localFolder) {
		return DownloadSummary{}, errorCode(CodeInvalidConfig)
	}

	arguments := []string{"filesystem", "download"}
	arguments = append(arguments, remotePaths...)
	arguments = append(arguments, localFolder,
		"--file-conflict-strategy", "skip",
		"--folder-conflict-strategy", "skip",
		"--json")

	stdout, err := client.invoke(ctx, arguments, true)
	if err != nil {
		return DownloadSummary{}, err
	}

	var summary DownloadSummary
	if !strictjson.DecodeValue(stdout, client.maxBytes, &summary) {
		return DownloadSummary{}, errorCode(CodeMalformedOutput)
	}

	return summary, nil
}

func decodeList(path string, stdout []byte, maxBytes int) (ListResult, error) {
	switch {
	case path == "/":
		var sections []RootSection
		if !strictjson.DecodeArray(stdout, maxBytes, &sections) {
			return ListResult{}, errorCode(CodeMalformedOutput)
		}

		return ListResult{Sections: sections}, nil
	case path == "/devices":
		var devices []Device
		if !strictjson.DecodeArray(stdout, maxBytes, &devices) {
			return ListResult{}, errorCode(CodeMalformedOutput)
		}

		return ListResult{Devices: devices}, nil
	default:
		var nodes []NodeEntity
		if !strictjson.DecodeArray(stdout, maxBytes, &nodes) {
			return ListResult{}, errorCode(CodeMalformedOutput)
		}

		return ListResult{Nodes: nodes}, nil
	}
}

// isAllowlistedInvocation matches argv element-by-element against the frozen
// read-only shapes: fixed tokens must match exactly and only operand slots vary.
func isAllowlistedInvocation(arguments []string) bool {
	switch {
	case len(arguments) == 1 && arguments[0] == "version":
		return true
	case len(arguments) == 4 && arguments[0] == "filesystem" && arguments[1] == "list":
		return isOperand(arguments[2]) && arguments[3] == "--json"
	case len(arguments) == 6 && arguments[0] == "filesystem" && arguments[1] == "list":
		return isOperand(arguments[2]) && arguments[3] == "--type" &&
			(arguments[4] == "file" || arguments[4] == "folder") && arguments[5] == "--json"
	case len(arguments) == 4 && arguments[0] == "filesystem" && arguments[1] == "info":
		return isOperand(arguments[2]) && arguments[3] == "--json"
	case len(arguments) == 4 && arguments[0] == "sharing" && arguments[1] == "status":
		return isOperand(arguments[2]) && arguments[3] == "--json"
	default:
		return isAllowlistedDownload(arguments)
	}
}

var downloadTrailer = [...]string{
	"--file-conflict-strategy", "skip",
	"--folder-conflict-strategy", "skip",
	"--json",
}

func isAllowlistedDownload(arguments []string) bool {
	if len(arguments) < 4+len(downloadTrailer) {
		return false
	}
	if arguments[0] != "filesystem" || arguments[1] != "download" {
		return false
	}

	trailer := arguments[len(arguments)-len(downloadTrailer):]
	for index, token := range downloadTrailer {
		if trailer[index] != token {
			return false
		}
	}

	for _, operand := range arguments[2 : len(arguments)-len(downloadTrailer)] {
		if !isOperand(operand) {
			return false
		}
	}

	return true
}

// isOperand accepts one variable slot; empty or flag-shaped values fail closed.
func isOperand(argument string) bool {
	return argument != "" && !strings.HasPrefix(argument, "-")
}

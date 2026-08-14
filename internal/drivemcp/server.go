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

// Package drivemcp constructs Croton Drive's independently runnable MCP server.
package drivemcp

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	version                = "0.0.0"
	currentProtocolVersion = "2026-07-28"
	legacyProtocolVersion  = "2025-11-25"
	unofficialDescription  = "Unofficial community wrapper for the Proton Drive CLI; not affiliated with or endorsed by Proton AG."
)

var errServerUnavailable = errors.New("server unavailable")

// Server owns Drive's SDK instance and intentionally has no Mail adapter,
// credentials, or tool registrations.
type Server struct {
	sdk *mcp.Server
}

// New creates the empty Drive tool catalog for protocol negotiation only.
func New() *Server {
	return &Server{sdk: mcp.NewServer(&mcp.Implementation{
		Name:        "croton-drive-mcp",
		Title:       "Croton Drive MCP (Unofficial)",
		Description: unofficialDescription,
		Version:     version,
	}, &mcp.ServerOptions{
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})}
}

// Connect opens one Drive server session without sharing the Mail server.
func (server *Server) Connect(ctx context.Context, transport mcp.Transport, options *mcp.ServerSessionOptions) (*mcp.ServerSession, error) {
	return server.sdk.Connect(ctx, transport, options)
}

// Serve runs one Drive server session and maps transport errors to a static
// diagnostic so protocol or configuration content is never exposed on stderr.
func Serve(ctx context.Context, server *Server, transport mcp.Transport) error {
	err := server.sdk.Run(ctx, transport)
	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, context.Canceled) {
		return nil
	}

	return errServerUnavailable
}

// NewStdioTransport keeps the Drive executable's standard output exclusively
// for SDK JSON-RPC frames; startup diagnostics are emitted by its command layer.
func NewStdioTransport(stdin io.ReadCloser, stdout io.Writer) mcp.Transport {
	return &mcp.IOTransport{Reader: stdin, Writer: nopWriteCloser{Writer: stdout}}
}

type nopWriteCloser struct {
	io.Writer
}

func (nopWriteCloser) Close() error { return nil }

package mcpserver

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
)

// New constructs Croton's MCP server without enabling mail capabilities.
func New() *mcp.Server {
	return mcp.NewServer(&mcp.Implementation{
		Name:        "croton-mcp",
		Title:       "Croton MCP for Proton Mail",
		Description: "Read-only MCP access to Proton Mail through Proton Mail Bridge",
		Version:     version,
	}, &mcp.ServerOptions{
		// Avoid advertising the SDK's deprecated default logging capability.
		Capabilities: &mcp.ServerCapabilities{},
	})
}

// Serve runs Croton over a persistent MCP transport.
func Serve(ctx context.Context, transport mcp.Transport) error {
	err := New().Run(ctx, transport)
	if errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) {
		return nil
	}
	return err
}

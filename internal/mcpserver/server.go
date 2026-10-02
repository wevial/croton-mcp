package mcpserver

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

const (
	version                = "0.0.0"
	currentProtocolVersion = "2026-07-28"
	legacyProtocolVersion  = "2025-11-25"
)

var errServerUnavailable = errors.New("server unavailable")

// Mail is the read-only adapter surface consumed by Croton's MCP tools.
// *bridge.Adapter satisfies it; tests substitute deterministic fakes.
type Mail interface {
	ListFolders(ctx context.Context) ([]bridge.Folder, error)
	Status(ctx context.Context, mailbox string) (bridge.MailboxStatus, error)
	SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error)
	GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error)
	GetMessageBody(ctx context.Context, identifier string) ([]byte, error)
}

// Options configures Croton's MCP server.
type Options struct {
	Mail Mail
	// Triage registers the opt-in mark_read, mark_unread and move_mail tools
	// when non-nil. Nil keeps the default catalog of six read-only tools.
	Triage Triage
	Audit  *Auditor
}

// Server wraps the SDK server with per-connection cancellation behavior.
type Server struct {
	sdk   *mcp.Server
	audit *Auditor
}

// New constructs Croton's MCP server exposing the six read-only tools, plus the
// Seen and move triage tools only when Options.Triage opts in.
func New(options Options) *Server {
	sdkServer := mcp.NewServer(&mcp.Implementation{
		Name:        "croton-mcp",
		Title:       "Croton MCP for Proton Mail",
		Description: "Read-only MCP access to Proton Mail through Proton Mail Bridge",
		Version:     version,
	}, &mcp.ServerOptions{
		// Avoid the SDK's deprecated default logging capability and declare
		// the fixed tool catalog without list-change notifications.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
	})

	registerTools(sdkServer, options)

	return &Server{sdk: sdkServer, audit: options.Audit}
}

// Serve runs an already-constructed server over a persistent MCP transport.
func Serve(ctx context.Context, server *Server, transport mcp.Transport) error {
	err := server.Run(ctx, transport)
	server.audit.transportEnd(err)

	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, context.Canceled) {
		return nil
	}

	return errServerUnavailable
}

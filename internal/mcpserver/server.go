package mcpserver

import (
	"context"
	"errors"
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

// version is stamped at release with
// -ldflags "-X github.com/wevial/croton-mcp/internal/mcpserver.version=<tag>".
var version = "dev"

var errServerUnavailable = errors.New("server unavailable")

// Mail is the read-only adapter surface consumed by Croton's MCP tools.
// *bridge.Adapter satisfies it; tests substitute deterministic fakes.
type Mail interface {
	ListFolders(ctx context.Context) ([]bridge.Folder, error)
	Status(ctx context.Context, mailbox string) (bridge.MailboxStatus, error)
	SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error)
	GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error)
	GetMessage(ctx context.Context, identifier string) (bridge.NormalizedMessage, error)
	GetMessageHeaders(ctx context.Context, identifier string) (bridge.MessageHeaders, error)
	GetMessageAttachmentStructure(ctx context.Context, identifier string) (bridge.MessageAttachmentStructure, error)
}

// Options configures Croton's MCP server.
type Options struct {
	Mail Mail
	// Triage registers the opt-in mark_read, mark_unread, move_mail,
	// archive_mail and trash_mail tools when non-nil. Nil keeps the default
	// catalog of six read-only tools.
	Triage Triage
	Audit  *Auditor
}

// Server is Croton's configured SDK server.
type Server struct {
	sdk *mcp.Server
}

// New constructs Croton's MCP server exposing the six read-only tools, plus the
// Seen, move, archive and trash triage tools only when Options.Triage opts in.
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

	return &Server{sdk: sdkServer}
}

// Serve runs an already-constructed server over a persistent MCP transport.
func Serve(ctx context.Context, server *Server, transport mcp.Transport) error {
	err := server.sdk.Run(ctx, cancelingTransport{Transport: transport})

	if err == nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) || errors.Is(err, context.Canceled) {
		return nil
	}

	return errServerUnavailable
}

// cancelingTransport closes its connection when the Serve context ends. The
// SDK's session Close waits for in-flight handlers; closing the connection
// instead makes the SDK cancel them, so a canceled server unwinds.
type cancelingTransport struct {
	mcp.Transport
}

func (transport cancelingTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	connection, err := transport.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}

	context.AfterFunc(ctx, func() { _ = connection.Close() })

	return connection, nil
}

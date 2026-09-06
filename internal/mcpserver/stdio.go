package mcpserver

import (
	"io"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/stdioframe"
)

// NewStdioTransport creates Croton's bounded newline-delimited stdio
// transport. Each complete input frame is admitted only after its size is
// proven within the ceiling; oversize frame fragments never reach the SDK.
func NewStdioTransport(stdin io.ReadCloser, stdout io.Writer) mcp.Transport {
	return stdioframe.NewTransport(stdin, stdout)
}

package main

import (
	"context"
	"fmt"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/mcpserver"
)

func main() {
	if err := mcpserver.Serve(context.Background(), &mcp.StdioTransport{}); err != nil {
		fmt.Fprintf(os.Stderr, "croton-mcp: %v\n", err)
		os.Exit(1)
	}
}

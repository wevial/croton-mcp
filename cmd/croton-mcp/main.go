// Command croton-mcp serves Croton's six read-only mail tools over stdio.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/config"
	"github.com/wevial/croton-mcp/internal/mcpserver"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "croton-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("croton-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "absolute path to the Croton configuration file")
	if err := flags.Parse(arguments); err != nil {
		return errors.New("invalid arguments")
	}
	if flags.NArg() != 0 || *configPath == "" {
		return errors.New("usage: croton-mcp --config <absolute path>")
	}

	loaded, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	adapter, err := bridge.NewAdapter(loaded)
	if err != nil {
		return err
	}
	defer func() { _ = adapter.Close() }()

	var auditor *mcpserver.Auditor
	if loaded.Audit.Enabled {
		auditor = mcpserver.NewAuditor(stderr)
	}

	server := mcpserver.New(mcpserver.Options{Mail: adapter, Audit: auditor})

	return mcpserver.Serve(ctx, server, mcpserver.NewStdioTransport(os.Stdin, os.Stdout))
}

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

// Command croton-drive-mcp serves Croton Drive's independent MCP lifecycle.
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

	"github.com/wevial/croton-mcp/internal/config"
	"github.com/wevial/croton-mcp/internal/drivemcp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()

	if err := run(ctx, os.Args[1:], os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "croton-drive-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stderr io.Writer) error {
	flags := flag.NewFlagSet("croton-drive-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	configPath := flags.String("config", "", "absolute path to the Croton Drive configuration file")
	if err := flags.Parse(arguments); err != nil {
		return errors.New("invalid arguments")
	}
	if flags.NArg() != 0 || *configPath == "" {
		return errors.New("usage: croton-drive-mcp --config <absolute path>")
	}

	if _, err := config.LoadDrive(*configPath); err != nil {
		return err
	}

	return drivemcp.Serve(ctx, drivemcp.New(), drivemcp.NewStdioTransport(os.Stdin, os.Stdout))
}

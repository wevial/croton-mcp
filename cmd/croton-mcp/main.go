// Command croton-mcp serves mail tools over stdio or read-only loopback HTTP.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/config"
	"github.com/wevial/croton-mcp/internal/mcpserver"
	"github.com/wevial/croton-mcp/internal/owneroauth"
	"github.com/wevial/croton-mcp/internal/stdioframe"
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
	command := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		command, arguments = arguments[0], arguments[1:]
		switch command {
		case "serve", "pair", "approve":
		default:
			return errors.New("invalid command")
		}
	}

	flags := flag.NewFlagSet("croton-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var configPath, bind, authorizer, cert, key, dir string
	settings := owneroauth.Defaults()
	if command == "" || command == "serve" {
		flags.StringVar(&configPath, "config", "", "absolute path to the Croton configuration file")
	}
	if command != "" {
		flags.StringVar(&dir, "state-dir", "", "private owner state directory")
	}
	if command == "serve" {
		flags.StringVar(&bind, "bind", "127.0.0.1:8787", "loopback IP and port")
		flags.StringVar(&authorizer, "authorizer", "", "owner")
		flags.StringVar(&cert, "tls-cert", "", "HTTPS certificate")
		flags.StringVar(&key, "tls-key", "", "HTTPS private key")
		flags.DurationVar(&settings.PairingWindow, "pairing-window", settings.PairingWindow, "registration and approval window")
		flags.DurationVar(&settings.HandoffLifetime, "handoff-lifetime", settings.HandoffLifetime, "owner handoff lifetime")
		flags.IntVar(&settings.PendingLimit, "pending-limit", settings.PendingLimit, "maximum pending registrations")
		flags.DurationVar(&settings.CodeLifetime, "code-lifetime", settings.CodeLifetime, "authorization code lifetime")
		flags.DurationVar(&settings.AccessLifetime, "access-lifetime", settings.AccessLifetime, "access token lifetime")
	}
	if err := flags.Parse(arguments); err != nil || flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}
	if command != "" && dir == "" {
		var err error
		dir, err = owneroauth.DefaultDir()
		if err != nil {
			return err
		}
	}
	if command == "pair" || command == "approve" {
		tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
		if err != nil {
			return errors.New("owner terminal unavailable")
		}
		defer tty.Close()
		return owneroauth.Terminal(ctx, dir, command, tty)
	}
	if configPath == "" {
		return errors.New("usage: croton-mcp --config <absolute path>")
	}

	loaded, err := config.Load(configPath)
	if err != nil {
		return err
	}

	var auditor *mcpserver.Auditor
	if loaded.Audit.Enabled {
		auditor = mcpserver.NewAuditor(stderr)
	}

	if command == "serve" {
		if authorizer != "owner" {
			return errors.New("HTTP authorizer unavailable")
		}
		if loaded.Mutations.Enabled {
			return errors.New("HTTP requires read-only configuration")
		}
		address, err := netip.ParseAddrPort(bind)
		if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
			return errors.New("invalid loopback bind")
		}
		return serveHTTP(ctx, loaded, address.String(), dir, cert, key, settings, auditor)
	}

	adapter, err := bridge.NewAdapter(loaded)
	if err != nil {
		return err
	}
	defer func() { _ = adapter.Close() }()

	options := mcpserver.Options{Mail: adapter, Audit: auditor}
	if loaded.Mutations.Enabled {
		options.Triage = adapter
	}

	server := mcpserver.New(options)

	return mcpserver.Serve(ctx, server, stdioframe.NewTransport(os.Stdin, os.Stdout))
}

func serveHTTP(ctx context.Context, cfg bridge.Config, address, dir, cert, key string, settings owneroauth.Settings, auditor *mcpserver.Auditor) error {
	origin := "http://" + address
	var tlsConfig *tls.Config
	if cert != "" || key != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return errors.New("HTTP TLS unavailable")
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		origin = "https://" + address
	}

	validated, err := bridge.ValidateConfig(cfg)
	if err != nil {
		return err
	}
	adapter, err := bridge.NewAdapter(cfg)
	if err != nil {
		return err
	}
	// Adapter.Close logs out within the configured IMAP timeout. Give logout
	// a shorter share of the CLI exit budget if the Bridge stops responding.
	defer func() {
		done := make(chan struct{})
		go func() { _ = adapter.Close(); close(done) }()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	}()

	authority, err := owneroauth.Open(dir, origin, settings)
	if err != nil {
		return err
	}
	defer authority.Close()

	handler, err := mcpserver.NewHTTP(origin, adapter, auditor, time.Duration(validated.IMAP.CommandTimeoutMs)*time.Millisecond, authority.ValidBearer, authority)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("HTTP listener unavailable")
	}
	defer listener.Close()
	stopOwner, err := authority.ListenOwner(ctx)
	if err != nil {
		return err
	}
	defer stopOwner()
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}

	return runHTTP(ctx, listener, handler)
}

func runHTTP(parent context.Context, listener net.Listener, handler http.Handler) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var connections sync.WaitGroup
	server := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(_ net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				connections.Add(1)
			case http.StateClosed:
				connections.Done()
			}
		},
	}
	done := make(chan struct{})
	context.AfterFunc(ctx, func() {
		// Close also interrupts partially read requests and idle keep-alives.
		// Handlers observe BaseContext cancellation while unwinding SDK calls.
		_ = server.Close()
		close(done)
	})
	err := server.Serve(listener)
	cancel()
	<-done
	connections.Wait()

	if !errors.Is(err, http.ErrServerClosed) {
		return errors.New("HTTP listener unavailable")
	}
	return nil
}

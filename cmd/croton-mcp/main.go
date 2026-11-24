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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/config"
	"github.com/wevial/croton-mcp/internal/forwardpolicy"
	"github.com/wevial/croton-mcp/internal/mcpserver"
	"github.com/wevial/croton-mcp/internal/owneroauth"
	"github.com/wevial/croton-mcp/internal/stdioframe"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stderr, owneroauth.Defaults()); err != nil {
		fmt.Fprintf(os.Stderr, "croton-mcp: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, arguments []string, stderr io.Writer, settings owneroauth.Settings) error {
	return runCLI(ctx, arguments, stderr, settings, confirmFunnel)
}

func runCLI(ctx context.Context, arguments []string, stderr io.Writer, settings owneroauth.Settings, confirm func(context.Context, string, string) error) error {
	command := ""
	if len(arguments) > 0 && !strings.HasPrefix(arguments[0], "-") {
		command, arguments = arguments[0], arguments[1:]
		switch command {
		case "pair", "approve", "clients", "unpair":
			tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
			if err != nil {
				return errors.New("owner terminal unavailable")
			}
			defer tty.Close()

			return runOwnerCommand(ctx, command, arguments, tty)
		case "serve", "init-forward-state", "limits", "allow", "forget":
		default:
			return errors.New("invalid command")
		}
	}

	flags := flag.NewFlagSet("croton-mcp", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	var configPath, bind, authorizer, cert, key, dir, publicOrigin, publicMode string
	flags.StringVar(&configPath, "config", "", "absolute path to the Croton configuration file")
	if command == "serve" {
		flags.StringVar(&publicMode, "public", "", "publish with tailscale")
		flags.Func("public-url", "external HTTPS origin of the owner-managed proxy", func(raw string) error {
			var err error
			publicOrigin, err = parsePublicOrigin(raw)
			return err
		})
		flags.StringVar(&dir, "state-dir", "", "private owner state directory")
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
	forwardOwner := command == "limits" || command == "allow" || command == "forget"
	// Accept the owner operand on either side of --config.
	if forwardOwner && len(arguments) > 1 {
		name, _, _ := strings.Cut(arguments[0], "=")
		if name != "--config" && name != "-config" && name != "--" {
			arguments = append(arguments[1:], "--", arguments[0])
		}
	}
	if err := flags.Parse(arguments); err != nil || (!forwardOwner && flags.NArg() != 0) {
		return errors.New("invalid arguments")
	}
	if command == "serve" && dir == "" {
		var err error
		dir, err = owneroauth.DefaultDir()
		if err != nil {
			return err
		}
	}
	if configPath == "" {
		return errors.New("usage: croton-mcp --config <absolute path>")
	}

	// Only the servers need graceful shutdown. Keep terminal prompts interruptible.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, os.Interrupt)
	defer stop()

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
		if loaded.Mutations.Enabled || loaded.Forward.Enabled {
			return errors.New("HTTP requires read-only configuration")
		}
		address, err := netip.ParseAddrPort(bind)
		if err != nil || !address.Addr().IsLoopback() || address.Addr().Zone() != "" || address.Port() == 0 {
			return errors.New("invalid loopback bind")
		}

		var funnel *tailscaleFunnel
		if publicMode != "" {
			if publicMode != "tailscale" || publicOrigin != "" || cert != "" || key != "" {
				return errors.New("use --public=tailscale with a plain loopback backend and no --public-url")
			}

			funnel, err = approveFunnel(ctx, address, confirm)
			if err != nil {
				return err
			}
			publicOrigin = "https://" + funnel.node.host
		}

		return serveHTTP(ctx, loaded, address, dir, cert, key, publicOrigin, settings, auditor, funnel)
	}

	if command == "init-forward-state" || forwardOwner {
		validated, err := bridge.ValidateConfig(loaded)
		if err != nil {
			return err
		}
		if !loaded.Forward.Enabled {
			return errors.New("forward submission disabled")
		}

		if command == "init-forward-state" {
			return forwardpolicy.Initialize(loaded.Forward.StateDir)
		}

		return runForwardOwner(ctx, loaded.Forward, validated.IMAP, command, flags.Args(), stderr)
	}

	adapter, err := bridge.NewAdapter(loaded)
	if err != nil {
		return err
	}
	defer func() { _ = adapter.Close() }()

	options := mcpserver.Options{Mail: adapter, Forward: mcpserver.NewForwardPreviews(adapter), Audit: auditor}
	options.Forward.Submission = loaded.Forward
	if loaded.Mutations.Enabled {
		options.Triage = adapter
	}

	server := mcpserver.New(options)

	return mcpserver.Serve(ctx, server, stdioframe.NewTransport(os.Stdin, os.Stdout))
}

func runOwnerCommand(ctx context.Context, command string, args []string, tty io.ReadWriter) error {
	flags := flag.NewFlagSet(command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	dir := flags.String("state-dir", "", "private owner state directory")

	// A client ID on either side of --state-dir can start with a dash.
	var id string
	if command == "unpair" && len(args) > 0 {
		flagName, _, _ := strings.Cut(args[0], "=")
		if flagName == "--state-dir" || flagName == "-state-dir" || flagName == "--" {
			id, args = args[len(args)-1], args[:len(args)-1]
		} else {
			id, args = args[0], args[1:]
		}
	}
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 {
		return errors.New("invalid arguments")
	}

	if *dir == "" {
		var err error
		*dir, err = owneroauth.DefaultDir()
		if err != nil {
			return err
		}
	}

	if command == "clients" || command == "unpair" {
		return owneroauth.Admin(ctx, *dir, command, id, tty)
	}

	return owneroauth.Terminal(ctx, *dir, command, tty)
}

func serveHTTP(ctx context.Context, cfg bridge.Config, address netip.AddrPort, dir, cert, key, publicOrigin string, settings owneroauth.Settings, auditor *mcpserver.Auditor, funnel *tailscaleFunnel) error {
	hostPort := net.JoinHostPort(httpHostname(address.Addr()), strconv.Itoa(int(address.Port())))
	origin := "http://" + strings.TrimSuffix(hostPort, ":80")
	var tlsConfig *tls.Config
	if cert != "" || key != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return errors.New("HTTP TLS unavailable")
		}
		tlsConfig = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
		origin = "https://" + strings.TrimSuffix(hostPort, ":443")
	}

	if publicOrigin != "" {
		origin = publicOrigin
		settings.BehindHTTPSProxy = true
	}

	validated, err := bridge.ValidateConfig(cfg)
	if err != nil {
		return err
	}
	adapter, err := bridge.NewAdapter(cfg)
	if err != nil {
		return err
	}
	defer waitForCleanup(func() { _ = adapter.Close() })

	authority, err := owneroauth.Open(dir, origin, settings)
	if err != nil {
		return err
	}
	var stopOwner func()
	defer waitForCleanup(func() {
		if stopOwner != nil {
			stopOwner()
		}
		_ = authority.Close()
	})

	handler, err := mcpserver.NewHTTP(origin, publicOrigin != "", adapter, auditor, time.Duration(validated.IMAP.CommandTimeoutMs)*time.Millisecond, authority.BearerClient, authority)
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", address.String())
	if err != nil {
		return errors.New("HTTP listener unavailable")
	}
	defer listener.Close()
	stopOwner, err = authority.ListenOwner(ctx)
	if err != nil {
		return err
	}
	if tlsConfig != nil {
		listener = tls.NewListener(listener, tlsConfig)
	}

	if funnel != nil {
		return funnel.serve(ctx, listener, handler)
	}
	return runHTTP(ctx, listener, handler)
}

// After the one-second HTTP drain, owner IPC and database cleanup share one
// second, and Bridge logout has another even if owner cleanup stalls.
func waitForCleanup(cleanup func()) {
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
	}
}

func runHTTP(parent context.Context, listener net.Listener, handler http.Handler) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()

	var sockets sync.Map // net.Conn to underlying transport

	server := &http.Server{
		Handler:           handler,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      70 * time.Second,
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0),
		ConnState: func(conn net.Conn, state http.ConnState) {
			switch state {
			case http.StateNew:
				raw := conn
				if secured, ok := conn.(*tls.Conn); ok {
					raw = secured.NetConn()
				}
				sockets.Store(conn, raw)
			case http.StateClosed:
				sockets.Delete(conn)
			}
		},
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	err := server.Serve(listener)
	cancel()

	// Reserve time for owner IPC and Bridge logout within the CLI exit budget.
	drain, stopDrain := context.WithTimeout(context.Background(), time.Second)
	defer stopDrain()
	drained := make(chan struct{})
	go func() {
		if server.Shutdown(drain) == nil {
			close(drained)
		}
	}()

	select {
	case <-drained:
	case <-drain.Done():
		// Close underneath TLS even if Shutdown is stuck writing close_notify.
		sockets.Range(func(_, raw any) bool {
			_ = raw.(net.Conn).Close()
			return true
		})
	}

	if !errors.Is(err, net.ErrClosed) || parent.Err() == nil {
		return errors.New("HTTP listener unavailable")
	}
	return nil
}

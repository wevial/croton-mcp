package owneroauth

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const socketName = "owner.sock"

var errOffline = errors.New("owner socket unavailable")

// JSON can use six bytes per byte of the 2048-byte redirect and 256-byte
// client name. The remaining space covers the loopback resource, IDs and framing.
const maxOwnerReplyBytes = 16 << 10

func sameUser(c *net.UnixConn) bool {
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	return raw.Control(func(fd uintptr) { ok = ownerPeer(int(fd), os.Geteuid()) }) == nil && ok
}

type reply struct {
	OK            bool
	Handoff       string        `json:",omitempty"`
	Review        *review       `json:",omitempty"`
	Client        *pairedClient `json:",omitempty"`
	UnknownClient bool          `json:",omitempty"`
}

// ListenOwner runs only while the caller holds the exclusive database lock.
func (a *Authority) ListenOwner(ctx context.Context) (func(), error) {
	path := filepath.Join(filepath.Dir(a.db.Path()), socketName)
	info, err := os.Lstat(path)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSocket == 0 || os.Remove(path) != nil {
			return nil, errState
		}
	case !errors.Is(err, os.ErrNotExist):
		return nil, errState
	}

	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return nil, errState
	}

	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		stop := context.AfterFunc(ctx, func() { listener.Close() })
		defer stop()

		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			a.handleOwner(conn)
			conn.Close()
		}
	}()
	return func() { cancel(); <-done }, nil
}

func (a *Authority) handleOwner(conn *net.UnixConn) {
	if !sameUser(conn) {
		return
	}
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		return
	}

	data, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
	if err != nil {
		return
	}

	command, arg, _ := strings.Cut(strings.TrimSuffix(data, "\n"), " ")
	encoder := json.NewEncoder(conn)
	var out reply
	switch {
	case command == "pair" && arg == "":
		handoff, err := a.start()
		out = reply{OK: err == nil, Handoff: handoff}
	case command == "inspect" && arg == "":
		if details, err := a.inspect(); err == nil {
			out = reply{OK: true, Review: &details}
		}
	case command == "clients" && arg == "":
		out.OK = listClients(a.db, func(client pairedClient) error {
			return encoder.Encode(reply{OK: true, Client: &client})
		}) == nil
	case command == "unpair" && validClientID(arg):
		err := unpair(a.db, arg)
		out.OK = err == nil
		out.UnknownClient = errors.Is(err, errUnknownClient)
	case command == "approve":
		id, code, _ := strings.Cut(arg, " ")
		out.OK = a.approve(id, code) == nil
	}
	_ = encoder.Encode(out)
}

func sendOwnerRequest(ctx context.Context, dir, command string) (net.Conn, error) {
	if err := secureDir(dir, false); err != nil {
		return nil, err
	}

	path := filepath.Join(dir, socketName)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errOffline
	}
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return nil, errState
	}

	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "unix", path)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return nil, errOffline
	}
	if err != nil {
		return nil, errState
	}
	if !sameUser(conn.(*net.UnixConn)) {
		conn.Close()
		return nil, errDenied
	}

	if conn.SetDeadline(time.Now().Add(2*time.Second)) != nil {
		conn.Close()
		return nil, errState
	}
	if _, err := fmt.Fprintln(conn, command); err != nil {
		conn.Close()
		return nil, errState
	}

	return conn, nil
}

func ownerRequest(ctx context.Context, dir, command string) (reply, error) {
	conn, err := sendOwnerRequest(ctx, dir, command)
	if err != nil {
		return reply{}, err
	}
	defer conn.Close()

	var out reply
	if json.NewDecoder(io.LimitReader(conn, maxOwnerReplyBytes)).Decode(&out) != nil || !out.OK {
		return reply{}, errDenied
	}
	return out, nil
}

// Terminal writes the secret handoff and untrusted client details only to the controlling terminal.
func Terminal(ctx context.Context, dir, command string, tty io.ReadWriter) error {
	if command == "pair" {
		out, err := ownerRequest(ctx, dir, "pair")
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(tty, out.Handoff)
		return err
	}
	if command != "approve" {
		return errDenied
	}

	out, err := ownerRequest(ctx, dir, "inspect")
	if err != nil || out.Review == nil {
		return errDenied
	}
	r := out.Review
	_, err = fmt.Fprintf(tty, "Self-reported client name: %+q\nClient ID: %+q\nFull redirect: %+q\nResource: %+q\nS256 challenge: %+q\nComparison code: %s\nEnter the matching browser code to approve: ", r.Name, r.Client, r.Redirect, r.Resource, r.Challenge, r.Code)
	if err != nil {
		return errDenied
	}
	line, err := bufio.NewReader(io.LimitReader(tty, 128)).ReadString('\n')
	if err != nil || !matches(r.Code, strings.TrimSpace(line)) {
		return errDenied
	}

	_, err = ownerRequest(ctx, dir, "approve "+r.RequestID+" "+r.Code)
	return err
}

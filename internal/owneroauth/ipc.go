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
	"time"
)

const socketName = "owner.sock"

func sameUser(c *net.UnixConn) bool {
	raw, err := c.SyscallConn()
	if err != nil {
		return false
	}
	ok := false
	return raw.Control(func(fd uintptr) { ok = ownerPeer(int(fd), os.Geteuid()) }) == nil && ok
}

type reply struct {
	OK      bool
	Handoff string  `json:",omitempty"`
	Review  *review `json:",omitempty"`
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
	conn.SetDeadline(time.Now().Add(time.Second))
	data, err := bufio.NewReader(io.LimitReader(conn, 128)).ReadString('\n')
	if err != nil {
		return
	}

	command, arg, _ := strings.Cut(strings.TrimSuffix(data, "\n"), " ")
	var out reply
	switch {
	case command == "pair" && arg == "":
		out = reply{OK: true, Handoff: a.start()}
	case command == "inspect" && arg == "":
		if details, err := a.inspect(); err == nil {
			out = reply{OK: true, Review: &details}
		}
	case command == "approve":
		id, code, _ := strings.Cut(arg, " ")
		out.OK = a.approve(id, code) == nil
	}
	_ = json.NewEncoder(conn).Encode(out)
}

func ownerRequest(ctx context.Context, dir, command string) (reply, error) {
	if err := secureDir(dir, false); err != nil {
		return reply{}, err
	}
	path := filepath.Join(dir, socketName)
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 {
		return reply{}, errState
	}

	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "unix", path)
	if err != nil {
		return reply{}, errState
	}
	defer conn.Close()
	if !sameUser(conn.(*net.UnixConn)) {
		return reply{}, errDenied
	}

	conn.SetDeadline(time.Now().Add(2 * time.Second))
	if _, err = fmt.Fprintln(conn, command); err != nil {
		return reply{}, errState
	}
	var out reply
	if json.NewDecoder(io.LimitReader(conn, 8192)).Decode(&out) != nil || !out.OK {
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

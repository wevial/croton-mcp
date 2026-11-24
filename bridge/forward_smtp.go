package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ForwardSMTP owns an authenticated, deadline-bound local SMTP connection.
type ForwardSMTP struct {
	conn net.Conn
}

// Close releases the authenticated transport.
func (s *ForwardSMTP) Close() error { return s.conn.Close() }

// OpenForwardSMTP rechecks the preview's authenticated Bridge session and
// helper login, then calls reserve before opening the SMTP socket. It never
// enters the adapter's IMAP retry loop.
func (adapter *Adapter) OpenForwardSMTP(ctx context.Context, cfg ForwardConfig, login string, closed <-chan struct{}, reserve func() error) (*ForwardSMTP, error) {
	if !cfg.Enabled || closed == nil || reserve == nil {
		return nil, errorCode(CodeInvalidRequest)
	}
	imap := adapter.config.IMAP
	endpoint, err := ParseLoopbackEndpoint(imap.Host, cfg.SMTPPort)
	if err != nil {
		return nil, err
	}
	if cfg.SMTPTLSMode != "" && cfg.SMTPTLSMode != TLSModeStartTLS && cfg.SMTPTLSMode != TLSModeImplicit {
		return nil, errorCode(CodeInvalidConfig)
	}
	trust := imap.TLS
	if cfg.SMTPSPKISHA256 != "" {
		trust = TLSConfig{SPKISHA256: cfg.SMTPSPKISHA256, MinVersion: trust.MinVersion}
	}
	policy, err := NewTLSConfig(trust)
	if err != nil {
		return nil, err
	}

	ctx, cancel := adapter.operationContext(ctx)
	defer cancel()
	if err := adapter.acquire(ctx); err != nil {
		return nil, err
	}
	defer func() { adapter.gate <- struct{}{} }()
	if adapter.closed || adapter.session == nil || adapter.connectionClosed != closed || adapter.login != login {
		return nil, errorCode(CodeAuthentication)
	}

	creds, err := LoadCredentials(ctx, imap.CredentialCommand, time.Duration(imap.CommandTimeoutMs)*time.Millisecond)
	if err != nil {
		return nil, err
	}
	defer creds.Zero()
	if string(creds.Username) != login || strings.IndexByte(login, 0) >= 0 || slices.Contains(creds.Password, 0) {
		return nil, errorCode(CodeAuthentication)
	}

	select {
	case <-closed:
		return nil, errorCode(CodeAuthentication)
	default:
	}

	if err := reserve(); err != nil {
		return nil, err
	}

	dial := net.Dialer{Timeout: time.Duration(imap.ConnectTimeoutMs) * time.Millisecond}
	raw, err := dial.DialContext(ctx, "tcp", endpoint.String())
	if err != nil {
		return nil, connectionError(ctx, err)
	}
	stop := closeOnContext(ctx, raw)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		raw.Close()
		return nil, errorCode(CodeBridgeUnreachable)
	}

	conn, err := authenticateSMTP(ctx, raw, policy, cfg.SMTPTLSMode, creds)
	if err != nil {
		raw.Close()
		return nil, err
	}
	return &ForwardSMTP{conn: conn}, nil
}

// SMTP replies share a fixed line, byte and count budget through AUTH.
func authenticateSMTP(ctx context.Context, raw net.Conn, policy *tls.Config, mode string, creds Credentials) (net.Conn, error) {
	conn := raw
	reader := bufio.NewReaderSize(conn, maxStartTLSLineBytes+1)
	var budget startTLSResponseBudget
	reply := func(code int) ([]string, error) {
		prefix := strconv.Itoa(code)
		var lines []string
		for {
			line, n, err := readStartTLSLine(reader, maxStartTLSLineBytes)
			if err != nil || budget.add(n) != nil || !strings.HasPrefix(line, prefix) {
				return nil, errorCode(CodeTLSNegotiation)
			}
			rest := line[3:]
			if rest == "" || rest[0] == ' ' {
				return append(lines, strings.TrimPrefix(rest, " ")), nil
			}
			if rest[0] != '-' {
				return nil, errorCode(CodeTLSNegotiation)
			}
			lines = append(lines, rest[1:])
		}
	}
	ehlo := func() ([]string, error) {
		if _, err := io.WriteString(conn, "EHLO localhost\r\n"); err != nil {
			return nil, errorCode(CodeBridgeUnreachable)
		}
		lines, err := reply(250)
		if err != nil {
			return nil, err
		}
		return lines[1:], nil // First line identifies the server.
	}

	if mode != TLSModeImplicit {
		if _, err := reply(220); err != nil {
			return nil, err
		}
		extensions, err := ehlo()
		if err != nil || !slices.ContainsFunc(extensions, func(ext string) bool { return strings.EqualFold(ext, "STARTTLS") }) {
			return nil, errorCode(CodeTLSNegotiation)
		}
		if _, err := io.WriteString(conn, "STARTTLS\r\n"); err != nil {
			return nil, errorCode(CodeTLSNegotiation)
		}
		if _, err := reply(220); err != nil || reader.Buffered() != 0 {
			return nil, errorCode(CodeTLSNegotiation)
		}
	}

	secure := tls.Client(raw, policy)
	if err := secure.HandshakeContext(ctx); err != nil {
		return nil, connectionError(ctx, err)
	}
	conn = secure
	reader.Reset(conn)
	if mode == TLSModeImplicit {
		if _, err := reply(220); err != nil {
			return nil, err
		}
	}

	extensions, err := ehlo()
	if err != nil {
		return nil, err
	}
	plain := slices.ContainsFunc(extensions, func(ext string) bool {
		fields := strings.Fields(strings.ToUpper(ext))
		return len(fields) > 1 && fields[0] == "AUTH" && slices.Contains(fields[1:], "PLAIN")
	})
	if !plain {
		return nil, errorCode(CodeAuthentication)
	}

	payload := make([]byte, 1, 2+len(creds.Username)+len(creds.Password))
	payload = append(payload, creds.Username...)
	payload = append(payload, 0)
	payload = append(payload, creds.Password...)
	command := base64.StdEncoding.AppendEncode([]byte("AUTH PLAIN "), payload)
	clear(payload)
	command = append(command, '\r', '\n')
	_, err = conn.Write(command)
	clear(command)
	if err != nil {
		return nil, errorCode(CodeBridgeUnreachable)
	}
	if _, err := reply(235); err != nil {
		return nil, errorCode(CodeAuthentication)
	}

	return conn, nil
}

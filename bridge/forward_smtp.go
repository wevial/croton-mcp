package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"io"
	"net"
	"net/textproto"
	"slices"
	"strconv"
	"strings"
	"time"
)

const forwardNotSubmitted = "not_submitted"

// SubmitForward spends the reservation while holding the preview's Bridge
// session, then submits the stored envelope and MIME in one SMTP transaction.
func (adapter *Adapter) SubmitForward(ctx context.Context, cfg ForwardConfig, login, to, message string, closed <-chan struct{}, reserve func() error) (string, error) {
	if !cfg.Enabled || closed == nil || reserve == nil || !ValidForwardMailbox(login) || !ValidForwardMailbox(to) ||
		len(message) > MaxForwardMIMEBytes || !strings.HasSuffix(message, "\r\n") {
		return "", errorCode(CodeInvalidRequest)
	}

	imap := adapter.config.IMAP
	endpoint, err := ParseLoopbackEndpoint(imap.Host, cfg.SMTPPort)
	if err != nil {
		return "", err
	}
	if !cfg.validTLSMode() {
		return "", errorCode(CodeInvalidConfig)
	}
	trust := imap.TLS
	if cfg.SMTPSPKISHA256 != "" {
		trust = TLSConfig{SPKISHA256: cfg.SMTPSPKISHA256, MinVersion: trust.MinVersion}
	}
	policy, err := NewTLSConfig(trust)
	if err != nil {
		return "", err
	}

	ctx, cancel := adapter.operationContext(ctx)
	defer cancel()
	if err := adapter.acquire(ctx); err != nil {
		return "", err
	}
	defer func() { adapter.gate <- struct{}{} }()

	if adapter.closed || adapter.session == nil || adapter.connectionClosed != closed || adapter.login != login {
		return "", errorCode(CodeAuthentication)
	}

	creds, err := LoadCredentials(ctx, imap.CredentialCommand, time.Duration(imap.CommandTimeoutMs)*time.Millisecond)
	if err != nil {
		return "", err
	}
	defer creds.Zero()
	if string(creds.Username) != login || slices.Contains(creds.Password, 0) {
		return "", errorCode(CodeAuthentication)
	}

	select {
	case <-closed:
		return "", errorCode(CodeAuthentication)
	case <-ctx.Done():
		return "", errorCode(CodeOperationCanceled)
	default:
	}
	if err := reserve(); err != nil {
		return "", err
	}

	dial := net.Dialer{Timeout: time.Duration(imap.ConnectTimeoutMs) * time.Millisecond}
	raw, err := dial.DialContext(ctx, "tcp", endpoint.String())
	if err != nil {
		return forwardNotSubmitted, nil
	}
	defer raw.Close()
	stop := closeOnContext(ctx, raw)
	defer stop()
	deadline, _ := ctx.Deadline()
	if err := raw.SetDeadline(deadline); err != nil {
		return forwardNotSubmitted, nil
	}

	smtp := smtpSession{conn: raw, reader: bufio.NewReaderSize(raw, maxStartTLSLineBytes+1)}
	if !smtp.authenticate(ctx, policy, cfg.SMTPTLSMode, creds) {
		return forwardNotSubmitted, nil
	}
	if smtp.command("MAIL FROM:<"+login+">", 250) == nil ||
		smtp.command("RCPT TO:<"+to+">", 250, 251, 252) == nil ||
		smtp.command("DATA", 354) == nil {
		return forwardNotSubmitted, nil
	}

	data := textproto.NewWriter(bufio.NewWriter(smtp.conn)).DotWriter()
	if _, err := io.WriteString(data, message); err != nil {
		return OutcomeUnknown, nil
	}
	if err := data.Close(); err != nil || smtp.reply(250) == nil {
		return OutcomeUnknown, nil
	}

	return "accepted", nil
}

// Replies share one line, count and byte budget from greeting to acceptance.
type smtpSession struct {
	conn   net.Conn
	reader *bufio.Reader
	budget startTLSResponseBudget
}

func (smtp *smtpSession) reply(allowed ...int) []string {
	var lines []string
	var code int
	for {
		line, n, err := readStartTLSLine(smtp.reader, maxStartTLSLineBytes)
		if err != nil || smtp.budget.add(n) != nil || len(line) < 3 {
			return nil
		}
		status, err := strconv.Atoi(line[:3])
		if err != nil || !slices.Contains(allowed, status) || (len(lines) > 0 && status != code) {
			return nil
		}
		code = status
		rest := line[3:]
		if rest == "" || rest[0] == ' ' {
			return append(lines, strings.TrimPrefix(rest, " "))
		}
		if rest[0] != '-' {
			return nil
		}
		lines = append(lines, rest[1:])
	}
}

func (smtp *smtpSession) command(line string, codes ...int) []string {
	if _, err := io.WriteString(smtp.conn, line+"\r\n"); err != nil {
		return nil
	}
	return smtp.reply(codes...)
}

func (smtp *smtpSession) hasExtension(match func(string) bool) bool {
	lines := smtp.command("EHLO localhost", 250)
	// The first EHLO line identifies the server.
	return len(lines) > 1 && slices.ContainsFunc(lines[1:], match)
}

func (smtp *smtpSession) authenticate(ctx context.Context, policy *tls.Config, mode string, creds Credentials) bool {
	if mode != TLSModeImplicit {
		if smtp.reply(220) == nil {
			return false
		}
		if !smtp.hasExtension(func(ext string) bool { return strings.EqualFold(ext, "STARTTLS") }) {
			return false
		}
		if smtp.command("STARTTLS", 220) == nil || smtp.reader.Buffered() != 0 {
			return false
		}
	}

	secure := tls.Client(smtp.conn, policy)
	if err := secure.HandshakeContext(ctx); err != nil {
		return false
	}
	smtp.conn = secure
	smtp.reader.Reset(secure)
	if mode == TLSModeImplicit && smtp.reply(220) == nil {
		return false
	}

	plain := smtp.hasExtension(func(ext string) bool {
		fields := strings.Fields(strings.ToUpper(ext))
		return len(fields) > 1 && fields[0] == "AUTH" && slices.Contains(fields[1:], "PLAIN")
	})
	if !plain {
		return false
	}

	payload := make([]byte, 1, 2+len(creds.Username)+len(creds.Password))
	payload = append(payload, creds.Username...)
	payload = append(payload, 0)
	payload = append(payload, creds.Password...)
	command := base64.StdEncoding.AppendEncode([]byte("AUTH PLAIN "), payload)
	clear(payload)
	command = append(command, '\r', '\n')
	_, err := smtp.conn.Write(command)
	clear(command)

	return err == nil && smtp.reply(235) != nil
}

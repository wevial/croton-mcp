package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Connection is a TLS-established connection to the configured local Bridge endpoint.
type Connection struct{ connection net.Conn }

// Close releases the underlying network connection.
func (connection *Connection) Close() error {
	if connection == nil || connection.connection == nil {
		return nil
	}
	return connection.connection.Close()
}

// Dial validates the local endpoint and establishes its configured verified TLS transport.
func Dial(parent context.Context, input Config) (*Connection, error) {
	config, err := ValidateConfig(input)
	if err != nil {
		return nil, err
	}

	endpoint, err := ParseLoopbackEndpoint(config.IMAP.Host, config.IMAP.Port)
	if err != nil {
		return nil, err
	}

	tlsConfig, err := NewTLSConfig(config.IMAP.TLS)
	if err != nil {
		return nil, err
	}

	contextWithTimeout, cancel := context.WithTimeout(parent, time.Duration(config.IMAP.ConnectTimeoutMs)*time.Millisecond)
	defer cancel()

	rawConnection, err := (&net.Dialer{}).DialContext(contextWithTimeout, "tcp", endpoint.String())
	if err != nil {
		return nil, connectionError(contextWithTimeout, err)
	}

	if config.IMAP.TLSMode == TLSModeImplicit {
		return establishImplicitTLS(contextWithTimeout, rawConnection, tlsConfig)
	}

	return establishStartTLS(contextWithTimeout, rawConnection, tlsConfig)
}

func establishImplicitTLS(ctx context.Context, rawConnection net.Conn, tlsConfig *tls.Config) (*Connection, error) {
	tlsConnection := tls.Client(rawConnection, tlsConfig)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = rawConnection.Close()
		return nil, connectionError(ctx, err)
	}

	return &Connection{connection: tlsConnection}, nil
}

func establishStartTLS(ctx context.Context, rawConnection net.Conn, tlsConfig *tls.Config) (*Connection, error) {
	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConnection.SetDeadline(deadline)
	}

	reader := bufio.NewReader(rawConnection)
	writer := bufio.NewWriter(rawConnection)
	if _, err := reader.ReadString('\n'); err != nil {
		_ = rawConnection.Close()
		return nil, connectionError(ctx, err)
	}

	capabilities, err := imapCommand(reader, writer, "a001", "CAPABILITY")
	if err != nil || !strings.Contains(strings.ToUpper(capabilities), "STARTTLS") {
		_ = rawConnection.Close()
		return nil, errorCode(CodeTLSNegotiation)
	}

	if _, err := imapCommand(reader, writer, "a002", "STARTTLS"); err != nil {
		_ = rawConnection.Close()
		return nil, errorCode(CodeTLSNegotiation)
	}

	tlsConnection := tls.Client(rawConnection, tlsConfig)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = rawConnection.Close()
		return nil, connectionError(ctx, err)
	}
	_ = tlsConnection.SetDeadline(time.Time{})

	return &Connection{connection: tlsConnection}, nil
}

func imapCommand(reader *bufio.Reader, writer *bufio.Writer, tag, command string) (string, error) {
	if _, err := writer.WriteString(tag + " " + command + "\r\n"); err != nil {
		return "", err
	}
	if err := writer.Flush(); err != nil {
		return "", err
	}

	var responses strings.Builder
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")
		responses.WriteString(line)
		responses.WriteByte('\n')

		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if !strings.HasPrefix(line, tag+" OK ") && line != tag+" OK" {
			return "", fmt.Errorf("IMAP command rejected")
		}
		return responses.String(), nil
	}
}

func connectionError(ctx context.Context, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errorCode(CodeBridgeUnreachable)
	}
	if errors.Is(err, context.Canceled) {
		return errorCode(CodeBridgeUnreachable)
	}
	if strings.Contains(err.Error(), CodeTLSMismatch) {
		return errorCode(CodeTLSMismatch)
	}
	if _, ok := err.(tls.RecordHeaderError); ok {
		return errorCode(CodeBridgeUnreachable)
	}
	if strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls:") {
		return errorCode(CodeTLSMismatch)
	}
	return errorCode(CodeBridgeUnreachable)
}

package bridge

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

const (
	maxStartTLSGreetingBytes = 4 * 1024
	maxStartTLSResponseBytes = 16 * 1024
	maxStartTLSResponseCount = 32
	maxStartTLSLineBytes     = 4 * 1024
)

var (
	errStartTLSInputLimit = errors.New("STARTTLS input limit exceeded")
	errStartTLSProtocol   = errors.New("invalid STARTTLS plaintext response")
)

// Connection is a TLS-established connection to the configured local Bridge endpoint.
type Connection struct {
	connection net.Conn
	startTLS   bool
}

// Close releases the underlying network connection.
func (connection *Connection) Close() error {
	if connection == nil || connection.connection == nil {
		return nil
	}
	return connection.connection.Close()
}

// takeConn transfers exclusive ownership to the package-local IMAP facade.
// The prior Connection becomes inert, so its Close method cannot interrupt a
// session after ownership has moved.
func (connection *Connection) takeConn() (net.Conn, bool, error) {
	if connection == nil || connection.connection == nil {
		return nil, false, errorCode(CodeAdapterClosed)
	}

	transport := connection.connection
	startTLS := connection.startTLS
	connection.connection = nil

	return transport, startTLS, nil
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
	stopCancelWatcher := closeOnContext(ctx, rawConnection)
	defer stopCancelWatcher()

	reader := bufio.NewReaderSize(rawConnection, maxStartTLSLineBytes+1)
	writer := bufio.NewWriter(rawConnection)
	budget := &startTLSResponseBudget{}
	greeting, _, err := readStartTLSLine(reader, maxStartTLSGreetingBytes)
	if err != nil {
		_ = rawConnection.Close()
		if ctx.Err() != nil {
			return nil, connectionError(ctx, err)
		}
		if errors.Is(err, errStartTLSProtocol) {
			return nil, errorCode(CodeTLSNegotiation)
		}
		return nil, connectionError(ctx, err)
	}
	if !validStartTLSGreeting(greeting) {
		_ = rawConnection.Close()
		return nil, errorCode(CodeTLSNegotiation)
	}

	hasStartTLS, err := imapCommand(reader, writer, budget, "a001", "CAPABILITY")
	if err != nil {
		_ = rawConnection.Close()
		if ctx.Err() != nil {
			return nil, connectionError(ctx, err)
		}
		return nil, errorCode(CodeTLSNegotiation)
	}
	if !hasStartTLS {
		_ = rawConnection.Close()
		return nil, errorCode(CodeTLSNegotiation)
	}

	if _, err := imapCommand(reader, writer, budget, "a002", "STARTTLS"); err != nil {
		_ = rawConnection.Close()
		if ctx.Err() != nil {
			return nil, connectionError(ctx, err)
		}
		return nil, errorCode(CodeTLSNegotiation)
	}

	tlsConnection := tls.Client(rawConnection, tlsConfig)
	if err := tlsConnection.HandshakeContext(ctx); err != nil {
		_ = rawConnection.Close()
		return nil, connectionError(ctx, err)
	}
	_ = tlsConnection.SetDeadline(time.Time{})
	stopCancelWatcher()
	if ctx.Err() != nil {
		_ = tlsConnection.Close()
		return nil, connectionError(ctx, ctx.Err())
	}

	return &Connection{connection: tlsConnection, startTLS: true}, nil
}

func imapCommand(reader *bufio.Reader, writer *bufio.Writer, budget *startTLSResponseBudget, tag, command string) (bool, error) {
	if _, err := writer.WriteString(tag + " " + command + "\r\n"); err != nil {
		return false, err
	}
	if err := writer.Flush(); err != nil {
		return false, err
	}

	hasStartTLS := false
	for {
		line, rawBytes, err := readStartTLSLine(reader, maxStartTLSLineBytes)
		if err != nil {
			return false, err
		}
		if err := budget.add(rawBytes); err != nil {
			return false, err
		}
		if capabilityHasStartTLS(line) {
			hasStartTLS = true
		}

		if !strings.HasPrefix(line, tag+" ") {
			continue
		}
		if !strings.HasPrefix(line, tag+" OK ") && line != tag+" OK" {
			return false, fmt.Errorf("IMAP command rejected")
		}
		return hasStartTLS, nil
	}
}

type startTLSResponseBudget struct {
	bytes int
	count int
}

func (budget *startTLSResponseBudget) add(rawBytes int) error {
	budget.count++
	budget.bytes += rawBytes
	if budget.count > maxStartTLSResponseCount || budget.bytes > maxStartTLSResponseBytes {
		return errStartTLSInputLimit
	}

	return nil
}

func readStartTLSLine(reader *bufio.Reader, limit int) (string, int, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return "", 0, errStartTLSInputLimit
		}
		return "", 0, err
	}
	if len(line) > limit {
		return "", 0, errStartTLSInputLimit
	}
	if len(line) < 2 || line[len(line)-2] != '\r' || line[len(line)-1] != '\n' {
		return "", 0, errStartTLSProtocol
	}
	content := line[:len(line)-2]
	for _, value := range content {
		if value < 0x20 || value > 0x7e {
			return "", 0, errStartTLSProtocol
		}
	}

	return string(content), len(line), nil
}

func validStartTLSGreeting(line string) bool {
	const prefix = "* OK "
	if len(line) < len(prefix) || line[0] != '*' || line[1] != ' ' || !strings.EqualFold(line[2:4], "OK") || line[4] != ' ' {
		return false
	}
	responseText := line[len(prefix):]
	if responseText == "" || responseText[0] != '[' {
		return true
	}
	closing := strings.IndexByte(responseText, ']')
	if closing < 2 || closing+1 >= len(responseText) || responseText[closing+1] != ' ' {
		return false
	}
	code := responseText[1:closing]
	atom := code
	if separator := strings.IndexByte(code, ' '); separator >= 0 {
		atom = code[:separator]
		if separator == len(code)-1 {
			return false
		}
	}
	return validIMAPAtom(atom)
}

func validIMAPAtom(atom string) bool {
	if atom == "" {
		return false
	}
	for _, value := range []byte(atom) {
		if value < 0x21 || value > 0x7e || strings.ContainsRune("(){%*\"\\]", rune(value)) {
			return false
		}
	}
	return true
}

func capabilityHasStartTLS(line string) bool {
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "*" || !strings.EqualFold(fields[1], "CAPABILITY") {
		return false
	}

	for _, capability := range fields[2:] {
		if strings.EqualFold(capability, "STARTTLS") {
			return true
		}
	}

	return false
}

func closeOnContext(ctx context.Context, connection net.Conn) func() {
	stop := make(chan struct{})
	exited := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		defer close(exited)
		select {
		case <-ctx.Done():
			_ = connection.Close()
		case <-stop:
		}
	}()

	return func() {
		stopOnce.Do(func() {
			close(stop)
			<-exited
		})
	}
}

func connectionError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return errorCode(CodeBridgeUnreachable)
	}
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

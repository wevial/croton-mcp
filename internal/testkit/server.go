// Package testkit provides deterministic, loopback-only IMAP test fixtures.
package testkit

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

const serverName = "fake-imap.test"

const syntheticMessageHeader = "From: Croton Fixture <fixture@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Welcome to Croton\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\nMessage-ID: <welcome@croton.test>\r\n\r\n"

const syntheticMessageBody = syntheticMessageHeader + "Welcome to Croton. This synthetic message exists only for adapter tests.\r\n"

// TLSMode selects how a fake IMAP server negotiates TLS.
type TLSMode uint8

const (
	// StartTLS accepts plaintext IMAP until a client issues STARTTLS.
	StartTLS TLSMode = iota
	// ImplicitTLS requires TLS before it sends the IMAP greeting.
	ImplicitTLS
)

// String returns the stable name of a TLS mode.
func (mode TLSMode) String() string {
	switch mode {
	case StartTLS:
		return "starttls"
	case ImplicitTLS:
		return "implicit-tls"
	default:
		return fmt.Sprintf("unknown-%d", mode)
	}
}

// Scenario controls deterministic protocol faults for adapter tests.
type Scenario struct {
	// Greeting replaces the normal initial IMAP greeting.
	Greeting string
	// RejectAuthentication makes LOGIN and AUTHENTICATE return a tagged NO response.
	RejectAuthentication bool
	// ResponseDelay waits before every server response.
	ResponseDelay time.Duration
	// DisconnectAfterCommand closes the first matching connection after this
	// connection-local command number, allowing reconnect-once tests to recover.
	DisconnectAfterCommand int
	// MalformedResponse replaces normal command responses with this literal wire response.
	MalformedResponse string
	// OversizedResponseBytes emits an untagged response of this size before normal responses.
	OversizedResponseBytes int
	// OversizedResponseCommand limits an oversized response to a command name.
	// An empty value emits it before every command.
	OversizedResponseCommand string
	// IgnoreBodyPartial makes UID FETCH return the complete body even when the
	// client requested a BODY.PEEK partial range.
	IgnoreBodyPartial bool
	// OversizedLiteralBytes replaces a normal FETCH literal with a deterministic
	// payload of this size.
	OversizedLiteralBytes int
	// OversizedBodyLiteralBytes replaces a body FETCH literal without changing
	// metadata FETCH literals used to obtain an opaque message ID.
	OversizedBodyLiteralBytes int
	// UIDValidityAfterExamine changes the mailbox generation after the first
	// EXAMINE, simulating a mailbox rebuild between opaque-ID operations.
	UIDValidityAfterExamine uint32
	// UIDNext replaces the mailbox UIDNEXT returned by EXAMINE when non-zero.
	UIDNext uint32
	// ListResponseCount controls the number of synthetic folders in a LIST response.
	ListResponseCount int
	// ListMailboxLiteralBytes replaces the first LIST mailbox name with a literal
	// of this size. Zero preserves the normal quoted mailbox name.
	ListMailboxLiteralBytes int
	// SearchResponse replaces the untagged response to UID SEARCH. It must be a
	// complete untagged SEARCH or ESEARCH response without a trailing CRLF.
	SearchResponse string
	// SearchWindowResult returns the highest requested UID from every search window.
	SearchWindowResult bool
	// StatusResponse replaces the normal untagged STATUS response. It must be a
	// complete untagged STATUS response without a trailing CRLF.
	StatusResponse string
	// OmitStatusResponse sends only the tagged STATUS completion.
	OmitStatusResponse bool
	// FetchResponseSection replaces the BODY section label returned by UID FETCH.
	FetchResponseSection string
	// BodyFetchResponseSection replaces only whole-body FETCH response labels.
	BodyFetchResponseSection string
	// FetchResponseUID replaces the UID in every metadata FETCH response.
	// Zero preserves the requested UID.
	FetchResponseUID uint32
	// BodyFetchResponseUID replaces only whole-body FETCH response UIDs.
	// Zero preserves the requested UID.
	BodyFetchResponseUID uint32
	// DuplicateFetchSection emits the requested FETCH body section twice.
	DuplicateFetchSection bool
	// DuplicateBodyFetchSection emits only whole-body FETCH sections twice.
	DuplicateBodyFetchSection bool
	// FetchLiteralDeclaredBytes replaces a FETCH literal declaration without
	// changing its payload. Zero preserves the payload length declaration.
	FetchLiteralDeclaredBytes int
	// BodyFetchLiteralDeclaredBytes replaces only whole-body FETCH declarations.
	BodyFetchLiteralDeclaredBytes int
	// UnexpectedFetchBinaryLiteralBytes appends an unrequested BINARY[] literal
	// to every FETCH response. Zero preserves the requested response shape.
	UnexpectedFetchBinaryLiteralBytes int
	// UnexpectedBodyFetchBinaryLiteralBytes appends an unrequested BINARY[]
	// literal only to whole-body FETCH responses.
	UnexpectedBodyFetchBinaryLiteralBytes int
}

// Options configures a fake server. The zero value uses STARTTLS without faults.
type Options struct {
	Mode     TLSMode
	Scenario Scenario
}

// Command is one client command observed by a Server.
type Command struct {
	Sequence     int
	ConnectionID int
	Raw          string
	Name         string
	TLS          bool
}

// Server is an in-process, loopback-only fake IMAP server for tests.
type Server struct {
	listener  net.Listener
	tlsConfig *tls.Config
	caDER     []byte
	leafDER   []byte
	spkiPin   [sha256.Size]byte
	options   Options
	done      chan struct{}

	mu               sync.Mutex
	closed           bool
	disconnectUsed   bool
	examineCount     int
	nextConnectionID int
	commands         []Command
	connections      map[net.Conn]struct{}
	closeOnce        sync.Once
	closeErr         error
	wg               sync.WaitGroup
}

// Start creates an ephemeral CA and leaf certificate, then begins serving on 127.0.0.1:0.
func Start(options Options) (*Server, error) {
	if options.Mode != StartTLS && options.Mode != ImplicitTLS {
		return nil, fmt.Errorf("testkit: unsupported TLS mode %d", options.Mode)
	}

	if options.Scenario.ResponseDelay < 0 {
		return nil, errors.New("testkit: response delay must not be negative")
	}

	if options.Scenario.OversizedResponseBytes < 0 {
		return nil, errors.New("testkit: oversized response size must not be negative")
	}

	if options.Scenario.OversizedLiteralBytes < 0 {
		return nil, errors.New("testkit: oversized literal size must not be negative")
	}

	if options.Scenario.OversizedBodyLiteralBytes < 0 {
		return nil, errors.New("testkit: oversized body literal size must not be negative")
	}

	if options.Scenario.ListResponseCount < 0 {
		return nil, errors.New("testkit: list response count must not be negative")
	}

	if options.Scenario.ListMailboxLiteralBytes < 0 {
		return nil, errors.New("testkit: list mailbox literal size must not be negative")
	}

	if options.Scenario.FetchLiteralDeclaredBytes < 0 {
		return nil, errors.New("testkit: fetch literal declaration must not be negative")
	}

	if options.Scenario.BodyFetchLiteralDeclaredBytes < 0 {
		return nil, errors.New("testkit: body fetch literal declaration must not be negative")
	}

	if options.Scenario.UnexpectedFetchBinaryLiteralBytes < 0 {
		return nil, errors.New("testkit: unexpected FETCH binary literal size must not be negative")
	}

	if options.Scenario.UnexpectedBodyFetchBinaryLiteralBytes < 0 {
		return nil, errors.New("testkit: unexpected body FETCH binary literal size must not be negative")
	}

	certificate, caDER, err := generateCertificate()
	if err != nil {
		return nil, err
	}

	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("testkit: parse generated leaf certificate: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("testkit: listen on loopback: %w", err)
	}

	server := &Server{
		listener: listener,
		tlsConfig: &tls.Config{
			Certificates: []tls.Certificate{certificate},
			MinVersion:   tls.VersionTLS12,
		},
		caDER:       caDER,
		leafDER:     certificate.Certificate[0],
		spkiPin:     sha256.Sum256(leaf.RawSubjectPublicKeyInfo),
		options:     options,
		done:        make(chan struct{}),
		connections: make(map[net.Conn]struct{}),
	}

	server.wg.Add(1)
	go server.serve()

	return server, nil
}

// Addr returns the loopback listener address.
func (server *Server) Addr() string {
	return server.listener.Addr().String()
}

// CAPEM returns a copy of this server's generated CA certificate in PEM form.
func (server *Server) CAPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.caDER})
}

// LeafPEM returns a copy of this server's generated leaf certificate in PEM form.
func (server *Server) LeafPEM() []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.leafDER})
}

// SPKISHA256 returns the lowercase hexadecimal SHA-256 pin of the leaf certificate's public key.
func (server *Server) SPKISHA256() string {
	return hex.EncodeToString(server.spkiPin[:])
}

// ClientTLSConfig returns an explicit trust configuration for this server's generated CA.
func (server *Server) ClientTLSConfig() *tls.Config {
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(server.CAPEM())

	return &tls.Config{
		RootCAs:    roots,
		ServerName: serverName,
		MinVersion: tls.VersionTLS12,
	}
}

// Commands returns a copy of all client commands recorded in receive order.
func (server *Server) Commands() []Command {
	server.mu.Lock()
	defer server.mu.Unlock()

	return append([]Command(nil), server.commands...)
}

// AssertNoInsecureAuthentication reports LOGIN or AUTHENTICATE commands received before TLS.
func (server *Server) AssertNoInsecureAuthentication() error {
	for _, command := range server.Commands() {
		if !command.TLS && isAuthentication(command.Name) {
			return fmt.Errorf("testkit: insecure %s command at sequence %d", command.Name, command.Sequence)
		}
	}

	return nil
}

// AssertReadOnlyCommands reports commands outside the explicit read-only allowlist
// and message-content fetches that can set the \Seen flag.
func (server *Server) AssertReadOnlyCommands() error {
	var violations []string
	for _, command := range server.Commands() {
		name := effectiveCommandName(command)
		if !isReadOnlyCommand(name) || isUnsafeBodyFetch(command) {
			violations = append(violations, fmt.Sprintf("%s at sequence %d", name, command.Sequence))
		}
	}

	if len(violations) == 0 {
		return nil
	}

	return fmt.Errorf("testkit: non-read-only commands: %s", strings.Join(violations, ", "))
}

// Close stops the listener, closes active client connections, and waits for handlers to exit.
func (server *Server) Close() error {
	server.closeOnce.Do(func() {
		close(server.done)

		server.mu.Lock()
		server.closed = true
		connections := make([]net.Conn, 0, len(server.connections))
		for connection := range server.connections {
			connections = append(connections, connection)
		}
		server.mu.Unlock()

		server.closeErr = server.listener.Close()
		for _, connection := range connections {
			_ = connection.Close()
		}
		server.wg.Wait()
	})

	return server.closeErr
}

func (server *Server) serve() {
	defer server.wg.Done()

	for {
		connection, err := server.listener.Accept()
		if err != nil {
			return
		}

		server.mu.Lock()
		if server.closed {
			server.mu.Unlock()
			_ = connection.Close()
			return
		}
		server.connections[connection] = struct{}{}
		server.nextConnectionID++
		connectionID := server.nextConnectionID
		server.wg.Add(1)
		server.mu.Unlock()

		go func() {
			defer server.wg.Done()
			defer server.untrack(connection)
			server.handle(connection, connectionID)
		}()
	}
}

func (server *Server) untrack(connection net.Conn) {
	server.mu.Lock()
	defer server.mu.Unlock()
	delete(server.connections, connection)
}

func (server *Server) handle(rawConnection net.Conn, connectionID int) {
	connection := rawConnection
	tlsEstablished := false
	if server.options.Mode == ImplicitTLS {
		tlsConnection := tls.Server(rawConnection, server.tlsConfig)
		if err := tlsConnection.Handshake(); err != nil {
			return
		}
		connection = tlsConnection
		tlsEstablished = true
	}

	defer connection.Close()

	reader := bufio.NewReader(connection)
	writer := bufio.NewWriter(connection)

	greeting := "* OK fake IMAP server ready"
	if server.options.Scenario.Greeting != "" {
		greeting = server.options.Scenario.Greeting
	}
	if err := server.writeLine(writer, greeting); err != nil {
		return
	}

	authenticated := false
	connectionCommands := 0
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		raw := strings.TrimRight(line, "\r\n")
		tag, name := parseCommand(raw)

		server.record(raw, name, tlsEstablished, connectionID)
		connectionCommands++

		if server.shouldDisconnect(connectionCommands) {
			return
		}
		if server.options.Scenario.MalformedResponse != "" {
			if err := server.writeLine(writer, server.options.Scenario.MalformedResponse); err != nil {
				return
			}
			continue
		}

		if server.options.Scenario.OversizedResponseBytes > 0 && (server.options.Scenario.OversizedResponseCommand == "" || strings.EqualFold(server.options.Scenario.OversizedResponseCommand, name)) {
			size := server.options.Scenario.OversizedResponseBytes
			if err := server.writeLine(writer, "* OK "+strings.Repeat("X", size-5)); err != nil {
				return
			}
		}

		switch name {
		case "CAPABILITY":
			capabilities := "* CAPABILITY IMAP4rev1 AUTH=PLAIN"
			if !tlsEstablished {
				capabilities = "* CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED"
			}

			if !server.writeLines(writer, capabilities, tagged(tag, "OK CAPABILITY completed")) {
				return
			}
		case "STARTTLS":
			if tlsEstablished {
				if err := server.writeLine(writer, tagged(tag, "BAD TLS already established")); err != nil {
					return
				}
				continue
			}

			if err := server.writeLine(writer, tagged(tag, "OK Begin TLS negotiation")); err != nil {
				return
			}

			tlsConnection := tls.Server(rawConnection, server.tlsConfig)
			if err := tlsConnection.Handshake(); err != nil {
				return
			}

			connection = tlsConnection
			reader = bufio.NewReader(connection)
			writer = bufio.NewWriter(connection)
			tlsEstablished = true
		case "LOGIN":
			if !tlsEstablished {
				if err := server.writeLine(writer, tagged(tag, "NO TLS required")); err != nil {
					return
				}
				continue
			}

			if server.options.Scenario.RejectAuthentication {
				if err := server.writeLine(writer, tagged(tag, "NO authentication failed")); err != nil {
					return
				}
				continue
			}

			authenticated = true

			if err := server.writeLine(writer, tagged(tag, "OK LOGIN completed")); err != nil {
				return
			}
		case "AUTHENTICATE":
			if !tlsEstablished {
				if err := server.writeLine(writer, tagged(tag, "NO TLS required")); err != nil {
					return
				}
				continue
			}

			fields := strings.Fields(raw)
			if len(fields) < 3 || !strings.EqualFold(fields[2], "PLAIN") {
				if err := server.writeLine(writer, tagged(tag, "BAD unsupported authentication mechanism")); err != nil {
					return
				}
				continue
			}

			if server.options.Scenario.RejectAuthentication {
				if err := server.writeLine(writer, tagged(tag, "NO authentication failed")); err != nil {
					return
				}
				continue
			}

			response := ""
			if len(fields) >= 4 {
				response = fields[3]
			} else {
				if err := server.writeLine(writer, "+"); err != nil {
					return
				}

				responseLine, err := reader.ReadString('\n')
				if err != nil {
					return
				}
				response = strings.TrimSpace(responseLine)
			}

			if response == "*" {
				if err := server.writeLine(writer, tagged(tag, "BAD AUTHENTICATE cancelled")); err != nil {
					return
				}
				continue
			}

			decoded, err := base64.StdEncoding.DecodeString(response)
			parts := strings.Split(string(decoded), "\x00")

			if err != nil || len(parts) != 3 || parts[1] == "" || parts[2] == "" {
				if err := server.writeLine(writer, tagged(tag, "BAD invalid PLAIN response")); err != nil {
					return
				}
				continue
			}

			authenticated = true

			if err := server.writeLine(writer, tagged(tag, "OK AUTHENTICATE completed")); err != nil {
				return
			}
		case "LIST":
			if !authenticated {
				if err := server.writeLine(writer, tagged(tag, "NO authenticate first")); err != nil {
					return
				}
				continue
			}

			if !server.writeList(writer, tag) {
				return
			}
		case "STATUS":
			if !authenticated {
				if err := server.writeLine(writer, tagged(tag, "NO authenticate first")); err != nil {
					return
				}
				continue
			}

			statusResponse := "* STATUS \"INBOX\" (MESSAGES 2 UIDNEXT 103 UIDVALIDITY 9001 UNSEEN 1)"
			if server.options.Scenario.StatusResponse != "" {
				statusResponse = server.options.Scenario.StatusResponse
			}
			if server.options.Scenario.OmitStatusResponse {
				if !server.writeLines(writer, tagged(tag, "OK STATUS completed")) {
					return
				}
			} else if !server.writeLines(writer, statusResponse, tagged(tag, "OK STATUS completed")) {
				return
			}
		case "EXAMINE":
			if !authenticated {
				if err := server.writeLine(writer, tagged(tag, "NO authenticate first")); err != nil {
					return
				}
				continue
			}

			uidValidity := server.nextUIDValidity()
			uidNext := server.options.Scenario.UIDNext
			if uidNext == 0 {
				uidNext = 103
			}
			if !server.writeLines(writer, "* 2 EXISTS", fmt.Sprintf("* OK [UIDVALIDITY %d] fixture generation", uidValidity), fmt.Sprintf("* OK [UIDNEXT %d] fixture next UID", uidNext), tagged(tag, "OK [READ-ONLY] EXAMINE completed")) {
				return
			}
		case "UID":
			if !authenticated {
				if err := server.writeLine(writer, tagged(tag, "NO authenticate first")); err != nil {
					return
				}
				continue
			}

			fields := strings.Fields(raw)
			if len(fields) < 3 {
				if err := server.writeLine(writer, tagged(tag, "BAD incomplete UID command")); err != nil {
					return
				}
				continue
			}

			switch strings.ToUpper(fields[2]) {
			case "SEARCH":
				searchResponse := "* SEARCH"
				if strings.Contains(raw, "UID 3:102") {
					searchResponse = "* SEARCH 101"
				}
				if server.options.Scenario.SearchResponse != "" {
					searchResponse = strings.ReplaceAll(server.options.Scenario.SearchResponse, "{TAG}", tag)
				} else if server.options.Scenario.SearchWindowResult {
					searchResponse = fmt.Sprintf("* SEARCH %d", searchWindowEnd(raw))
				}
				if !server.writeLines(writer, searchResponse, tagged(tag, "OK SEARCH completed")) {
					return
				}
			case "FETCH":
				literal := syntheticMessageBody
				header := strings.Contains(strings.ToUpper(raw), "HEADER")
				if header {
					literal = syntheticMessageHeader
				}
				if server.options.Scenario.OversizedLiteralBytes > 0 {
					literal = strings.Repeat("x", server.options.Scenario.OversizedLiteralBytes)
				}
				if !header && server.options.Scenario.OversizedBodyLiteralBytes > 0 {
					literal = strings.Repeat("x", server.options.Scenario.OversizedBodyLiteralBytes)
				}
				if !header && !server.options.Scenario.IgnoreBodyPartial {
					literal = partialBodyLiteral(raw, literal)
				}
				section := fetchResponseSection(raw, header)
				if server.options.Scenario.FetchResponseSection != "" {
					section = server.options.Scenario.FetchResponseSection
				}
				if !header && server.options.Scenario.BodyFetchResponseSection != "" {
					section = server.options.Scenario.BodyFetchResponseSection
				}
				declaredBytes := server.options.Scenario.FetchLiteralDeclaredBytes
				if !header && server.options.Scenario.BodyFetchLiteralDeclaredBytes != 0 {
					declaredBytes = server.options.Scenario.BodyFetchLiteralDeclaredBytes
				}
				duplicate := server.options.Scenario.DuplicateFetchSection
				if !header && server.options.Scenario.DuplicateBodyFetchSection {
					duplicate = true
				}
				unexpectedBinaryLiteralBytes := server.options.Scenario.UnexpectedFetchBinaryLiteralBytes
				if !header && server.options.Scenario.UnexpectedBodyFetchBinaryLiteralBytes > 0 {
					unexpectedBinaryLiteralBytes = server.options.Scenario.UnexpectedBodyFetchBinaryLiteralBytes
				}
				responseUID := fetchResponseUID(raw)
				if server.options.Scenario.FetchResponseUID != 0 {
					responseUID = server.options.Scenario.FetchResponseUID
				}
				if !header && server.options.Scenario.BodyFetchResponseUID != 0 {
					responseUID = server.options.Scenario.BodyFetchResponseUID
				}
				if !server.writeFetch(writer, tag, responseUID, literal, section, declaredBytes, duplicate, unexpectedBinaryLiteralBytes) {
					return
				}
			default:
				if err := server.writeLine(writer, tagged(tag, "BAD unsupported UID command")); err != nil {
					return
				}
			}
		case "LOGOUT":
			server.writeLines(writer, "* BYE fake IMAP server logging out", tagged(tag, "OK LOGOUT completed"))
			return
		default:
			if err := server.writeLine(writer, tagged(tag, "BAD unsupported command")); err != nil {
				return
			}
		}
	}
}

func (server *Server) record(raw, name string, tlsEstablished bool, connectionID int) Command {
	server.mu.Lock()
	defer server.mu.Unlock()

	command := Command{
		Sequence:     len(server.commands) + 1,
		ConnectionID: connectionID,
		Raw:          raw,
		Name:         name,
		TLS:          tlsEstablished,
	}

	server.commands = append(server.commands, command)

	return command
}

func (server *Server) shouldDisconnect(connectionSequence int) bool {
	threshold := server.options.Scenario.DisconnectAfterCommand
	if threshold <= 0 || connectionSequence < threshold {
		return false
	}

	server.mu.Lock()
	defer server.mu.Unlock()

	if server.disconnectUsed {
		return false
	}

	server.disconnectUsed = true

	return true
}

func (server *Server) nextUIDValidity() uint32 {
	server.mu.Lock()
	defer server.mu.Unlock()

	server.examineCount++
	if server.examineCount > 1 && server.options.Scenario.UIDValidityAfterExamine != 0 {
		return server.options.Scenario.UIDValidityAfterExamine
	}

	return 9001
}

func (server *Server) writeLines(writer *bufio.Writer, lines ...string) bool {
	for _, line := range lines {
		if err := server.writeLine(writer, line); err != nil {
			return false
		}
	}

	return true
}

func (server *Server) writeList(writer *bufio.Writer, tag string) bool {
	count := server.options.Scenario.ListResponseCount
	if count == 0 {
		count = 1
	}
	if server.options.Scenario.ListMailboxLiteralBytes > 0 {
		literal := strings.Repeat("x", server.options.Scenario.ListMailboxLiteralBytes)
		response := fmt.Sprintf("* LIST (\\HasNoChildren) \"/\" {%d}\r\n%s\r\n%s OK LIST completed\r\n", len(literal), literal, tag)
		if _, err := writer.WriteString(response); err != nil {
			return false
		}

		return writer.Flush() == nil
	}

	lines := make([]string, 0, count+1)
	for index := range count {
		mailbox := "INBOX"
		if index > 0 {
			mailbox = fmt.Sprintf("Archive-%d", index)
		}
		lines = append(lines, fmt.Sprintf("* LIST (\\HasNoChildren) \"/\" \"%s\"", mailbox))
	}
	lines = append(lines, tagged(tag, "OK LIST completed"))

	return server.writeLines(writer, lines...)
}

func (server *Server) writeLine(writer *bufio.Writer, line string) error {
	if delay := server.options.Scenario.ResponseDelay; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-server.done:
			return net.ErrClosed
		}
	}

	line = strings.TrimRight(line, "\r\n") + "\r\n"

	if _, err := writer.WriteString(line); err != nil {
		return err
	}

	return writer.Flush()
}

func (server *Server) writeFetch(writer *bufio.Writer, tag string, uid uint32, literal, section string, declaredBytes int, duplicate bool, unexpectedBinaryLiteralBytes int) bool {
	if delay := server.options.Scenario.ResponseDelay; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-server.done:
			return false
		}
	}

	if declaredBytes == 0 {
		declaredBytes = len(literal)
	}

	items := fmt.Sprintf("BODY%s {%d}\r\n%s", section, declaredBytes, literal)
	if duplicate {
		items += fmt.Sprintf(" BODY%s {%d}\r\n%s", section, declaredBytes, literal)
	}
	if unexpectedBinaryLiteralBytes > 0 {
		unexpectedLiteral := strings.Repeat("x", unexpectedBinaryLiteralBytes)
		items += fmt.Sprintf(" BINARY[] {%d}\r\n%s", len(unexpectedLiteral), unexpectedLiteral)
	}

	response := fmt.Sprintf("* 1 FETCH (UID %d FLAGS () INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" RFC822.SIZE %d %s)\r\n%s OK FETCH completed\r\n", uid, len(syntheticMessageBody), items, tag)
	if _, err := writer.WriteString(response); err != nil {
		return false
	}

	return writer.Flush() == nil
}

func fetchResponseSection(raw string, header bool) string {
	if header {
		return "[HEADER]"
	}

	upper := strings.ToUpper(raw)
	const marker = "BODY.PEEK[]<"
	start := strings.Index(upper, marker)
	if start < 0 {
		return "[]"
	}
	start += len(marker)
	end := strings.IndexByte(upper[start:], '.')
	if end < 0 {
		return "[]"
	}

	return "[]<" + upper[start:start+end] + ">"
}

func searchWindowEnd(raw string) uint32 {
	for _, field := range strings.Fields(raw) {
		start, end, ok := strings.Cut(field, ":")
		if !ok || start == "" || end == "" {
			continue
		}

		value, err := strconv.ParseUint(end, 10, 32)
		if err == nil && value > 0 {
			return uint32(value)
		}
	}

	return 1
}

func fetchResponseUID(raw string) uint32 {
	fields := strings.Fields(raw)
	for index := range fields {
		if !strings.EqualFold(fields[index], "FETCH") || index+1 >= len(fields) {
			continue
		}

		value, err := strconv.ParseUint(strings.Trim(fields[index+1], "(),"), 10, 32)
		if err == nil && value > 0 {
			return uint32(value)
		}
	}

	return 101
}

func partialBodyLiteral(raw, literal string) string {
	const marker = "BODY.PEEK[]<0."

	start := strings.Index(strings.ToUpper(raw), marker)
	if start < 0 {
		return literal
	}
	end := strings.IndexByte(raw[start+len(marker):], '>')
	if end < 0 {
		return literal
	}
	size, err := strconv.Atoi(raw[start+len(marker) : start+len(marker)+end])
	if err != nil || size < 0 || size >= len(literal) {
		return literal
	}

	return literal[:size]
}

func generateCertificate() (tls.Certificate, []byte, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("testkit: generate CA key: %w", err)
	}

	caTemplate, err := certificateTemplate("fake-imap-test-ca", true)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	caTemplate.KeyUsage = x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature

	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("testkit: create CA certificate: %w", err)
	}

	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("testkit: generate leaf key: %w", err)
	}

	leafTemplate, err := certificateTemplate(serverName, false)
	if err != nil {
		return tls.Certificate{}, nil, err
	}

	leafTemplate.DNSNames = []string{serverName}
	leafTemplate.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
	leafTemplate.KeyUsage = x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment
	leafTemplate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}

	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, caTemplate, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, nil, fmt.Errorf("testkit: create leaf certificate: %w", err)
	}

	return tls.Certificate{Certificate: [][]byte{leafDER, caDER}, PrivateKey: leafKey}, caDER, nil
}

func certificateTemplate(commonName string, isCA bool) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("testkit: generate certificate serial: %w", err)
	}

	now := time.Now()

	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: commonName},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  isCA,
	}, nil
}

func parseCommand(raw string) (string, string) {
	fields := strings.Fields(raw)
	if len(fields) == 0 {
		return "*", ""
	}

	if len(fields) == 1 {
		return fields[0], ""
	}

	return fields[0], strings.ToUpper(fields[1])
}

func tagged(tag, response string) string {
	return tag + " " + response
}

func isAuthentication(name string) bool {
	return name == "LOGIN" || name == "AUTHENTICATE"
}

func effectiveCommandName(command Command) string {
	if command.Name != "UID" {
		return command.Name
	}

	fields := strings.Fields(command.Raw)
	if len(fields) < 3 {
		return command.Name
	}

	return "UID " + strings.ToUpper(fields[2])
}

func isReadOnlyCommand(name string) bool {
	if strings.HasPrefix(name, "UID ") {
		switch strings.TrimPrefix(name, "UID ") {
		case "FETCH", "SEARCH", "SORT", "THREAD":
			return true
		default:
			return false
		}
	}

	// Fail closed: adding another command to Croton requires an explicit decision
	// that it cannot change mailbox, subscription, metadata, quota, or auth state.
	switch name {
	case "CAPABILITY", "STARTTLS", "LOGIN", "AUTHENTICATE", "LOGOUT", "NOOP", "ID", "ENABLE",
		"LIST", "LSUB", "STATUS", "NAMESPACE", "EXAMINE", "SEARCH", "FETCH", "SORT", "THREAD",
		"GETACL", "LISTRIGHTS", "MYRIGHTS", "GETQUOTA", "GETQUOTAROOT", "GETMETADATA", "GETANNOTATION",
		"IDLE", "UNSELECT":
		return true
	default:
		return false
	}
}

func isUnsafeBodyFetch(command Command) bool {
	name := strings.TrimPrefix(effectiveCommandName(command), "UID ")
	if name != "FETCH" {
		return false
	}

	upper := strings.ToUpper(command.Raw)

	// Remove each explicitly non-marking fetch item before looking for any
	// remaining body fetch. A command that mixes BODY[] with BODY.PEEK[] must
	// still be rejected.
	upper = strings.ReplaceAll(upper, "BODY.PEEK[", "")
	upper = strings.ReplaceAll(upper, "BINARY.PEEK[", "")
	upper = strings.ReplaceAll(upper, "RFC822.SIZE", "")
	upper = strings.ReplaceAll(upper, "RFC822.HEADER", "")

	return strings.Contains(upper, "BODY[") ||
		strings.Contains(upper, "BODY<") ||
		strings.Contains(upper, "BINARY[") ||
		strings.Contains(upper, "RFC822")
}

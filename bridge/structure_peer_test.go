package bridge

import (
	"bufio"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// Bounds of the private structure peer. They limit the test fixture only.
const (
	structurePeerLineBytes = 4096
	structurePeerCommands  = 32
	structurePeerTimeout   = 30 * time.Second
	structurePeerUID       = 7
)

// structurePeerScript is the post-TLS conversation of one dedicated session.
// Consumed frames are FLAGS-only FETCH responses the quarantine must swallow;
// structure is the complete target FETCH response, CRLF included, which may
// carry literals the public testkit cannot render. adverse, when set,
// replaces the reply to one command with a private schedule.
type structurePeerScript struct {
	greeting     bool
	beforeSearch []string
	duringFetch  []string
	structure    string
	adverse      *structurePeerAdverse
}

// structurePeerKind is what the quarantine must do with one scheduled part.
type structurePeerKind int

const (
	peerApproved structurePeerKind = iota
	peerConsumed
	peerRefused
)

// structurePeerPart is one response, or the sent part of one, of an adverse
// schedule.
type structurePeerPart struct {
	kind structurePeerKind
	text string
}

// structurePeerWrite is one Write of an adverse schedule; its parts are
// coalesced into it. gate, when set, must close before the write.
type structurePeerWrite struct {
	gate  <-chan struct{}
	parts []structurePeerPart
}

// structurePeerAdverse replaces the reply to UID FETCH, or to UID SEARCH when
// search is set, with a bounded private schedule built from what the peer
// already sent. With complete set the peer then sends the tagged completion
// and continues; otherwise it sends nothing more and holds the connection
// open, or closes it when truncate is set, until it observes the client close
// the transport. commanded closes when the command arrives and sent after the
// last write.
type structurePeerAdverse struct {
	search    bool
	complete  bool
	truncate  bool
	schedule  func(sent structurePeerRecord) []structurePeerWrite
	commanded chan struct{}
	sent      chan struct{}
}

// structurePeer is a bounded scripted IMAP server for one connection. It
// records which bytes it sent as approved responses and which as consumed
// frames, so a test can compare the decoder-facing tap with them exactly.
type structurePeer struct {
	script structurePeerScript
	done   chan struct{}

	// waiting counts the command reads the peer has begun, so a test knows
	// every earlier response was written and recorded.
	waiting  structureCond
	awaiting int

	mu sync.Mutex
	structurePeerRecord
}

func newStructurePeer(script structurePeerScript) *structurePeer {
	return &structurePeer{script: script, done: make(chan struct{})}
}

// structureCond is a broadcast condition for the private taps: await
// rechecks ready after each change, so tests synchronize on channels and
// never on sleeps.
type structureCond struct {
	mu      sync.Mutex
	changed chan struct{}
}

func (cond *structureCond) update(change func()) {
	cond.mu.Lock()
	defer cond.mu.Unlock()

	change()
	if cond.changed != nil {
		close(cond.changed)
	}
	cond.changed = make(chan struct{})
}

// await returns once ready, called under the lock, reports true. It fails
// the test after the peer timeout instead of hanging.
func (cond *structureCond) await(t *testing.T, what string, ready func() bool) {
	t.Helper()

	timeout := time.After(structurePeerTimeout)
	for {
		cond.mu.Lock()
		if ready() {
			cond.mu.Unlock()
			return
		}
		if cond.changed == nil {
			cond.changed = make(chan struct{})
		}
		changed := cond.changed
		cond.mu.Unlock()

		select {
		case <-changed:
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

// structureTap records every byte the quarantine releases toward the client,
// including bytes returned with an error, and whether a decoder-facing Read
// is outstanding. When hold is set, a Read that fails closes held and waits
// for hold before it returns the failure to the client.
type structureTap struct {
	net.Conn
	structureCond
	seen    []byte
	pending int
	failure error

	hold chan struct{}
	held chan struct{}
}

func (tap *structureTap) Read(buffer []byte) (int, error) {
	tap.update(func() { tap.pending++ })

	count, err := tap.Conn.Read(buffer)

	if err != nil && tap.hold != nil {
		tap.update(func() {
			if tap.failure == nil {
				tap.failure = err
				close(tap.held)
			}
		})
		<-tap.hold
	}
	tap.update(func() {
		tap.pending--
		tap.seen = append(tap.seen, buffer[:count]...)
		if err != nil && tap.failure == nil {
			tap.failure = err
		}
	})
	return count, err
}

func (tap *structureTap) bytes() []byte {
	tap.mu.Lock()
	defer tap.mu.Unlock()

	return append([]byte(nil), tap.seen...)
}

// awaitBytes waits until the tap has released exactly want with a Read
// outstanding again, so a later byte can only reach it through that Read.
func (tap *structureTap) awaitBytes(t *testing.T, want []byte) {
	t.Helper()

	tap.await(t, "decoder-facing bytes and a pending Read", func() bool {
		if len(tap.seen) > len(want) || !bytes.Equal(tap.seen, want[:len(tap.seen)]) {
			t.Fatalf("decoder-facing tap = %q, want a prefix of %q", tap.seen, want)
		}
		return len(tap.seen) == len(want) && tap.pending > 0
	})
}

// quarantineOf returns the quarantine below an installed tap.
func (tap *structureTap) quarantine(t *testing.T) *structureQuarantine {
	t.Helper()

	reader, ok := tap.Conn.(*quarantineConn)
	if !ok {
		t.Fatal("tap is not installed directly above the quarantine")
	}
	return reader.quarantine
}

// structureSourceTap sits below the quarantine on the transport and records
// every byte the guard consumed and whether a source Read is outstanding.
// When fail is set, the next Read that returns bytes returns them together
// with fail.
type structureSourceTap struct {
	net.Conn
	structureCond
	consumed []byte
	pending  int
	fail     error

	deadlines []time.Time
}

func (tap *structureSourceTap) Read(buffer []byte) (int, error) {
	tap.update(func() { tap.pending++ })

	count, err := tap.Conn.Read(buffer)

	tap.update(func() {
		tap.pending--
		tap.consumed = append(tap.consumed, buffer[:count]...)
		if count > 0 && err == nil && tap.fail != nil {
			err, tap.fail = tap.fail, nil
		}
	})
	return count, err
}

func (tap *structureSourceTap) SetDeadline(deadline time.Time) error {
	tap.update(func() { tap.deadlines = append(tap.deadlines, deadline) })
	return tap.Conn.SetDeadline(deadline)
}

func (tap *structureSourceTap) SetReadDeadline(deadline time.Time) error {
	tap.update(func() { tap.deadlines = append(tap.deadlines, deadline) })
	return tap.Conn.SetReadDeadline(deadline)
}

func (tap *structureSourceTap) SetWriteDeadline(deadline time.Time) error {
	tap.update(func() { tap.deadlines = append(tap.deadlines, deadline) })
	return tap.Conn.SetWriteDeadline(deadline)
}

// newStructureTapSeam returns the private seam and the tap it installs.
func newStructureTapSeam() (func(net.Conn) net.Conn, func() *structureTap) {
	var mu sync.Mutex
	var installed *structureTap

	seam := func(connection net.Conn) net.Conn {
		mu.Lock()
		defer mu.Unlock()

		installed = &structureTap{Conn: connection}
		return installed
	}
	return seam, func() *structureTap {
		mu.Lock()
		defer mu.Unlock()

		return installed
	}
}

// newHeldStructureTapSeam is newStructureTapSeam with a tap that closes held
// at its first failing Read and holds that Read until hold closes.
func newHeldStructureTapSeam(hold, held chan struct{}) (func(net.Conn) net.Conn, func() *structureTap) {
	var mu sync.Mutex
	var installed *structureTap

	seam := func(connection net.Conn) net.Conn {
		mu.Lock()
		defer mu.Unlock()

		installed = &structureTap{Conn: connection, hold: hold, held: held}
		return installed
	}
	current := func() *structureTap {
		mu.Lock()
		defer mu.Unlock()

		return installed
	}
	return seam, current
}

// serve runs the scripted conversation over connection and closes it.
func (peer *structurePeer) serve(connection net.Conn) {
	defer close(peer.done)
	defer connection.Close()

	_ = connection.SetDeadline(time.Now().Add(structurePeerTimeout))
	peer.fail(peer.converse(bufio.NewReaderSize(connection, structurePeerLineBytes+1), connection))
}

func (peer *structurePeer) converse(reader *bufio.Reader, writer net.Conn) error {
	if peer.script.greeting {
		if err := peer.send(writer, "* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] structure peer ready\r\n", true); err != nil {
			return err
		}
	}

	for count := 0; count < structurePeerCommands; count++ {
		peer.waiting.update(func() { peer.awaiting++ })
		line, err := readStructurePeerLine(reader)
		if err != nil {
			return err
		}
		tag, command, _ := strings.Cut(line, " ")
		name := strings.ToUpper(command)
		if strings.HasPrefix(name, "LOGIN ") {
			peer.record(tag + " LOGIN")
		} else {
			peer.record(line)
		}

		var responses []string
		switch {
		case name == "CAPABILITY":
			responses = []string{"* CAPABILITY IMAP4rev1 AUTH=PLAIN\r\n"}
		case strings.HasPrefix(name, "LOGIN "), name == "NOOP":
		case name == "EXAMINE INBOX" || name == `EXAMINE "INBOX"`:
			responses = []string{
				"* FLAGS (\\Seen \\Answered \\Flagged)\r\n",
				"* 3 EXISTS\r\n",
				"* 0 RECENT\r\n",
				"* OK [UIDVALIDITY 9001] structure generation\r\n",
				"* OK [UIDNEXT 104] structure next UID\r\n",
			}
		case strings.HasPrefix(name, "UID SEARCH "):
			if err := peer.sendConsumed(writer, peer.script.beforeSearch); err != nil {
				return err
			}
			if adverse := peer.script.adverse; adverse != nil && adverse.search {
				return peer.runAdverse(reader, writer, tag)
			}
			responses = []string{fmt.Sprintf("* SEARCH %d\r\n", structurePeerUID)}
		case strings.HasPrefix(name, "UID FETCH "):
			if adverse := peer.script.adverse; adverse != nil && !adverse.search {
				if !adverse.complete {
					return peer.runAdverse(reader, writer, tag)
				}
				if err := peer.writeSchedule(writer, nil); err != nil {
					return err
				}
				break
			}
			if err := peer.sendConsumed(writer, peer.script.duringFetch); err != nil {
				return err
			}
			responses = []string{peer.script.structure}
		case name == "LOGOUT":
			if err := peer.send(writer, "* BYE structure peer closing\r\n", true); err != nil {
				return err
			}
			return peer.send(writer, tag+" OK LOGOUT completed\r\n", true)
		default:
			return peer.send(writer, tag+" BAD unsupported\r\n", true)
		}

		for _, response := range responses {
			if err := peer.send(writer, response, true); err != nil {
				return err
			}
		}
		if err := peer.send(writer, tag+" OK completed\r\n", true); err != nil {
			return err
		}
	}

	return errors.New("structure peer command limit reached")
}

// runAdverse writes the adverse schedule while a concurrent reader observes
// client writes and the closure of the transport, then sends nothing more.
func (peer *structurePeer) runAdverse(reader *bufio.Reader, writer net.Conn, tag string) error {
	adverse := peer.script.adverse
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		peer.observeClosure(reader)
	}()

	// A write the closed transport refuses ends the schedule; it is the
	// expected outcome of a refusal, not a peer failure.
	_ = peer.writeSchedule(writer, closed)
	if adverse.truncate {
		_ = writer.Close()
	}

	<-closed
	return nil
}

// writeSchedule writes every scheduled Write in order. A gate is abandoned
// when closed, the observed closure, closes first.
func (peer *structurePeer) writeSchedule(writer net.Conn, closed <-chan struct{}) error {
	adverse := peer.script.adverse
	if adverse.commanded != nil {
		close(adverse.commanded)
	}
	if adverse.sent != nil {
		defer close(adverse.sent)
	}

	for _, write := range adverse.schedule(peer.snapshot()) {
		if write.gate != nil {
			select {
			case <-write.gate:
			case <-closed:
				return net.ErrClosed
			}
		}
		if err := peer.writeParts(writer, write.parts); err != nil {
			return err
		}
	}

	return nil
}

// writeParts coalesces parts into one Write and records each by kind.
func (peer *structurePeer) writeParts(writer net.Conn, parts []structurePeerPart) error {
	var output strings.Builder
	for _, part := range parts {
		output.WriteString(part.text)
	}
	if _, err := writer.Write([]byte(output.String())); err != nil {
		return err
	}

	peer.mu.Lock()
	defer peer.mu.Unlock()

	for _, part := range parts {
		peer.written += len(part.text)
		switch part.kind {
		case peerApproved:
			peer.responses++
			peer.approved = append(peer.approved, part.text...)
		case peerConsumed:
			peer.responses++
			peer.consumed = append(peer.consumed, part.text)
		default:
			peer.refused = append(peer.refused, part.text...)
		}
	}
	return nil
}

// observeClosure records bounded client bytes after the adverse command and
// whether the client closed the transport rather than the peer timing out.
func (peer *structurePeer) observeClosure(reader *bufio.Reader) {
	buffer := make([]byte, structurePeerLineBytes)
	for {
		count, err := reader.Read(buffer)

		peer.mu.Lock()
		if len(peer.trailing)+count <= structurePeerLineBytes {
			peer.trailing = append(peer.trailing, buffer[:count]...)
		}
		if err != nil {
			peer.closed = !errors.Is(err, os.ErrDeadlineExceeded)
		}
		peer.mu.Unlock()

		if err != nil {
			return
		}
	}
}

func (peer *structurePeer) sendConsumed(writer net.Conn, frames []string) error {
	for _, frame := range frames {
		if err := peer.send(writer, frame, false); err != nil {
			return err
		}
	}

	return nil
}

func (peer *structurePeer) send(writer net.Conn, response string, approved bool) error {
	if _, err := writer.Write([]byte(response)); err != nil {
		return err
	}

	peer.mu.Lock()
	defer peer.mu.Unlock()

	peer.written += len(response)
	peer.responses++
	if approved {
		peer.approved = append(peer.approved, response...)
	} else {
		peer.consumed = append(peer.consumed, response)
	}
	return nil
}

func (peer *structurePeer) record(command string) {
	peer.mu.Lock()
	defer peer.mu.Unlock()

	peer.commands = append(peer.commands, command)
}

func (peer *structurePeer) fail(err error) {
	peer.mu.Lock()
	defer peer.mu.Unlock()

	peer.err = err
}

// awaitCommand waits until the peer has answered every recorded command and
// is reading the next one.
func (peer *structurePeer) awaitCommand(t *testing.T) {
	t.Helper()

	commands := len(peer.snapshot().commands)
	peer.waiting.await(t, "the peer to finish its responses", func() bool {
		return peer.awaiting > commands
	})
}

// wait returns once the peer closed its connection, bounded by the peer
// timeout.
func (peer *structurePeer) wait(t *testing.T) {
	t.Helper()

	select {
	case <-peer.done:
	case <-time.After(structurePeerTimeout):
		t.Fatal("structure peer did not finish")
	}
}

type structurePeerRecord struct {
	plaintext []string
	commands  []string
	approved  []byte
	consumed  []string
	refused   []byte
	trailing  []byte
	closed    bool
	written   int
	responses int
	err       error
}

func (peer *structurePeer) snapshot() structurePeerRecord {
	peer.mu.Lock()
	defer peer.mu.Unlock()

	record := peer.structurePeerRecord
	record.plaintext = append([]string(nil), peer.plaintext...)
	record.commands = append([]string(nil), peer.commands...)
	record.approved = append([]byte(nil), peer.approved...)
	record.consumed = append([]string(nil), peer.consumed...)
	record.refused = append([]byte(nil), peer.refused...)
	record.trailing = append([]byte(nil), peer.trailing...)
	return record
}

func readStructurePeerLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadSlice('\n')
	if err != nil {
		return "", err
	}
	body, complete := strings.CutSuffix(string(line), "\r\n")
	if !complete || strings.ContainsAny(body, "\r\n") || strings.HasSuffix(body, "}") {
		return "", errors.New("structure peer received an unsupported command line")
	}

	return body, nil
}

// structureTLSPeer serves one verified loopback TLS connection, implicit or
// after a plaintext STARTTLS exchange, with a fresh synthetic leaf.
type structureTLSPeer struct {
	*structurePeer
	listener net.Listener
	pin      string
}

func startStructureTLSPeer(t *testing.T, startTLS bool, script structurePeerScript) *structureTLSPeer {
	t.Helper()

	certificate, pin := structurePeerCertificate(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	script.greeting = !startTLS
	peer := &structureTLSPeer{structurePeer: newStructurePeer(script), listener: listener, pin: pin}
	t.Cleanup(func() { _ = listener.Close() })

	config := &tls.Config{Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12}
	go func() {
		connection, err := listener.Accept()
		_ = listener.Close()
		if err != nil {
			peer.fail(err)
			close(peer.done)
			return
		}

		_ = connection.SetDeadline(time.Now().Add(structurePeerTimeout))
		if startTLS {
			if err := peer.negotiateStartTLS(connection); err != nil {
				peer.fail(err)
				_ = connection.Close()
				close(peer.done)
				return
			}
		}

		secured := tls.Server(connection, config)
		if err := secured.Handshake(); err != nil {
			peer.fail(err)
			_ = connection.Close()
			close(peer.done)
			return
		}
		peer.serve(secured)
	}()

	return peer
}

// negotiateStartTLS answers the plaintext greeting, CAPABILITY and STARTTLS
// that Dial consumes before the handshake. Nothing past STARTTLS is buffered.
func (peer *structureTLSPeer) negotiateStartTLS(connection net.Conn) error {
	reader := bufio.NewReaderSize(connection, structurePeerLineBytes+1)
	if _, err := connection.Write([]byte("* OK structure peer plaintext\r\n")); err != nil {
		return err
	}

	for _, expected := range []string{"CAPABILITY", "STARTTLS"} {
		line, err := readStructurePeerLine(reader)
		if err != nil {
			return err
		}
		tag, command, _ := strings.Cut(line, " ")
		if command != expected {
			return fmt.Errorf("plaintext command %q", command)
		}
		peer.mu.Lock()
		peer.plaintext = append(peer.plaintext, line)
		peer.mu.Unlock()

		response := tag + " OK begin TLS\r\n"
		if expected == "CAPABILITY" {
			response = "* CAPABILITY IMAP4rev1 STARTTLS LOGINDISABLED\r\n" + tag + " OK completed\r\n"
		}
		if _, err := connection.Write([]byte(response)); err != nil {
			return err
		}
	}
	if reader.Buffered() != 0 {
		return errors.New("plaintext bytes after STARTTLS")
	}

	return nil
}

// structurePeerCertificate is a fresh self-signed loopback server leaf and its
// SPKI pin.
func structurePeerCertificate(t *testing.T) (tls.Certificate, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatalf("generate serial: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "structure-peer.test"},
		DNSNames:              []string{"structure-peer.test"},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)

	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, hex.EncodeToString(pin[:])
}

// flatFetchTokens counts the quarantine's lexical tokens of a FETCH response
// whose strings contain no space, parenthesis or literal: each parenthesis
// and each atom, number or quoted string.
func flatFetchTokens(frame string) int {
	body := strings.TrimSuffix(frame, "\r\n")
	fields := strings.SplitN(body, " ", 4)
	tokens := 0
	inToken := false
	for _, value := range fields[3] {
		switch value {
		case '(', ')':
			tokens++
			inToken = false
		case ' ':
			inToken = false
		default:
			if !inToken {
				tokens++
			}
			inToken = true
		}
	}

	return tokens
}

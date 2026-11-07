package bridge

import (
	"bufio"
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
// carry literals the public testkit cannot render.
type structurePeerScript struct {
	greeting     bool
	beforeSearch []string
	duringFetch  []string
	structure    string
}

// structurePeer is a bounded scripted IMAP server for one connection. It
// records which bytes it sent as approved responses and which as consumed
// frames, so a test can compare the decoder-facing tap with them exactly.
type structurePeer struct {
	script structurePeerScript
	done   chan struct{}

	mu sync.Mutex
	structurePeerRecord
}

func newStructurePeer(script structurePeerScript) *structurePeer {
	return &structurePeer{script: script, done: make(chan struct{})}
}

// structureTap records every byte the quarantine releases toward the client.
type structureTap struct {
	net.Conn
	mu   sync.Mutex
	seen []byte
}

func (tap *structureTap) Read(buffer []byte) (int, error) {
	count, err := tap.Conn.Read(buffer)

	tap.mu.Lock()
	tap.seen = append(tap.seen, buffer[:count]...)
	tap.mu.Unlock()
	return count, err
}

func (tap *structureTap) bytes() []byte {
	tap.mu.Lock()
	defer tap.mu.Unlock()

	return append([]byte(nil), tap.seen...)
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
			responses = []string{fmt.Sprintf("* SEARCH %d\r\n", structurePeerUID)}
		case strings.HasPrefix(name, "UID FETCH "):
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

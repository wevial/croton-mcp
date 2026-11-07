package bridge

import (
	"bytes"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"
)

// errStructureEarlyClose is the injected result of a peer Write that
// delivered only part of its bytes after the client closed the transport.
var errStructureEarlyClose = errors.New("structure peer write after client close")

// structureEarlyCloseConn wraps the peer side of the secured connection. The
// Write whose bytes equal fault waits until the peer has observed the client
// close the transport, delivers half of them and returns that count with an
// error, the result a kernel may give once the client has closed.
type structureEarlyCloseConn struct {
	net.Conn
	fault    []byte
	observed chan struct{}
	once     sync.Once
}

func (conn *structureEarlyCloseConn) Read(buffer []byte) (int, error) {
	count, err := conn.Conn.Read(buffer)

	if err != nil && !errors.Is(err, os.ErrDeadlineExceeded) {
		conn.once.Do(func() { close(conn.observed) })
	}
	return count, err
}

func (conn *structureEarlyCloseConn) Write(buffer []byte) (int, error) {
	if !bytes.Equal(buffer, conn.fault) {
		return conn.Conn.Write(buffer)
	}

	select {
	case <-conn.observed:
	case <-time.After(structurePeerTimeout):
	}

	sent := len(buffer) / 2
	_, _ = conn.Conn.Write(buffer[:sent])
	return sent, errStructureEarlyClose
}

func TestStructureRefusalEarlyClose(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	var test structureRefusalCase
	for _, candidate := range structureRefusalCases() {
		if candidate.name == "unknown_attribute" {
			test = candidate
		}
	}
	if test.plan == nil {
		t.Fatal("no unknown_attribute refusal case")
	}

	// The guard refuses within the first refused fragment, so the second
	// fragment is written only after the client closed the transport.
	plan := test.plan(structurePeerRecord{})
	writes := fragmentRefusal(plan, false)
	if len(writes) != 3 || len(writes[0].parts[len(writes[0].parts)-1].text) < plan.cut {
		t.Fatalf("unknown_attribute fragments do not place the cut in the first Write")
	}
	fault := []byte(writes[1].parts[0].text)

	for _, mode := range []struct {
		name     string
		startTLS bool
		mode     string
	}{
		{name: "starttls_early_close", startTLS: true, mode: TLSModeStartTLS},
		{name: "implicit_tls_early_close", startTLS: false, mode: TLSModeImplicit},
	} {
		t.Run(mode.name, func(t *testing.T) {
			early := test
			early.wrap = func(connection net.Conn) net.Conn {
				return &structureEarlyCloseConn{Conn: connection, fault: fault, observed: make(chan struct{})}
			}
			runStructureTLSRefusal(t, mode.startTLS, mode.mode, early)
		})
	}
}

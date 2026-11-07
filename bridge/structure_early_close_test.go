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

// structureEarlyCloseConn wraps the peer side of the secured connection. The
// Write whose bytes equal fault waits until the guard has refused, with the
// client's close held at the decoder-facing tap, so the transport is still
// open; it then writes half of the bytes, releases the held close, waits
// until the peer observes the client close the transport and returns the
// count the transport reported together with an error. early reports that
// both barriers were reached rather than timed out.
type structureEarlyCloseConn struct {
	net.Conn
	fault    []byte
	held     <-chan struct{}
	hold     chan struct{}
	observed chan struct{}
	once     sync.Once

	early     bool
	delivered int
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

	refused := false
	select {
	case <-conn.held:
		refused = true
	case <-time.After(structurePeerTimeout):
	}

	conn.delivered, _ = conn.Conn.Write(buffer[:len(buffer)/2])
	close(conn.hold)

	select {
	case <-conn.observed:
		conn.early = refused
	case <-time.After(structurePeerTimeout):
	}
	return conn.delivered, errors.New("structure peer write after client close")
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
	// fragment is written only after the guard refused, and its Write
	// returns only after the client closed the transport.
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
			var faulted *structureEarlyCloseConn
			early := test
			early.hold, early.held = make(chan struct{}), make(chan struct{})
			early.wrap = func(connection net.Conn) net.Conn {
				faulted = &structureEarlyCloseConn{Conn: connection, fault: fault, held: early.held, hold: early.hold, observed: make(chan struct{})}
				return faulted
			}

			record := runStructureTLSRefusal(t, mode.startTLS, mode.mode, early)

			// The schedule really ended early: the faulted Write began after
			// the refusal, returned n>0 with an error only after the client
			// closed the transport, the peer
			// recorded exactly the delivered bytes, and the last fragment
			// was never attempted.
			if !faulted.early {
				t.Fatal("the faulted Write did not span the refusal and the observed client close")
			}
			if faulted.delivered != len(fault)/2 {
				t.Fatalf("faulted Write delivered %d of %d bytes", faulted.delivered, len(fault))
			}
			first := writes[0].parts[len(writes[0].parts)-1].text
			if want := first + string(fault[:faulted.delivered]); string(record.refused) != want {
				t.Fatalf("peer recorded %d refused bytes, want the %d delivered", len(record.refused), len(want))
			}
		})
	}
}

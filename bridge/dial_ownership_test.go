package bridge

import (
	"errors"
	"net"
	"testing"
	"time"
)

func TestConnectionTakeConnTransfersOwnershipExactlyOnce(t *testing.T) {
	client, peer := net.Pipe()
	t.Cleanup(func() { _ = peer.Close() })

	connection := &Connection{connection: client, startTLS: true}
	transferred, startTLS, err := connection.takeConn()
	if err != nil {
		t.Fatalf("takeConn: %v", err)
	}
	if !startTLS {
		t.Fatal("takeConn lost STARTTLS handoff marker")
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("old Connection.Close: %v", err)
	}
	if _, _, err := connection.takeConn(); CodeOf(err) != CodeAdapterClosed {
		t.Fatalf("second takeConn error = %v, want %q", err, CodeAdapterClosed)
	}

	read := make(chan error, 1)
	go func() {
		buffer := make([]byte, 1)
		_, err := peer.Read(buffer)
		read <- err
	}()
	if _, err := transferred.Write([]byte("x")); err != nil {
		t.Fatalf("write through transferred connection: %v", err)
	}
	select {
	case err := <-read:
		if err != nil {
			t.Fatalf("read after old Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("old Connection.Close closed the transferred transport")
	}
	if err := transferred.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
		t.Fatalf("close transferred connection: %v", err)
	}
}

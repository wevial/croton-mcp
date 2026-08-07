package bridge

import (
	"context"
	"errors"
	"net"
	"syscall"
	"testing"
)

func TestMapIMAPErrorClassifiesTransportFailuresForReplay(t *testing.T) {
	for _, err := range []error{
		syscall.ECONNRESET,
		syscall.EPIPE,
		&net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET},
	} {
		mapped := mapIMAPError(context.Background(), err)
		if CodeOf(mapped) != CodeBridgeUnreachable {
			t.Fatalf("mapIMAPError(%v) = %v, want %q", err, mapped, CodeBridgeUnreachable)
		}
		if !(&Adapter{}).canReplay(context.Background(), mapped) {
			t.Fatalf("mapIMAPError(%v) did not permit replay", err)
		}
	}
}

func TestMapIMAPErrorDoesNotClassifyNonTransportErrorsForReplay(t *testing.T) {
	mapped := mapIMAPError(context.Background(), errors.New("malformed IMAP response"))
	if CodeOf(mapped) != CodeIMAPProtocol {
		t.Fatalf("mapIMAPError(non-transport) = %v, want %q", mapped, CodeIMAPProtocol)
	}
	if (&Adapter{}).canReplay(context.Background(), mapped) {
		t.Fatalf("mapIMAPError(non-transport) unexpectedly permitted replay")
	}
}

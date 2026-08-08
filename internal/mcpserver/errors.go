package mcpserver

import "github.com/wevial/croton-mcp/bridge"

// mapAdapterError converts any adapter failure to one stable, secret-free
// tool error code. Unknown errors collapse to errInternal so that no wrapped
// detail can ever cross the protocol boundary.
func mapAdapterError(err error) string {
	switch bridge.CodeOf(err) {
	case bridge.CodeMailboxNotFound:
		return errNotFound
	case bridge.CodeStaleMessageID:
		return errStaleID
	case bridge.CodeBoundsExceeded:
		return errBoundsExceeded
	case bridge.CodeCommandTimedOut, bridge.CodeCredentialTimeout:
		return errTimedOut
	case bridge.CodeOperationCanceled:
		return errCanceled
	case bridge.CodeInvalidConfig, bridge.CodeInvalidEndpoint, bridge.CodeTLSRequired,
		bridge.CodeTLSMismatch, bridge.CodeTLSNegotiation, bridge.CodeCredentialCommand,
		bridge.CodeCredentialOutput, bridge.CodeCredentialOverflow, bridge.CodeBridgeUnreachable,
		bridge.CodeAuthentication, bridge.CodeIMAPCommand, bridge.CodeIMAPProtocol,
		bridge.CodeAdapterClosed:
		return errUnavailable
	default:
		return errInternal
	}
}

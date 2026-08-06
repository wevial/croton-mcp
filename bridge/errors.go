// Package bridge provides a bounded, read-only connection boundary for Proton Mail Bridge.
package bridge

import "errors"

const (
	CodeInvalidConfig      = "invalid_config"
	CodeInvalidEndpoint    = "invalid_endpoint"
	CodeTLSRequired        = "tls_trust_required"
	CodeTLSMismatch        = "tls_mismatch"
	CodeTLSNegotiation     = "tls_negotiation_failed"
	CodeCredentialCommand  = "credential_command_failed"
	CodeCredentialTimeout  = "credential_command_timed_out"
	CodeCredentialOutput   = "credential_command_invalid_output"
	CodeCredentialOverflow = "credential_command_output_too_large"
	CodeBridgeUnreachable  = "bridge_unreachable"
	CodeBoundsExceeded     = "bounds_exceeded"
)

// Error is a stable, secret-safe error exposed across the bridge boundary.
type Error struct {
	Code string
}

// Error returns only the stable code; it never includes credentials, commands, or peer material.
func (err *Error) Error() string {
	return err.Code
}

func errorCode(code string) error {
	return &Error{Code: code}
}

// CodeOf returns the stable code for a bridge error, or the empty string for an unknown error.
func CodeOf(err error) string {
	var bridgeError *Error
	if errors.As(err, &bridgeError) {
		return bridgeError.Code
	}

	return ""
}

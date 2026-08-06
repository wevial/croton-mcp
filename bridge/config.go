package bridge

import (
	"strings"
	"time"
)

const (
	TLSModeStartTLS = "starttls"
	TLSModeImplicit = "implicit"

	defaultHost           = "127.0.0.1"
	defaultPort           = 1143
	defaultConnectTimeout = 5 * time.Second
	defaultCommandTimeout = 5 * time.Second
	maxOperationTimeout   = time.Minute
)

// Config is the untrusted, in-memory connection configuration supplied to bridge.
// CredentialCommand is an exception: it is an operator-controlled privileged
// capability that executes the configured absolute argv verbatim.
type Config struct {
	IMAP   IMAPConfig  `json:"imap"`
	Bounds BoundsPatch `json:"bounds"`
	Audit  AuditConfig `json:"audit"`
}

// IMAPConfig configures the local Proton Mail Bridge endpoint.
type IMAPConfig struct {
	Host              string    `json:"host"`
	Port              int       `json:"port"`
	TLSMode           string    `json:"tlsMode"`
	CredentialCommand []string  `json:"credentialCommand"`
	TLS               TLSConfig `json:"tls"`
	ConnectTimeoutMs  int       `json:"connectTimeoutMs"`
	CommandTimeoutMs  int       `json:"commandTimeoutMs"`
}

// TLSConfig supplies explicit trust material for the Bridge certificate.
type TLSConfig struct {
	// TrustAnchorFile names one regular file containing the exact Bridge leaf
	// certificate PEM. Certificate-authority bundles are intentionally rejected.
	TrustAnchorFile string `json:"trustAnchorFile,omitempty"`
	// CertificateSHA256 is the lowercase hexadecimal SHA-256 pin of the
	// certificate's SubjectPublicKeyInfo, not of the complete certificate.
	CertificateSHA256 string `json:"certificateSha256,omitempty"`
	MinVersion        string `json:"minVersion,omitempty"`
}

// AuditConfig controls metadata-only audit logging in the MCP layer.
type AuditConfig struct {
	Enabled bool `json:"enabled"`
}

// ValidatedConfig has defaults and hard ceilings applied and is safe to use for connections.
type ValidatedConfig struct {
	IMAP   IMAPConfig
	Bounds Bounds
	Audit  AuditConfig
}

// ValidateConfig validates configuration without reading files or inheriting process configuration.
func ValidateConfig(input Config) (ValidatedConfig, error) {
	imap := input.IMAP
	if imap.Host == "" {
		imap.Host = defaultHost
	}
	if imap.Port == 0 {
		imap.Port = defaultPort
	}
	if _, err := ParseLoopbackEndpoint(imap.Host, imap.Port); err != nil {
		return ValidatedConfig{}, err
	}

	if imap.TLSMode == "" {
		imap.TLSMode = TLSModeStartTLS
	}
	if imap.TLSMode != TLSModeStartTLS && imap.TLSMode != TLSModeImplicit {
		return ValidatedConfig{}, errorCode(CodeInvalidConfig)
	}
	if !isSafeCredentialCommand(imap.CredentialCommand) {
		return ValidatedConfig{}, errorCode(CodeInvalidConfig)
	}
	if strings.TrimSpace(imap.TLS.TrustAnchorFile) == "" && strings.TrimSpace(imap.TLS.CertificateSHA256) == "" {
		return ValidatedConfig{}, errorCode(CodeTLSRequired)
	}
	if _, err := validateTLSConfig(imap.TLS); err != nil {
		return ValidatedConfig{}, err
	}

	var err error
	if imap.ConnectTimeoutMs, err = timeoutMilliseconds(imap.ConnectTimeoutMs, defaultConnectTimeout); err != nil {
		return ValidatedConfig{}, err
	}
	if imap.CommandTimeoutMs, err = timeoutMilliseconds(imap.CommandTimeoutMs, defaultCommandTimeout); err != nil {
		return ValidatedConfig{}, err
	}

	bounds, err := mergeBounds(input.Bounds)
	if err != nil {
		return ValidatedConfig{}, err
	}

	return ValidatedConfig{IMAP: imap, Bounds: bounds, Audit: input.Audit}, nil
}

func timeoutMilliseconds(value int, fallback time.Duration) (int, error) {
	if value == 0 {
		return int(fallback / time.Millisecond), nil
	}
	if value < 0 || value > int(maxOperationTimeout/time.Millisecond) {
		return 0, errorCode(CodeInvalidConfig)
	}
	return value, nil
}

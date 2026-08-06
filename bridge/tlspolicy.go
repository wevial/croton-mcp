package bridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"io"
	"os"
	"strings"
	"time"
)

const trustAnchorMaxBytes = 16 * 1024

// NewTLSConfig constructs the only TLS configuration used by bridge connections.
// Its verifier is installed atomically with InsecureSkipVerify so callers cannot
// accidentally pair IP-literal transport with omitted certificate verification.
func NewTLSConfig(input TLSConfig) (*tls.Config, error) {
	verifier, minimumVersion, err := newTLSVerifier(input)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		InsecureSkipVerify: true, // Verification is performed by VerifyConnection below.
		MinVersion:         minimumVersion,
		VerifyConnection:   verifier.verify,
	}, nil
}

type tlsVerifier struct {
	anchor *x509.Certificate
	pin    []byte
}

func newTLSVerifier(input TLSConfig) (*tlsVerifier, uint16, error) {
	if strings.TrimSpace(input.TrustAnchorFile) == "" && strings.TrimSpace(input.CertificateSHA256) == "" {
		return nil, 0, errorCode(CodeTLSRequired)
	}

	minimumVersion, err := parseTLSMinimumVersion(input.MinVersion)
	if err != nil {
		return nil, 0, err
	}

	verifier := &tlsVerifier{}
	if input.TrustAnchorFile != "" {
		anchor, err := loadTrustAnchor(input.TrustAnchorFile)
		if err != nil {
			return nil, 0, errorCode(CodeInvalidConfig)
		}
		verifier.anchor = anchor
	}

	if input.CertificateSHA256 != "" {
		pin, err := parseSPKIPin(input.CertificateSHA256)
		if err != nil {
			return nil, 0, err
		}
		verifier.pin = pin
	}

	return verifier, minimumVersion, nil
}

func loadTrustAnchor(path string) (*x509.Certificate, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, errorCode(CodeInvalidConfig)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errorCode(CodeInvalidConfig)
	}

	pemBytes, err := io.ReadAll(io.LimitReader(file, trustAnchorMaxBytes+1))
	if err != nil || len(pemBytes) > trustAnchorMaxBytes {
		return nil, errorCode(CodeInvalidConfig)
	}

	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		return nil, errorCode(CodeInvalidConfig)
	}

	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || certificate.IsCA {
		return nil, errorCode(CodeInvalidConfig)
	}

	return certificate, nil
}

func validateTLSConfig(input TLSConfig) (uint16, error) {
	if strings.TrimSpace(input.TrustAnchorFile) == "" && strings.TrimSpace(input.CertificateSHA256) == "" {
		return 0, errorCode(CodeTLSRequired)
	}

	version, err := parseTLSMinimumVersion(input.MinVersion)
	if err != nil {
		return 0, err
	}
	if input.CertificateSHA256 != "" {
		if _, err := parseSPKIPin(input.CertificateSHA256); err != nil {
			return 0, err
		}
	}

	return version, nil
}

func parseSPKIPin(value string) ([]byte, error) {
	pin, err := hex.DecodeString(value)
	if err != nil || len(pin) != sha256.Size || strings.ToLower(value) != value {
		return nil, errorCode(CodeInvalidConfig)
	}

	return pin, nil
}

func parseTLSMinimumVersion(value string) (uint16, error) {
	switch value {
	case "", "TLSv1.2":
		return tls.VersionTLS12, nil
	case "TLSv1.3":
		return tls.VersionTLS13, nil
	default:
		return 0, errorCode(CodeInvalidConfig)
	}
}

func (verifier *tlsVerifier) verify(state tls.ConnectionState) error {
	if len(state.PeerCertificates) == 0 {
		return errorCode(CodeTLSMismatch)
	}

	leaf := state.PeerCertificates[0]
	if verifier.anchor != nil {
		if subtle.ConstantTimeCompare(leaf.Raw, verifier.anchor.Raw) != 1 {
			return errorCode(CodeTLSMismatch)
		}
		if err := verifyLeafCertificate(leaf, time.Now()); err != nil {
			return errorCode(CodeTLSMismatch)
		}
	}

	if verifier.pin != nil {
		actual := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
		if subtle.ConstantTimeCompare(actual[:], verifier.pin) != 1 {
			return errorCode(CodeTLSMismatch)
		}
	}

	return nil
}

func verifyLeafCertificate(certificate *x509.Certificate, now time.Time) error {
	if now.Before(certificate.NotBefore) || now.After(certificate.NotAfter) {
		return errorCode(CodeTLSMismatch)
	}

	for _, usage := range certificate.ExtKeyUsage {
		if usage == x509.ExtKeyUsageServerAuth || usage == x509.ExtKeyUsageAny {
			return nil
		}
	}

	return errorCode(CodeTLSMismatch)
}

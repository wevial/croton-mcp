package bridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"os"
	"strings"
)

// NewTLSConfig constructs the only TLS configuration used by bridge connections.
// Its verifier is installed atomically with InsecureSkipVerify so callers cannot
// accidentally pair IP-literal transport with omitted identity verification.
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
	roots *x509.CertPool
	pin   []byte
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
		pemBytes, err := os.ReadFile(input.TrustAnchorFile)
		if err != nil {
			return nil, 0, errorCode(CodeInvalidConfig)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(pemBytes) {
			return nil, 0, errorCode(CodeInvalidConfig)
		}
		verifier.roots = roots
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
	if verifier.roots != nil {
		intermediates := x509.NewCertPool()
		for _, certificate := range state.PeerCertificates[1:] {
			intermediates.AddCert(certificate)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: verifier.roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
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

package bridge

import (
	"bytes"
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
	minimumVersion, err := input.validateTrustAndVersion()
	if err != nil {
		return nil, err
	}

	var verifier tlsVerifier
	if input.TrustAnchorFile != "" {
		verifier.anchor, err = loadTrustAnchorWith(input.TrustAnchorFile, trustAnchorFileSupported, openTrustAnchor)
		if err != nil {
			return nil, err
		}
	}

	verifier.pin, err = input.spkiPin()
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

type trustAnchorOpener func(string) (*os.File, error)

func loadTrustAnchorWith(path string, supported bool, open trustAnchorOpener) (*x509.Certificate, error) {
	if !supported {
		return nil, errorCode(CodeInvalidConfig)
	}

	file, err := open(path)
	if err != nil {
		return nil, errorCode(CodeInvalidConfig)
	}
	defer file.Close()

	pemBytes, err := io.ReadAll(io.LimitReader(file, trustAnchorMaxBytes+1))
	if err != nil || len(pemBytes) > trustAnchorMaxBytes {
		return nil, errorCode(CodeInvalidConfig)
	}

	block, rest := pem.Decode(pemBytes)
	if !bytes.HasPrefix(pemBytes, []byte("-----BEGIN CERTIFICATE-----")) || block == nil || block.Type != "CERTIFICATE" || len(rest) != 0 {
		return nil, errorCode(CodeInvalidConfig)
	}

	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil || certificate.IsCA {
		return nil, errorCode(CodeInvalidConfig)
	}

	return certificate, nil
}

func (input TLSConfig) spkiPin() ([]byte, error) {
	if input.SPKISHA256 == "" {
		return nil, nil
	}

	pin, err := hex.DecodeString(input.SPKISHA256)
	if err != nil || len(pin) != sha256.Size || strings.ToLower(input.SPKISHA256) != input.SPKISHA256 {
		return nil, errorCode(CodeInvalidConfig)
	}

	return pin, nil
}

func (input TLSConfig) validateTrustAndVersion() (uint16, error) {
	if strings.TrimSpace(input.TrustAnchorFile) == "" && strings.TrimSpace(input.SPKISHA256) == "" {
		return 0, errorCode(CodeTLSRequired)
	}

	switch input.MinVersion {
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
	if err := verifyLeafCertificate(leaf, time.Now()); err != nil {
		return errorCode(CodeTLSMismatch)
	}

	if verifier.anchor != nil && subtle.ConstantTimeCompare(leaf.Raw, verifier.anchor.Raw) != 1 {
		return errorCode(CodeTLSMismatch)
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

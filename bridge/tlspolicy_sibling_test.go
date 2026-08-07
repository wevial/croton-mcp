package bridge

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

func TestTLSVerifierRejectsSiblingLeafFromTrustedCA(t *testing.T) {
	ca, caKey := newTestCertificateAuthority(t)
	anchor := newTestLeafCertificate(t, ca, caKey, 2)
	sibling := newTestLeafCertificate(t, ca, caKey, 3)

	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := sibling.Verify(x509.VerifyOptions{
		Roots:   roots,
		DNSName: "fixture.test",
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageServerAuth,
		},
	}); err != nil {
		t.Fatalf("verify sibling with shared CA: %v", err)
	}

	verifier := &tlsVerifier{anchor: anchor}
	if err := verifier.verify(tls.ConnectionState{PeerCertificates: []*x509.Certificate{sibling}}); CodeOf(err) != CodeTLSMismatch {
		t.Fatalf("sibling leaf verification error = %v, want %q", err, CodeTLSMismatch)
	}
}

func newTestCertificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "fixture-ca.test"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	return certificate, key
}

func newTestLeafCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64) *x509.Certificate {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate leaf key: %v", err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: "fixture.test"},
		DNSNames:     []string{"fixture.test"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create leaf certificate: %v", err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf certificate: %v", err)
	}

	return certificate
}

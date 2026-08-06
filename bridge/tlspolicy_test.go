package bridge_test

import (
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func TestNewTLSConfigAlwaysInstallsCustomVerifier(t *testing.T) {
	t.Parallel()

	if _, err := bridge.NewTLSConfig(bridge.TLSConfig{}); bridge.CodeOf(err) != bridge.CodeTLSRequired {
		t.Fatalf("missing trust error = %v, want %q", err, bridge.CodeTLSRequired)
	}

	config, err := bridge.NewTLSConfig(bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)})
	if err != nil {
		t.Fatalf("NewTLSConfig: %v", err)
	}
	if !config.InsecureSkipVerify {
		t.Fatal("custom-verifier configuration did not disable incompatible hostname verification")
	}
	if config.VerifyConnection == nil {
		t.Fatal("custom-verifier configuration omitted VerifyConnection")
	}
	if config.MinVersion == 0 {
		t.Fatal("custom-verifier configuration omitted a TLS minimum version")
	}

	if _, err := bridge.NewTLSConfig(bridge.TLSConfig{CertificateSHA256: "not-a-pin"}); bridge.CodeOf(err) != bridge.CodeInvalidConfig {
		t.Fatalf("invalid pin error = %v, want %q", err, bridge.CodeInvalidConfig)
	}
}

package bridge_test

import (
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func TestValidateConfigMergesDefaultsAndClampsBounds(t *testing.T) {
	t.Parallel()

	config, err := bridge.ValidateConfig(bridge.Config{
		IMAP: bridge.IMAPConfig{
			CredentialCommand: []string{"/bin/true"},
			TLS: bridge.TLSConfig{
				CertificateSHA256: strings.Repeat("a", 64),
			},
		},
		Bounds: bridge.BoundsPatch{
			MaxSearchResults:   bridge.Int(300),
			MaxBodyBytes:       bridge.Int(128),
			MaxPreviewChars:    bridge.Int(0),
			MaxThreadFetches:   bridge.Int(99),
			MaxAttachmentCount: bridge.Int(101),
			MaxFolderResults:   bridge.Int(10),
			MaxOutputBytes:     bridge.Int(400001),
			MaxThreadMessages:  bridge.Int(101),
			MaxHeaderBytes:     bridge.Int(65537),
			MaxMimeParts:       bridge.Int(201),
		},
	})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}

	if got, want := config.IMAP.Host, "127.0.0.1"; got != want {
		t.Fatalf("default host = %q, want %q", got, want)
	}
	if got, want := config.IMAP.Port, 1143; got != want {
		t.Fatalf("default port = %d, want %d", got, want)
	}
	if got, want := config.IMAP.TLSMode, bridge.TLSModeStartTLS; got != want {
		t.Fatalf("default TLS mode = %q, want %q", got, want)
	}

	for _, test := range []struct {
		name string
		got  int
		want int
	}{
		{name: "search", got: config.Bounds.MaxSearchResults, want: 250},
		{name: "body", got: config.Bounds.MaxBodyBytes, want: 128},
		{name: "preview", got: config.Bounds.MaxPreviewChars, want: 0},
		{name: "thread fetches", got: config.Bounds.MaxThreadFetches, want: 99},
		{name: "attachments", got: config.Bounds.MaxAttachmentCount, want: 100},
		{name: "folders", got: config.Bounds.MaxFolderResults, want: 10},
		{name: "output", got: config.Bounds.MaxOutputBytes, want: 400000},
		{name: "thread messages", got: config.Bounds.MaxThreadMessages, want: 100},
		{name: "headers", got: config.Bounds.MaxHeaderBytes, want: 65536},
		{name: "MIME parts", got: config.Bounds.MaxMimeParts, want: 200},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if test.got != test.want {
				t.Fatalf("bound = %d, want %d", test.got, test.want)
			}
		})
	}
}

func TestValidateConfigRejectsMissingTrustRelativeCommandsAndUnboundedTimeouts(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name   string
		config bridge.Config
		code   string
	}{
		{
			name:   "missing trust",
			config: bridge.Config{IMAP: bridge.IMAPConfig{CredentialCommand: []string{"/bin/true"}}},
			code:   bridge.CodeTLSRequired,
		},
		{
			name: "relative command",
			config: bridge.Config{IMAP: bridge.IMAPConfig{
				CredentialCommand: []string{"pass"},
				TLS:               bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)},
			}},
			code: bridge.CodeInvalidConfig,
		},
		{
			name: "shell executable",
			config: bridge.Config{IMAP: bridge.IMAPConfig{
				CredentialCommand: []string{"/bin/sh", "-c", "echo credential"},
				TLS:               bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)},
			}},
			code: bridge.CodeInvalidConfig,
		},
		{
			name: "unbounded connect timeout",
			config: bridge.Config{IMAP: bridge.IMAPConfig{
				CredentialCommand: []string{"/bin/true"},
				TLS:               bridge.TLSConfig{CertificateSHA256: strings.Repeat("a", 64)},
				ConnectTimeoutMs:  60001,
			}},
			code: bridge.CodeInvalidConfig,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			if _, err := bridge.ValidateConfig(test.config); bridge.CodeOf(err) != test.code {
				t.Fatalf("ValidateConfig error = %v, want %q", err, test.code)
			}
		})
	}
}

func TestValidateConfigDoesNotReadTrustAnchorFiles(t *testing.T) {
	t.Parallel()

	config, err := bridge.ValidateConfig(bridge.Config{IMAP: bridge.IMAPConfig{
		CredentialCommand: []string{"/bin/true"},
		TLS:               bridge.TLSConfig{TrustAnchorFile: "/not-a-real-test-trust-anchor.pem"},
	}})
	if err != nil {
		t.Fatalf("ValidateConfig read a trust-anchor file: %v", err)
	}
	if got, want := config.IMAP.TLS.TrustAnchorFile, "/not-a-real-test-trust-anchor.pem"; got != want {
		t.Fatalf("trust-anchor path = %q, want %q", got, want)
	}
}

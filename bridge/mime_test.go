package bridge_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

func TestNormalizeMessagePrefersPlainText(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"From: Sender <sender@fixture.test>",
		"To: Reader <reader@fixture.test>",
		"Subject: Plain text wins",
		"Message-ID: <plain-wins@fixture.test>",
		"MIME-Version: 1.0",
		"Content-Type: multipart/alternative; boundary=alternative",
		"",
		"--alternative",
		"Content-Type: text/html; charset=UTF-8",
		"",
		"<p>HTML fallback must not win.</p>",
		"--alternative",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"Plain text is the canonical body.",
		"--alternative--",
		"",
	}, "\r\n")), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Headers.Subject != "Plain text wins" {
		t.Errorf("subject = %q, want %q", message.Headers.Subject, "Plain text wins")
	}

	if message.Headers.MessageID != "<plain-wins@fixture.test>" {
		t.Errorf("message ID = %q, want %q", message.Headers.MessageID, "<plain-wins@fixture.test>")
	}

	if message.Text != "Plain text is the canonical body." {
		t.Errorf("text = %q, want plain-text alternative", message.Text)
	}

	if message.TextSource != bridge.TextSourcePlain {
		t.Errorf("text source = %q, want %q", message.TextSource, bridge.TextSourcePlain)
	}

	if message.Truncation.Any() {
		t.Errorf("unexpected truncation: %+v", message.Truncation)
	}
}

func TestNormalizeMessageDecodesQuotedPrintableText(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"From: Sender <sender@fixture.test>",
		"Content-Type: text/plain; charset=ISO-8859-1",
		"Content-Transfer-Encoding: quoted-printable",
		"",
		"caf=E9 =E2=80=94 safe Unicode boundary",
		"",
	}, "\r\n")), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Text != "café â€” safe Unicode boundary" {
		t.Errorf("decoded text = %q", message.Text)
	}
}

func TestNormalizeMessageMarksTextTruncation(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader("Content-Type: text/plain; charset=UTF-8\r\n\r\n0123456789"), bridge.NormalizeOptions{
		Limits: bridge.NormalizeLimits{MaxDecodedTextBytes: 5},
	})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Text != "01234" {
		t.Errorf("text = %q, want bounded prefix", message.Text)
	}

	if !message.Truncation.Text {
		t.Errorf("truncation = %+v, want text truncation", message.Truncation)
	}
}

func TestNormalizeMessageUsesOfflineHTMLFallback(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"Subject: =?UTF-8?B?4pyTIFN5bnRoZXRpYyBoZWFkaW5n?=",
		"Content-Type: text/html; charset=UTF-8",
		"",
		"<html><head><title>Ignored</title><script>steal('https://tracker.invalid')</script></head><body><p>Visible <strong>text</strong>.</p><img src=\"https://tracker.invalid/pixel\"><noscript>Hidden</noscript></body></html>",
	}, "\r\n")), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Headers.Subject != "✓ Synthetic heading" {
		t.Errorf("subject = %q", message.Headers.Subject)
	}

	if message.Text != "Visible text ." {
		t.Errorf("HTML text = %q", message.Text)
	}

	if message.TextSource != bridge.TextSourceHTML {
		t.Errorf("text source = %q, want HTML fallback", message.TextSource)
	}

	if strings.Contains(message.Text, "tracker") || strings.Contains(message.Text, "Hidden") {
		t.Errorf("active or remote HTML content survived: %q", message.Text)
	}
}

func TestNormalizeMessagePrefersAnEmptyPlainAlternative(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"Content-Type: multipart/alternative; boundary=alternative",
		"",
		"--alternative",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"",
		"--alternative",
		"Content-Type: text/html; charset=UTF-8",
		"",
		"<p>HTML must remain fallback-only.</p>",
		"--alternative--",
		"",
	}, "\r\n")), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Text != "" || message.TextSource != bridge.TextSourcePlain {
		t.Errorf("empty plain alternative = text %q, source %q", message.Text, message.TextSource)
	}
}

func TestNormalizeMessageDefaultsMissingContentTypeToPlainText(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader("Subject: Synthetic default\r\n\r\nplain body without a content type"), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Text != "plain body without a content type" || message.TextSource != bridge.TextSourcePlain {
		t.Errorf("default content type result = %+v", message)
	}
}

func TestNormalizeMessageKeepsNearestReferencesWhenCapped(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"References: <old@fixture.test> <nearest@fixture.test>",
		"Content-Type: text/plain",
		"",
		"synthetic body",
	}, "\r\n")), bridge.NormalizeOptions{Limits: bridge.NormalizeLimits{MaxReferenceCount: 1}})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if !message.Truncation.References || len(message.Headers.References) != 1 || message.Headers.References[0] != "<nearest@fixture.test>" {
		t.Errorf("references = %#v, truncation = %+v", message.Headers.References, message.Truncation)
	}
}

func TestNormalizeMessageTruncatesHeadersAtHeaderCountLimit(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"Subject: Bounded headers",
		"Content-Type: text/plain",
		"From: Ignored Sender <ignored@fixture.test>",
		"",
		"synthetic body",
	}, "\r\n")), bridge.NormalizeOptions{Limits: bridge.NormalizeLimits{MaxHeaderCount: 2}})
	if err != nil {
		t.Fatalf("normalize header-count bounded message: %v", err)
	}

	if !message.Truncation.HeaderCount {
		t.Errorf("truncation = %+v, want header-count metadata", message.Truncation)
	}

	if message.Headers.Subject != "Bounded headers" || message.Headers.From != "" || message.Text != "synthetic body" {
		t.Errorf("bounded header result = %+v", message)
	}
}

func TestNormalizeMessageDoesNotTruncateAtExactHeaderCountLimit(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader("Subject: Exact boundary\r\nContent-Type: text/plain\r\n\r\nsynthetic body"), bridge.NormalizeOptions{
		Limits: bridge.NormalizeLimits{MaxHeaderCount: 2},
	})
	if err != nil {
		t.Fatalf("normalize exact header-count boundary: %v", err)
	}

	if message.Truncation.HeaderCount || message.Headers.Subject != "Exact boundary" || message.Text != "synthetic body" {
		t.Errorf("exact header-count result = %+v", message)
	}
}

func TestNormalizeMessageHonorsHeaderCountLimitForEOFHeaderBlock(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name          string
		raw           string
		wantTo        string
		wantTruncated bool
	}{
		{
			name:   "exact boundary",
			raw:    "Subject: Exact EOF boundary\r\nFrom: sender@fixture.test",
			wantTo: "",
		},
		{
			name:          "above boundary",
			raw:           "Subject: Exact EOF boundary\r\nFrom: sender@fixture.test\r\nTo: omitted@fixture.test",
			wantTo:        "",
			wantTruncated: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			message, err := bridge.NormalizeMessage(strings.NewReader(testCase.raw), bridge.NormalizeOptions{
				Limits: bridge.NormalizeLimits{MaxHeaderCount: 2},
			})
			if err != nil {
				t.Fatalf("normalize EOF-terminated header block: %v", err)
			}

			if message.Truncation.HeaderCount != testCase.wantTruncated {
				t.Errorf("header-count truncation = %t, want %t", message.Truncation.HeaderCount, testCase.wantTruncated)
			}

			if message.Headers.Subject != "Exact EOF boundary" || message.Headers.From != "sender@fixture.test" || message.Headers.To != testCase.wantTo {
				t.Errorf("EOF-terminated header result = %+v", message)
			}
		})
	}
}

func TestNormalizeMessageRejectsHeaderBlockPastByteLimit(t *testing.T) {
	t.Parallel()

	_, err := bridge.NormalizeMessage(strings.NewReader("Subject: header block crosses its byte limit\r\n\r\nsynthetic body"), bridge.NormalizeOptions{
		Limits: bridge.NormalizeLimits{MaxHeaderBytes: 32},
	})

	var normalizeError *bridge.NormalizeError
	if !errors.As(err, &normalizeError) || normalizeError.Code != "malformed_header" {
		t.Fatalf("error = %#v, want safe malformed_header", err)
	}
}

func TestNormalizeMessageReturnsNestedAttachmentMetadataOnly(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"Content-Type: multipart/mixed; boundary=outer",
		"",
		"--outer",
		"Content-Type: multipart/alternative; boundary=inner",
		"",
		"--inner",
		"Content-Type: text/plain; charset=UTF-8",
		"",
		"Synthetic body.",
		"--inner--",
		"--outer",
		"Content-Type: application/octet-stream; name=quarterly.txt",
		"Content-Disposition: attachment; filename=quarterly.txt; size=42",
		"Content-Transfer-Encoding: base64",
		"",
		"c2Vuc2l0aXZlIGZpeHR1cmUgYXR0YWNobWVudA==",
		"--outer--",
		"",
	}, "\r\n")), bridge.NormalizeOptions{})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if message.Text != "Synthetic body." {
		t.Errorf("text = %q", message.Text)
	}

	if len(message.Attachments) != 1 {
		t.Fatalf("attachment count = %d, want 1", len(message.Attachments))
	}

	attachment := message.Attachments[0]
	if attachment.Filename != "quarterly.txt" || attachment.ContentType != "application/octet-stream" || attachment.Disposition != "attachment" || !attachment.HasDeclaredSize || attachment.DeclaredSize != 42 {
		t.Errorf("attachment metadata = %+v", attachment)
	}
}

func TestNormalizeMessageReturnsSafeErrorsForOversizedAndMalformedInput(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		name    string
		input   string
		options bridge.NormalizeOptions
		want    string
	}{
		{
			name:    "raw input",
			input:   "Content-Type: text/plain\r\n\r\n0123456789",
			options: bridge.NormalizeOptions{Limits: bridge.NormalizeLimits{MaxRawBytes: 32}},
			want:    "input_too_large",
		},
		{
			name:    "encoded header",
			input:   "Content-Type: text/plain\r\nSubject: =?UTF-8?B?%%%?=\r\n\r\nbody",
			options: bridge.NormalizeOptions{},
			want:    "malformed_header",
		},
		{
			name:    "transfer encoding",
			input:   "Content-Type: text/plain\r\nContent-Transfer-Encoding: x-unsafe\r\n\r\nbody",
			options: bridge.NormalizeOptions{},
			want:    "malformed_encoding",
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := bridge.NormalizeMessage(strings.NewReader(testCase.input), testCase.options)

			var normalizeError *bridge.NormalizeError
			if !errors.As(err, &normalizeError) || normalizeError.Code != testCase.want {
				t.Fatalf("error = %#v, want safe code %q", err, testCase.want)
			}

			if strings.Contains(err.Error(), "body") || strings.Contains(err.Error(), "unsafe") {
				t.Errorf("error exposed input: %q", err)
			}
		})
	}
}

func TestNormalizeMessageMarksMIMEDepthTruncation(t *testing.T) {
	t.Parallel()

	message, err := bridge.NormalizeMessage(strings.NewReader(strings.Join([]string{
		"Content-Type: multipart/mixed; boundary=outer",
		"",
		"--outer",
		"Content-Type: multipart/mixed; boundary=inner",
		"",
		"--inner",
		"Content-Type: text/plain",
		"",
		"deep synthetic text",
		"--inner--",
		"--outer--",
		"",
	}, "\r\n")), bridge.NormalizeOptions{Limits: bridge.NormalizeLimits{MaxMIMEDepth: 1}})
	if err != nil {
		t.Fatalf("normalize message: %v", err)
	}

	if !message.Truncation.MIMEDepth {
		t.Errorf("truncation = %+v, want MIME depth", message.Truncation)
	}
}

func TestNormalizeMessageStopsMultipartTraversalAtPartCap(t *testing.T) {
	t.Parallel()

	var input strings.Builder
	input.WriteString("Content-Type: multipart/mixed; boundary=bounded\r\n\r\n")
	for part := range 20 {
		input.WriteString("--bounded\r\n")
		if part == 19 {
			input.WriteString("malformed header after cap\r\n\r\nignored\r\n")

			continue
		}

		input.WriteString("Content-Type: text/plain\r\n\r\n")
		input.WriteString("tiny synthetic part\r\n")
	}
	input.WriteString("--bounded--\r\n")

	message, err := bridge.NormalizeMessage(strings.NewReader(input.String()), bridge.NormalizeOptions{
		Limits: bridge.NormalizeLimits{MaxMIMEParts: 5},
	})
	if err != nil {
		t.Fatalf("normalize bounded multipart: %v", err)
	}

	if !message.Truncation.MIMEParts || message.Text != "tiny synthetic part" {
		t.Errorf("bounded multipart result = text %q, truncation %+v", message.Text, message.Truncation)
	}
}

func TestNormalizeMessageSyntheticFixtureSuite(t *testing.T) {
	t.Parallel()

	testCases := []struct {
		filename string
		wantErr  bool
	}{
		{filename: "html-fallback.eml"},
		{filename: "nested-attachment.eml"},
		{filename: "malformed-boundary.eml", wantErr: true},
	}

	for _, testCase := range testCases {
		t.Run(testCase.filename, func(t *testing.T) {
			fixture, err := os.ReadFile(filepath.Join("..", "internal", "testkit", "testdata", "messages", testCase.filename))
			if err != nil {
				t.Fatalf("read synthetic fixture: %v", err)
			}

			message, err := bridge.NormalizeMessage(strings.NewReader(string(fixture)), bridge.NormalizeOptions{})
			if testCase.wantErr {
				if err == nil {
					t.Fatal("malformed synthetic fixture normalized successfully")
				}

				return
			}

			if err != nil {
				t.Fatalf("normalize synthetic fixture: %v", err)
			}

			if message.Headers.MessageID == "" {
				t.Fatal("fixture lost its synthetic Message-ID")
			}
		})
	}
}

func FuzzNormalizeMessage(f *testing.F) {
	f.Add([]byte("Content-Type: text/plain; charset=UTF-8\r\n\r\nsynthetic seed"))
	f.Add([]byte("Content-Type: multipart/mixed; boundary=x\r\n\r\n--x\r\nContent-Type: text/plain\r\n\r\nok\r\n--x--\r\n"))
	f.Add([]byte("Subject: =?UTF-8?B?4pyT?=\r\n\r\nπ"))

	f.Fuzz(func(t *testing.T, input []byte) {
		_, _ = bridge.NormalizeMessage(strings.NewReader(string(input)), bridge.NormalizeOptions{
			Limits: bridge.NormalizeLimits{MaxRawBytes: 4096, MaxHeaderBytes: 1024, MaxDecodedTextBytes: 1024},
		})
	})
}

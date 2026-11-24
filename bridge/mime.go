package bridge

import (
	"bytes"
	"cmp"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"net/textproto"
	"strconv"
	"strings"
	"unicode/utf8"

	"golang.org/x/net/html"
	"golang.org/x/net/html/charset"
)

const (
	errorCodeInputTooLarge      = "input_too_large"
	errorCodeMalformedHeader    = "malformed_header"
	errorCodeMalformedMIME      = "malformed_mime"
	errorCodeUnsupportedCharset = "unsupported_charset"
	errorCodeMalformedEncoding  = "malformed_encoding"
)

var errMIMEPartsLimit = errors.New("MIME part limit reached")

// NormalizeError is a stable, content-free normalization failure.
type NormalizeError struct {
	Code string
}

// Error implements error without exposing untrusted message content.
func (errorValue *NormalizeError) Error() string {
	return errorValue.Code
}

func normalizeMessage(reader io.Reader, limits NormalizeLimits) (NormalizedMessage, error) {
	if reader == nil {
		return NormalizedMessage{}, &NormalizeError{Code: errorCodeMalformedMIME}
	}

	limits = normalizedLimits(limits)

	raw, err := io.ReadAll(io.LimitReader(reader, int64(limits.MaxRawBytes)+1))
	if err != nil {
		return NormalizedMessage{}, &NormalizeError{Code: errorCodeMalformedMIME}
	}

	if len(raw) > limits.MaxRawBytes {
		return NormalizedMessage{}, &NormalizeError{Code: errorCodeInputTooLarge}
	}

	headerBytes, err := boundedHeaderBlock(raw, limits)
	if err != nil {
		return NormalizedMessage{}, err
	}

	bodyOffset := len(headerBytes)
	headerBytes, headerCountTruncated := boundedHeaderCount(headerBytes, limits.MaxHeaderCount)

	message, err := mail.ReadMessage(io.MultiReader(bytes.NewReader(headerBytes), bytes.NewReader(raw[bodyOffset:])))
	if err != nil {
		return NormalizedMessage{}, &NormalizeError{Code: errorCodeMalformedHeader}
	}

	normalized := NormalizedMessage{TextSource: TextSourceNone}
	normalized.Headers, normalized.Truncation, err = canonicalHeaders(message.Header, headerBytes, limits)
	if err != nil {
		return NormalizedMessage{}, err
	}

	normalized.Truncation.HeaderCount = headerCountTruncated

	collector := mimeCollector{limits: limits, result: &normalized}
	if err := collector.parseEntity(textproto.MIMEHeader(message.Header), message.Body, 0); err != nil && !errors.Is(err, errMIMEPartsLimit) {
		return NormalizedMessage{}, err
	}

	if normalized.TextSource == TextSourceHTML {
		text, truncated := htmlText(normalized.Text, limits.MaxDecodedTextBytes)
		normalized.Text = text
		normalized.Truncation.Text = normalized.Truncation.Text || truncated
	}

	return normalized, nil
}

func boundedHeaderBlock(raw []byte, limits NormalizeLimits) ([]byte, error) {
	window := raw[:min(len(raw), limits.MaxHeaderBytes)]
	for _, separator := range []string{"\r\n\r\n", "\n\n"} {
		if end := bytes.Index(window, []byte(separator)); end >= 0 {
			return raw[:end+len(separator)], nil
		}
	}

	if len(raw) > limits.MaxHeaderBytes {
		return nil, &NormalizeError{Code: errorCodeMalformedHeader}
	}

	return raw, nil
}

func boundedHeaderCount(header []byte, limit int) ([]byte, bool) {
	fieldCount, offset := 0, 0
	for rawLine := range bytes.Lines(header) {
		line := bytes.TrimSuffix(bytes.TrimSuffix(rawLine, []byte("\n")), []byte("\r"))
		if len(line) == 0 {
			break
		}

		if line[0] != ' ' && line[0] != '\t' {
			fieldCount++
			if fieldCount > limit {
				separator := "\n"
				if bytes.HasSuffix(header[:offset], []byte("\r\n")) {
					separator = "\r\n"
				}

				// The body may share the backing array with header.
				return append(header[:offset:offset], separator...), true
			}
		}
		offset += len(rawLine)
	}

	return header, false
}

func canonicalHeaders(header mail.Header, headerBytes []byte, limits NormalizeLimits) (CanonicalHeaders, Truncation, error) {
	var headers CanonicalHeaders
	var references string
	truncation := Truncation{Headers: len(headerBytes) > limits.MaxHeaderBytes}
	for _, field := range []struct {
		name  string
		value *string
	}{
		{"Date", &headers.Date},
		{"From", &headers.From},
		{"To", &headers.To},
		{"Subject", &headers.Subject},
		{"Message-ID", &headers.MessageID},
		{"In-Reply-To", &headers.InReplyTo},
		{"References", &references},
	} {
		value, truncated, err := canonicalHeaderValue(header, field.name, limits.MaxHeaderBytes)
		if err != nil {
			return CanonicalHeaders{}, Truncation{}, err
		}

		*field.value = value
		truncation.Headers = truncation.Headers || truncated
	}

	headers.MessageID = firstMessageID(headers.MessageID)
	headers.InReplyTo = firstMessageID(headers.InReplyTo)
	headers.References = messageIDs(references)
	if len(headers.References) > limits.MaxReferenceCount {
		headers.References = headers.References[len(headers.References)-limits.MaxReferenceCount:]
		truncation.References = true
	}

	return headers, truncation, nil
}

func canonicalHeaderValue(header mail.Header, name string, limit int) (string, bool, error) {
	value := header.Get(name)
	if value == "" {
		return "", false, nil
	}

	truncated := false
	if len(value) > limit {
		value = value[:limit]
		truncated = true
	}

	if !validEncodedWords(value) {
		return "", false, &NormalizeError{Code: errorCodeMalformedHeader}
	}

	decoder := mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
	decoded, err := decoder.DecodeHeader(value)
	if err != nil {
		return "", false, &NormalizeError{Code: errorCodeMalformedHeader}
	}

	decoded, wasTruncated := truncateUTF8(decoded, limit)

	return strings.TrimSpace(decoded), truncated || wasTruncated, nil
}

func validEncodedWords(value string) bool {
	for {
		_, afterMarker, started := strings.Cut(value, "=?")
		word, rest, ended := strings.Cut(afterMarker, "?=")
		if !started || !ended {
			return true
		}

		parts := strings.Split(word, "?")
		value = rest
		if len(parts) != 3 || parts[0] == "" || (!strings.EqualFold(parts[1], "b") && !strings.EqualFold(parts[1], "q")) {
			continue
		}

		if parts[2] == "" {
			return false
		}

		if strings.EqualFold(parts[1], "b") {
			if _, err := base64.StdEncoding.DecodeString(parts[2]); err != nil {
				return false
			}
		}
	}
}

func firstMessageID(value string) string {
	identifiers := messageIDs(value)
	if len(identifiers) == 0 {
		return ""
	}

	return identifiers[0]
}

func messageIDs(value string) []string {
	var identifiers []string
	for {
		start := strings.IndexByte(value, '<')
		if start < 0 {
			break
		}

		value = value[start:]
		end := strings.IndexByte(value, '>')
		if end < 0 {
			break
		}

		identifier := value[:end+1]
		if len(identifier) > 2 && !strings.ContainsAny(identifier, " \t\r\n") {
			identifiers = append(identifiers, identifier)
		}

		value = value[end+1:]
	}

	return identifiers
}

type mimeCollector struct {
	limits NormalizeLimits
	result *NormalizedMessage
	parts  int
}

func (collector *mimeCollector) parseEntity(header textproto.MIMEHeader, body io.Reader, depth int) error {
	if depth > collector.limits.MaxMIMEDepth {
		collector.result.Truncation.MIMEDepth = true
		return nil
	}

	collector.parts++

	contentType, parameters, err := mime.ParseMediaType(cmp.Or(header.Get("Content-Type"), "text/plain"))
	if err != nil {
		return &NormalizeError{Code: errorCodeMalformedMIME}
	}

	var disposition string
	var dispositionParameters map[string]string
	if raw := header.Get("Content-Disposition"); raw != "" {
		disposition, dispositionParameters, err = mime.ParseMediaType(raw)
		if err != nil {
			return &NormalizeError{Code: errorCodeMalformedMIME}
		}
	}

	filename := cmp.Or(dispositionParameters["filename"], parameters["name"])
	if disposition == "attachment" || filename != "" {
		if metadata, ok := boundedAttachment(len(collector.result.Attachments), collector.limits, &collector.result.Truncation, filename, contentType, disposition, dispositionParameters["size"]); ok {
			collector.result.Attachments = append(collector.result.Attachments, metadata)
		}

		return nil
	}

	if strings.HasPrefix(contentType, "multipart/") {
		boundary := parameters["boundary"]
		if boundary == "" || len(boundary) > 200 {
			return &NormalizeError{Code: errorCodeMalformedMIME}
		}

		reader := multipart.NewReader(body, boundary)
		for {
			// NextPart parses headers, so check the budget before reading.
			if collector.parts >= collector.limits.MaxMIMEParts {
				collector.result.Truncation.MIMEParts = true

				return errMIMEPartsLimit
			}

			part, err := reader.NextPart()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return &NormalizeError{Code: errorCodeMalformedMIME}
			}

			if err := collector.parseEntity(part.Header, part, depth+1); err != nil {
				return err
			}
		}
	}

	if contentType == "message/rfc822" {
		collector.result.Truncation.MIMEDepth = true

		return nil
	}

	if contentType != "text/plain" && contentType != "text/html" {
		return nil
	}

	decoded, transferTruncated, err := decodeTransfer(body, header.Get("Content-Transfer-Encoding"), collector.limits.MaxDecodedTextBytes)
	if err != nil {
		return err
	}

	text, charsetTruncated, err := decodeCharset(decoded, parameters["charset"], collector.limits.MaxDecodedTextBytes)
	if err != nil {
		return err
	}

	collector.result.Truncation.Text = collector.result.Truncation.Text || transferTruncated || charsetTruncated

	switch {
	case contentType == "text/plain" && collector.result.TextSource != TextSourcePlain:
		collector.result.Text = text
		collector.result.TextSource = TextSourcePlain
	case contentType == "text/html" && collector.result.TextSource == TextSourceNone:
		collector.result.Text = text
		collector.result.TextSource = TextSourceHTML
	}

	return nil
}

// boundedAttachment builds one attachment's metadata within the count and
// field limits, given count attachments already listed. Fields are truncated
// and repaired by truncateUTF8, and any truncation or overflow sets
// Attachments. It reports false when the count limit leaves no room.
func boundedAttachment(count int, limits NormalizeLimits, truncation *Truncation, filename, contentType, disposition, size string) (AttachmentMetadata, bool) {
	if count >= limits.MaxAttachmentCount {
		truncation.Attachments = true

		return AttachmentMetadata{}, false
	}

	filename, filenameTruncated := truncateUTF8(filename, limits.MaxFilenameBytes)
	contentType, contentTypeTruncated := truncateUTF8(contentType, limits.MaxContentTypeBytes)
	disposition, dispositionTruncated := truncateUTF8(disposition, limits.MaxDispositionBytes)
	if filenameTruncated || contentTypeTruncated || dispositionTruncated {
		truncation.Attachments = true
	}

	metadata := AttachmentMetadata{Filename: filename, ContentType: contentType, Disposition: disposition}
	if declared, err := strconv.ParseInt(size, 10, 64); err == nil && declared >= 0 {
		metadata.DeclaredSize = declared
		metadata.HasDeclaredSize = true
	}

	return metadata, true
}

func decodeTransfer(reader io.Reader, encoding string, limit int) ([]byte, bool, error) {
	var decoded io.Reader
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "7bit", "8bit", "binary":
		decoded = reader
	case "base64":
		decoded = base64.NewDecoder(base64.StdEncoding, reader)
	case "quoted-printable":
		decoded = quotedprintable.NewReader(reader)
	default:
		return nil, false, &NormalizeError{Code: errorCodeMalformedEncoding}
	}

	content, err := io.ReadAll(io.LimitReader(decoded, int64(limit)+1))
	if err != nil {
		return nil, false, &NormalizeError{Code: errorCodeMalformedEncoding}
	}

	if len(content) > limit {
		return content[:limit], true, nil
	}

	return content, false, nil
}

func decodeCharset(content []byte, label string, limit int) (string, bool, error) {
	if label == "" || strings.EqualFold(label, "utf-8") || strings.EqualFold(label, "us-ascii") {
		text, truncated := truncateUTF8(string(content), limit)

		return strings.TrimRight(text, "\r\n"), truncated, nil
	}

	decoded, err := charset.NewReaderLabel(label, bytes.NewReader(content))
	if err != nil {
		return "", false, &NormalizeError{Code: errorCodeUnsupportedCharset}
	}

	text, err := io.ReadAll(io.LimitReader(decoded, int64(limit)+1))
	if err != nil {
		return "", false, &NormalizeError{Code: errorCodeUnsupportedCharset}
	}

	result, truncated := truncateUTF8(string(text), limit)

	return strings.TrimRight(result, "\r\n"), truncated, nil
}

func htmlText(source string, limit int) (string, bool) {
	document, err := html.Parse(strings.NewReader(source))
	if err != nil {
		return "", false
	}

	var fragments []string
	var collect func(*html.Node)
	collect = func(node *html.Node) {
		switch node.Type {
		case html.TextNode:
			fragments = append(fragments, node.Data)
		case html.ElementNode:
			switch strings.ToLower(node.Data) {
			case "script", "style", "head", "template", "noscript":
				return
			}
		}

		for child := range node.ChildNodes() {
			collect(child)
		}
	}
	collect(document)

	return truncateUTF8(strings.Join(strings.Fields(strings.Join(fragments, " ")), " "), limit)
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return strings.ToValidUTF8(value, "�"), false
	}

	truncated := value[:limit]
	for !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	return truncated, true
}

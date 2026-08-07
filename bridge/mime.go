package bridge

import (
	"bytes"
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
	headers, truncation, err := canonicalHeaders(message.Header, headerBytes, limits)
	if err != nil {
		return NormalizedMessage{}, err
	}

	normalized.Headers = headers
	normalized.Truncation = truncation
	normalized.Truncation.HeaderCount = headerCountTruncated

	collector := mimeCollector{limits: limits, result: &normalized}
	if err := collector.parseEntity(textproto.MIMEHeader(message.Header), message.Body, 0); err != nil && !errors.Is(err, errMIMEPartsLimit) {
		return NormalizedMessage{}, err
	}

	collector.applyText()

	return normalized, nil
}

func boundedHeaderBlock(raw []byte, limits NormalizeLimits) ([]byte, error) {
	limit := limits.MaxHeaderBytes
	if limit > len(raw) {
		limit = len(raw)
	}

	headerEnd := bytes.Index(raw[:limit], []byte("\r\n\r\n"))
	separatorLength := len("\r\n\r\n")
	if headerEnd < 0 {
		headerEnd = bytes.Index(raw[:limit], []byte("\n\n"))
		separatorLength = len("\n\n")
	}

	if headerEnd < 0 {
		if len(raw) > limits.MaxHeaderBytes {
			return nil, &NormalizeError{Code: errorCodeMalformedHeader}
		}

		return raw, nil
	}

	headerEnd += separatorLength
	if headerEnd > limits.MaxHeaderBytes {
		return nil, &NormalizeError{Code: errorCodeMalformedHeader}
	}

	return raw[:headerEnd], nil
}

func boundedHeaderCount(header []byte, limit int) ([]byte, bool) {
	fieldCount := 0
	for lineStart := 0; lineStart < len(header); {
		lineLength := bytes.IndexByte(header[lineStart:], '\n')
		if lineLength < 0 {
			return header, false
		}

		lineEnd := lineStart + lineLength
		line := bytes.TrimSuffix(header[lineStart:lineEnd], []byte{'\r'})
		if len(line) == 0 {
			return header, false
		}

		if line[0] != ' ' && line[0] != '	' {
			fieldCount++
			if fieldCount > limit {
				separator := []byte{'\n'}
				if lineStart >= 2 && bytes.Equal(header[lineStart-2:lineStart], []byte("\r\n")) {
					separator = []byte("\r\n")
				}

				bounded := make([]byte, 0, lineStart+len(separator))
				bounded = append(bounded, header[:lineStart]...)
				bounded = append(bounded, separator...)

				return bounded, true
			}
		}

		lineStart = lineEnd + 1
	}

	return header, false
}

func canonicalHeaders(header mail.Header, headerBytes []byte, limits NormalizeLimits) (CanonicalHeaders, Truncation, error) {
	truncation := Truncation{}
	if len(headerBytes) > limits.MaxHeaderBytes {
		truncation.Headers = true
	}

	date, dateTruncated, err := canonicalHeaderValue(header, "Date", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	from, fromTruncated, err := canonicalHeaderValue(header, "From", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	to, toTruncated, err := canonicalHeaderValue(header, "To", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	subject, subjectTruncated, err := canonicalHeaderValue(header, "Subject", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	messageID, messageIDTruncated, err := canonicalHeaderValue(header, "Message-ID", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	inReplyTo, inReplyToTruncated, err := canonicalHeaderValue(header, "In-Reply-To", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	referenceValues, referenceTruncated, err := canonicalHeaderValue(header, "References", limits.MaxHeaderBytes)
	if err != nil {
		return CanonicalHeaders{}, Truncation{}, err
	}

	references := messageIDs(referenceValues)
	if len(references) > limits.MaxReferenceCount {
		references = references[len(references)-limits.MaxReferenceCount:]
		truncation.References = true
	}

	truncation.Headers = truncation.Headers || dateTruncated || fromTruncated || toTruncated || subjectTruncated || messageIDTruncated || inReplyToTruncated || referenceTruncated

	return CanonicalHeaders{
		Date:       date,
		From:       from,
		To:         to,
		Subject:    subject,
		MessageID:  firstMessageID(messageID),
		InReplyTo:  firstMessageID(inReplyTo),
		References: references,
	}, truncation, nil
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
		start := strings.Index(value, "=?")
		if start < 0 {
			return true
		}

		value = value[start+2:]
		end := strings.Index(value, "?=")
		if end < 0 {
			return false
		}

		parts := strings.Split(value[:end], "?")
		if len(parts) != 3 || parts[0] == "" || parts[2] == "" {
			return false
		}

		if !strings.EqualFold(parts[1], "b") && !strings.EqualFold(parts[1], "q") {
			return false
		}

		if strings.EqualFold(parts[1], "b") {
			if _, err := base64.StdEncoding.DecodeString(parts[2]); err != nil {
				return false
			}
		}

		value = value[end+2:]
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
	limits   NormalizeLimits
	result   *NormalizedMessage
	parts    int
	plain    string
	plainSet bool
	html     string
	htmlSet  bool
}

func (collector *mimeCollector) parseEntity(header textproto.MIMEHeader, body io.Reader, depth int) error {
	if depth > collector.limits.MaxMIMEDepth {
		collector.result.Truncation.MIMEDepth = true
		return nil
	}

	if collector.parts >= collector.limits.MaxMIMEParts {
		collector.result.Truncation.MIMEParts = true

		return errMIMEPartsLimit
	}
	collector.parts++

	contentType := "text/plain"
	parameters := map[string]string{}
	if rawContentType := header.Get("Content-Type"); rawContentType != "" {
		var err error
		contentType, parameters, err = mime.ParseMediaType(rawContentType)
		if err != nil {
			return &NormalizeError{Code: errorCodeMalformedMIME}
		}
	}

	disposition := ""
	dispositionParameters := map[string]string{}
	if contentDisposition := header.Get("Content-Disposition"); contentDisposition != "" {
		parsedDisposition, parsedParameters, err := mime.ParseMediaType(contentDisposition)
		if err != nil {
			return &NormalizeError{Code: errorCodeMalformedMIME}
		}

		disposition = parsedDisposition
		dispositionParameters = parsedParameters
	}

	filename := dispositionParameters["filename"]
	if filename == "" {
		filename = parameters["name"]
	}

	attachment := strings.EqualFold(disposition, "attachment") || filename != ""
	if attachment {
		collector.appendAttachment(filename, contentType, disposition, dispositionParameters)

		return nil
	}

	if strings.HasPrefix(strings.ToLower(contentType), "multipart/") {
		boundary := parameters["boundary"]
		if boundary == "" || len(boundary) > 200 {
			return &NormalizeError{Code: errorCodeMalformedMIME}
		}

		reader := multipart.NewReader(body, boundary)
		for {
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

	if strings.EqualFold(contentType, "message/rfc822") {
		collector.result.Truncation.MIMEDepth = true

		return nil
	}

	if !strings.EqualFold(contentType, "text/plain") && !strings.EqualFold(contentType, "text/html") {
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

	if strings.EqualFold(contentType, "text/plain") && !collector.plainSet {
		collector.plain = text
		collector.plainSet = true
	}

	if strings.EqualFold(contentType, "text/html") && !collector.htmlSet {
		collector.html = text
		collector.htmlSet = true
	}

	return nil
}

func (collector *mimeCollector) appendAttachment(filename, contentType, disposition string, parameters map[string]string) {
	if len(collector.result.Attachments) >= collector.limits.MaxAttachmentCount {
		collector.result.Truncation.Attachments = true

		return
	}

	filename, filenameTruncated := truncateUTF8(filename, collector.limits.MaxFilenameBytes)
	contentType, contentTypeTruncated := truncateUTF8(contentType, collector.limits.MaxContentTypeBytes)
	disposition, dispositionTruncated := truncateUTF8(disposition, collector.limits.MaxDispositionBytes)
	if filenameTruncated || contentTypeTruncated || dispositionTruncated {
		collector.result.Truncation.Attachments = true
	}

	metadata := AttachmentMetadata{Filename: filename, ContentType: contentType, Disposition: disposition}
	if size, err := strconv.ParseInt(parameters["size"], 10, 64); err == nil && size >= 0 {
		metadata.DeclaredSize = size
		metadata.HasDeclaredSize = true
	}

	collector.result.Attachments = append(collector.result.Attachments, metadata)
}

func (collector *mimeCollector) applyText() {
	if collector.plainSet {
		collector.result.Text = collector.plain
		collector.result.TextSource = TextSourcePlain

		return
	}

	if !collector.htmlSet {
		return
	}

	text, truncated := htmlText(collector.html, collector.limits.MaxDecodedTextBytes)
	collector.result.Text = text
	collector.result.TextSource = TextSourceHTML
	collector.result.Truncation.Text = collector.result.Truncation.Text || truncated
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
	var collect func(*html.Node, bool)
	collect = func(node *html.Node, blocked bool) {
		if node.Type == html.ElementNode {
			switch strings.ToLower(node.Data) {
			case "script", "style", "head", "template", "noscript":
				blocked = true
			}
		}

		if node.Type == html.TextNode && !blocked {
			fragments = append(fragments, node.Data)
		}

		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collect(child, blocked)
		}
	}
	collect(document, false)

	return truncateUTF8(strings.Join(strings.Fields(strings.Join(fragments, " ")), " "), limit)
}

func truncateUTF8(value string, limit int) (string, bool) {
	if len(value) <= limit {
		return strings.ToValidUTF8(value, "�"), false
	}

	truncated := value[:limit]
	for len(truncated) > 0 && !utf8.ValidString(truncated) {
		truncated = truncated[:len(truncated)-1]
	}

	return strings.ToValidUTF8(truncated, "�"), true
}

// Package bridge provides bounded, MCP-neutral mail normalization primitives.
package bridge

import "io"

const (
	defaultMaxRawBytes           = 256 << 10
	maximumMaxRawBytes           = 1 << 20
	defaultMaxHeaderBytes        = 16 << 10
	maximumMaxHeaderBytes        = 64 << 10
	defaultMaxHeaderCount        = 100
	maximumMaxHeaderCount        = 500
	defaultMaxMIMEParts          = 50
	maximumMaxMIMEParts          = 200
	defaultMaxMIMEDepth          = 10
	maximumMaxMIMEDepth          = 20
	defaultMaxDecodedTextBytes   = 256 << 10
	maximumMaxDecodedTextBytes   = 1 << 20
	defaultMaxAttachmentCount    = 20
	maximumMaxAttachmentCount    = 100
	defaultMaxFilenameBytes      = 255
	maximumMaxFilenameBytes      = 1024
	defaultMaxContentTypeBytes   = 255
	maximumMaxContentTypeBytes   = 1024
	defaultMaxDispositionBytes   = 127
	maximumMaxDispositionBytes   = 512
	defaultMaxReferenceCount     = 50
	maximumMaxReferenceCount     = 200
	defaultMaxThreadDepth        = 20
	maximumMaxThreadDepth        = 100
	defaultMaxThreadMessageCount = 20
	maximumMaxThreadMessageCount = 100
)

// TextSource identifies which MIME representation supplied NormalizedMessage.Text.
type TextSource string

const (
	// TextSourceNone indicates that no eligible text body was available.
	TextSourceNone TextSource = "none"
	// TextSourcePlain indicates that text/plain supplied the normalized text.
	TextSourcePlain TextSource = "plain"
	// TextSourceHTML indicates that offline HTML text extraction supplied the normalized text.
	TextSourceHTML TextSource = "html"
)

// Truncation records every limit that affected a returned result.
type Truncation struct {
	Headers        bool `json:"headers,omitempty"`
	HeaderCount    bool `json:"headerCount,omitempty"`
	Text           bool `json:"text,omitempty"`
	MIMEParts      bool `json:"mimeParts,omitempty"`
	MIMEDepth      bool `json:"mimeDepth,omitempty"`
	Attachments    bool `json:"attachments,omitempty"`
	References     bool `json:"references,omitempty"`
	ThreadCycle    bool `json:"threadCycle,omitempty"`
	ThreadDepth    bool `json:"threadDepth,omitempty"`
	ThreadMessages bool `json:"threadMessages,omitempty"`
}

// Any reports whether at least one result limit was applied.
func (truncation Truncation) Any() bool {
	return truncation.Headers || truncation.HeaderCount || truncation.Text || truncation.MIMEParts || truncation.MIMEDepth || truncation.Attachments || truncation.References || truncation.ThreadCycle || truncation.ThreadDepth || truncation.ThreadMessages
}

// CanonicalHeaders contains the bounded message headers needed by Croton's public results.
type CanonicalHeaders struct {
	Date       string   `json:"date,omitempty"`
	From       string   `json:"from,omitempty"`
	To         string   `json:"to,omitempty"`
	Subject    string   `json:"subject,omitempty"`
	MessageID  string   `json:"messageId,omitempty"`
	InReplyTo  string   `json:"inReplyTo,omitempty"`
	References []string `json:"references,omitempty"`
}

// AttachmentMetadata describes an attachment without retaining any attachment bytes.
type AttachmentMetadata struct {
	Filename        string `json:"filename,omitempty"`
	ContentType     string `json:"contentType,omitempty"`
	Disposition     string `json:"disposition,omitempty"`
	DeclaredSize    int64  `json:"declaredSize,omitempty"`
	HasDeclaredSize bool   `json:"hasDeclaredSize,omitempty"`
}

// NormalizedMessage is a bounded, transport-independent representation of one raw MIME message.
type NormalizedMessage struct {
	Headers     CanonicalHeaders     `json:"headers"`
	Text        string               `json:"text,omitempty"`
	TextSource  TextSource           `json:"textSource"`
	Attachments []AttachmentMetadata `json:"attachments,omitempty"`
	Truncation  Truncation           `json:"truncation"`
}

// NormalizeLimits bounds all MIME normalization work. Zero values use safe defaults; values above
// Croton's hard ceilings are clamped before parsing begins.
type NormalizeLimits struct {
	MaxRawBytes         int
	MaxHeaderBytes      int
	MaxHeaderCount      int
	MaxMIMEParts        int
	MaxMIMEDepth        int
	MaxDecodedTextBytes int
	MaxAttachmentCount  int
	MaxFilenameBytes    int
	MaxContentTypeBytes int
	MaxDispositionBytes int
	MaxReferenceCount   int
	MaxThreadDepth      int
	MaxThreadMessages   int
}

// NormalizeOptions configures NormalizeMessage without any transport, filesystem, or session state.
type NormalizeOptions struct {
	Limits NormalizeLimits
}

// NormalizeMessage accepts only an already-open byte reader and returns a safe normalized message.
func NormalizeMessage(reader io.Reader, options NormalizeOptions) (NormalizedMessage, error) {
	return normalizeMessage(reader, options.Limits)
}

func normalizedLimits(limits NormalizeLimits) NormalizeLimits {
	return NormalizeLimits{
		MaxRawBytes:         boundedLimit(limits.MaxRawBytes, defaultMaxRawBytes, maximumMaxRawBytes),
		MaxHeaderBytes:      boundedLimit(limits.MaxHeaderBytes, defaultMaxHeaderBytes, maximumMaxHeaderBytes),
		MaxHeaderCount:      boundedLimit(limits.MaxHeaderCount, defaultMaxHeaderCount, maximumMaxHeaderCount),
		MaxMIMEParts:        boundedLimit(limits.MaxMIMEParts, defaultMaxMIMEParts, maximumMaxMIMEParts),
		MaxMIMEDepth:        boundedLimit(limits.MaxMIMEDepth, defaultMaxMIMEDepth, maximumMaxMIMEDepth),
		MaxDecodedTextBytes: boundedLimit(limits.MaxDecodedTextBytes, defaultMaxDecodedTextBytes, maximumMaxDecodedTextBytes),
		MaxAttachmentCount:  boundedLimit(limits.MaxAttachmentCount, defaultMaxAttachmentCount, maximumMaxAttachmentCount),
		MaxFilenameBytes:    boundedLimit(limits.MaxFilenameBytes, defaultMaxFilenameBytes, maximumMaxFilenameBytes),
		MaxContentTypeBytes: boundedLimit(limits.MaxContentTypeBytes, defaultMaxContentTypeBytes, maximumMaxContentTypeBytes),
		MaxDispositionBytes: boundedLimit(limits.MaxDispositionBytes, defaultMaxDispositionBytes, maximumMaxDispositionBytes),
		MaxReferenceCount:   boundedLimit(limits.MaxReferenceCount, defaultMaxReferenceCount, maximumMaxReferenceCount),
		MaxThreadDepth:      boundedLimit(limits.MaxThreadDepth, defaultMaxThreadDepth, maximumMaxThreadDepth),
		MaxThreadMessages:   boundedLimit(limits.MaxThreadMessages, defaultMaxThreadMessageCount, maximumMaxThreadMessageCount),
	}
}

func boundedLimit(value, defaultValue, maximum int) int {
	if value <= 0 {
		return defaultValue
	}

	if value > maximum {
		return maximum
	}

	return value
}

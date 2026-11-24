package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxForwardMIMEBytes bounds both the source literals and the serialized message.
const MaxForwardMIMEBytes = 256 << 10

// ForwardSource contains the complete inline source and the lifetime of the
// authenticated connection on which its identity and contents were verified.
type ForwardSource struct {
	Login, From, Date, Subject, Text string
	Omissions                        ForwardOmissions
	ConnectionClosed                 <-chan struct{}
}

// ForwardOmissions counts excluded entities without reading their content.
type ForwardOmissions struct {
	HTMLAlternatives int `json:"htmlAlternatives"`
	Files            int `json:"files"`
	AttachedMessages int `json:"attachedMessages"`
}

// ReadForwardSource selects and decodes exactly one complete plain section.
// A transport retry starts again at EXAMINE on the new authenticated session.
func (adapter *Adapter) ReadForwardSource(ctx context.Context, identifier string, textOnly bool) (ForwardSource, error) {
	id, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return ForwardSource{}, err
	}

	bounds := adapter.config.Bounds
	var source ForwardSource
	err = adapter.execute(ctx, func(ctx context.Context, session bridgeSession) error {
		if err := id.checkUID(ctx, session); err != nil {
			return err
		}

		structure, err := session.fetchUIDStructure(ctx, id.UID)
		if err != nil {
			return err
		}

		plan := forwardSelection{remaining: bounds.MaxMimeParts}
		if err := plan.visit(structure, nil, ""); err != nil {
			return err
		}
		if plan.section == nil || (!textOnly && (plan.omissions.Files > 0 || plan.omissions.AttachedMessages > 0)) {
			return errorCode(CodeUnsupported)
		}

		section := plan.section
		// A partial PEEK that fills the entire requested range cannot
		// distinguish a complete section from a longer one. Reserve room
		// for EOF detection within both the section and total read budgets.
		if uint64(section.size) >= uint64(min(bounds.MaxBodyBytes, MaxForwardMIMEBytes)) {
			return errorCode(CodeBoundsExceeded)
		}

		probe := int(section.size) + 1
		headerLimit := min(bounds.MaxHeaderBytes, MaxForwardMIMEBytes-probe)
		headers, err := session.UIDFetchHeaders(ctx, id.UID, headerLimit)
		if err != nil {
			return err
		}

		h := headers.Headers
		if headers.Truncation.HeaderCount || headers.Truncation.Headers {
			return errorCode(CodeBoundsExceeded)
		}
		if headers.Metadata.From == "" || headers.Metadata.DateSource != DateSourceHeader ||
			!validSenderEncodedWords(headers.Metadata.Subject) || !utf8.ValidString(headers.Metadata.Subject) ||
			strings.Contains(h.Subject, "=?") {
			return errorCode(CodeUnsupported)
		}
		for _, value := range []string{h.From, h.Date, h.Subject} {
			if strings.ContainsFunc(value, func(r rune) bool {
				return r == utf8.RuneError || (unicode.IsControl(r) && r != '\t')
			}) {
				return errorCode(CodeUnsupported)
			}
		}

		raw, err := session.UIDFetchBody(ctx, id.UID, section.part, probe)
		if err != nil {
			return err
		}
		if len(raw) != int(section.size) {
			return errorCode(CodeIMAPProtocol)
		}
		body, err := decodeForwardText(raw, section)
		if err != nil {
			return err
		}

		source = ForwardSource{Login: adapter.login, ConnectionClosed: adapter.connectionClosed,
			From: h.From, Date: h.Date, Subject: h.Subject, Text: body, Omissions: plan.omissions}
		return nil
	})

	return source, err
}

type forwardSelection struct {
	remaining int
	section   *textSection
	omissions ForwardOmissions
}

func (plan *forwardSelection) visit(structure bodyStructure, path []int, parent string) error {
	const mixed, alternative = "multipart/mixed", "multipart/alternative"

	if plan.remaining <= 0 || len(path) > defaultMaxMIMEDepth {
		return errorCode(CodeBoundsExceeded)
	}
	plan.remaining--

	entity, err := structureEntity(structure)
	if err != nil {
		return errorCode(CodeUnsupported)
	}
	// An invalid RFC 2231 continuation may disappear during media-type
	// parsing. Its presence on the wire still marks a file.
	var params map[string]string
	switch part := structure.(type) {
	case *singlePartStructure:
		params = part.Params
	case *multiPartStructure:
		if part.Extended != nil {
			params = part.Extended.Params
		}
	}
	attached := entity.disposition == "attachment" || forwardFileParam(params, "name")
	if disp := structure.Disposition(); disp != nil {
		attached = attached || forwardFileParam(disp.Params, "filename")
	}
	nested := entity.mediaType == "message/rfc822" || entity.mediaType == "message/global"
	if nested && (attached || (parent == mixed && plan.section != nil)) {
		plan.omissions.AttachedMessages++
		return nil
	}
	if attached {
		plan.omissions.Files++
		return nil
	}
	if entity.disposition != "" && entity.disposition != "inline" {
		return errorCode(CodeUnsupported)
	}

	switch entity.mediaType {
	case mixed, alternative:
		if plan.section != nil {
			return errorCode(CodeUnsupported)
		}

		for index, child := range entity.children {
			if err := plan.visit(child, append(slices.Clone(path), index+1), entity.mediaType); err != nil {
				return err
			}
		}
		if plan.section == nil {
			return errorCode(CodeUnsupported)
		}
	case "text/plain":
		if plan.section != nil || !forwardTextParameter(params, entity.parameters, "charset") ||
			!forwardTextParameter(params, entity.parameters, "format") {
			return errorCode(CodeUnsupported)
		}
		if format := entity.parameters["format"]; format != "" && !strings.EqualFold(format, "fixed") {
			return errorCode(CodeUnsupported)
		}
		part := structure.(*singlePartStructure)
		if len(path) == 0 {
			path = []int{1}
		}
		plan.section = &textSection{part: path, encoding: part.Encoding, charset: entity.parameters["charset"], size: part.Size}
	case "text/html":
		if parent != alternative {
			return errorCode(CodeUnsupported)
		}
		plan.omissions.HTMLAlternatives++
	default:
		if parent != mixed || strings.HasPrefix(entity.mediaType, "multipart/") ||
			strings.HasPrefix(entity.mediaType, "text/") || strings.HasPrefix(entity.mediaType, "message/") {
			return errorCode(CodeUnsupported)
		}
		plan.omissions.Files++
	}

	return nil
}

// ParseMediaType can drop an invalid extended value, override a regular
// value or partially recover a continuation. Only use an explicit charset
// or format when a single parameter supplies its entire value.
func forwardTextParameter(raw, parsed map[string]string, name string) bool {
	declared := false
	for key := range raw {
		base, suffix, _ := strings.Cut(key, "*")
		if !strings.EqualFold(base, name) {
			continue
		}
		if declared || suffix != "" || parsed[name] == "" {
			return false
		}
		declared = true
	}

	return true
}

func forwardFileParam(params map[string]string, name string) bool {
	for key := range params {
		base, _, _ := strings.Cut(key, "*")
		if strings.EqualFold(base, name) {
			return true
		}
	}
	return false
}

func decodeForwardText(raw []byte, part *textSection) (string, error) {
	var err error
	switch strings.ToLower(part.encoding) {
	case "", "7bit":
		if hasNonASCII(raw) {
			return "", errorCode(CodeUnsupported)
		}
	case "8bit", "binary":
	case "base64":
		raw, err = base64.StdEncoding.Strict().DecodeString(string(raw))
	case "quoted-printable":
		raw, err = decodeForwardQP(raw)
	default:
		return "", errorCode(CodeUnsupported)
	}
	if err != nil {
		return "", errorCode(CodeUnsupported)
	}

	switch strings.ToLower(part.charset) {
	case "", "us-ascii":
		if hasNonASCII(raw) {
			return "", errorCode(CodeUnsupported)
		}
	case "utf-8":
	case "iso-8859-1":
		var decoded strings.Builder
		for _, b := range raw {
			decoded.WriteRune(rune(b))
		}
		raw = []byte(decoded.String())
	default:
		return "", errorCode(CodeUnsupported)
	}

	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !utf8.ValidString(body) || strings.ContainsRune(body, utf8.RuneError) ||
		strings.ContainsFunc(body, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return "", errorCode(CodeUnsupported)
	}
	return body, nil
}

func hasNonASCII(raw []byte) bool {
	return bytes.ContainsFunc(raw, func(r rune) bool { return r >= utf8.RuneSelf })
}

// Decode in place. The standard reader repairs malformed escapes and strips
// unencoded trailing whitespace, both of which would produce a lossy preview.
func decodeForwardQP(raw []byte) ([]byte, error) {
	out := raw[:0]
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		switch b {
		case '=':
			if len(raw)-i < 3 {
				return nil, errorCode(CodeUnsupported)
			}
			if raw[i+1] != '\r' || raw[i+2] != '\n' {
				var octet [1]byte
				if _, err := hex.Decode(octet[:], raw[i+1:i+3]); err != nil {
					return nil, errorCode(CodeUnsupported)
				}
				out = append(out, octet[0])
			}
			i += 2
			continue
		case '\t', ' ':
			if i+1 == len(raw) || raw[i+1] == '\r' || raw[i+1] == '\n' {
				return nil, errorCode(CodeUnsupported)
			}
		case '\r':
			if i+1 == len(raw) || raw[i+1] != '\n' {
				return nil, errorCode(CodeUnsupported)
			}
		case '\n':
			if i == 0 || raw[i-1] != '\r' {
				return nil, errorCode(CodeUnsupported)
			}
		default:
			if b < '!' || b > '~' {
				return nil, errorCode(CodeUnsupported)
			}
		}
		out = append(out, b)
	}
	return out, nil
}

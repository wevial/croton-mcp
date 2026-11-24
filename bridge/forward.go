package bridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/hex"
	"mime"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/encoding/ianaindex"
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
		// Reserve one octet in both read budgets to detect an underdeclared section.
		if uint64(section.size) >= uint64(min(bounds.MaxBodyBytes, MaxForwardMIMEBytes)) {
			return errorCode(CodeBoundsExceeded)
		}

		probe := int(section.size) + 1
		headers, err := plan.readHeaders(ctx, session, id.UID, bounds.MaxHeaderBytes, MaxForwardMIMEBytes-probe)
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

type inlineEntity struct {
	part      []int
	mediaType string
}

type forwardSelection struct {
	inline    []inlineEntity
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

	// Only inline entities need original declaration checks. File and nested
	// message contents stay opaque, even with text-only consent.
	if entity.mediaType == mixed || entity.mediaType == alternative || strings.HasPrefix(entity.mediaType, "text/") {
		plan.inline = append(plan.inline, inlineEntity{part: path, mediaType: entity.mediaType})
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
		if plan.section != nil || !forwardParameter(params, entity.parameters, "charset") ||
			!forwardParameter(params, entity.parameters, "format") {
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

// BODYSTRUCTURE can normalize conflicting parameters or omit a malformed
// disposition. Check the original declarations before fetching any body bytes.
func (plan *forwardSelection) readHeaders(ctx context.Context, session bridgeSession, uid uint32, headerLimit, remaining int) (MessageHeaders, error) {
	var root MessageHeaders
	for _, entity := range plan.inline {
		limit := min(headerLimit, remaining)
		raw, err := session.fetchMIMEHeader(ctx, uid, entity.part, limit)
		if err != nil {
			return MessageHeaders{}, err
		}
		remaining -= len(raw)

		if headerTerminatorEnd(raw) != len(raw) {
			return MessageHeaders{}, errorCode(CodeIMAPProtocol)
		}
		if _, truncated := boundedHeaderCount(raw, defaultMaxHeaderCount); truncated {
			return MessageHeaders{}, errorCode(CodeBoundsExceeded)
		}
		message, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil || !entity.validHeader(message.Header, plan.section) {
			return MessageHeaders{}, errorCode(CodeUnsupported)
		}

		if len(entity.part) == 0 {
			root, err = selectedHeaderRecord(MessageMetadata{}, raw, limit)
			if err != nil {
				return MessageHeaders{}, err
			}
		}
	}

	return root, nil
}

func (entity inlineEntity) validHeader(header mail.Header, plain *textSection) bool {
	for key, fields := range header {
		name := strings.TrimSpace(key)
		switch strings.ToLower(name) {
		case "content-type", "content-disposition", "content-transfer-encoding":
			if name != key || len(fields) != 1 || fields[0] == "" {
				return false
			}
		}
	}

	if disposition := header.Get("Content-Disposition"); disposition != "" {
		kind, _, ok := inlineMediaType(disposition, "filename")
		if !ok || kind != "inline" {
			return false
		}
	}

	kind, params, ok := inlineMediaType(cmp.Or(header.Get("Content-Type"), "text/plain"), "name")
	if !ok || kind != entity.mediaType {
		return false
	}
	if kind != "text/plain" {
		return true
	}

	return strings.EqualFold(cmp.Or(params["format"], "fixed"), "fixed") &&
		strings.EqualFold(cmp.Or(params["charset"], "us-ascii"), cmp.Or(plain.charset, "us-ascii")) &&
		strings.EqualFold(cmp.Or(header.Get("Content-Transfer-Encoding"), "7bit"), cmp.Or(plain.encoding, "7bit"))
}

var quotedParameterValue = regexp.MustCompile(`"(?:[^"\\]|\\.)*"`)

func inlineMediaType(value, fileParam string) (string, map[string]string, bool) {
	kind, params, err := mime.ParseMediaType(value)
	if err != nil {
		return "", nil, false
	}

	// Inspect the original parameter names, including RFC 2231 suffixes.
	// Quoted values can themselves contain semicolons and equals signs.
	_, declarations, _ := strings.Cut(quotedParameterValue.ReplaceAllString(value, `""`), ";")
	names := make(map[string]string)
	for declaration := range strings.SplitSeq(declarations, ";") {
		key, _, ok := strings.Cut(declaration, "=")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		if _, duplicate := names[key]; duplicate {
			return "", nil, false
		}
		names[key] = ""
	}

	if strings.HasPrefix(kind, "multipart/") && (params["boundary"] == "" || !forwardParameter(names, params, "boundary")) {
		return "", nil, false
	}

	return kind, params, !forwardFileParam(names, fileParam) &&
		forwardParameter(names, params, "charset") && forwardParameter(names, params, "format")
}

// ParseMediaType can drop an invalid extended value, override a regular
// value or partially recover a continuation. Require a single declaration
// for parameters that determine the section or its interpretation.
func forwardParameter(raw, parsed map[string]string, name string) bool {
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
	default:
		encoding, err := ianaindex.MIME.Encoding(part.charset)
		if err != nil || encoding == nil {
			return "", errorCode(CodeUnsupported)
		}

		decoded, err := encoding.NewDecoder().Bytes(raw)
		if err != nil {
			return "", errorCode(CodeUnsupported)
		}
		original, err := encoding.NewEncoder().Bytes(decoded)
		if err != nil || !bytes.Equal(original, raw) {
			return "", errorCode(CodeUnsupported)
		}
		raw = decoded
	}

	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !utf8.ValidString(body) ||
		strings.ContainsFunc(body, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return "", errorCode(CodeUnsupported)
	}
	return body, nil
}

func hasNonASCII(raw []byte) bool {
	return bytes.ContainsFunc(raw, func(r rune) bool { return r >= utf8.RuneSelf })
}

// The standard reader repairs malformed escapes and strips
// unencoded trailing whitespace, both of which would produce a lossy preview.
func decodeForwardQP(raw []byte) ([]byte, error) {
	for line := range bytes.SplitSeq(raw, []byte("\r\n")) {
		if len(line) > 76 {
			return nil, errorCode(CodeUnsupported)
		}
	}

	out := raw[:0]
	for i := 0; i < len(raw); i++ {
		b := raw[i]
		switch b {
		case '=':
			if len(raw)-i < 3 {
				return nil, errorCode(CodeUnsupported)
			}
			if raw[i+1] != '\r' || raw[i+2] != '\n' {
				var err error
				out, err = hex.AppendDecode(out, raw[i+1:i+3])
				if err != nil {
					return nil, errorCode(CodeUnsupported)
				}
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

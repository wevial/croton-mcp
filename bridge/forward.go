package bridge

import (
	"bytes"
	"cmp"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"mime"
	"net/mail"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html/charset"
	"golang.org/x/text/encoding/ianaindex"
	"golang.org/x/text/encoding/japanese"
	"golang.org/x/text/encoding/simplifiedchinese"
	textunicode "golang.org/x/text/encoding/unicode"
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
			return errPreflightBounds
		}

		probe := int(section.size) + 1
		headers, err := plan.readHeaders(ctx, session, id.UID, bounds.MaxHeaderBytes, MaxForwardMIMEBytes-probe)
		if err != nil {
			return err
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
			From: headers.From, Date: headers.Date, Subject: headers.Subject, Text: body, Omissions: plan.omissions}
		return nil
	})

	return source, err
}

type inlineEntity struct {
	part      []int
	mediaType string
	rootID    string // Related container's start parameter or its HTML root's Content-ID.
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
	if parent == "multipart/related" && attached {
		return errorCode(CodeUnsupported)
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
		inline := inlineEntity{part: path, mediaType: entity.mediaType}
		if parent == "multipart/related" {
			inline.rootID = structure.(*singlePartStructure).ID
		}
		plan.inline = append(plan.inline, inline)
	}

	switch entity.mediaType {
	case "multipart/related":
		if parent != alternative || !forwardParameter(params, entity.parameters, "type") ||
			!forwardParameter(params, entity.parameters, "start") {
			return errorCode(CodeUnsupported)
		}

		return plan.visitRelatedHTML(entity, path)
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
		if parent != alternative && parent != "multipart/related" {
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

// The related root defaults to the first child. A start parameter identifies
// it by Content-ID instead. Only its HTML declaration needs a header PEEK.
func (plan *forwardSelection) visitRelatedHTML(entity projectedEntity, path []int) error {
	if len(entity.children) > plan.remaining {
		return errorCode(CodeBoundsExceeded)
	}
	if !strings.EqualFold(entity.parameters["type"], "text/html") {
		return errorCode(CodeUnsupported)
	}

	start := entity.parameters["start"]
	root := -1
	for i, child := range entity.children {
		part, ok := child.(*singlePartStructure)
		if !ok {
			return errorCode(CodeUnsupported)
		}
		kind := part.MediaType()
		if (start == "" && i == 0) || (start != "" && start == part.ID) {
			if kind != "text/html" || root >= 0 {
				return errorCode(CodeUnsupported)
			}
			root = i
		} else if kind == "message/rfc822" || kind == "message/global" {
			plan.omissions.AttachedMessages++
		} else {
			plan.omissions.Files++
		}
	}
	if root < 0 {
		return errorCode(CodeUnsupported)
	}

	plan.inline = append(plan.inline, inlineEntity{part: path, mediaType: entity.mediaType, rootID: start})
	plan.remaining -= len(entity.children) - 1

	return plan.visit(entity.children[root], append(slices.Clone(path), root+1), entity.mediaType)
}

// BODYSTRUCTURE can normalize conflicting parameters or omit a malformed
// disposition. Check the original declarations before fetching any body bytes.
func (plan *forwardSelection) readHeaders(ctx context.Context, session bridgeSession, uid uint32, headerLimit, remaining int) (CanonicalHeaders, error) {
	var root CanonicalHeaders
	for _, entity := range plan.inline {
		limit := min(headerLimit, remaining)
		raw, err := session.fetchMIMEHeader(ctx, uid, entity.part, limit)
		if err != nil {
			return CanonicalHeaders{}, err
		}
		remaining -= len(raw)

		if headerTerminatorEnd(raw) != len(raw) {
			return CanonicalHeaders{}, errorCode(CodeIMAPProtocol)
		}
		if _, truncated := boundedHeaderCount(raw, defaultMaxHeaderCount); truncated {
			return CanonicalHeaders{}, errorCode(CodeBoundsExceeded)
		}
		message, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil || !entity.validHeader(message.Header, plan.section) {
			return CanonicalHeaders{}, errorCode(CodeUnsupported)
		}

		if len(entity.part) == 0 {
			record, err := selectedHeaderRecord(MessageMetadata{}, raw, limit)
			if err != nil {
				return CanonicalHeaders{}, err
			}
			if record.Truncation.HeaderCount || record.Truncation.Headers {
				return CanonicalHeaders{}, errorCode(CodeBoundsExceeded)
			}

			root = record.Headers
			if len(message.Header["From"]) != 1 || root.From == "" || record.Metadata.DateSource != DateSourceHeader ||
				!validForwardHeader(message.Header.Get("From"), root.From) ||
				!validForwardHeader(message.Header.Get("Date"), root.Date) ||
				!validForwardHeader(message.Header.Get("Subject"), root.Subject) {
				return CanonicalHeaders{}, errorCode(CodeUnsupported)
			}
		}
	}

	return root, nil
}

var forwardWordPrefix = regexp.MustCompile(`=\?([^?[:space:]]+)\?[^?[:space:]]*\?`)

// The projection repairs invalid UTF-8. Account for literal U+FFFD in both
// raw text and encoded words before accepting that character in the preview.
func validForwardHeader(value, projected string) bool {
	if !utf8.ValidString(value) || strings.ContainsFunc(projected, func(r rune) bool { return unicode.IsControl(r) && r != '\t' }) {
		return false
	}

	decoder := mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
	replacements := strings.Count(value, "\uFFFD")
	for {
		prefix := forwardWordPrefix.FindStringSubmatchIndex(value)
		if prefix == nil {
			break
		}
		payload, rest, ok := strings.Cut(value[prefix[1]:], "?=")
		if !ok {
			break // An unterminated prefix is literal header text.
		}
		if payload == "" || strings.ContainsFunc(payload, unicode.IsSpace) {
			return false
		}
		word := value[prefix[0] : len(value)-len(rest)]
		decoded, err := decoder.Decode(word)
		if err != nil || !utf8.ValidString(decoded) {
			return false
		}
		label := value[prefix[2]:prefix[3]]
		if !strings.EqualFold(label, "utf-8") && strings.ContainsRune(decoded, utf8.RuneError) {
			// Recover the original octets to distinguish a literal U+FFFD
			// from a charset decoder's substitution.
			octets, err := decoder.Decode("=?utf-8" + word[len("=?")+len(label):])
			if err != nil {
				return false
			}
			strict, err := decodeForwardCharset([]byte(octets), label)
			if err != nil || string(strict) != decoded {
				return false
			}
		}
		replacements += strings.Count(decoded, "\uFFFD")
		value = rest
	}

	return replacements == strings.Count(projected, "\uFFFD")
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
	if kind == "multipart/related" && (!strings.EqualFold(params["type"], "text/html") ||
		params["start"] != entity.rootID) {
		return false
	}
	if kind == "text/html" && entity.rootID != "" && (len(header["Content-Id"]) != 1 || header.Get("Content-ID") != entity.rootID) {
		return false
	}
	transfer := cmp.Or(header.Get("Content-Transfer-Encoding"), "7bit")
	if strings.HasPrefix(kind, "multipart/") {
		return strings.EqualFold(transfer, "7bit") || strings.EqualFold(transfer, "8bit") || strings.EqualFold(transfer, "binary")
	}
	if kind != "text/plain" {
		return true
	}

	return strings.EqualFold(cmp.Or(params["format"], "fixed"), "fixed") &&
		strings.EqualFold(cmp.Or(params["charset"], "us-ascii"), cmp.Or(plain.charset, "us-ascii")) &&
		strings.EqualFold(transfer, cmp.Or(plain.encoding, "7bit"))
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
	if kind == "multipart/related" && (!forwardParameter(names, params, "type") || !forwardParameter(names, params, "start")) {
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
		fallthrough
	case "8bit":
		for line := range bytes.SplitSeq(raw, []byte("\r\n")) {
			if len(line) > 998 || bytes.ContainsAny(line, "\x00\r\n") {
				return "", errorCode(CodeUnsupported)
			}
		}
	case "binary":
	case "base64":
		for line := range bytes.SplitSeq(raw, []byte("\r\n")) {
			if len(line) > 76 || bytes.ContainsAny(line, "\r\n") {
				return "", errorCode(CodeUnsupported)
			}
		}
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
		raw, err = decodeForwardCharset(raw, part.charset)
		if err != nil {
			return "", err
		}
	}

	body := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if !utf8.ValidString(body) ||
		strings.ContainsFunc(body, func(r rune) bool { return unicode.IsControl(r) && r != '\n' && r != '\t' }) {
		return "", errorCode(CodeUnsupported)
	}
	return body, nil
}

// In HZ ASCII mode a tilde starts an escape. Each GB run contains pairs
// of graphic bytes between an opening ~{ and its closing ~} (RFC 1843).
var forwardHZ = regexp.MustCompile(`^(?:[\x00-\x7d\x7f]|~[~\n]|~\{(?:[!-\x77][!-~])+~\})*$`)

func decodeForwardCharset(raw []byte, charset string) ([]byte, error) {
	enc, err := ianaindex.MIME.Encoding(charset)
	if err != nil || enc == nil {
		return nil, errorCode(CodeUnsupported)
	}
	switch enc {
	case japanese.ISO2022JP:
		return decodeForwardISO2022JP(raw)
	case simplifiedchinese.HZGB2312:
		if !forwardHZ.Match(raw) {
			return nil, errorCode(CodeUnsupported)
		}
	case textunicode.UTF8:
		return raw, nil // The caller checks UTF-8, including charset aliases.
	}

	decoded, err := enc.NewDecoder().Bytes(raw)
	if err != nil {
		return nil, errorCode(CodeUnsupported)
	}

	// x/text substitutes U+FFFD for malformed code units. Only UTF and
	// GB18030 in the IANA decoder table can encode that character literally.
	replacements := bytes.Count(decoded, []byte("�"))
	switch enc {
	case textunicode.UTF16(textunicode.BigEndian, textunicode.UseBOM),
		textunicode.UTF16(textunicode.BigEndian, textunicode.IgnoreBOM),
		textunicode.UTF16(textunicode.LittleEndian, textunicode.IgnoreBOM):
		if replacements == 0 {
			return decoded, nil
		}

		var order binary.ByteOrder = binary.BigEndian
		if enc == textunicode.UTF16(textunicode.LittleEndian, textunicode.IgnoreBOM) ||
			(enc == textunicode.UTF16(textunicode.BigEndian, textunicode.UseBOM) && bytes.HasPrefix(raw, []byte{0xff, 0xfe})) {
			order = binary.LittleEndian
		}
		for len(raw) >= 2 {
			if order.Uint16(raw) == utf8.RuneError {
				replacements--
			}
			raw = raw[2:]
		}
	case simplifiedchinese.GB18030:
		for len(raw) > 0 {
			if raw[0] <= 0x80 {
				raw = raw[1:]
				continue
			}
			if raw[0] == 0xff || len(raw) < 2 {
				return nil, errorCode(CodeUnsupported)
			}

			switch {
			case raw[1] >= 0x40 && raw[1] != 0x7f && raw[1] != 0xff:
				raw = raw[2:]
			case len(raw) >= 4 && raw[1] >= '0' && raw[1] <= '9' &&
				raw[2] >= 0x81 && raw[2] <= 0xfe && raw[3] >= '0' && raw[3] <= '9':
				if string(raw[:4]) == "\x84\x31\xa4\x37" {
					replacements--
				}
				raw = raw[4:]
			default:
				return nil, errorCode(CodeUnsupported)
			}
		}
	}
	if replacements != 0 {
		return nil, errorCode(CodeUnsupported)
	}
	return decoded, nil
}

var jisRoman = strings.NewReplacer("\\", "¥", "~", "‾")

// RFC 1468 designations select the character set for each run. Validate the
// original byte ranges and final ASCII state before accepting the decoded text.
func decodeForwardISO2022JP(raw []byte) ([]byte, error) {
	if hasNonASCII(raw) {
		return nil, errorCode(CodeUnsupported)
	}

	decoder := japanese.ISO2022JP.NewDecoder()
	var out []byte
	for len(raw) > 0 {
		mode := "(B"
		encoded := raw
		if raw[0] == 0x1b {
			if len(raw) < 3 {
				return nil, errorCode(CodeUnsupported)
			}
			mode, raw = string(raw[1:3]), raw[3:]
		}

		run, _, _ := bytes.Cut(raw, []byte{0x1b})
		raw = raw[len(run):]
		switch mode {
		case "(B":
			out = append(out, run...)
		case "(J":
			out = append(out, jisRoman.Replace(string(run))...)
		case "$@", "$B":
			if len(run) == 0 || len(run)%2 != 0 || bytes.ContainsFunc(run, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
				return nil, errorCode(CodeUnsupported)
			}

			decoded, err := decoder.Bytes(encoded[:3+len(run)])
			if err != nil || bytes.ContainsRune(decoded, utf8.RuneError) {
				return nil, errorCode(CodeUnsupported)
			}
			out = append(out, decoded...)
		default:
			return nil, errorCode(CodeUnsupported)
		}
		if len(raw) == 0 && mode != "(B" {
			return nil, errorCode(CodeUnsupported)
		}
	}

	return out, nil
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
				if bytes.ContainsAny(raw[i+1:i+3], "abcdef") {
					return nil, errorCode(CodeUnsupported)
				}
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

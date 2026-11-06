package testkit

import (
	"bufio"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Construction limits for typed BODYSTRUCTURE descriptors. They bound the
// fixture only and say nothing about production parser safety limits.
const (
	maxBodyStructureMessages = 64
	maxBodyStructureNodes    = 128
	maxBodyStructureDepth    = 16
	maxBodyStructureParams   = 8
	maxBodyStructureValue    = 512
	maxBodyStructureBytes    = 64 << 10
)

// BodyPartKind selects how a BodyPart is rendered.
type BodyPartKind uint8

const (
	// BodyPartText is a text/* leaf with a line count.
	BodyPartText BodyPartKind = iota + 1
	// BodyPartBasic is a non-text, non-message leaf such as application/*.
	BodyPartBasic
	// BodyPartMultipart is a multipart/* node with ordered children.
	BodyPartMultipart
)

// BodyParam is one ordered name/value parameter.
type BodyParam struct {
	Name  string
	Value string
}

// BodyDisposition is an explicit Content-Disposition value and its ordered
// parameters. A size parameter is caller-supplied text, not a decoded size.
type BodyDisposition struct {
	Value  string
	Params []BodyParam
}

// BodyPart is one typed node of a synthetic BODYSTRUCTURE tree. Nothing is
// inferred from message bytes: Size is the caller-supplied transfer octet
// count and Lines applies only to text parts.
type BodyPart struct {
	Kind        BodyPartKind
	Type        string // ignored for multipart, which always renders children
	Subtype     string
	Params      []BodyParam
	Encoding    string // 7bit, 8bit, binary, base64 or quoted-printable; empty renders 7bit
	Size        uint32
	Lines       uint32
	Disposition *BodyDisposition
	Children    []*BodyPart
}

// bodyStructureEncodings lists accepted transfer encodings, case-insensitive.
var bodyStructureEncodings = map[string]bool{"7bit": true, "8bit": true, "binary": true, "base64": true, "quoted-printable": true}

// newBodyStructures validates descriptors against the rest of the options and
// renders each tree once, so later caller mutation cannot change served bytes.
// It returns nil when no descriptors are configured.
func newBodyStructures(options Options) ([]string, error) {
	if options.BodyStructures == nil {
		return nil, nil
	}

	if len(options.Messages) == 0 {
		return nil, errors.New("testkit: BodyStructures require custom Messages")
	}

	if len(options.BodyStructures) != len(options.Messages) {
		return nil, fmt.Errorf("testkit: %d BodyStructures for %d Messages", len(options.BodyStructures), len(options.Messages))
	}

	if len(options.Messages) > maxBodyStructureMessages {
		return nil, fmt.Errorf("testkit: BodyStructures support at most %d messages", maxBodyStructureMessages)
	}

	if options.MetadataFlags != MetadataFlagsOrdinary || options.Scenario != (Scenario{}) || options.Stateful != nil || options.Labels != nil {
		return nil, errors.New("testkit: BodyStructures cannot be combined with MetadataFlags, Scenario, Stateful or Labels")
	}

	rendered := make([]string, 0, len(options.BodyStructures))
	for index, root := range options.BodyStructures {
		nodes := 0
		if err := validateBodyPart(root, 1, &nodes, map[*BodyPart]bool{}); err != nil {
			return nil, fmt.Errorf("testkit: BodyStructures[%d]: %w", index, err)
		}

		var builder strings.Builder
		renderBodyPart(&builder, root)
		if builder.Len() > maxBodyStructureBytes {
			return nil, fmt.Errorf("testkit: BodyStructures[%d] renders %d bytes, limit %d", index, builder.Len(), maxBodyStructureBytes)
		}

		rendered = append(rendered, builder.String())
	}

	return rendered, nil
}

// validateBodyPart checks one node before descending. path holds the nodes
// on the current branch, so a cycle is refused before depth limits recurse.
func validateBodyPart(part *BodyPart, depth int, nodes *int, path map[*BodyPart]bool) error {
	if part == nil {
		return errors.New("nil body part")
	}

	if path[part] {
		return errors.New("cyclic body part")
	}

	*nodes++
	if *nodes > maxBodyStructureNodes {
		return fmt.Errorf("more than %d body parts", maxBodyStructureNodes)
	}

	if depth > maxBodyStructureDepth {
		return fmt.Errorf("body part depth exceeds %d", maxBodyStructureDepth)
	}

	if err := validateBodyPartFields(part); err != nil {
		return err
	}

	path[part] = true
	for _, child := range part.Children {
		if err := validateBodyPart(child, depth+1, nodes, path); err != nil {
			return err
		}
	}
	delete(path, part)

	return nil
}

func validateBodyPartFields(part *BodyPart) error {
	params := len(part.Params)
	if part.Disposition != nil {
		params += len(part.Disposition.Params)
	}
	if params > maxBodyStructureParams {
		return fmt.Errorf("more than %d parameters on one body part", maxBodyStructureParams)
	}

	values := []string{part.Subtype}
	for _, param := range part.Params {
		values = append(values, param.Name, param.Value)
	}
	if part.Disposition != nil {
		values = append(values, part.Disposition.Value)
		for _, param := range part.Disposition.Params {
			values = append(values, param.Name, param.Value)
		}
	}
	for _, value := range values {
		if err := validateBodyValue(value, false); err != nil {
			return err
		}
	}
	for _, value := range []string{part.Type, part.Encoding} {
		if err := validateBodyValue(value, true); err != nil {
			return err
		}
	}

	if part.Disposition != nil && part.Disposition.Value == "" {
		return errors.New("empty disposition value")
	}

	switch part.Kind {
	case BodyPartMultipart:
		if part.Type != "" && !strings.EqualFold(part.Type, "multipart") {
			return fmt.Errorf("multipart body part has type %q", part.Type)
		}

		if len(part.Children) == 0 || part.Encoding != "" || part.Size != 0 || part.Lines != 0 {
			return errors.New("multipart body part needs children and no encoding, size or lines")
		}
	case BodyPartText, BodyPartBasic:
		if len(part.Children) != 0 {
			return errors.New("leaf body part has children")
		}

		if part.Encoding != "" && !bodyStructureEncodings[strings.ToLower(part.Encoding)] {
			return fmt.Errorf("unsupported transfer encoding %q", part.Encoding)
		}

		text := strings.EqualFold(part.Type, "text")
		if part.Kind == BodyPartText && !text {
			return fmt.Errorf("text body part has type %q", part.Type)
		}

		if part.Kind == BodyPartBasic && (part.Type == "" || text || strings.EqualFold(part.Type, "multipart") || strings.EqualFold(part.Type, "message")) {
			return fmt.Errorf("unsupported basic body part type %q", part.Type)
		}

		if part.Kind == BodyPartBasic && part.Lines != 0 {
			return errors.New("basic body part has lines")
		}
	default:
		return fmt.Errorf("unsupported body part kind %d", part.Kind)
	}

	return nil
}

// validateBodyValue bounds one descriptor string. Only optional fields may be empty.
func validateBodyValue(value string, optional bool) error {
	if value == "" && !optional {
		return errors.New("empty body structure value")
	}

	if len(value) > maxBodyStructureValue {
		return fmt.Errorf("body structure value exceeds %d bytes", maxBodyStructureValue)
	}

	if !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
		return errors.New("body structure value is not single-line UTF-8")
	}

	return nil
}

// renderBodyPart writes the extended BODYSTRUCTURE form of a validated part.
func renderBodyPart(builder *strings.Builder, part *BodyPart) {
	builder.WriteByte('(')
	if part.Kind == BodyPartMultipart {
		for _, child := range part.Children {
			renderBodyPart(builder, child)
		}
		builder.WriteByte(' ')
		renderBodyString(builder, part.Subtype)
		builder.WriteByte(' ')
		renderBodyParams(builder, part.Params)
		builder.WriteByte(' ')
		renderBodyDisposition(builder, part.Disposition)
		builder.WriteString(" NIL NIL)")

		return
	}

	encoding := part.Encoding
	if encoding == "" {
		encoding = "7bit"
	}

	renderBodyString(builder, part.Type)
	builder.WriteByte(' ')
	renderBodyString(builder, part.Subtype)
	builder.WriteByte(' ')
	renderBodyParams(builder, part.Params)
	builder.WriteString(" NIL NIL ")
	renderBodyString(builder, encoding)
	builder.WriteString(" " + strconv.FormatUint(uint64(part.Size), 10))
	if part.Kind == BodyPartText {
		builder.WriteString(" " + strconv.FormatUint(uint64(part.Lines), 10))
	}
	builder.WriteString(" NIL ")
	renderBodyDisposition(builder, part.Disposition)
	builder.WriteString(" NIL NIL)")
}

func renderBodyParams(builder *strings.Builder, params []BodyParam) {
	if len(params) == 0 {
		builder.WriteString("NIL")
		return
	}

	builder.WriteByte('(')
	for index, param := range params {
		if index > 0 {
			builder.WriteByte(' ')
		}
		renderBodyString(builder, param.Name)
		builder.WriteByte(' ')
		renderBodyString(builder, param.Value)
	}
	builder.WriteByte(')')
}

func renderBodyDisposition(builder *strings.Builder, disposition *BodyDisposition) {
	if disposition == nil {
		builder.WriteString("NIL")
		return
	}

	builder.WriteByte('(')
	renderBodyString(builder, disposition.Value)
	builder.WriteByte(' ')
	renderBodyParams(builder, disposition.Params)
	builder.WriteByte(')')
}

// renderBodyString emits printable ASCII as a quoted string and anything else
// as a literal, which IMAP4rev1 requires for 8-bit data.
func renderBodyString(builder *strings.Builder, value string) {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] > 0x7e {
			builder.WriteString("{" + strconv.Itoa(len(value)) + "}\r\n" + value)
			return
		}
	}

	builder.WriteByte('"')
	builder.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value))
	builder.WriteByte('"')
}

// structureFetchItem is one parsed, supported FETCH data item.
type structureFetchItem struct {
	name    string // UID, FLAGS, RFC822.SIZE, BODYSTRUCTURE, HEADER or BODY
	partial bool
	offset  uint64
	size    uint64
}

// parseStructureFetch parses "<tag> UID FETCH <set> <items>" into the UID set
// and requested items. It refuses unsupported, non-peek and duplicate items.
func parseStructureFetch(raw string) (string, []structureFetchItem, error) {
	fields := strings.SplitN(raw, " ", 5)
	if len(fields) < 5 {
		return "", nil, errors.New("missing UID set or items")
	}

	list := fields[4]
	if strings.HasPrefix(list, "(") && strings.HasSuffix(list, ")") {
		list = list[1 : len(list)-1]
	}

	var items []structureFetchItem
	seen := map[string]bool{}
	for _, token := range strings.Fields(list) {
		item, err := parseStructureFetchItem(strings.ToUpper(token))
		if err != nil {
			return "", nil, err
		}

		if seen[item.name] {
			return "", nil, fmt.Errorf("duplicate FETCH item %s", item.name)
		}
		seen[item.name] = true

		if item.name != "UID" {
			items = append(items, item)
		}
	}

	return fields[3], items, nil
}

func parseStructureFetchItem(token string) (structureFetchItem, error) {
	switch token {
	case "UID", "FLAGS", "RFC822.SIZE", "BODYSTRUCTURE":
		return structureFetchItem{name: token}, nil
	case "BODY.PEEK[HEADER]":
		return structureFetchItem{name: "HEADER"}, nil
	case "BODY.PEEK[]":
		return structureFetchItem{name: "BODY"}, nil
	}

	partial, ok := strings.CutPrefix(token, "BODY.PEEK[]<")
	if !ok || !strings.HasSuffix(partial, ">") {
		return structureFetchItem{}, fmt.Errorf("unsupported FETCH item %q", token)
	}

	offsetText, sizeText, _ := strings.Cut(strings.TrimSuffix(partial, ">"), ".")
	offset, offsetErr := strconv.ParseUint(offsetText, 10, 32)
	size, sizeErr := strconv.ParseUint(sizeText, 10, 32)
	if offsetErr != nil || sizeErr != nil || size == 0 {
		return structureFetchItem{}, fmt.Errorf("invalid partial FETCH item %q", token)
	}

	return structureFetchItem{name: "BODY", partial: true, offset: offset, size: size}, nil
}

// uidSetContains reports whether uid is in a set of numbers, ranges and
// comma lists.
func uidSetContains(set string, uid uint32) (bool, error) {
	matched := false
	for _, span := range strings.Split(set, ",") {
		startText, endText, bounded := strings.Cut(span, ":")
		if !bounded {
			endText = startText
		}

		start, startErr := strconv.ParseUint(startText, 10, 32)
		end, endErr := strconv.ParseUint(endText, 10, 32)
		if startErr != nil || endErr != nil {
			return false, fmt.Errorf("invalid UID set %q", set)
		}

		if start > end {
			start, end = end, start
		}
		matched = matched || uint64(uid) >= start && uint64(uid) <= end
	}

	return matched, nil
}

// writeStructureFetch answers a custom-message UID FETCH when BodyStructures
// are configured. UID always leads; every other item, including literals, is
// emitted only when requested. It never changes Seen.
func (server *Server) writeStructureFetch(writer *bufio.Writer, tag, raw string) bool {
	set, items, err := parseStructureFetch(raw)
	if err != nil {
		return server.writeLines(writer, tagged(tag, "BAD "+err.Error()))
	}

	if _, err := uidSetContains(set, 0); err != nil {
		return server.writeLines(writer, tagged(tag, "BAD "+err.Error()))
	}

	for index, body := range server.options.Messages {
		uid := uint32(101 + index)
		if matched, _ := uidSetContains(set, uid); !matched {
			continue
		}

		var response strings.Builder
		fmt.Fprintf(&response, "* %d FETCH (UID %d", index+1, uid)
		for _, item := range items {
			switch item.name {
			case "FLAGS":
				flags := ""
				if server.fixtureSeen(index) {
					flags = `\Seen`
				}
				fmt.Fprintf(&response, " FLAGS (%s)", flags)
			case "RFC822.SIZE":
				fmt.Fprintf(&response, " RFC822.SIZE %d", len(body))
			case "BODYSTRUCTURE":
				response.WriteString(" BODYSTRUCTURE " + server.bodyStructures[index])
			case "HEADER":
				header, _, _ := strings.Cut(body, "\r\n\r\n")
				header += "\r\n\r\n"
				fmt.Fprintf(&response, " BODY[HEADER] {%d}\r\n%s", len(header), header)
			case "BODY":
				literal, section := body, "BODY[]"
				if item.partial {
					start := min(item.offset, uint64(len(body)))
					end := min(start+item.size, uint64(len(body)))
					literal, section = body[start:end], fmt.Sprintf("BODY[]<%d>", item.offset)
				}
				fmt.Fprintf(&response, " %s {%d}\r\n%s", section, len(literal), literal)
			}
		}
		response.WriteByte(')')

		if err := server.writeLine(writer, response.String()); err != nil {
			return false
		}
	}

	return server.writeLines(writer, tagged(tag, "OK FETCH completed"))
}

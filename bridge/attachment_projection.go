package bridge

import (
	"errors"
	"maps"
	"mime"
	"slices"
	"strings"
)

// attachmentProjector walks a decoded BODYSTRUCTURE in server order with the
// selection and limit rules of NormalizeMessage's parseEntity: every visited
// entity counts as a MIME part, an attachment-qualified entity is listed once
// without enumerating inside it, and an unqualified embedded message is
// omitted with MIMEDepth set.
type attachmentProjector struct {
	limits      NormalizeLimits
	parts       int
	attachments []AttachmentStructureEntry
	truncation  Truncation
}

// projectedEntity is the normalized media type, parameters and disposition
// of one visited entity.
type projectedEntity struct {
	mediaType             string
	parameters            map[string]string
	disposition           string
	dispositionParameters map[string]string
	children              []bodyStructure
}

// projectAttachmentStructure projects the attachment metadata of structure
// under the adapter's MIME part and attachment count bounds and the default
// depth and field limits. A list that is empty without any truncation flag is
// complete.
func projectAttachmentStructure(structure bodyStructure, bounds Bounds) ([]AttachmentStructureEntry, Truncation, error) {
	projector := attachmentProjector{
		limits:      normalizedLimits(NormalizeLimits{MaxMIMEParts: bounds.MaxMimeParts, MaxAttachmentCount: bounds.MaxAttachmentCount}),
		attachments: []AttachmentStructureEntry{},
	}

	projector.parts++
	if err := projector.visit(structure, 0); err != nil && !errors.Is(err, errMIMEPartsLimit) {
		return nil, Truncation{}, err
	}

	return projector.attachments, projector.truncation, nil
}

// visit projects one entity already counted as a part. Each actual child is
// counted before it is visited, so a container whose last child fits the
// part limit exactly stays complete.
func (projector *attachmentProjector) visit(structure bodyStructure, depth int) error {
	if depth > projector.limits.MaxMIMEDepth {
		projector.truncation.MIMEDepth = true

		return nil
	}

	entity, err := structureEntity(structure)
	if err != nil {
		return err
	}

	filename := entity.dispositionParameters["filename"]
	if filename == "" {
		filename = entity.parameters["name"]
	}

	if entity.disposition == "attachment" || filename != "" {
		if metadata, ok := boundedAttachment(len(projector.attachments), projector.limits, &projector.truncation, filename, entity.mediaType, entity.disposition, entity.dispositionParameters["size"]); ok {
			projector.attachments = append(projector.attachments, AttachmentStructureEntry{AttachmentMetadata: metadata})
		}

		return nil
	}

	for _, child := range entity.children {
		if projector.parts >= projector.limits.MaxMIMEParts {
			projector.truncation.MIMEParts = true

			return errMIMEPartsLimit
		}
		projector.parts++

		if err := projector.visit(child, depth+1); err != nil {
			return err
		}
	}

	if entity.mediaType == "message/rfc822" || entity.mediaType == "message/global" {
		projector.truncation.MIMEDepth = true
	}

	return nil
}

// structureEntity normalizes one decoded node.
func structureEntity(structure bodyStructure) (projectedEntity, error) {
	entity := projectedEntity{}
	mediaType := ""
	var parameters map[string]string

	switch part := structure.(type) {
	case *singlePartStructure:
		if part == nil {
			return projectedEntity{}, &NormalizeError{Code: errorCodeMalformedMIME}
		}

		mediaType = part.Type + "/" + part.Subtype
		parameters = part.Params
	case *multiPartStructure:
		if part == nil {
			return projectedEntity{}, &NormalizeError{Code: errorCodeMalformedMIME}
		}

		mediaType = "multipart/" + part.Subtype
		if part.Extended != nil {
			parameters = part.Extended.Params
		}
		entity.children = part.Children
	default:
		return projectedEntity{}, &NormalizeError{Code: errorCodeMalformedMIME}
	}

	var err error
	entity.mediaType, entity.parameters, err = parseStructureMediaType(mediaType, parameters)
	if err != nil {
		return projectedEntity{}, err
	}

	entity.dispositionParameters = map[string]string{}
	if value := structure.Disposition(); value != nil {
		entity.disposition, entity.dispositionParameters, err = parseStructureMediaType(value.Value, value.Params)
		if err != nil {
			return projectedEntity{}, err
		}
	}

	return entity, nil
}

// parseStructureMediaType rebuilds a header value from a decoded media type
// or disposition and its parameter map, and parses it with
// mime.ParseMediaType, so RFC 2231 extended and continued parameters decode
// and take precedence exactly as on the body path. Keys, starred ones
// included, are kept verbatim and every value is quoted as received; nothing
// is re-encoded. A base or key that is not a token, or a value that cannot be
// quoted, fails as malformed MIME.
func parseStructureMediaType(base string, parameters map[string]string) (string, map[string]string, error) {
	if strings.ContainsRune(base, ';') {
		return "", nil, &NormalizeError{Code: errorCodeMalformedMIME}
	}

	var header strings.Builder
	header.WriteString(base)
	for _, key := range slices.Sorted(maps.Keys(parameters)) {
		if !structureParameterToken(key) {
			return "", nil, &NormalizeError{Code: errorCodeMalformedMIME}
		}

		header.WriteString("; ")
		header.WriteString(key)
		header.WriteString(`="`)
		header.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(parameters[key]))
		header.WriteString(`"`)
	}

	mediaType, parsed, err := mime.ParseMediaType(header.String())
	if err != nil {
		return "", nil, &NormalizeError{Code: errorCodeMalformedMIME}
	}

	return mediaType, parsed, nil
}

// structureParameterToken reports whether key is an RFC 2045 token, which
// admits the stars of RFC 2231 parameter names.
func structureParameterToken(key string) bool {
	if key == "" {
		return false
	}

	for index := 0; index < len(key); index++ {
		character := key[index]
		if character <= ' ' || character >= 0x7f || strings.IndexByte(`()<>@,;:\"/[]?=`, character) >= 0 {
			return false
		}
	}

	return true
}

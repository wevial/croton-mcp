package bridge

import (
	"cmp"
	"errors"
	"maps"
	"mime"
	"slices"
	"strings"
)

type attachmentProjector struct {
	limits      NormalizeLimits
	parts       int
	attachments []AttachmentStructureEntry
	truncation  Truncation
}

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

	if err := projector.visit(structure, 0); err != nil && !errors.Is(err, errMIMEPartsLimit) {
		return nil, Truncation{}, err
	}

	return projector.attachments, projector.truncation, nil
}

// Count only actual nodes, including a node omitted at the depth limit.
func (projector *attachmentProjector) visit(structure bodyStructure, depth int) error {
	if projector.parts >= projector.limits.MaxMIMEParts {
		projector.truncation.MIMEParts = true

		return errMIMEPartsLimit
	}
	projector.parts++

	if depth > projector.limits.MaxMIMEDepth {
		projector.truncation.MIMEDepth = true

		return nil
	}

	entity, err := structureEntity(structure)
	if err != nil {
		return err
	}

	filename := cmp.Or(entity.dispositionParameters["filename"], entity.parameters["name"])
	if entity.disposition == "attachment" || filename != "" {
		if metadata, ok := boundedAttachment(len(projector.attachments), projector.limits, &projector.truncation, filename, entity.mediaType, entity.disposition, entity.dispositionParameters["size"]); ok {
			projector.attachments = append(projector.attachments, AttachmentStructureEntry{AttachmentMetadata: metadata})
		}

		return nil
	}

	for _, child := range entity.children {
		if err := projector.visit(child, depth+1); err != nil {
			return err
		}
	}

	if entity.mediaType == "message/rfc822" || entity.mediaType == "message/global" {
		projector.truncation.MIMEDepth = true
	}

	return nil
}

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

	if value := structure.Disposition(); value != nil {
		entity.disposition, entity.dispositionParameters, err = parseStructureMediaType(value.Value, value.Params)
		if err != nil {
			return projectedEntity{}, err
		}
	}

	return entity, nil
}

var structureParameterEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// Quote values without re-encoding RFC 2231 keys or their continuations.
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
		header.WriteString(structureParameterEscaper.Replace(parameters[key]))
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

package bridge

import (
	"errors"
	"maps"
	"mime"
	"slices"
	"strconv"
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
// of one visited entity, and its transfer octets when it is a single part.
type projectedEntity struct {
	mediaType             string
	parameters            map[string]string
	disposition           string
	dispositionParameters map[string]string
	children              []bodyStructure
	hasTransferOctets     bool
	transferOctets        uint32
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
		projector.appendAttachment(entity, filename)

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

// appendAttachment lists one attachment within the count and field limits.
// Fields are truncated and repaired by truncateUTF8 exactly as
// NormalizeMessage does, and any truncation sets Attachments.
func (projector *attachmentProjector) appendAttachment(entity projectedEntity, filename string) {
	if len(projector.attachments) >= projector.limits.MaxAttachmentCount {
		projector.truncation.Attachments = true

		return
	}

	filename, filenameTruncated := truncateUTF8(filename, projector.limits.MaxFilenameBytes)
	contentType, contentTypeTruncated := truncateUTF8(entity.mediaType, projector.limits.MaxContentTypeBytes)
	disposition, dispositionTruncated := truncateUTF8(entity.disposition, projector.limits.MaxDispositionBytes)
	if filenameTruncated || contentTypeTruncated || dispositionTruncated {
		projector.truncation.Attachments = true
	}

	entry := AttachmentStructureEntry{
		AttachmentMetadata: AttachmentMetadata{Filename: filename, ContentType: contentType, Disposition: disposition},
		HasTransferOctets:  entity.hasTransferOctets,
		TransferOctets:     entity.transferOctets,
	}
	if size, err := strconv.ParseInt(entity.dispositionParameters["size"], 10, 64); err == nil && size >= 0 {
		entry.DeclaredSize = size
		entry.HasDeclaredSize = true
	}

	projector.attachments = append(projector.attachments, entry)
}

// structureEntity normalizes one decoded node. A multipart container has no
// transfer octets of its own: the server declares none, and neither a sum of
// its children nor a declared size stands in for them.
func structureEntity(structure bodyStructure) (projectedEntity, error) {
	entity := projectedEntity{}
	mediaType, disposition, hasDisposition := "", "", false
	var parameters, dispositionParameters map[string]string

	switch part := structure.(type) {
	case *singlePartStructure:
		if part == nil {
			return projectedEntity{}, &NormalizeError{Code: errorCodeMalformedMIME}
		}

		mediaType = part.Type + "/" + part.Subtype
		parameters = part.Params
		if value := part.Disposition(); value != nil {
			disposition, dispositionParameters, hasDisposition = value.Value, value.Params, true
		}
		entity.hasTransferOctets = true
		entity.transferOctets = part.Size
	case *multiPartStructure:
		if part == nil {
			return projectedEntity{}, &NormalizeError{Code: errorCodeMalformedMIME}
		}

		mediaType = "multipart/" + part.Subtype
		if part.Extended != nil {
			parameters = part.Extended.Params
		}
		if value := part.Disposition(); value != nil {
			disposition, dispositionParameters, hasDisposition = value.Value, value.Params, true
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
	if hasDisposition {
		entity.disposition, entity.dispositionParameters, err = parseStructureMediaType(disposition, dispositionParameters)
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

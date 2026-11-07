package bridge

import (
	"context"
	"strconv"
	"strings"
)

// MessageAttachmentStructure is the attachment metadata of one selected
// message, projected from the extended BODYSTRUCTURE of a fresh dedicated
// session bound to the opaque ID's generation. It never carries body bytes,
// message size or read state.
type MessageAttachmentStructure struct {
	ID          string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Attachments []AttachmentStructureEntry
	Truncation  Truncation
}

// AttachmentStructureEntry is one attachment's metadata and the transfer
// octets the server declares for its part, which are neither its decoded size
// nor the message size.
type AttachmentStructureEntry struct {
	AttachmentMetadata
	HasTransferOctets bool
	TransferOctets    uint32
}

// GetMessageAttachmentStructure resolves an opaque message identifier on a
// fresh dedicated session that confirms its UIDVALIDITY generation and UID
// before the one structure FETCH. It mirrors execute's serialization, closed
// check and single transport replay without touching the pooled session.
//
// It is an internal first slice: only a plain single-part root is projected,
// and every other layout or parameter form is refused as unsupported.
func (adapter *Adapter) GetMessageAttachmentStructure(ctx context.Context, identifier string) (MessageAttachmentStructure, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	if err := adapter.acquire(ctx); err != nil {
		return MessageAttachmentStructure{}, err
	}
	defer func() { adapter.gate <- struct{}{} }()

	adapter.mu.Lock()
	closed := adapter.closed
	adapter.mu.Unlock()
	if closed {
		return MessageAttachmentStructure{}, errorCode(CodeAdapterClosed)
	}

	operationContext, cancel := adapter.operationContext(ctx)
	defer cancel()

	selected, err := adapter.readStructureAttempts(operationContext, structureRequest{Mailbox: payload.Mailbox, UID: payload.UID, UIDValidity: payload.UIDValidity})
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	attachments, err := plainAttachmentStructure(selected.Structure)
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	return MessageAttachmentStructure{
		ID:          identifier,
		Mailbox:     payload.Mailbox,
		UIDValidity: payload.UIDValidity,
		UID:         payload.UID,
		Attachments: attachments,
	}, nil
}

// readStructureAttempts runs at most two fresh dedicated reads under ctx.
// Only a first-attempt transport failure is replayed, and nothing of that
// attempt is kept.
func (adapter *Adapter) readStructureAttempts(ctx context.Context, request structureRequest) (selectedStructure, error) {
	for attempt := 0; attempt < 2; attempt++ {
		selected, err := adapter.reader(ctx, adapter.config, request)
		if err == nil {
			return selected, nil
		}

		err = mapIMAPError(ctx, err)
		if attempt == 0 && adapter.canReplay(ctx, err) {
			continue
		}

		return selectedStructure{}, err
	}

	return selectedStructure{}, errorCode(CodeBridgeUnreachable)
}

// plainAttachmentStructure projects a plain single-part root with the
// selection rules of NormalizeMessage: a disposition filename before a
// Content-Type name, and an attachment disposition or any filename makes the
// part an attachment. A supported non-attachment root is a complete empty
// list.
//
// Interim restriction: multipart and embedded-message roots, RFC 2231
// extended or continued filename and name parameters, and values over the
// per-field limits are refused as unsupported rather than partially listed.
func plainAttachmentStructure(structure bodyStructure) ([]AttachmentStructureEntry, error) {
	part, ok := structure.(*singlePartStructure)
	if !ok || part == nil || part.MessageRFC822 != nil {
		return nil, errorCode(CodeUnsupported)
	}

	contentType := part.MediaType()
	if contentType == "message/rfc822" || contentType == "message/global" {
		return nil, errorCode(CodeUnsupported)
	}

	disposition := ""
	dispositionParameters := map[string]string{}
	if value := part.Disposition(); value != nil {
		disposition = strings.ToLower(value.Value)
		if value.Params != nil {
			dispositionParameters = value.Params
		}
	}
	if extendedParameter(part.Params, "name") || extendedParameter(dispositionParameters, "filename") {
		return nil, errorCode(CodeUnsupported)
	}

	filename := dispositionParameters["filename"]
	if filename == "" {
		filename = part.Params["name"]
	}

	attachments := []AttachmentStructureEntry{}
	if !strings.EqualFold(disposition, "attachment") && filename == "" {
		return attachments, nil
	}

	filename, filenameTruncated := truncateUTF8(filename, defaultMaxFilenameBytes)
	contentType, contentTypeTruncated := truncateUTF8(contentType, defaultMaxContentTypeBytes)
	disposition, dispositionTruncated := truncateUTF8(disposition, defaultMaxDispositionBytes)
	if filenameTruncated || contentTypeTruncated || dispositionTruncated {
		return nil, errorCode(CodeUnsupported)
	}

	entry := AttachmentStructureEntry{
		AttachmentMetadata: AttachmentMetadata{Filename: filename, ContentType: contentType, Disposition: disposition},
		HasTransferOctets:  true,
		TransferOctets:     part.Size,
	}
	if size, err := strconv.ParseInt(dispositionParameters["size"], 10, 64); err == nil && size >= 0 {
		entry.DeclaredSize = size
		entry.HasDeclaredSize = true
	}

	return append(attachments, entry), nil
}

// extendedParameter reports whether parameters carry an RFC 2231 extended or
// continued form of name, such as name* or name*0*.
func extendedParameter(parameters map[string]string, name string) bool {
	for key := range parameters {
		if strings.HasPrefix(key, name+"*") {
			return true
		}
	}

	return false
}

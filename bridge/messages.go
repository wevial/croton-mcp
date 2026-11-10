package bridge

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"strings"
)

// GetMessageMetadata resolves an opaque message identifier only after a fresh
// read-only EXAMINE confirms its UIDVALIDITY generation.
func (adapter *Adapter) GetMessageMetadata(ctx context.Context, identifier string) (MessageMetadata, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageMetadata{}, err
	}

	var result MessageMetadata
	err = adapter.execute(ctx, func(operationContext context.Context, session bridgeSession) error {
		snapshot, err := session.Examine(operationContext, payload.Mailbox)
		if err != nil {
			return err
		}
		if snapshot.UIDValidity != payload.UIDValidity {
			return errorCode(CodeStaleMessageID)
		}
		if err := requireMessageUID(operationContext, session, payload.UID); err != nil {
			return err
		}
		metadata, err := session.UIDFetchMetadata(operationContext, payload.Mailbox, []uint32{payload.UID}, adapter.config.Bounds.MaxHeaderBytes)
		if err != nil {
			return err
		}
		if len(metadata) != 1 || metadata[0].uid != payload.UID {
			return errorCode(CodeStaleMessageID)
		}
		result = metadata[0]
		result.ID = identifier
		result.Mailbox = payload.Mailbox
		result.UIDValidity = payload.UIDValidity
		result.UID = payload.UID
		return nil
	})
	if err != nil {
		return MessageMetadata{}, err
	}

	return result, nil
}

// GetMessage resolves an opaque message identifier after a fresh read-only
// EXAMINE confirms its UIDVALIDITY generation and UID, then reads its
// BODYSTRUCTURE, its header and at most MaxBodyBytes of its first text/plain
// leaf, else its first text/html leaf. Attachment bytes are never read.
func (adapter *Adapter) GetMessage(ctx context.Context, identifier string) (NormalizedMessage, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return NormalizedMessage{}, err
	}

	bounds := adapter.config.Bounds
	limits := normalizedLimits(NormalizeLimits{MaxMIMEParts: bounds.MaxMimeParts, MaxDecodedTextBytes: bounds.MaxBodyBytes})
	var structure bodyStructure
	var headers MessageHeaders
	var section *textSection
	var raw []byte
	err = adapter.execute(ctx, func(operationContext context.Context, session bridgeSession) error {
		snapshot, err := session.Examine(operationContext, payload.Mailbox)
		if err != nil {
			return err
		}
		if snapshot.UIDValidity != payload.UIDValidity {
			return errorCode(CodeStaleMessageID)
		}
		if err := requireMessageUID(operationContext, session, payload.UID); err != nil {
			return err
		}

		structure, err = session.fetchUIDStructure(operationContext, payload.UID)
		if err != nil {
			return err
		}
		headers, err = session.UIDFetchHeaders(operationContext, payload.UID, bounds.MaxHeaderBytes)
		if err != nil {
			return err
		}

		selector := textSelector{limits: limits, parts: 1}
		if err := selector.visit(structure, nil, 0); err != nil && !errors.Is(err, errMIMEPartsLimit) {
			return err
		}
		section = selector.plain
		if section == nil {
			section = selector.html
		}
		if section == nil {
			return nil
		}

		raw, err = session.UIDFetchBody(operationContext, payload.UID, section.part, bounds.MaxBodyBytes)
		return err
	})
	if err != nil {
		return NormalizedMessage{}, err
	}

	attachments, truncation, err := projectAttachmentStructure(structure, bounds)
	if err != nil {
		return NormalizedMessage{}, err
	}

	message := NormalizedMessage{Headers: headers.Headers, TextSource: TextSourceNone, Truncation: truncation}
	message.Truncation.Headers = headers.Truncation.Headers
	message.Truncation.HeaderCount = headers.Truncation.HeaderCount
	message.Truncation.References = headers.Truncation.References
	for _, attachment := range attachments {
		message.Attachments = append(message.Attachments, attachment.AttachmentMetadata)
	}

	if section != nil {
		if err := section.decode(&message, raw, int64(bounds.MaxBodyBytes), limits.MaxDecodedTextBytes); err != nil {
			return NormalizedMessage{}, err
		}
	}

	return message, nil
}

// textSelector finds the first text/plain and first text/html leaves that
// are not attachments, under the MIME part and depth limits.
type textSelector struct {
	limits NormalizeLimits
	parts  int
	plain  *textSection
	html   *textSection
}

// textSection is one selected leaf's section path and decoding parameters.
type textSection struct {
	part     []int
	source   TextSource
	encoding string
	charset  string
	size     uint32
}

func (selector *textSelector) visit(structure bodyStructure, part []int, depth int) error {
	if depth > selector.limits.MaxMIMEDepth || selector.plain != nil {
		return nil
	}

	entity, err := structureEntity(structure)
	if err != nil {
		return err
	}

	if entity.disposition == "attachment" || entity.dispositionParameters["filename"] != "" || entity.parameters["name"] != "" {
		return nil
	}

	for index, child := range entity.children {
		if selector.parts >= selector.limits.MaxMIMEParts {
			return errMIMEPartsLimit
		}
		selector.parts++

		if err := selector.visit(child, append(slices.Clone(part), index+1), depth+1); err != nil {
			return err
		}
	}

	leaf, ok := structure.(*singlePartStructure)
	if !ok {
		return nil
	}
	if len(part) == 0 {
		part = []int{1}
	}

	section := &textSection{part: part, encoding: leaf.Encoding, charset: entity.parameters["charset"], size: leaf.Size}
	switch {
	case entity.mediaType == "text/plain" && selector.plain == nil:
		section.source = TextSourcePlain
		selector.plain = section
	case entity.mediaType == "text/html" && selector.html == nil:
		section.source = TextSourceHTML
		selector.html = section
	}

	return nil
}

// decode sets message's text from raw, the first fetchLimit bytes of the
// section. A cut base64 section is decoded only up to its last complete
// quantum and a cut quoted-printable section only up to its last complete
// line, so a split quantum or escape is never decoded.
func (section *textSection) decode(message *NormalizedMessage, raw []byte, fetchLimit int64, textLimit int) error {
	cut := int64(section.size) > fetchLimit
	encoding := strings.ToLower(strings.TrimSpace(section.encoding))
	switch {
	case cut && encoding == "base64":
		keep, characters := 0, 0
		for index, value := range raw {
			if value == '\r' || value == '\n' {
				continue
			}

			characters++
			if characters%4 == 0 {
				keep = index + 1
			}
		}
		raw = raw[:keep]
	case cut && encoding == "quoted-printable":
		raw = raw[:bytes.LastIndexByte(raw, '\n')+1]
	}

	decoded, transferTruncated, err := decodeTransfer(bytes.NewReader(raw), encoding, textLimit)
	if err != nil {
		return err
	}

	text, charsetTruncated, err := decodeCharset(decoded, section.charset, textLimit)
	if err != nil {
		return err
	}

	htmlTruncated := false
	if section.source == TextSourceHTML {
		text, htmlTruncated = htmlText(text, textLimit)
	}

	message.Text = text
	message.TextSource = section.source
	message.Truncation.Text = cut || transferTruncated || charsetTruncated || htmlTruncated
	return nil
}

// GetMessageHeaders resolves an opaque message identifier after a fresh
// read-only EXAMINE confirms its UIDVALIDITY generation, then returns the
// metadata and bounded canonical headers of one header-only FETCH. The body
// is never read.
func (adapter *Adapter) GetMessageHeaders(ctx context.Context, identifier string) (MessageHeaders, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageHeaders{}, err
	}

	var result MessageHeaders
	err = adapter.execute(ctx, func(operationContext context.Context, session bridgeSession) error {
		snapshot, err := session.Examine(operationContext, payload.Mailbox)
		if err != nil {
			return err
		}
		if snapshot.UIDValidity != payload.UIDValidity {
			return errorCode(CodeStaleMessageID)
		}
		if err := requireMessageUID(operationContext, session, payload.UID); err != nil {
			return err
		}

		record, err := session.UIDFetchHeaders(operationContext, payload.UID, adapter.config.Bounds.MaxHeaderBytes)
		if err != nil {
			return err
		}

		result = record
		result.Metadata.ID = identifier
		result.Metadata.Mailbox = payload.Mailbox
		result.Metadata.UIDValidity = payload.UIDValidity
		result.Metadata.UID = payload.UID
		return nil
	})
	if err != nil {
		return MessageHeaders{}, err
	}

	return result, nil
}

func requireMessageUID(ctx context.Context, session bridgeSession, uid uint32) error {
	uids, err := session.UIDSearchWindow(ctx, SearchQuery{}, uidWindow{Start: uid, End: uid}, 1)
	if err != nil {
		if CodeOf(err) == CodeBoundsExceeded {
			return errorCode(CodeIMAPProtocol)
		}
		return err
	}
	if len(uids) == 0 {
		return errorCode(CodeStaleMessageID)
	}
	if len(uids) != 1 || uids[0] != uid {
		return errorCode(CodeIMAPProtocol)
	}
	return nil
}

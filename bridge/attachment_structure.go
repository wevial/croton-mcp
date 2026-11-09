package bridge

import "context"

// MessageAttachmentStructure is the attachment metadata of one selected
// message, projected from its extended BODYSTRUCTURE after the opaque ID's
// generation is confirmed. It never carries body bytes, message size or read
// state.
type MessageAttachmentStructure struct {
	ID          string
	Mailbox     string
	UIDValidity uint32
	UID         uint32
	Attachments []AttachmentStructureEntry
	Truncation  Truncation
}

// AttachmentStructureEntry is one attachment's metadata.
type AttachmentStructureEntry struct {
	AttachmentMetadata
}

// GetMessageAttachmentStructure resolves an opaque message identifier after a
// fresh read-only EXAMINE confirms its UIDVALIDITY generation and UID, then
// projects the structure of one bounded UID FETCH. A session without the
// optional structure capability is refused; the body is never read as a
// fallback.
func (adapter *Adapter) GetMessageAttachmentStructure(ctx context.Context, identifier string) (MessageAttachmentStructure, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	var structure bodyStructure
	err = adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		reader, ok := session.(structureSession)
		if !ok {
			return errorCode(CodeUnsupported)
		}

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

		structure, err = reader.fetchUIDStructure(operationContext, payload.UID)
		return err
	})
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	attachments, truncation, err := projectAttachmentStructure(structure, adapter.config.Bounds)
	if err != nil {
		return MessageAttachmentStructure{}, err
	}

	return MessageAttachmentStructure{
		ID:          identifier,
		Mailbox:     payload.Mailbox,
		UIDValidity: payload.UIDValidity,
		UID:         payload.UID,
		Attachments: attachments,
		Truncation:  truncation,
	}, nil
}

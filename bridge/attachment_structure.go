package bridge

import "context"

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
// The structure is projected by projectAttachmentStructure under the
// adapter's bounds.
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

	attachments, truncation, err := projectAttachmentStructure(selected.Structure, adapter.config.Bounds)
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

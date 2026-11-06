package bridge

import "context"

// GetMessageMetadata resolves an opaque message identifier only after a fresh
// read-only EXAMINE confirms its UIDVALIDITY generation.
func (adapter *Adapter) GetMessageMetadata(ctx context.Context, identifier string) (MessageMetadata, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageMetadata{}, err
	}

	var result MessageMetadata
	err = adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
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

// GetMessageBody returns at most MaxBodyBytes of a BODY.PEEK partial fetch.
// The session independently enforces the cap if the server ignores the range.
func (adapter *Adapter) GetMessageBody(ctx context.Context, identifier string) ([]byte, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return nil, err
	}

	var body []byte
	err = adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
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
		body, err = session.UIDFetchBody(operationContext, payload.UID, adapter.config.Bounds.MaxBodyBytes)
		return err
	})
	if err != nil {
		return nil, err
	}

	return body, nil
}

// GetMessageHeaders resolves an opaque message identifier after a fresh
// read-only EXAMINE confirms its UIDVALIDITY generation, then returns the
// metadata and bounded canonical headers of one header-only FETCH. A session
// without the optional header capability is refused before any content
// FETCH; the body is never read as a fallback.
func (adapter *Adapter) GetMessageHeaders(ctx context.Context, identifier string) (MessageHeaders, error) {
	payload, err := adapter.decodeMessageID(identifier)
	if err != nil {
		return MessageHeaders{}, err
	}

	var result MessageHeaders
	err = adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		result = MessageHeaders{}
		reader, ok := session.(headerSession)
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

		record, err := reader.UIDFetchHeaders(operationContext, payload.UID, adapter.config.Bounds.MaxHeaderBytes)
		if err != nil {
			return err
		}
		if record.metadata.uid != payload.UID {
			return errorCode(CodeIMAPProtocol)
		}

		metadata := record.metadata
		metadata.ID = identifier
		metadata.Mailbox = payload.Mailbox
		metadata.UIDValidity = payload.UIDValidity
		metadata.UID = payload.UID
		result = MessageHeaders{Metadata: metadata, Headers: record.headers, Truncation: record.truncation}
		return nil
	})
	if err != nil {
		return MessageHeaders{}, err
	}

	return result, nil
}

func requireMessageUID(ctx context.Context, session readSession, uid uint32) error {
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

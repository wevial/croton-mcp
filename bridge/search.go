package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"time"
)

const (
	maxMessageIDBytes        = 1024
	maxMessageIDEncodedBytes = 1366
)

type messageIDPayload struct {
	Mailbox     string `json:"m"`
	UIDValidity uint32 `json:"v"`
	UID         uint32 `json:"u"`
}

// SearchQuery accepts structured, bounded search criteria. It deliberately
// does not expose raw IMAP query syntax.
type SearchQuery struct {
	Mailbox string
	Since   time.Time
	Before  time.Time
	Sender  string
	Subject string
	Unread  bool
}

// MessageMetadata is bounded metadata for one message. ID is an adapter-scoped
// opaque identifier that binds the mailbox, UIDVALIDITY and UID.
type MessageMetadata struct {
	ID      string
	Mailbox string
	Subject string
	Size    int64

	uid uint32
}

// SearchMail searches a finite sequence of descending UID windows. It never
// issues an unbounded whole-mailbox SEARCH.
func (adapter *Adapter) SearchMail(ctx context.Context, query SearchQuery) ([]MessageMetadata, error) {
	if query.Mailbox == "" {
		return nil, errorCode(CodeMailboxNotFound)
	}
	if len(query.Mailbox) > maxMailboxNameBytes || len(query.Sender) > maxSearchTermBytes || len(query.Subject) > maxSearchTermBytes || adapter.config.Bounds.MaxOutputBytes < 2 {
		return nil, errorCode(CodeBoundsExceeded)
	}

	var results []MessageMetadata
	err := adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		attemptResults := make([]MessageMetadata, 0)
		attemptOutputBytes := 2 // JSON array brackets.

		snapshot, err := session.Examine(operationContext, query.Mailbox)
		if err != nil {
			return err
		}

		for remaining, windows := snapshot.UIDNext, 0; remaining > 1 && windows < maxSearchWindows && len(attemptResults) < adapter.config.Bounds.MaxSearchResults; windows++ {
			start := uint32(1)
			if remaining > searchWindowWidth {
				start = remaining - searchWindowWidth
			}
			end := remaining - 1
			uids, err := session.UIDSearchWindow(operationContext, query, uidWindow{Start: start, End: end}, adapter.config.Bounds.MaxSearchResults-len(attemptResults))
			if err != nil {
				return err
			}
			if len(uids) > adapter.config.Bounds.MaxSearchResults-len(attemptResults) {
				return errorCode(CodeBoundsExceeded)
			}
			metadata, err := session.UIDFetchMetadata(operationContext, query.Mailbox, uids, adapter.config.Bounds.MaxHeaderBytes)
			if err != nil {
				return err
			}
			for _, item := range metadata {
				item.ID, err = adapter.encodeMessageID(messageIDPayload{Mailbox: query.Mailbox, UIDValidity: snapshot.UIDValidity, UID: item.uid})
				if err != nil {
					return err
				}
				item.Mailbox = query.Mailbox
				encodedItem, err := json.Marshal(item)
				if err != nil {
					return errorCode(CodeIMAPProtocol)
				}
				separatorBytes := 0
				if len(attemptResults) > 0 {
					separatorBytes = 1
				}
				if attemptOutputBytes > adapter.config.Bounds.MaxOutputBytes || len(encodedItem)+separatorBytes > adapter.config.Bounds.MaxOutputBytes-attemptOutputBytes {
					return errorCode(CodeBoundsExceeded)
				}
				attemptOutputBytes += len(encodedItem) + separatorBytes
				attemptResults = append(attemptResults, item)
			}
			remaining = start
		}

		results = attemptResults
		return nil
	})
	if err != nil {
		return nil, err
	}

	return results, nil
}

func (adapter *Adapter) encodeMessageID(payload messageIDPayload) (string, error) {
	if payload.Mailbox == "" || payload.UIDValidity == 0 || payload.UID == 0 {
		return "", errorCode(CodeIMAPProtocol)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", errorCode(CodeIMAPProtocol)
	}
	if len(encoded)+sha256.Size > maxMessageIDBytes {
		return "", errorCode(CodeBoundsExceeded)
	}
	mac := hmac.New(sha256.New, adapter.idKey[:])
	_, _ = mac.Write(encoded)
	value := base64.RawURLEncoding.EncodeToString(append(encoded, mac.Sum(nil)...))
	if len(value) > maxMessageIDEncodedBytes {
		return "", errorCode(CodeBoundsExceeded)
	}
	return value, nil
}

func (adapter *Adapter) decodeMessageID(value string) (messageIDPayload, error) {
	if len(value) > maxMessageIDEncodedBytes {
		return messageIDPayload{}, errorCode(CodeStaleMessageID)
	}

	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) <= sha256.Size || len(decoded) > maxMessageIDBytes {
		return messageIDPayload{}, errorCode(CodeStaleMessageID)
	}
	payloadBytes := decoded[:len(decoded)-sha256.Size]
	mac := hmac.New(sha256.New, adapter.idKey[:])
	_, _ = mac.Write(payloadBytes)
	if !hmac.Equal(decoded[len(payloadBytes):], mac.Sum(nil)) {
		return messageIDPayload{}, errorCode(CodeStaleMessageID)
	}
	var payload messageIDPayload
	if err := json.Unmarshal(payloadBytes, &payload); err != nil || payload.Mailbox == "" || payload.UIDValidity == 0 || payload.UID == 0 {
		return messageIDPayload{}, errorCode(CodeStaleMessageID)
	}
	return payload, nil
}

func newAdapterIDKey() ([sha256.Size]byte, error) {
	var key [sha256.Size]byte
	if _, err := rand.Read(key[:]); err != nil {
		return [sha256.Size]byte{}, err
	}
	return key, nil
}

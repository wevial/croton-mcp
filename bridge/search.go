package bridge

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"sort"
	"time"
)

const (
	maxMessageIDBytes        = 1024
	maxMessageIDEncodedBytes = 1366
	maxSenderBytes           = 512
)

// DateSource names where MessageMetadata.Date came from. The zero value is
// DateSourceUnknown, so metadata built without a parsed header Date never
// claims a source.
type DateSource uint8

const (
	// DateSourceUnknown means no usable header Date was found.
	DateSourceUnknown DateSource = iota
	// DateSourceHeader means Date was parsed from the message's own Date
	// header. It is sender-controlled and is not arrival or delivery time.
	DateSourceHeader
)

// MarshalText renders the source as a closed lowercase token.
func (source DateSource) MarshalText() ([]byte, error) {
	if source == DateSourceHeader {
		return []byte("header"), nil
	}
	return []byte("unknown"), nil
}

// ReadState is the three-state Seen status from a metadata FETCH. The zero
// value is ReadStateUnknown, so metadata without a FLAGS item is never
// reported as unread.
type ReadState uint8

const (
	// ReadStateUnknown means the server returned no FLAGS item.
	ReadStateUnknown ReadState = iota
	// ReadStateUnread means FLAGS was present without \Seen.
	ReadStateUnread
	// ReadStateRead means FLAGS was present with \Seen.
	ReadStateRead
)

// MarshalText renders the state as a closed lowercase token.
func (state ReadState) MarshalText() ([]byte, error) {
	switch state {
	case ReadStateUnread:
		return []byte("unread"), nil
	case ReadStateRead:
		return []byte("read"), nil
	default:
		return []byte("unknown"), nil
	}
}

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
// opaque identifier that binds the mailbox, UIDVALIDITY and UID. UIDValidity
// and UID are the same positive tuple the adapter validated and bound into ID
// during the successful read attempt; they confer no account or write
// authority.
//
// From, Date, DateSource and ReadState come from the same bounded metadata
// FETCH. From is untrusted decoded header text of at most 512 UTF-8 bytes and
// is empty when the header is missing or invalid; it asserts no account or
// address ownership. Date is RFC3339 UTC parsed only from the header Date and
// is empty unless DateSource is DateSourceHeader. MetadataTruncated reports
// that From was cut to its byte bound.
type MessageMetadata struct {
	ID                string
	Mailbox           string
	UIDValidity       uint32
	UID               uint32
	Subject           string
	Size              int64
	From              string
	Date              string
	DateSource        DateSource
	ReadState         ReadState
	MetadataTruncated bool

	uid uint32
}

// SearchPage returns bounded message metadata in descending UID order and
// reports whether matches were omitted or a UID range was left unexamined.
type SearchPage struct {
	Messages  []MessageMetadata
	Truncated bool
}

// SearchMail preserves the original adapter API for existing callers.
func (adapter *Adapter) SearchMail(ctx context.Context, query SearchQuery) ([]MessageMetadata, error) {
	page, err := adapter.SearchMailPage(ctx, query)
	if err != nil {
		return nil, err
	}
	return page.Messages, nil
}

// SearchMailPage searches a finite sequence of descending UID windows and
// returns the highest matching UIDs that fit the result budget, newest UID
// first. UID order is not header-Date or delivery-date order. Truncated reports
// matches omitted from an examined window or an older range left unexamined.
// It never issues an unbounded whole-mailbox SEARCH.
func (adapter *Adapter) SearchMailPage(ctx context.Context, query SearchQuery) (SearchPage, error) {
	if query.Mailbox == "" {
		return SearchPage{}, errorCode(CodeMailboxNotFound)
	}
	if len(query.Mailbox) > maxMailboxNameBytes || len(query.Sender) > maxSearchTermBytes || len(query.Subject) > maxSearchTermBytes || adapter.config.Bounds.MaxOutputBytes < 2 {
		return SearchPage{}, errorCode(CodeBoundsExceeded)
	}

	var page SearchPage
	err := adapter.execute(ctx, func(operationContext context.Context, session readSession) error {
		attemptResults := make([]MessageMetadata, 0)
		attemptOutputBytes := 2 // JSON array brackets.
		omitted := false

		snapshot, err := session.Examine(operationContext, query.Mailbox)
		if err != nil {
			return err
		}

		remaining := snapshot.UIDNext
		for windows := 0; remaining > 1 && windows < maxSearchWindows && len(attemptResults) < adapter.config.Bounds.MaxSearchResults; windows++ {
			start := uint32(1)
			if remaining > searchWindowWidth {
				start = remaining - searchWindowWidth
			}
			window := uidWindow{Start: start, End: remaining - 1}
			uids, err := session.UIDSearchWindow(operationContext, query, window, window.width())
			if err != nil {
				return err
			}

			selected, err := selectHighestUIDs(uids, window, adapter.config.Bounds.MaxSearchResults-len(attemptResults))
			if err != nil {
				return err
			}
			omitted = omitted || len(selected) < len(uids)

			metadata, err := session.UIDFetchMetadata(operationContext, query.Mailbox, selected, adapter.config.Bounds.MaxHeaderBytes)
			if err != nil {
				return err
			}
			if err := validateFetchedMetadataUIDs(selected, metadata); err != nil {
				return err
			}
			sort.Slice(metadata, func(i, j int) bool { return metadata[i].uid > metadata[j].uid })

			for _, item := range metadata {
				payload := messageIDPayload{Mailbox: query.Mailbox, UIDValidity: snapshot.UIDValidity, UID: item.uid}
				item.ID, err = adapter.encodeMessageID(payload)
				if err != nil {
					return err
				}
				item.Mailbox = payload.Mailbox
				item.UIDValidity = payload.UIDValidity
				item.UID = payload.UID
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

		page = SearchPage{Messages: attemptResults, Truncated: omitted || remaining > 1}
		return nil
	})
	if err != nil {
		return SearchPage{}, err
	}

	return page, nil
}

// selectHighestUIDs validates one decoded window and returns at most budget of
// its highest UIDs in descending order. Duplicate, zero or out-of-window UIDs
// refuse the whole window rather than yielding a partial page.
func selectHighestUIDs(uids []uint32, window uidWindow, budget int) ([]uint32, error) {
	if len(uids) > window.width() {
		return nil, errorCode(CodeBoundsExceeded)
	}

	sorted := append([]uint32(nil), uids...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] > sorted[j] })
	for index, uid := range sorted {
		if uid == 0 {
			return nil, errorCode(CodeIMAPProtocol)
		}
		if uid < window.Start || uid > window.End {
			return nil, errorCode(CodeBoundsExceeded)
		}
		if index > 0 && sorted[index-1] == uid {
			return nil, errorCode(CodeIMAPProtocol)
		}
	}

	if len(sorted) > budget {
		sorted = sorted[:budget]
	}
	return sorted, nil
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

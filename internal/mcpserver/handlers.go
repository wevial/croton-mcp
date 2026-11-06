package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/strictjson"
)

// folderResult is one entry of the list_folders result.
type folderResult struct {
	Name      string `json:"name"`
	Delimiter string `json:"delimiter,omitempty"`
}

type listFoldersResult struct {
	Folders   []folderResult `json:"folders"`
	Truncated bool           `json:"truncated,omitempty"`
}

const maxToolArgumentsBytes = 24 * 1024

// decodeArguments strictly decodes one bounded JSON object into target,
// rejecting unknown or duplicate fields and trailing values regardless of any
// client-side schema behavior.
func decodeArguments(arguments json.RawMessage, target any) bool {
	return strictjson.DecodeObject(arguments, maxToolArgumentsBytes, target)
}

func runListFolders(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input struct{}
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}

	folders, err := deps.Mail.ListFolders(ctx)
	if err != nil {
		return nil, mapAdapterError(err)
	}

	result := listFoldersResult{Folders: make([]folderResult, 0, len(folders))}
	for _, folder := range folders {
		result.Folders = append(result.Folders, folderResult{Name: folder.Name, Delimiter: folder.Delimiter})
	}

	return &result, ""
}

// messageResult is bounded message metadata shared by search and digest
// results. From, Date, DateSource and ReadState come from the same metadata
// observation as the identity; unknown values are explicit, never guessed.
type messageResult struct {
	ID                string            `json:"id"`
	Mailbox           string            `json:"mailbox"`
	UIDValidity       uint32            `json:"uidvalidity,omitempty"`
	UID               uint32            `json:"uid,omitempty"`
	Subject           string            `json:"subject,omitempty"`
	Size              int64             `json:"size,omitempty"`
	From              string            `json:"from,omitempty"`
	Date              string            `json:"date,omitempty"`
	DateSource        bridge.DateSource `json:"dateSource"`
	ReadState         bridge.ReadState  `json:"readState"`
	MetadataTruncated bool              `json:"metadataTruncated,omitempty"`
}

// maxSenderResultBytes mirrors the bridge's per-entry sender bound.
const maxSenderResultBytes = 512

// newMessageResult maps one adapter observation. A date is published only
// when it is header-sourced RFC3339 UTC, and only an observed FLAGS item
// yields read or unread.
func newMessageResult(item bridge.MessageMetadata) messageResult {
	sender, capped := boundedSenderResult(item.From)
	result := messageResult{
		ID:                item.ID,
		Mailbox:           item.Mailbox,
		UIDValidity:       item.UIDValidity,
		UID:               item.UID,
		Subject:           item.Subject,
		Size:              item.Size,
		From:              sender,
		ReadState:         item.ReadState,
		MetadataTruncated: sender != "" && (item.MetadataTruncated || capped),
	}

	if item.DateSource == bridge.DateSourceHeader && validHeaderDate(item.Date) {
		result.Date = item.Date
		result.DateSource = bridge.DateSourceHeader
	}

	return result
}

// boundedSenderResult omits invalid UTF-8 and cuts a longer sender to its
// byte bound on a rune boundary, reporting the cut.
func boundedSenderResult(sender string) (string, bool) {
	if !utf8.ValidString(sender) {
		return "", false
	}
	if len(sender) <= maxSenderResultBytes {
		return sender, false
	}

	boundary := maxSenderResultBytes
	for boundary > 0 && !utf8.RuneStart(sender[boundary]) {
		boundary--
	}

	return sender[:boundary], true
}

// validHeaderDate accepts only the canonical RFC3339 UTC form the bridge
// produces.
func validHeaderDate(date string) bool {
	parsed, err := time.Parse(time.RFC3339, date)
	if err != nil {
		return false
	}

	return parsed.Location() == time.UTC && parsed.Format(time.RFC3339) == date
}

// validReadIdentity reports whether the adapter supplied the positive
// same-generation tuple that flat read results expose beside the opaque id.
func validReadIdentity(metadata bridge.MessageMetadata) bool {
	return metadata.ID != "" && metadata.Mailbox != "" && metadata.UIDValidity != 0 && metadata.UID != 0
}

// flatMessageResults maps one search page into flat results, refusing the
// whole page when any message lacks its read identity.
func flatMessageResults(messages []bridge.MessageMetadata, limit int) ([]messageResult, bool, string) {
	results := make([]messageResult, 0, len(messages))
	for _, item := range messages {
		if len(results) == limit {
			return results, true, ""
		}
		if !validReadIdentity(item) {
			return nil, false, errUnavailable
		}

		results = append(results, newMessageResult(item))
	}

	return results, false, ""
}

type searchMailResult struct {
	Results   []messageResult `json:"results"`
	Truncated bool            `json:"truncated,omitempty"`
}

func runSearchMail(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input struct {
		Mailbox    string `json:"mailbox"`
		Since      string `json:"since"`
		Before     string `json:"before"`
		Sender     string `json:"sender"`
		Subject    string `json:"subject"`
		UnreadOnly bool   `json:"unreadOnly"`
		Limit      *int   `json:"limit"`
	}
	if !decodeArguments(arguments, &input) {
		return nil, errInvalidArgument
	}
	if !validMailboxArgument(input.Mailbox) {
		return nil, errInvalidArgument
	}
	if len(input.Sender) > maxSearchTermBytes || len(input.Subject) > maxSearchTermBytes {
		return nil, errInvalidArgument
	}
	since, ok := parseTimestamp(input.Since)
	if !ok {
		return nil, errInvalidArgument
	}
	before, ok := parseTimestamp(input.Before)
	if !ok {
		return nil, errInvalidArgument
	}
	limit, ok := clampLimit(input.Limit, defaultSearchLimit, maxSearchLimit)
	if !ok {
		return nil, errInvalidArgument
	}

	page, err := deps.Mail.SearchMailPage(ctx, bridge.SearchQuery{
		Mailbox: input.Mailbox,
		Since:   since,
		Before:  before,
		Sender:  input.Sender,
		Subject: input.Subject,
		Unread:  input.UnreadOnly,
	})
	if err != nil {
		return nil, mapAdapterError(err)
	}

	results, limited, errCode := flatMessageResults(page.Messages, limit)
	if errCode != "" {
		return nil, errCode
	}

	return &searchMailResult{Results: results, Truncated: page.Truncated || limited}, ""
}

func validMailboxArgument(mailbox string) bool {
	return mailbox != "" && len(mailbox) <= maxMailboxArgumentBytes
}

// parseTimestamp accepts an optional bounded RFC 3339 timestamp.
func parseTimestamp(value string) (time.Time, bool) {
	if value == "" {
		return time.Time{}, true
	}
	if len(value) > maxTimestampArgumentBytes {
		return time.Time{}, false
	}

	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		return time.Time{}, false
	}

	return parsed, true
}

// clampLimit applies the server-authoritative range regardless of schema
// enforcement: absent selects the default, non-positive is rejected, and
// oversized values clamp to the ceiling.
func clampLimit(value *int, fallback, ceiling int) (int, bool) {
	if value == nil {
		return fallback, true
	}
	if *value <= 0 {
		return 0, false
	}
	if *value > ceiling {
		return ceiling, true
	}

	return *value, true
}

// attachmentResult mirrors bridge.AttachmentMetadata without attachment bytes.
// DeclaredSize is the sender's Content-Disposition size parameter, present
// exactly when HasDeclaredSize is true, so a declared zero stays distinct
// from an unknown size.
type attachmentResult struct {
	Filename        string `json:"filename,omitempty"`
	ContentType     string `json:"contentType,omitempty"`
	Disposition     string `json:"disposition,omitempty"`
	HasDeclaredSize bool   `json:"hasDeclaredSize"`
	DeclaredSize    *int64 `json:"declaredSize,omitempty"`
}

// messageTruncation names every limit that shaped a get_message result. Only
// true reasons are serialized, so a complete result carries an empty object.
type messageTruncation struct {
	Headers     bool `json:"headers,omitempty"`
	HeaderCount bool `json:"headerCount,omitempty"`
	Text        bool `json:"text,omitempty"`
	MIMEParts   bool `json:"mimeParts,omitempty"`
	MIMEDepth   bool `json:"mimeDepth,omitempty"`
	Attachments bool `json:"attachments,omitempty"`
	References  bool `json:"references,omitempty"`
	Output      bool `json:"output,omitempty"`
}

// attachmentListTruncation names the limits that can omit or shorten
// list_attachments entries; text and reference limits do not apply.
type attachmentListTruncation struct {
	Headers     bool `json:"headers,omitempty"`
	HeaderCount bool `json:"headerCount,omitempty"`
	MIMEParts   bool `json:"mimeParts,omitempty"`
	MIMEDepth   bool `json:"mimeDepth,omitempty"`
	Attachments bool `json:"attachments,omitempty"`
	Output      bool `json:"output,omitempty"`
}

func (truncation attachmentListTruncation) any() bool {
	return truncation.Headers || truncation.HeaderCount || truncation.MIMEParts || truncation.MIMEDepth || truncation.Attachments || truncation.Output
}

type getMessageResult struct {
	ID          string                  `json:"id"`
	Mailbox     string                  `json:"mailbox"`
	UIDValidity uint32                  `json:"uidvalidity,omitempty"`
	UID         uint32                  `json:"uid,omitempty"`
	Size        int64                   `json:"size,omitempty"`
	Headers     bridge.CanonicalHeaders `json:"headers"`
	Text        string                  `json:"text,omitempty"`
	TextSource  string                  `json:"textSource"`
	Attachments []attachmentResult      `json:"attachments,omitempty"`
	Truncation  messageTruncation       `json:"truncation"`
	Truncated   bool                    `json:"truncated,omitempty"`
}

type messageIdentifierInput struct {
	MessageID string `json:"messageId"`
}

func validMessageIdentifier(identifier string) bool {
	return identifier != "" && len(identifier) <= maxMessageIDArgumentBytes
}

// fetchNormalizedMessage composes the two read-only adapter operations shared
// by get_message and list_attachments.
func fetchNormalizedMessage(ctx context.Context, deps Options, identifier string) (bridge.MessageMetadata, bridge.NormalizedMessage, string) {
	metadata, err := deps.Mail.GetMessageMetadata(ctx, identifier)
	if err != nil {
		return bridge.MessageMetadata{}, bridge.NormalizedMessage{}, mapAdapterError(err)
	}

	body, err := deps.Mail.GetMessageBody(ctx, identifier)
	if err != nil {
		return bridge.MessageMetadata{}, bridge.NormalizedMessage{}, mapAdapterError(err)
	}

	normalized, err := bridge.NormalizeMessage(bytes.NewReader(body), bridge.NormalizeOptions{})
	if err != nil {
		return bridge.MessageMetadata{}, bridge.NormalizedMessage{}, mapAdapterError(err)
	}

	return metadata, normalized, ""
}

func attachmentResults(attachments []bridge.AttachmentMetadata) []attachmentResult {
	results := make([]attachmentResult, 0, len(attachments))
	for _, attachment := range attachments {
		result := attachmentResult{
			Filename:    attachment.Filename,
			ContentType: attachment.ContentType,
			Disposition: attachment.Disposition,
		}
		if attachment.HasDeclaredSize && attachment.DeclaredSize >= 0 {
			result.HasDeclaredSize = true
			result.DeclaredSize = &attachment.DeclaredSize
		}

		results = append(results, result)
	}

	return results
}

func runGetMessage(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input messageIdentifierInput
	if !decodeArguments(arguments, &input) || !validMessageIdentifier(input.MessageID) {
		return nil, errInvalidArgument
	}

	metadata, normalized, errCode := fetchNormalizedMessage(ctx, deps, input.MessageID)
	if errCode != "" {
		return nil, errCode
	}
	if !validReadIdentity(metadata) {
		return nil, errUnavailable
	}

	return &getMessageResult{
		ID:          metadata.ID,
		Mailbox:     metadata.Mailbox,
		UIDValidity: metadata.UIDValidity,
		UID:         metadata.UID,
		Size:        metadata.Size,
		Headers:     normalized.Headers,
		Text:        normalized.Text,
		TextSource:  string(normalized.TextSource),
		Attachments: attachmentResults(normalized.Attachments),
		Truncation: messageTruncation{
			Headers:     normalized.Truncation.Headers,
			HeaderCount: normalized.Truncation.HeaderCount,
			Text:        normalized.Truncation.Text,
			MIMEParts:   normalized.Truncation.MIMEParts,
			MIMEDepth:   normalized.Truncation.MIMEDepth,
			Attachments: normalized.Truncation.Attachments,
			References:  normalized.Truncation.References,
		},
		Truncated: normalized.Truncation.Any(),
	}, ""
}

// threadNodeResult exposes header-level metadata and the member's own read
// identity for one thread member; it never carries message body text.
type threadNodeResult struct {
	Key         string `json:"key"`
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidvalidity,omitempty"`
	UID         uint32 `json:"uid,omitempty"`
	MessageID   string `json:"messageId,omitempty"`
	ParentKey   string `json:"parentKey,omitempty"`
	Depth       int    `json:"depth"`
	Subject     string `json:"subject,omitempty"`
	From        string `json:"from,omitempty"`
	Date        string `json:"date,omitempty"`
}

type getThreadResult struct {
	ID        string             `json:"id"`
	Mailbox   string             `json:"mailbox"`
	Nodes     []threadNodeResult `json:"nodes"`
	Truncated bool               `json:"truncated,omitempty"`
}

func runGetThread(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input struct {
		MessageID   string `json:"messageId"`
		MaxMessages *int   `json:"maxMessages"`
	}
	if !decodeArguments(arguments, &input) || !validMessageIdentifier(input.MessageID) {
		return nil, errInvalidArgument
	}
	limit, ok := clampLimit(input.MaxMessages, defaultThreadMessages, maxThreadMessages)
	if !ok {
		return nil, errInvalidArgument
	}

	reader, ok := deps.Mail.(MessageHeaderReader)
	if !ok {
		return nil, errUnsupported
	}

	target, err := reader.GetMessageHeaders(ctx, input.MessageID)
	if err != nil {
		return nil, mapAdapterError(err)
	}
	targetMetadata := target.Metadata
	if !validReadIdentity(targetMetadata) {
		return nil, errUnavailable
	}

	identities := map[string]bridge.MessageMetadata{input.MessageID: targetMetadata}
	members := []bridge.ThreadMessage{{Key: input.MessageID, Headers: target.Headers}}
	members, siblingsTruncated, errCode := appendThreadSiblings(ctx, reader, deps, members, identities, targetMetadata, limit)
	if errCode != "" {
		return nil, errCode
	}

	thread := bridge.BuildThread(members, bridge.ThreadOptions{Limits: bridge.NormalizeLimits{MaxThreadMessages: limit}})

	result := getThreadResult{
		ID:        input.MessageID,
		Mailbox:   targetMetadata.Mailbox,
		Nodes:     make([]threadNodeResult, 0, len(thread.Nodes)),
		Truncated: siblingsTruncated || thread.Truncation.Any(),
	}
	for _, node := range thread.Nodes {
		identity, ok := identities[node.Key]
		if !ok {
			return nil, errInternal
		}

		result.Nodes = append(result.Nodes, threadNodeResult{
			Key:         node.Key,
			Mailbox:     identity.Mailbox,
			UIDValidity: identity.UIDValidity,
			UID:         identity.UID,
			MessageID:   node.MessageID,
			ParentKey:   node.ParentKey,
			Depth:       node.Depth,
			Subject:     node.Message.Headers.Subject,
			From:        node.Message.Headers.From,
			Date:        node.Message.Headers.Date,
		})
	}

	return &result, ""
}

// appendThreadSiblings finds same-subject mailbox siblings with one bounded
// search and at most limit-1 additional selected header-only reads. It
// records each appended sibling's metadata from the same header read in
// identities by opaque key, and omits a sibling whose read identity is
// incomplete.
func appendThreadSiblings(ctx context.Context, reader MessageHeaderReader, deps Options, members []bridge.ThreadMessage, identities map[string]bridge.MessageMetadata, target bridge.MessageMetadata, limit int) ([]bridge.ThreadMessage, bool, string) {
	subject := baseSubject(target.Subject)
	if subject == "" || target.Mailbox == "" {
		return members, false, ""
	}

	page, err := deps.Mail.SearchMailPage(ctx, bridge.SearchQuery{Mailbox: target.Mailbox, Subject: subject})
	if err != nil {
		return nil, false, mapAdapterError(err)
	}

	truncated := page.Truncated
	attempts := 0
	maximumAttempts := limit - len(members)
	seen := make(map[string]struct{}, len(members)+1)
	seen[target.ID] = struct{}{}
	for _, member := range members {
		seen[member.Key] = struct{}{}
	}

	for _, candidate := range page.Messages {
		if candidate.ID == "" {
			continue
		}
		if _, duplicate := seen[candidate.ID]; duplicate {
			continue
		}
		seen[candidate.ID] = struct{}{}
		if !strings.EqualFold(baseSubject(candidate.Subject), subject) {
			continue
		}
		if attempts == maximumAttempts {
			truncated = true
			break
		}
		attempts++

		sibling, err := reader.GetMessageHeaders(ctx, candidate.ID)
		if err != nil {
			errCode := mapAdapterError(err)
			if errCode == errStaleID {
				continue
			}

			return nil, false, errCode
		}
		if !strings.EqualFold(baseSubject(sibling.Headers.Subject), subject) {
			continue
		}
		if !validReadIdentity(sibling.Metadata) {
			truncated = true
			continue
		}

		identities[candidate.ID] = sibling.Metadata
		members = append(members, bridge.ThreadMessage{Key: candidate.ID, Headers: sibling.Headers})
	}

	return members, truncated, ""
}

// baseSubject strips repeated reply and forward prefixes so thread siblings
// share one bounded search term.
func baseSubject(subject string) string {
	trimmed := strings.TrimSpace(subject)
	for {
		lowered := strings.ToLower(trimmed)
		stripped := trimmed
		for _, prefix := range []string{"re:", "fwd:", "fw:"} {
			if strings.HasPrefix(lowered, prefix) {
				stripped = strings.TrimSpace(trimmed[len(prefix):])
				break
			}
		}
		if stripped == trimmed {
			break
		}

		trimmed = stripped
	}

	if len(trimmed) > maxSearchTermBytes {
		boundary := maxSearchTermBytes
		for boundary > 0 && !utf8.RuneStart(trimmed[boundary]) {
			boundary--
		}
		trimmed = trimmed[:boundary]
	}

	return trimmed
}

type listAttachmentsResult struct {
	ID          string                   `json:"id"`
	Mailbox     string                   `json:"mailbox"`
	UIDValidity uint32                   `json:"uidvalidity,omitempty"`
	UID         uint32                   `json:"uid,omitempty"`
	Attachments []attachmentResult       `json:"attachments"`
	Truncation  attachmentListTruncation `json:"truncation"`
	Truncated   bool                     `json:"truncated,omitempty"`
}

func runListAttachments(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input messageIdentifierInput
	if !decodeArguments(arguments, &input) || !validMessageIdentifier(input.MessageID) {
		return nil, errInvalidArgument
	}

	metadata, normalized, errCode := fetchNormalizedMessage(ctx, deps, input.MessageID)
	if errCode != "" {
		return nil, errCode
	}
	if !validReadIdentity(metadata) {
		return nil, errUnavailable
	}

	truncation := attachmentListTruncation{
		Headers:     normalized.Truncation.Headers,
		HeaderCount: normalized.Truncation.HeaderCount,
		MIMEParts:   normalized.Truncation.MIMEParts,
		MIMEDepth:   normalized.Truncation.MIMEDepth,
		Attachments: normalized.Truncation.Attachments,
	}

	return &listAttachmentsResult{
		ID:          metadata.ID,
		Mailbox:     metadata.Mailbox,
		UIDValidity: metadata.UIDValidity,
		UID:         metadata.UID,
		Attachments: attachmentResults(normalized.Attachments),
		Truncation:  truncation,
		Truncated:   truncation.any(),
	}, ""
}

type selectDigestResult struct {
	Mailbox       string          `json:"mailbox"`
	Candidates    []messageResult `json:"candidates"`
	TotalMessages int             `json:"totalMessages"`
	UnseenCount   int             `json:"unseenCount"`
	Truncated     bool            `json:"truncated,omitempty"`
}

// runSelectDigestCandidates composes two existing read-only adapter
// operations — STATUS counters and one bounded structured search — and stays
// metadata-first: it never fetches message bodies.
func runSelectDigestCandidates(ctx context.Context, deps Options, arguments json.RawMessage) (any, string) {
	var input struct {
		Mailbox    string `json:"mailbox"`
		SinceHours *int   `json:"sinceHours"`
		Limit      *int   `json:"limit"`
		UnreadOnly *bool  `json:"unreadOnly"`
	}
	if !decodeArguments(arguments, &input) || !validMailboxArgument(input.Mailbox) {
		return nil, errInvalidArgument
	}
	sinceHours, ok := clampLimit(input.SinceHours, defaultDigestHours, maxDigestSinceHours)
	if !ok {
		return nil, errInvalidArgument
	}
	limit, ok := clampLimit(input.Limit, defaultDigestLimit, maxDigestLimit)
	if !ok {
		return nil, errInvalidArgument
	}
	unreadOnly := true
	if input.UnreadOnly != nil {
		unreadOnly = *input.UnreadOnly
	}

	now := time.Now().UTC()
	upperDate := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 0, 0, time.UTC)

	status, err := deps.Mail.Status(ctx, input.Mailbox)
	if err != nil {
		return nil, mapAdapterError(err)
	}

	page, err := deps.Mail.SearchMailPage(ctx, bridge.SearchQuery{
		Mailbox: input.Mailbox,
		Since:   now.Add(-time.Duration(sinceHours) * time.Hour),
		Before:  upperDate,
		Unread:  unreadOnly,
	})
	if err != nil {
		return nil, mapAdapterError(err)
	}

	candidates, limited, errCode := flatMessageResults(page.Messages, limit)
	if errCode != "" {
		return nil, errCode
	}

	return &selectDigestResult{
		Mailbox:       input.Mailbox,
		Candidates:    candidates,
		TotalMessages: status.Messages,
		UnseenCount:   status.Unseen,
		Truncated:     page.Truncated || limited,
	}, ""
}

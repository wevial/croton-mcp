package bridge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
	"golang.org/x/net/html/charset"
)

const (
	syntheticStartTLSGreeting         = "* OK Croton local TLS handoff\r\n"
	maxSearchWindows                  = 8
	searchWindowWidth          uint32 = 100
	maxSearchResponseBytes            = 4 << 10
	maxControlResponseBytes           = 64 << 10
	maxListResponseBytes              = maxControlResponseBytes
	fetchResponseOverheadBytes        = 4 << 10
	maxStructureResponseBytes         = 128 << 10
)

var errIMAPInputLimit = errors.New("IMAP response exceeds input budget")

// bridgeSession intentionally exposes only Croton-owned operations. The
// concrete go-imap client never escapes this file. SelectSource, StoreSeen
// and MoveUID are the only mutating surfaces: a read-write selection of the
// source mailbox, a single-UID Seen-only incremental STORE and a single-UID
// native MOVE, reached only from the opt-in triage path.
type bridgeSession interface {
	Authenticate(context.Context, Credentials) error
	List(context.Context, int) ([]Folder, error)
	ListMailboxes(context.Context, string, int) ([]listedMailbox, error)
	Status(context.Context, string) (MailboxStatus, error)
	Examine(context.Context, string) (mailboxSnapshot, error)
	SelectSource(context.Context, string) (mailboxSnapshot, error)
	UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error)
	UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error)
	UIDFetchHeaders(context.Context, uint32, int) (MessageHeaders, error)
	UIDFetchBody(context.Context, uint32, []int, int) ([]byte, error)
	fetchUIDStructure(context.Context, uint32) (bodyStructure, error)
	StoreSeen(context.Context, uint32, bool) error
	SupportsMove() bool
	MoveUID(context.Context, uint32, string) error
	Logout(context.Context) error
	Abort() error
}

// listedMailbox is one ordinary LIST entry with its protocol attributes.
type listedMailbox struct {
	Name       string
	Delimiter  rune
	Attributes []string
}

type mailboxSnapshot struct {
	UIDNext     uint32
	UIDValidity uint32
}

type uidWindow struct {
	Start uint32
	End   uint32
}

// width is the number of UIDs a well-formed window spans, which also bounds
// how many unique UIDs one SEARCH response for it may decode to.
func (window uidWindow) width() int {
	if window.Start == 0 || window.End < window.Start {
		return 0
	}
	return int(uint64(window.End) - uint64(window.Start) + 1)
}

type imapSession struct {
	client *imapclient.Client
	conn   *deadlineConn
	budget *readBudgetConn
	guard  *dispatchGuardConn
}

func newIMAPSession(ctx context.Context, connection *Connection) (*imapSession, error) {
	transport, startTLS, err := connection.takeConn()
	if err != nil {
		return nil, err
	}

	return newIMAPSessionOver(ctx, transport, startTLS)
}

// newIMAPSessionOver builds the client stack over an established transport.
// Every client write passes through the dispatch guard.
func newIMAPSessionOver(ctx context.Context, transport net.Conn, startTLS bool) (*imapSession, error) {
	wrapped := &deadlineConn{Conn: transport}
	guard := &dispatchGuardConn{Conn: wrapped}
	budget := newReadBudgetConn(guard, maxControlResponseBytes)
	session := &imapSession{conn: wrapped, budget: budget, guard: guard}

	if err := session.startClient(ctx, budget, startTLS); err != nil {
		return nil, err
	}

	return session, nil
}

// startClient constructs the client over reader, the top of a read stack
// whose bottom is session.conn, and waits for its greeting and capabilities.
// After STARTTLS the locally generated greeting is served above reader, so it
// never carries server bytes past any layer of the stack.
func (session *imapSession) startClient(ctx context.Context, reader net.Conn, startTLS bool) error {
	clientTransport := reader
	if startTLS {
		clientTransport = &syntheticGreetingConn{Conn: reader, greeting: []byte(syntheticStartTLSGreeting)}
	}

	client := imapclient.New(clientTransport, nil)
	session.client = client
	if err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		if err := client.WaitGreeting(); err != nil {
			return err
		}
		return waitForCapabilities(client)
	}); err != nil {
		_ = client.Close()
		return err
	}

	return nil
}

func (session *imapSession) Authenticate(ctx context.Context, credentials Credentials) error {
	return session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		username := string(credentials.Username)
		password := string(credentials.Password)
		defer credentials.Zero()

		if err := session.client.Login(username, password).Wait(); err != nil {
			var imapError *imap.Error
			if errors.As(err, &imapError) && imapError.Type == imap.StatusResponseTypeNo {
				return authenticationFailure{err: err}
			}
			return err
		}
		// go-imap signals LOGIN completion immediately before it invalidates the
		// pre-login capabilities and launches its automatic refresh. NOOP is a
		// decoder-ordering barrier: once it completes, that invalidation has run.
		if err := session.client.Noop().Wait(); err != nil {
			var imapError *imap.Error
			if errors.As(err, &imapError) {
				return err
			}
			return io.EOF
		}
		return waitForCapabilities(session.client)
	})
}

func waitForCapabilities(client *imapclient.Client) error {
	if client.Caps() == nil {
		return io.EOF
	}
	return nil
}

func (session *imapSession) List(ctx context.Context, limit int) ([]Folder, error) {
	mailboxes, err := session.ListMailboxes(ctx, "*", limit)
	if err != nil {
		return nil, err
	}

	folders := make([]Folder, 0, len(mailboxes))
	for _, mailbox := range mailboxes {
		folders = append(folders, Folder{Name: mailbox.Name, Delimiter: string(mailbox.Delimiter)})
	}

	return folders, nil
}

// ListMailboxes sends one ordinary LIST "" <pattern> without selection or
// RETURN options and keeps each entry's attributes.
func (session *imapSession) ListMailboxes(ctx context.Context, pattern string, limit int) ([]listedMailbox, error) {
	var mailboxes []listedMailbox
	err := session.withContext(ctx, func() error {
		exceeded, listErr := session.withInputBudget(maxListResponseBytes, func() error {
			command := session.client.List("", pattern, nil)
			for {
				data := command.Next()
				if data == nil {
					break
				}
				if len(mailboxes) == limit {
					_ = session.conn.Close()
					return errorCode(CodeBoundsExceeded)
				}

				attributes := make([]string, 0, len(data.Attrs))
				for _, attribute := range data.Attrs {
					attributes = append(attributes, string(attribute))
				}
				mailboxes = append(mailboxes, listedMailbox{Name: data.Mailbox, Delimiter: data.Delim, Attributes: attributes})
			}
			return command.Close()
		})
		if exceeded {
			return errorCode(CodeBoundsExceeded)
		}
		return listErr
	})
	if err != nil {
		return nil, err
	}

	return mailboxes, nil
}

func (session *imapSession) Status(ctx context.Context, mailbox string) (MailboxStatus, error) {
	wireMailbox := canonicalMailbox(mailbox)
	var result MailboxStatus
	err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		data, err := session.client.Status(wireMailbox, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true}).Wait()
		if err != nil {
			return err
		}
		if !mailboxIdentityMatches(wireMailbox, data.Mailbox) || data.NumMessages == nil || data.NumUnseen == nil || data.UIDNext == 0 || data.UIDValidity == 0 {
			return errorCode(CodeIMAPProtocol)
		}
		maxInt := uint64(^uint(0) >> 1)
		if uint64(*data.NumMessages) > maxInt || uint64(*data.NumUnseen) > maxInt {
			return errorCode(CodeIMAPProtocol)
		}
		result.Messages = int(*data.NumMessages)
		result.Unseen = int(*data.NumUnseen)
		result.UIDNext = uint32(data.UIDNext)
		result.UIDValidity = data.UIDValidity
		return nil
	})
	return result, err
}

func canonicalMailbox(mailbox string) string {
	if strings.EqualFold(mailbox, "INBOX") {
		return "INBOX"
	}
	return mailbox
}

func mailboxIdentityMatches(requested, actual string) bool {
	if strings.EqualFold(requested, "INBOX") {
		return strings.EqualFold(actual, "INBOX")
	}
	return requested == actual
}

func (session *imapSession) Examine(ctx context.Context, mailbox string) (mailboxSnapshot, error) {
	var snapshot mailboxSnapshot
	err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		data, err := session.client.Select(mailbox, &imap.SelectOptions{ReadOnly: true}).Wait()
		if err != nil {
			return err
		}
		snapshot.UIDNext = uint32(data.UIDNext)
		snapshot.UIDValidity = data.UIDValidity
		if snapshot.UIDNext == 0 || snapshot.UIDValidity == 0 {
			return errorCode(CodeIMAPProtocol)
		}
		return nil
	})
	return snapshot, err
}

// SelectSource opens mailbox read-write so a later StoreSeen can apply. It
// returns the fresh generation the caller must compare before any write.
func (session *imapSession) SelectSource(ctx context.Context, mailbox string) (mailboxSnapshot, error) {
	var snapshot mailboxSnapshot
	err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		data, err := session.client.Select(mailbox, &imap.SelectOptions{ReadOnly: false}).Wait()
		if err != nil {
			return err
		}
		snapshot.UIDNext = uint32(data.UIDNext)
		snapshot.UIDValidity = data.UIDValidity
		if snapshot.UIDNext == 0 || snapshot.UIDValidity == 0 {
			return errorCode(CodeIMAPProtocol)
		}
		return nil
	})
	return snapshot, err
}

// StoreSeen sends exactly one UID STORE of +FLAGS.SILENT or -FLAGS.SILENT
// with the single flag \Seen. No other flag or STORE form is reachable.
func (session *imapSession) StoreSeen(ctx context.Context, uid uint32, seen bool) error {
	if uid == 0 {
		return errorCode(CodeIMAPProtocol)
	}

	operation := imap.StoreFlagsDel
	if seen {
		operation = imap.StoreFlagsAdd
	}
	store := &imap.StoreFlags{Op: operation, Silent: true, Flags: []imap.Flag{imap.FlagSeen}}

	return session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		return session.client.Store(imap.UIDSetNum(imap.UID(uid)), store, nil).Close()
	})
}

// SupportsMove reports whether the authenticated server advertises native
// MOVE, directly or through IMAP4rev2.
func (session *imapSession) SupportsMove() bool {
	return session.client.Caps().Has(imap.CapMove)
}

// MoveUID sends exactly one native UID MOVE of a single UID. go-imap
// substitutes COPY, STORE \Deleted and EXPUNGE whenever its cached
// capabilities lack MOVE, and a server response can replace that cache at any
// moment, so a capability check alone cannot keep the dispatch native. The
// check refuses early; the dispatch guard is what enforces it.
func (session *imapSession) MoveUID(ctx context.Context, uid uint32, destination string) error {
	if uid == 0 || destination == "" {
		return errorCode(CodeIMAPProtocol)
	}
	if !session.SupportsMove() {
		return errorCode(CodeUnsupported)
	}

	return session.dispatchMove(ctx, uid, destination)
}

// dispatchMove arms the guard so the only bytes that can leave the session are
// one complete "<tag> UID MOVE <uid> ..." line. A fallback write is refused
// before reaching the transport and closes the connection, so the outcome is
// a definite refusal with nothing sent.
func (session *imapSession) dispatchMove(ctx context.Context, uid uint32, destination string) error {
	session.guard.arm(uid)

	err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		_, err := session.client.Move(imap.UIDSetNum(imap.UID(uid)), destination).Wait()
		return err
	})

	if session.guard.disarm() {
		return errorCode(CodeUnsupported)
	}

	return err
}

// UIDSearchWindow decodes at most limit unique UIDs, which must not exceed the
// window width. Callers pass the width so a dense window is decoded whole and
// result selection happens after the response has been fully consumed.
func (session *imapSession) UIDSearchWindow(ctx context.Context, query SearchQuery, window uidWindow, limit int) ([]uint32, error) {
	if limit < 1 || limit > window.width() {
		return nil, errorCode(CodeBoundsExceeded)
	}

	set := imap.UIDSet{}
	set.AddRange(imap.UID(window.Start), imap.UID(window.End))
	criteria := &imap.SearchCriteria{UID: []imap.UIDSet{set}, Since: query.Since, Before: query.Before}
	if query.Sender != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "FROM", Value: query.Sender})
	}
	if query.Subject != "" {
		criteria.Header = append(criteria.Header, imap.SearchCriteriaHeaderField{Key: "SUBJECT", Value: query.Subject})
	}
	if query.Unread {
		criteria.NotFlag = append(criteria.NotFlag, imap.FlagSeen)
	}

	var data *imap.SearchData
	err := session.withBoundedInput(ctx, maxSearchResponseBytes, func() error {
		var searchErr error
		data, searchErr = session.client.UIDSearch(criteria, nil).Wait()
		return searchErr
	})
	if err != nil {
		return nil, err
	}

	return boundedSearchUIDs(data, window, limit)
}

func (session *imapSession) UIDFetchMetadata(ctx context.Context, _ string, uids []uint32, headerLimit int) ([]MessageMetadata, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if headerLimit < 1 {
		return nil, errorCode(CodeBoundsExceeded)
	}

	set := imap.UIDSet{}
	for _, uid := range uids {
		if uid == 0 {
			return nil, errorCode(CodeIMAPProtocol)
		}
		set.AddNum(imap.UID(uid))
	}
	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	options := &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}
	var results []MessageMetadata
	fetch := func() (err error) {
		results = nil
		if session.budget != nil {
			session.budget.begin(fetchInputBudget(headerLimit, len(uids)))
			defer func() {
				if session.budget.end() {
					err = errorCode(CodeBoundsExceeded)
				}
			}()
		}

		command := session.client.Fetch(set, options)
		defer func() {
			if err != nil && err != errFlagsOnly {
				session.abortFetch(command)
			}
		}()

		for message := command.Next(); message != nil; message = command.Next() {
			var item MessageMetadata
			bodySections := 0
			uidItems := 0
			flagsItems := 0
			sizeItems := 0
			for data := message.Next(); data != nil; data = message.Next() {
				switch value := data.(type) {
				case imapclient.FetchItemDataUID:
					if uidItems != 0 || value.UID == 0 {
						return errorCode(CodeIMAPProtocol)
					}
					uidItems++
					item.uid = uint32(value.UID)
				case imapclient.FetchItemDataFlags:
					if flagsItems != 0 {
						return errorCode(CodeIMAPProtocol)
					}
					flagsItems++
					item.ReadState = metadataReadState(value.Flags)
				case imapclient.FetchItemDataRFC822Size:
					sizeItems++
					item.Size = value.Size
				case imapclient.FetchItemDataBodySection:
					if value.Literal == nil || value.Section == nil || !value.MatchCommand(section) || bodySections != 0 {
						return errorCode(CodeIMAPProtocol)
					}
					bodySections++
					header, err := readBoundedLiteral(value.Literal, headerLimit)
					if err != nil {
						return err
					}
					parsed, err := mail.ReadMessage(bytesReader(header))
					if err != nil {
						return errorCode(CodeIMAPProtocol)
					}
					applyHeaderMetadata(&item, parsed.Header)
				}
			}
			if uidItems == 1 && sizeItems == 0 && bodySections == 0 {
				return closeFlagsOnly(command)
			}
			if uidItems != 1 || item.uid == 0 || bodySections != 1 {
				return errorCode(CodeIMAPProtocol)
			}
			results = append(results, item)
		}
		if err := command.Close(); err != nil {
			return err
		}
		return validateFetchedMetadataUIDs(uids, results)
	}
	err := retryFlagsOnly(func() error { return session.withContext(ctx, fetch) })
	return results, err
}

func applyHeaderMetadata(item *MessageMetadata, header mail.Header) {
	item.Subject = header.Get("Subject")
	item.From, item.MetadataTruncated = boundedSender(header)
	item.Date, item.DateSource = headerDate(header)
}

// metadataReadState sees only a present FLAGS item; a missing one stays
// ReadStateUnknown.
func metadataReadState(flags []imap.Flag) ReadState {
	for _, flag := range flags {
		if strings.EqualFold(string(flag), string(imap.FlagSeen)) {
			return ReadStateRead
		}
	}

	return ReadStateUnread
}

// boundedSender decodes the single From header into at most maxSenderBytes of
// valid UTF-8. Missing, repeated, malformed or control-bearing values are
// omitted rather than reported, so decoder text and errors never escape.
// Charset decoders replace bytes invalid for the declared charset with
// U+FFFD instead of failing, so any replacement character marks a lossy
// decode and the sender is omitted.
func boundedSender(header mail.Header) (string, bool) {
	values := header["From"]
	if len(values) != 1 {
		return "", false
	}

	// The raw value is already bounded by the header literal limit; it is
	// decoded whole so the cap never splits an encoded word.
	raw := values[0]
	if !utf8.ValidString(raw) || !validSenderEncodedWords(raw) {
		return "", false
	}

	decoder := mime.WordDecoder{CharsetReader: charset.NewReaderLabel}
	decoded, err := decoder.DecodeHeader(raw)
	if err != nil || !utf8.ValidString(decoded) || strings.ContainsFunc(decoded, unicode.IsControl) || strings.Contains(decoded, "=?") || strings.ContainsRune(decoded, utf8.RuneError) {
		return "", false
	}

	return truncateUTF8(strings.TrimSpace(decoded), maxSenderBytes)
}

// validSenderEncodedWords requires every "=?" in the sender to open a
// complete RFC 2047 word with a charset, a B or Q encoding and a well-formed
// payload. The standard decoder passes unterminated words, unknown encodings
// and bad Q escapes through verbatim, so those shapes are refused here.
func validSenderEncodedWords(value string) bool {
	for {
		start := strings.Index(value, "=?")
		if start < 0 {
			return true
		}

		// The charset and encoding are located by position so a Q payload that
		// opens with "=" is not mistaken for the closing "?=".
		value = value[start+2:]
		charsetEnd := strings.IndexByte(value, '?')
		if charsetEnd < 1 || len(value) < charsetEnd+3 || value[charsetEnd+2] != '?' {
			return false
		}

		encoding := value[charsetEnd+1 : charsetEnd+2]
		value = value[charsetEnd+3:]
		end := strings.Index(value, "?=")
		if end < 1 {
			return false
		}

		payload := value[:end]
		value = value[end+2:]
		if strings.Contains(payload, "?") || strings.ContainsFunc(payload, unicode.IsSpace) {
			return false
		}

		switch {
		case strings.EqualFold(encoding, "b"):
			if _, err := base64.StdEncoding.DecodeString(payload); err != nil {
				return false
			}
		case strings.EqualFold(encoding, "q"):
			if !validQEscapes(payload) {
				return false
			}
		default:
			return false
		}
	}
}

func validQEscapes(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] != '=' {
			continue
		}
		if index+2 >= len(text) {
			return false
		}
		if _, err := hex.DecodeString(text[index+1 : index+3]); err != nil {
			return false
		}
		index += 2
	}

	return true
}

// headerDate parses the single Date header into RFC3339 UTC. It never falls
// back to another clock: an absent, repeated or invalid Date is unknown.
func headerDate(header mail.Header) (string, DateSource) {
	values := header["Date"]
	if len(values) != 1 {
		return "", DateSourceUnknown
	}

	value, ok := numericDateZone(values[0])
	if !ok {
		return "", DateSourceUnknown
	}

	parsed, err := mail.ParseDate(value)
	if err != nil || parsed.UTC().Year() < 1 || parsed.UTC().Year() > 9999 {
		return "", DateSourceUnknown
	}

	return parsed.UTC().Format(time.RFC3339), DateSourceHeader
}

// rfc5322NamedZones are the obsolete zone names RFC 5322 section 4.3 defines,
// with their offsets in seconds. UTC is accepted as a common synonym of UT.
var rfc5322NamedZones = map[string]int{
	"UT": 0, "UTC": 0, "GMT": 0,
	"EST": -5 * 3600, "EDT": -4 * 3600,
	"CST": -6 * 3600, "CDT": -5 * 3600,
	"MST": -7 * 3600, "MDT": -6 * 3600,
	"PST": -8 * 3600, "PDT": -7 * 3600,
}

// numericDateZone rewrites a trailing RFC 5322 named zone as its numeric
// offset. Go's date parser otherwise gives an unrecognised abbreviation a zero
// offset, or the host's offset when it matches the local zone. Dates that
// already carry a numeric offset pass through; any other named zone, including
// military letters whose meaning RFC 5322 leaves unknown, is refused. Only the
// zone token is replaced, and anything after the zone must be whitespace or
// comments, because the parser ignores unconsumed trailing text.
func numericDateZone(value string) (string, bool) {
	uncommented := value
	if index := strings.IndexByte(uncommented, '('); index >= 0 {
		uncommented = uncommented[:index]
	}
	if index := strings.IndexAny(uncommented, "+-"); index >= 0 && len(uncommented) >= index+5 {
		return value, onlyCFWS(value[index+5:])
	}

	zoneEnd := len(strings.TrimRight(uncommented, " \t\r\n"))
	zoneStart := strings.LastIndexAny(uncommented[:zoneEnd], " \t\r\n") + 1
	if zoneStart == 0 {
		return "", false
	}

	offset, known := rfc5322NamedZones[strings.ToUpper(uncommented[zoneStart:zoneEnd])]
	if !known || !onlyCFWS(value[zoneEnd:]) {
		return "", false
	}

	sign := "+"
	if offset < 0 {
		sign = "-"
		offset = -offset
	}

	return value[:zoneStart] + fmt.Sprintf("%s%02d%02d", sign, offset/3600, offset%3600/60) + value[zoneEnd:], true
}

// onlyCFWS reports whether s holds nothing but folding whitespace and
// balanced, possibly nested RFC 5322 comments.
func onlyCFWS(s string) bool {
	depth := 0
	for index := 0; index < len(s); index++ {
		switch char := s[index]; {
		case depth > 0 && char == '\\':
			index++
		case char == '(':
			depth++
		case char == ')':
			if depth == 0 {
				return false
			}
			depth--
		case depth > 0, char == ' ', char == '\t', char == '\r', char == '\n':
		default:
			return false
		}
	}

	return depth == 0
}

func validateFetchedMetadataUIDs(requested []uint32, results []MessageMetadata) error {
	if len(results) != len(requested) {
		return errorCode(CodeIMAPProtocol)
	}

	remaining := make(map[uint32]struct{}, len(requested))
	for _, uid := range requested {
		if uid == 0 {
			return errorCode(CodeIMAPProtocol)
		}
		if _, exists := remaining[uid]; exists {
			return errorCode(CodeIMAPProtocol)
		}
		remaining[uid] = struct{}{}
	}
	for _, result := range results {
		if _, exists := remaining[result.uid]; !exists {
			return errorCode(CodeIMAPProtocol)
		}
		delete(remaining, result.uid)
	}
	if len(remaining) != 0 {
		return errorCode(CodeIMAPProtocol)
	}

	return nil
}

// UIDFetchBody reads at most limit bytes of one body part section with a
// BODY.PEEK partial fetch. A literal longer than limit is refused.
func (session *imapSession) UIDFetchBody(ctx context.Context, uid uint32, part []int, limit int) ([]byte, error) {
	if uid == 0 || len(part) == 0 || limit < 1 {
		return nil, errorCode(CodeBoundsExceeded)
	}
	section := &imap.FetchItemBodySection{Part: part, Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: int64(limit)}}
	options := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	var body []byte
	fetch := func() (err error) {
		if session.budget != nil {
			session.budget.begin(fetchInputBudget(limit, 1))
			defer func() {
				if session.budget.end() {
					err = errorCode(CodeBoundsExceeded)
				}
			}()
		}

		command := session.client.Fetch(imap.UIDSetNum(imap.UID(uid)), options)
		defer func() {
			if err != nil && err != errFlagsOnly {
				session.abortFetch(command)
			}
		}()

		message := command.Next()
		if message == nil {
			if err := command.Close(); err != nil {
				return err
			}

			return errorCode(CodeIMAPProtocol)
		}
		bodySections := 0
		uidItems := 0
		for data := message.Next(); data != nil; data = message.Next() {
			switch value := data.(type) {
			case imapclient.FetchItemDataUID:
				if uidItems != 0 || uint32(value.UID) != uid {
					return errorCode(CodeIMAPProtocol)
				}
				uidItems++
			case imapclient.FetchItemDataBodySection:
				if value.Literal == nil || value.Section == nil || !value.MatchCommand(section) || bodySections != 0 {
					return errorCode(CodeIMAPProtocol)
				}
				bodySections++
				result, err := readBoundedLiteral(value.Literal, limit)
				if err != nil {
					return err
				}
				body = result
			}
		}
		if uidItems == 1 && bodySections == 0 {
			return closeFlagsOnly(command)
		}
		if command.Next() != nil || body == nil || bodySections != 1 || uidItems != 1 {
			return errorCode(CodeIMAPProtocol)
		}
		return command.Close()
	}
	err := retryFlagsOnly(func() error { return session.withContext(ctx, fetch) })
	return body, err
}

// UIDFetchHeaders reads exactly one UID's FLAGS, RFC822.SIZE and
// BODY.PEEK[HEADER] in one FETCH. The response must carry the requested UID
// and header section exactly once, and the header literal must fit the limit
// and end at the header terminator; any other shape is refused, aborting the
// transport when the response framing was left unconsumed.
func (session *imapSession) UIDFetchHeaders(ctx context.Context, uid uint32, headerLimit int) (MessageHeaders, error) {
	if uid == 0 || headerLimit < 1 {
		return MessageHeaders{}, errorCode(CodeBoundsExceeded)
	}

	section := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
	options := &imap.FetchOptions{UID: true, Flags: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}
	var item MessageMetadata
	var header []byte
	fetch := func() (err error) {
		item = MessageMetadata{}
		if session.budget != nil {
			session.budget.begin(fetchInputBudget(headerLimit, 1))
			defer func() {
				if session.budget.end() {
					err = errorCode(CodeBoundsExceeded)
				}
			}()
		}

		command := session.client.Fetch(imap.UIDSetNum(imap.UID(uid)), options)
		defer func() {
			if err != nil && err != errFlagsOnly {
				session.abortFetch(command)
			}
		}()

		message := command.Next()
		if message == nil {
			if err := command.Close(); err != nil {
				return err
			}

			return errorCode(CodeIMAPProtocol)
		}

		uidItems := 0
		flagsItems := 0
		sizeItems := 0
		bodySections := 0
		for data := message.Next(); data != nil; data = message.Next() {
			switch value := data.(type) {
			case imapclient.FetchItemDataUID:
				if uidItems != 0 || uint32(value.UID) != uid {
					return errorCode(CodeIMAPProtocol)
				}
				uidItems++
			case imapclient.FetchItemDataFlags:
				if flagsItems != 0 {
					return errorCode(CodeIMAPProtocol)
				}
				flagsItems++
				item.ReadState = metadataReadState(value.Flags)
			case imapclient.FetchItemDataRFC822Size:
				sizeItems++
				item.Size = value.Size
			case imapclient.FetchItemDataBodySection:
				if value.Literal == nil || value.Section == nil || !value.MatchCommand(section) || bodySections != 0 {
					return errorCode(CodeIMAPProtocol)
				}
				bodySections++
				result, err := readBoundedLiteral(value.Literal, headerLimit)
				if err != nil {
					return err
				}
				header = result
			}
		}
		if uidItems == 1 && sizeItems == 0 && bodySections == 0 {
			return closeFlagsOnly(command)
		}
		if command.Next() != nil || uidItems != 1 || bodySections != 1 {
			return errorCode(CodeIMAPProtocol)
		}

		return command.Close()
	}
	err := retryFlagsOnly(func() error { return session.withContext(ctx, fetch) })
	if err != nil {
		return MessageHeaders{}, err
	}

	item.uid = uid
	return selectedHeaderRecord(item, header, headerLimit)
}

// selectedHeaderRecord normalizes a complete header literal with the same
// bounded canonical rules as message normalization. A literal whose first
// blank line is missing or is followed by more data is not a whole header
// block and is never treated as one. The count limit applies before parsing,
// so omitted fields cannot make the retained fields unavailable. The four
// threading fields are recovered from fixed slots of the same block instead,
// so a late Subject or reply link survives generic count omission.
func selectedHeaderRecord(item MessageMetadata, header []byte, headerLimit int) (MessageHeaders, error) {
	if headerTerminatorEnd(header) != len(header) {
		return MessageHeaders{}, errorCode(CodeIMAPProtocol)
	}

	limits := normalizedLimits(NormalizeLimits{MaxHeaderBytes: headerLimit})
	bounded, countTruncated := boundedHeaderCount(header, limits.MaxHeaderCount)
	parsed, err := mail.ReadMessage(bytes.NewReader(bounded))
	if err != nil {
		return MessageHeaders{}, errorCode(CodeIMAPProtocol)
	}
	threadBlock := threadFieldBlock(header)
	threadParsed, err := mail.ReadMessage(bytes.NewReader(threadBlock))
	if err != nil {
		return MessageHeaders{}, errorCode(CodeIMAPProtocol)
	}
	applyHeaderMetadata(&item, parsed.Header)
	item.Subject = threadParsed.Header.Get("Subject")

	headers, truncation, err := canonicalHeaders(parsed.Header, bounded, limits)
	if err != nil {
		return MessageHeaders{}, err
	}

	threadHeaders, threadTruncation, err := canonicalHeaders(threadParsed.Header, threadBlock, limits)
	if err != nil {
		return MessageHeaders{}, err
	}

	headers.Subject = threadHeaders.Subject
	headers.MessageID = threadHeaders.MessageID
	headers.InReplyTo = threadHeaders.InReplyTo
	headers.References = threadHeaders.References
	truncation.Headers = truncation.Headers || threadTruncation.Headers
	truncation.References = truncation.References || threadTruncation.References
	truncation.HeaderCount = countTruncated

	return MessageHeaders{Metadata: item, Headers: headers, Truncation: truncation}, nil
}

// headerTerminatorEnd returns the offset just past the first blank line, the
// same line net/mail treats as the header/body separator, or -1 without one.
func headerTerminatorEnd(header []byte) int {
	for lineStart := 0; lineStart < len(header); {
		lineLength := bytes.IndexByte(header[lineStart:], '\n')
		if lineLength < 0 {
			return -1
		}

		lineEnd := lineStart + lineLength
		if len(bytes.TrimSuffix(header[lineStart:lineEnd], []byte{'\r'})) == 0 {
			return lineEnd + 1
		}

		lineStart = lineEnd + 1
	}

	return -1
}

// errFlagsOnly reports a FETCH reply that carried UID or FLAGS but none of
// the requested items. go-imap takes one untagged FETCH per requested UID, so
// an unsolicited flag update that arrives first fills the slot and the real
// response is discarded. It carries the imap_protocol code, so it surfaces
// unchanged through mapIMAPError and as the result of a second such reply.
var errFlagsOnly = &Error{Code: CodeIMAPProtocol}

// closeFlagsOnly closes command after a flags-only reply without aborting
// the transport; the remaining responses are drained and the tagged status
// read, so the session can issue the FETCH again.
func closeFlagsOnly(command *imapclient.FetchCommand) error {
	if err := command.Close(); err != nil {
		return err
	}

	return errFlagsOnly
}

func retryFlagsOnly(fetch func() error) error {
	if err := fetch(); err != errFlagsOnly {
		return err
	}

	return fetch()
}

func (session *imapSession) abortFetch(command *imapclient.FetchCommand) {
	if session.conn != nil {
		_ = session.conn.Close()
	}
	_ = command.Close()
}

// bodyStructure is go-imap's decoded BODYSTRUCTURE, named here so the
// projection never imports go-imap itself.
type bodyStructure = imap.BodyStructure

// singlePartStructure is one leaf of a bodyStructure, named for the same
// reason.
type singlePartStructure = imap.BodyStructureSinglePart

// multiPartStructure is one multipart container of a bodyStructure, named
// for the same reason.
type multiPartStructure = imap.BodyStructureMultiPart

// fetchUIDStructure sends one UID FETCH of uid for UID and extended
// BODYSTRUCTURE. The response must carry that UID and one extended structure,
// and may carry FLAGS.
func (session *imapSession) fetchUIDStructure(ctx context.Context, uid uint32) (bodyStructure, error) {
	options := &imap.FetchOptions{UID: true, BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}
	var structure imap.BodyStructure
	fetch := func() (err error) {
		command := session.client.Fetch(imap.UIDSetNum(imap.UID(uid)), options)
		defer func() {
			if err != nil && err != errFlagsOnly {
				session.abortFetch(command)
			}
		}()

		message := command.Next()
		if message == nil {
			if err := command.Close(); err != nil {
				return err
			}

			return errorCode(CodeIMAPProtocol)
		}

		uidItems := 0
		flagsItems := 0
		for data := message.Next(); data != nil; data = message.Next() {
			switch value := data.(type) {
			case imapclient.FetchItemDataUID:
				if uidItems != 0 || uint32(value.UID) != uid {
					return errorCode(CodeIMAPProtocol)
				}
				uidItems++
			case imapclient.FetchItemDataFlags:
				if flagsItems != 0 {
					return errorCode(CodeIMAPProtocol)
				}
				flagsItems++
			case imapclient.FetchItemDataBodyStructure:
				if structure != nil || !value.IsExtended || value.BodyStructure == nil {
					return errorCode(CodeIMAPProtocol)
				}
				structure = value.BodyStructure
			default:
				return errorCode(CodeIMAPProtocol)
			}
		}
		if uidItems == 1 && structure == nil {
			return closeFlagsOnly(command)
		}
		if command.Next() != nil || uidItems != 1 || structure == nil {
			return errorCode(CodeIMAPProtocol)
		}

		return command.Close()
	}
	err := retryFlagsOnly(func() error { return session.withBoundedInput(ctx, maxStructureResponseBytes, fetch) })
	if err != nil {
		return nil, err
	}

	return structure, nil
}

func (session *imapSession) Logout(ctx context.Context) error {
	if session == nil || session.client == nil {
		return nil
	}
	defer session.client.Close()

	return session.withContext(ctx, func() error {
		return session.client.Logout().Wait()
	})
}

func (session *imapSession) Abort() error {
	if session == nil || session.client == nil {
		return nil
	}
	if session.conn != nil {
		// A FETCH literal can be intentionally left unread after a bound check.
		// imapclient.Client.Close waits for its response reader, which in turn
		// waits for that literal consumer. Closing the transport is the bounded
		// abort path: it releases the reader without trying to drain attacker-
		// controlled payload bytes.
		return session.conn.Close()
	}

	return session.client.Close()
}

func (session *imapSession) withContext(ctx context.Context, operation func() error) error {
	deadline, hasDeadline := ctx.Deadline()
	if hasDeadline {
		session.conn.setLimit(deadline)
	}
	stop := closeOnContext(ctx, session.conn)
	defer func() {
		stop()
		session.conn.clearLimit()
	}()

	return mapIMAPError(ctx, operation())
}

func (session *imapSession) withBoundedInput(ctx context.Context, limit int, operation func() error) error {
	return session.withContext(ctx, func() error {
		exceeded, err := session.withInputBudget(limit, operation)
		if exceeded {
			return errorCode(CodeBoundsExceeded)
		}

		return err
	})
}

func (session *imapSession) withInputBudget(limit int, operation func() error) (bool, error) {
	if session.budget == nil {
		return false, operation()
	}

	session.budget.begin(limit)
	err := operation()
	return session.budget.end(), err
}

func fetchInputBudget(literalLimit, messageCount int) int {
	return messageCount * (literalLimit + fetchResponseOverheadBytes)
}

func boundedSearchUIDs(data *imap.SearchData, window uidWindow, limit int) ([]uint32, error) {
	uidSet, ok := data.All.(imap.UIDSet)
	if !ok && data.All != nil {
		return nil, errorCode(CodeIMAPProtocol)
	}

	results := make([]uint32, 0, limit)
	seen := make(map[uint32]struct{}, limit)
	for _, item := range uidSet {
		if item.Start == 0 || item.Stop == 0 {
			return nil, errorCode(CodeIMAPProtocol)
		}
		if uint32(item.Start) < window.Start || uint32(item.Stop) > window.End {
			return nil, errorCode(CodeBoundsExceeded)
		}

		count := uint64(item.Stop) - uint64(item.Start) + 1
		if count > uint64(limit-len(results)) {
			return nil, errorCode(CodeBoundsExceeded)
		}
		for uid := item.Start; ; uid++ {
			if _, duplicate := seen[uint32(uid)]; duplicate {
				return nil, errorCode(CodeIMAPProtocol)
			}
			seen[uint32(uid)] = struct{}{}
			results = append(results, uint32(uid))
			if uid == item.Stop {
				break
			}
		}
	}

	return results, nil
}

func readBoundedLiteral(literal imap.LiteralReader, limit int) ([]byte, error) {
	if literal.Size() > int64(limit) {
		return nil, errorCode(CodeBoundsExceeded)
	}
	result := make([]byte, literal.Size())
	if _, err := io.ReadFull(literal, result); err != nil {
		return nil, err
	}
	return result, nil
}

func bytesReader(input []byte) io.Reader {
	return &byteReader{input: input}
}

type byteReader struct {
	input []byte
}

func (reader *byteReader) Read(buffer []byte) (int, error) {
	if len(reader.input) == 0 {
		return 0, io.EOF
	}
	count := copy(buffer, reader.input)
	reader.input = reader.input[count:]
	return count, nil
}

type authenticationFailure struct {
	err error
}

func (failure authenticationFailure) Error() string {
	return failure.err.Error()
}

func (failure authenticationFailure) Unwrap() error {
	return failure.err
}

func mapIMAPError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx != nil {
		if contextError := ctx.Err(); contextError != nil {
			if errors.Is(contextError, context.Canceled) {
				return errorCode(CodeOperationCanceled)
			}
			return errorCode(CodeCommandTimedOut)
		}
		if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
			return errorCode(CodeCommandTimedOut)
		}
	}
	var bridgeError *Error
	if errors.As(err, &bridgeError) {
		return bridgeError
	}
	var authenticationError authenticationFailure
	if errors.As(err, &authenticationError) {
		return errorCode(CodeAuthentication)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return errorCode(CodeBridgeUnreachable)
	}
	var netOperationError *net.OpError
	if errors.As(err, &netOperationError) {
		return errorCode(CodeBridgeUnreachable)
	}
	var imapError *imap.Error
	if errors.As(err, &imapError) {
		return errorCode(CodeIMAPCommand)
	}
	return errorCode(CodeIMAPProtocol)
}

// syntheticGreetingConn serves one locally generated greeting before it reads
// the established TLS connection. It is used only after Dial consumed the
// server greeting during STARTTLS; no server bytes are fabricated or replayed.
type syntheticGreetingConn struct {
	net.Conn
	greeting []byte
}

func (connection *syntheticGreetingConn) Read(buffer []byte) (int, error) {
	if len(connection.greeting) > 0 {
		count := copy(buffer, connection.greeting)
		connection.greeting = connection.greeting[count:]
		return count, nil
	}
	return connection.Conn.Read(buffer)
}

type readBudgetConn struct {
	net.Conn
	mu        sync.Mutex
	active    bool
	idleLimit int
	remaining int
	exceeded  bool
}

func newReadBudgetConn(connection net.Conn, idleLimit int) *readBudgetConn {
	return &readBudgetConn{
		Conn:      connection,
		active:    true,
		idleLimit: idleLimit,
		remaining: idleLimit,
	}
}

func (connection *readBudgetConn) begin(limit int) {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	connection.active = true
	connection.remaining = limit
	connection.exceeded = false
}

func (connection *readBudgetConn) end() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	exceeded := connection.exceeded
	connection.active = true
	connection.remaining = connection.idleLimit
	connection.exceeded = false
	return exceeded
}

func (connection *readBudgetConn) Read(buffer []byte) (int, error) {
	connection.mu.Lock()
	if !connection.active {
		connection.mu.Unlock()
		return connection.Conn.Read(buffer)
	}
	if connection.remaining == 0 {
		connection.exceeded = true
		connection.mu.Unlock()
		return 0, errIMAPInputLimit
	}
	if len(buffer) > connection.remaining {
		buffer = buffer[:connection.remaining]
	}
	connection.mu.Unlock()

	count, err := connection.Conn.Read(buffer)
	connection.mu.Lock()
	if connection.active {
		connection.remaining -= count
	}
	connection.mu.Unlock()
	return count, err
}

var errDispatchRefused = errors.New("IMAP write outside the armed native MOVE")

// dispatchGuardConn enforces the native-MOVE-only write allowlist. While
// armed it forwards exactly one write, which must be one complete
// "<tag> UID MOVE <uid> <mailbox>" line. Any other write, including a
// COPY/STORE/EXPUNGE fallback, a capability refresh, a second command or a
// split or literal-bearing line, is refused without forwarding a byte and
// closes the connection. Disarmed, it is a pass-through.
type dispatchGuardConn struct {
	net.Conn
	mu      sync.Mutex
	armed   bool
	used    bool
	tripped bool
	prefix  string
}

func (connection *dispatchGuardConn) arm(uid uint32) {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	connection.armed, connection.used, connection.tripped = true, false, false
	connection.prefix = "UID MOVE " + strconv.FormatUint(uint64(uid), 10) + " "
}

// disarm reports whether a write was refused while armed.
func (connection *dispatchGuardConn) disarm() bool {
	connection.mu.Lock()
	defer connection.mu.Unlock()

	connection.armed = false
	return connection.tripped
}

func (connection *dispatchGuardConn) Write(buffer []byte) (int, error) {
	connection.mu.Lock()
	if connection.armed {
		if connection.tripped || connection.used || !nativeMoveLine(buffer, connection.prefix) {
			connection.tripped = true
			connection.mu.Unlock()
			_ = connection.Conn.Close()
			return 0, errDispatchRefused
		}
		connection.used = true
	}
	connection.mu.Unlock()

	return connection.Conn.Write(buffer)
}

// nativeMoveLine accepts only "<tag> " + prefix + "<mailbox>\r\n" with no
// other line break and no literal, whose body would need a second write.
func nativeMoveLine(buffer []byte, prefix string) bool {
	line := string(buffer)
	body, complete := strings.CutSuffix(line, "\r\n")
	if !complete || strings.ContainsAny(body, "\r\n") {
		return false
	}

	tag, command, found := strings.Cut(body, " ")
	if !found || tag == "" {
		return false
	}

	mailbox, native := strings.CutPrefix(command, prefix)
	return native && mailbox != "" && !strings.HasSuffix(mailbox, "}")
}

type deadlineConn struct {
	net.Conn
	mu       sync.Mutex
	deadline time.Time
}

func (connection *deadlineConn) setLimit(deadline time.Time) {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.deadline = deadline
	_ = connection.Conn.SetDeadline(deadline)
}

func (connection *deadlineConn) clearLimit() {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	connection.deadline = time.Time{}
	_ = connection.Conn.SetDeadline(time.Time{})
}

func (connection *deadlineConn) SetReadDeadline(deadline time.Time) error {
	return connection.setDeadline(deadline, connection.Conn.SetReadDeadline)
}

func (connection *deadlineConn) SetWriteDeadline(deadline time.Time) error {
	return connection.setDeadline(deadline, connection.Conn.SetWriteDeadline)
}

func (connection *deadlineConn) setDeadline(deadline time.Time, set func(time.Time) error) error {
	connection.mu.Lock()
	defer connection.mu.Unlock()
	if !connection.deadline.IsZero() && (deadline.IsZero() || deadline.After(connection.deadline)) {
		deadline = connection.deadline
	}
	return set(deadline)
}

package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/mail"
	"strings"
	"sync"
	"syscall"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	syntheticStartTLSGreeting         = "* OK Croton local TLS handoff\r\n"
	maxSearchWindows                  = 8
	searchWindowWidth          uint32 = 100
	maxSearchResponseBytes            = 4 << 10
	maxControlResponseBytes           = 64 << 10
	maxListResponseBytes              = maxControlResponseBytes
	fetchResponseOverheadBytes        = 4 << 10
)

var errIMAPInputLimit = errors.New("IMAP response exceeds input budget")

// readSession intentionally exposes only Croton-owned read operations. The
// concrete go-imap client never escapes this file.
type readSession interface {
	Authenticate(context.Context, Credentials) error
	List(context.Context, int) ([]Folder, error)
	Status(context.Context, string) (MailboxStatus, error)
	Examine(context.Context, string) (mailboxSnapshot, error)
	UIDSearchWindow(context.Context, SearchQuery, uidWindow, int) ([]uint32, error)
	UIDFetchMetadata(context.Context, string, []uint32, int) ([]MessageMetadata, error)
	UIDFetchBody(context.Context, uint32, int) ([]byte, error)
	Logout(context.Context) error
	Abort() error
}

type mailboxSnapshot struct {
	UIDNext     uint32
	UIDValidity uint32
}

type uidWindow struct {
	Start uint32
	End   uint32
}

type imapSession struct {
	client *imapclient.Client
	conn   *deadlineConn
	budget *readBudgetConn
}

func newIMAPSession(ctx context.Context, connection *Connection) (*imapSession, error) {
	transport, startTLS, err := connection.takeConn()
	if err != nil {
		return nil, err
	}

	wrapped := &deadlineConn{Conn: transport}
	budget := newReadBudgetConn(wrapped, maxControlResponseBytes)
	clientTransport := net.Conn(budget)
	if startTLS {
		clientTransport = &syntheticGreetingConn{Conn: budget, greeting: []byte(syntheticStartTLSGreeting)}
	}

	client := imapclient.New(clientTransport, nil)
	session := &imapSession{client: client, conn: wrapped, budget: budget}
	if err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		if err := client.WaitGreeting(); err != nil {
			return err
		}

		_, err := client.Capability().Wait()
		return err
	}); err != nil {
		_ = client.Close()
		return nil, err
	}

	return session, nil
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
		_, err := session.client.Capability().Wait()
		return err
	})
}

func (session *imapSession) List(ctx context.Context, limit int) ([]Folder, error) {
	var folders []Folder
	err := session.withContext(ctx, func() error {
		exceeded, listErr := session.withInputBudget(maxListResponseBytes, func() error {
			command := session.client.List("", "*", nil)
			for {
				data := command.Next()
				if data == nil {
					break
				}
				if len(folders) == limit {
					_ = session.conn.Close()
					return errorCode(CodeBoundsExceeded)
				}
				folders = append(folders, Folder{Name: data.Mailbox, Delimiter: string(data.Delim)})
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

	return folders, nil
}

func (session *imapSession) Status(ctx context.Context, mailbox string) (MailboxStatus, error) {
	var result MailboxStatus
	err := session.withBoundedInput(ctx, maxControlResponseBytes, func() error {
		data, err := session.client.Status(mailbox, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true}).Wait()
		if err != nil {
			return err
		}
		if !mailboxIdentityMatches(mailbox, data.Mailbox) || data.NumMessages == nil || data.NumUnseen == nil || data.UIDNext == 0 || data.UIDValidity == 0 {
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

func (session *imapSession) UIDSearchWindow(ctx context.Context, query SearchQuery, window uidWindow, limit int) ([]uint32, error) {
	if window.Start == 0 || window.End < window.Start || limit < 1 {
		return nil, errorCode(CodeBoundsExceeded)
	}

	var results []uint32
	err := session.withContext(ctx, func() error {
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
		var searchErr error
		exceeded := false
		exceeded, searchErr = session.withSearchInputBudget(func() error {
			data, searchErr = session.client.UIDSearch(criteria, nil).Wait()
			return searchErr
		})
		if exceeded {
			return errorCode(CodeBoundsExceeded)
		}
		if searchErr != nil {
			return searchErr
		}

		var rangeErr error
		results, rangeErr = boundedSearchUIDs(data, window, limit)
		return rangeErr
	})
	return results, err
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
	options := &imap.FetchOptions{UID: true, RFC822Size: true, BodySection: []*imap.FetchItemBodySection{section}}
	var results []MessageMetadata
	err := session.withContext(ctx, func() (err error) {
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
			if err != nil {
				session.abortFetch(command)
			}
		}()

		for message := command.Next(); message != nil; message = command.Next() {
			var item MessageMetadata
			bodySections := 0
			for data := message.Next(); data != nil; data = message.Next() {
				switch value := data.(type) {
				case imapclient.FetchItemDataUID:
					item.uid = uint32(value.UID)
				case imapclient.FetchItemDataRFC822Size:
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
					item.Subject = parsed.Header.Get("Subject")
				}
			}
			if item.uid == 0 || bodySections != 1 {
				return errorCode(CodeIMAPProtocol)
			}
			results = append(results, item)
		}
		if err := command.Close(); err != nil {
			return err
		}
		return validateFetchedMetadataUIDs(uids, results)
	})
	return results, err
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

func (session *imapSession) UIDFetchBody(ctx context.Context, uid uint32, limit int) ([]byte, error) {
	if uid == 0 || limit < 1 {
		return nil, errorCode(CodeBoundsExceeded)
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: int64(limit) + 1}}
	options := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	var body []byte
	err := session.withContext(ctx, func() (err error) {
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
			if err != nil {
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
		if command.Next() != nil || body == nil || bodySections != 1 || uidItems != 1 {
			return errorCode(CodeIMAPProtocol)
		}
		return command.Close()
	})
	return body, err
}

func (session *imapSession) abortFetch(command *imapclient.FetchCommand) {
	if session.conn != nil {
		_ = session.conn.Close()
	}
	_ = command.Close()
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

func (session *imapSession) withSearchInputBudget(operation func() error) (bool, error) {
	return session.withInputBudget(maxSearchResponseBytes, operation)
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
	if !ok {
		return nil, errorCode(CodeIMAPProtocol)
	}

	results := make([]uint32, 0, limit)
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
	if errors.Is(ctx.Err(), context.Canceled) {
		return errorCode(CodeOperationCanceled)
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errorCode(CodeCommandTimedOut)
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

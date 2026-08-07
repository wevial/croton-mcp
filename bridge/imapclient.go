package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"net/mail"
	"sync"
	"time"

	imap "github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	syntheticStartTLSGreeting        = "* OK Croton local TLS handoff\r\n"
	maxSearchWindows                 = 8
	searchWindowWidth         uint32 = 100
)

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
}

func newIMAPSession(connection *Connection) (*imapSession, error) {
	transport, startTLS, err := connection.takeConn()
	if err != nil {
		return nil, err
	}

	wrapped := &deadlineConn{Conn: transport}
	clientTransport := net.Conn(wrapped)
	if startTLS {
		clientTransport = &syntheticGreetingConn{Conn: wrapped, greeting: []byte(syntheticStartTLSGreeting)}
	}

	client := imapclient.New(clientTransport, nil)
	if err := client.WaitGreeting(); err != nil {
		_ = client.Close()
		return nil, mapIMAPError(context.Background(), err)
	}
	if _, err := client.Capability().Wait(); err != nil {
		_ = client.Close()
		return nil, mapIMAPError(context.Background(), err)
	}

	return &imapSession{client: client, conn: wrapped}, nil
}

func (session *imapSession) Authenticate(ctx context.Context, credentials Credentials) error {
	return session.withContext(ctx, func() error {
		username := string(credentials.Username)
		password := string(credentials.Password)
		defer credentials.Zero()

		if err := session.client.Login(username, password).Wait(); err != nil {
			return err
		}
		_, err := session.client.Capability().Wait()
		return err
	})
}

func (session *imapSession) List(ctx context.Context, limit int) ([]Folder, error) {
	var folders []Folder
	err := session.withContext(ctx, func() error {
		command := session.client.List("", "*", nil)
		for {
			data := command.Next()
			if data == nil {
				break
			}
			if len(folders) == limit {
				_ = session.client.Close()
				return errorCode(CodeBoundsExceeded)
			}
			folders = append(folders, Folder{Name: data.Mailbox, Delimiter: string(data.Delim)})
		}
		return command.Close()
	})
	if err != nil {
		return nil, err
	}

	return folders, nil
}

func (session *imapSession) Status(ctx context.Context, mailbox string) (MailboxStatus, error) {
	var result MailboxStatus
	err := session.withContext(ctx, func() error {
		data, err := session.client.Status(mailbox, &imap.StatusOptions{NumMessages: true, UIDNext: true, UIDValidity: true, NumUnseen: true}).Wait()
		if err != nil {
			return err
		}
		if data.NumMessages != nil {
			result.Messages = int(*data.NumMessages)
		}
		if data.NumUnseen != nil {
			result.Unseen = int(*data.NumUnseen)
		}
		result.UIDNext = uint32(data.UIDNext)
		result.UIDValidity = data.UIDValidity
		return nil
	})
	return result, err
}

func (session *imapSession) Examine(ctx context.Context, mailbox string) (mailboxSnapshot, error) {
	var snapshot mailboxSnapshot
	err := session.withContext(ctx, func() error {
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
		data, err := session.client.UIDSearch(criteria, nil).Wait()
		if err != nil {
			return err
		}
		for _, uid := range data.AllUIDs() {
			if uint32(uid) < window.Start || uint32(uid) > window.End || len(results) == limit {
				return errorCode(CodeBoundsExceeded)
			}
			results = append(results, uint32(uid))
		}
		return nil
	})
	return results, err
}

func (session *imapSession) UIDFetchMetadata(ctx context.Context, _ string, uids []uint32, headerLimit int) ([]MessageMetadata, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if headerLimit < 1 {
		panic(headerLimit)
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
	err := session.withContext(ctx, func() error {
		command := session.client.Fetch(set, options)
		for message := command.Next(); message != nil; message = command.Next() {
			var item MessageMetadata
			for data := message.Next(); data != nil; data = message.Next() {
				switch value := data.(type) {
				case imapclient.FetchItemDataUID:
					item.uid = uint32(value.UID)
				case imapclient.FetchItemDataRFC822Size:
					item.Size = value.Size
				case imapclient.FetchItemDataBodySection:
					if value.Literal == nil {
						return errorCode(CodeIMAPProtocol)
					}
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
			if item.uid == 0 {
				return errorCode(CodeIMAPProtocol)
			}
			results = append(results, item)
		}
		if err := command.Close(); err != nil {
			return err
		}
		if len(results) != len(uids) {
			return errorCode(CodeIMAPProtocol)
		}
		return nil
	})
	return results, err
}

func (session *imapSession) UIDFetchBody(ctx context.Context, uid uint32, limit int) ([]byte, error) {
	if uid == 0 || limit < 1 {
		return nil, errorCode(CodeBoundsExceeded)
	}
	section := &imap.FetchItemBodySection{Peek: true, Partial: &imap.SectionPartial{Offset: 0, Size: int64(limit) + 1}}
	options := &imap.FetchOptions{UID: true, BodySection: []*imap.FetchItemBodySection{section}}
	var body []byte
	err := session.withContext(ctx, func() error {
		command := session.client.Fetch(imap.UIDSetNum(imap.UID(uid)), options)
		message := command.Next()
		if message == nil {
			return command.Close()
		}
		for data := message.Next(); data != nil; data = message.Next() {
			if value, ok := data.(imapclient.FetchItemDataBodySection); ok {
				if value.Literal == nil {
					return errorCode(CodeIMAPProtocol)
				}
				result, err := readBoundedLiteral(value.Literal, limit)
				if err != nil {
					return err
				}
				body = result
			}
		}
		if command.Next() != nil || body == nil {
			return errorCode(CodeIMAPProtocol)
		}
		return command.Close()
	})
	return body, err
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
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
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

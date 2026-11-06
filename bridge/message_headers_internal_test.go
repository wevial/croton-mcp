package bridge

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
)

const (
	headerTestUID         = 101
	headerTestUIDValidity = 9001
	headerTestLimit       = 4096
	headerBodySentinel    = "croton-header-body-sentinel"
)

// selectedHeaderResult mirrors the expected MessageHeaders fields using only
// pre-existing types, so this file compiles before the operation exists.
type selectedHeaderResult struct {
	Metadata   MessageMetadata
	Headers    CanonicalHeaders
	Truncation Truncation
}

// callGetMessageHeaders invokes Adapter.GetMessageHeaders through reflection
// and requires its result to hold exactly Metadata, Headers and Truncation.
func callGetMessageHeaders(t *testing.T, adapter *Adapter, identifier string) (selectedHeaderResult, error) {
	t.Helper()

	method := reflect.ValueOf(adapter).MethodByName("GetMessageHeaders")
	if !method.IsValid() {
		t.Fatal("Adapter.GetMessageHeaders is absent")
	}

	methodType := method.Type()
	errorType := reflect.TypeFor[error]()
	if methodType.NumIn() != 2 || methodType.In(0) != reflect.TypeFor[context.Context]() || methodType.In(1).Kind() != reflect.String || methodType.NumOut() != 2 || methodType.Out(1) != errorType {
		t.Fatalf("Adapter.GetMessageHeaders has signature %s", methodType)
	}

	resultType := methodType.Out(0)
	if resultType.Name() != "MessageHeaders" || resultType.Kind() != reflect.Struct || resultType.NumField() != 3 {
		t.Fatalf("GetMessageHeaders result type %s does not hold exactly three fields", resultType)
	}

	outputs := method.Call([]reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(identifier)})
	value := outputs[0]
	result := selectedHeaderResult{}
	for index, field := range []any{&result.Metadata, &result.Headers, &result.Truncation} {
		name := []string{"Metadata", "Headers", "Truncation"}[index]
		member := value.FieldByName(name)
		if !member.IsValid() || member.Type() != reflect.TypeOf(field).Elem() {
			t.Fatalf("MessageHeaders.%s is missing or has the wrong type", name)
		}
		reflect.ValueOf(field).Elem().Set(member)
	}

	err, _ := outputs[1].Interface().(error)
	return result, err
}

// headerFetchScript is the canned response to one header FETCH.
type headerFetchScript struct {
	uidItems string
	flags    string
	size     int
	sections []string
	header   string
	omit     bool
	drop     bool
}

func headerScript(flags, header string) headerFetchScript {
	return headerFetchScript{uidItems: fmt.Sprintf("UID %d", headerTestUID), flags: flags, size: 300 << 10, sections: []string{"[HEADER]"}, header: header}
}

func (script headerFetchScript) render(tag string) string {
	if script.omit {
		return tag + " OK FETCH completed\r\n"
	}

	response := fmt.Sprintf("* 1 FETCH (%s%s RFC822.SIZE %d", script.uidItems, script.flags, script.size)
	for _, section := range script.sections {
		response += fmt.Sprintf(" BODY%s {%d}\r\n%s", section, len(script.header), script.header)
	}

	return response + ")\r\n" + tag + " OK FETCH completed\r\n"
}

// headerPeer is a closed in-process IMAP peer answering only the greeting,
// UID FETCH and LOGOUT. It records every command it receives.
type headerPeer struct {
	mu       sync.Mutex
	commands []string
}

func (peer *headerPeer) serve(connection net.Conn, script headerFetchScript) {
	defer connection.Close()

	reader := bufio.NewReader(connection)
	if _, err := connection.Write([]byte("* OK [CAPABILITY IMAP4rev1] closed header script\r\n")); err != nil {
		return
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		raw := strings.TrimRight(line, "\r\n")
		peer.mu.Lock()
		peer.commands = append(peer.commands, raw)
		peer.mu.Unlock()

		tag, command, _ := strings.Cut(raw, " ")
		var response string
		switch {
		case strings.HasPrefix(strings.ToUpper(command), "UID FETCH "):
			if script.drop {
				// The whole untagged FETCH arrives, then the transport drops
				// before tagged completion.
				untagged, _, _ := strings.Cut(script.render(tag), tag+" OK")
				_, _ = connection.Write([]byte(untagged))
				return
			}
			response = script.render(tag)
		case strings.EqualFold(command, "LOGOUT"):
			response = "* BYE closing\r\n" + tag + " OK LOGOUT completed\r\n"
		default:
			response = tag + " BAD unsupported by script\r\n"
		}
		if _, err := connection.Write([]byte(response)); err != nil {
			return
		}
	}
}

func (peer *headerPeer) fetches() []string {
	peer.mu.Lock()
	defer peer.mu.Unlock()

	var fetches []string
	for _, command := range peer.commands {
		if strings.Contains(strings.ToUpper(command), " FETCH ") {
			fetches = append(fetches, command)
		}
	}

	return fetches
}

// headerTestSession uses the real go-imap FETCH parser while stubbing mailbox
// selection and the exact-UID existence check.
type headerTestSession struct {
	*imapSession
	peer        *headerPeer
	uidValidity uint32
	present     bool
	examines    int
	bodies      int
	aborts      int
}

func (session *headerTestSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	session.examines++
	return mailboxSnapshot{UIDValidity: session.uidValidity, UIDNext: headerTestUID + 1}, nil
}

func (session *headerTestSession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, _ int) ([]uint32, error) {
	if !session.present || headerTestUID < window.Start || headerTestUID > window.End {
		return nil, nil
	}

	return []uint32{headerTestUID}, nil
}

func (session *headerTestSession) UIDFetchBody(context.Context, uint32, int) ([]byte, error) {
	session.bodies++
	return nil, errorCode(CodeIMAPProtocol)
}

func (session *headerTestSession) Abort() error {
	session.aborts++
	return session.imapSession.Abort()
}

func startHeaderSession(t *testing.T, script headerFetchScript) *headerTestSession {
	t.Helper()

	client, server := net.Pipe()
	peer := &headerPeer{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		peer.serve(server, script)
	}()

	session, err := newIMAPSessionOver(context.Background(), client, false)
	if err != nil {
		t.Fatalf("newIMAPSessionOver: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Abort()
		<-done
	})

	return &headerTestSession{imapSession: session, peer: peer, uidValidity: headerTestUIDValidity, present: true}
}

// newHeaderAdapter hands out the given sessions in order, one per open.
func newHeaderAdapter(key byte, headerLimit int, sessions ...readSession) (*Adapter, *int) {
	opened := 0
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 2000},
			Bounds: Bounds{MaxHeaderBytes: headerLimit, MaxBodyBytes: defaultMaxBodyBytes, MaxOutputBytes: defaultMaxOutputBytes},
		},
		gate: make(chan struct{}, 1),
	}
	adapter.idKey[0] = key
	adapter.factory = func(context.Context) (readSession, error) {
		if opened >= len(sessions) {
			return nil, errorCode(CodeBridgeUnreachable)
		}
		opened++
		return sessions[opened-1], nil
	}
	adapter.gate <- struct{}{}

	return adapter, &opened
}

func headerTestID(t *testing.T, adapter *Adapter, uidValidity, uid uint32) string {
	t.Helper()

	identifier, err := adapter.encodeMessageID(messageIDPayload{Mailbox: "INBOX", UIDValidity: uidValidity, UID: uid})
	if err != nil {
		t.Fatalf("encodeMessageID: %v", err)
	}

	return identifier
}

func headerBlock(fields ...string) string {
	return strings.Join(fields, "\r\n") + "\r\n\r\n"
}

// paddedHeader returns a header block of exactly size bytes ending in the
// header terminator, padding with one X-Pad field.
func paddedHeader(t *testing.T, size int, fields ...string) string {
	t.Helper()

	base := headerBlock(append(fields, "X-Pad: ")...)
	if size < len(base) {
		t.Fatalf("padded header size %d below base %d", size, len(base))
	}
	padded := headerBlock(append(fields, "X-Pad: "+strings.Repeat("p", size-len(base)))...)
	if len(padded) != size {
		t.Fatalf("padded header is %d bytes, want %d", len(padded), size)
	}

	return padded
}

func requireHeaderOnlyFetches(t *testing.T, session *headerTestSession) {
	t.Helper()

	fetches := session.peer.fetches()
	if len(fetches) != 1 {
		t.Fatalf("FETCH commands = %q, want exactly one", fetches)
	}
	upper := strings.ToUpper(fetches[0])
	if !strings.Contains(upper, fmt.Sprintf("UID FETCH %d ", headerTestUID)) || !strings.Contains(upper, "BODY.PEEK[HEADER]") || !strings.Contains(upper, "FLAGS") || strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "BODY[") {
		t.Fatalf("FETCH %q is not a single-UID header-only read", fetches[0])
	}
	if session.bodies != 0 {
		t.Fatalf("UIDFetchBody called %d times", session.bodies)
	}
}

func requireHeaderRefusal(t *testing.T, name string, result selectedHeaderResult, err error, code string) {
	t.Helper()

	if CodeOf(err) != code || err.Error() != code {
		t.Fatalf("%s: error %v, want code-only %q", name, err, code)
	}
	if !reflect.DeepEqual(result, selectedHeaderResult{}) {
		t.Fatalf("%s: refused read returned %+v", name, result)
	}
}

// replyHeader is the full readable header used for identity checks.
var replyHeader = headerBlock(
	"From: =?utf-8?q?R=C3=A9ply_Sender?= <reply@croton.test>",
	"To: Reader <reader@croton.test>",
	"Subject: Re: selected headers",
	"Date: Tue, 03 Nov 2026 09:15:00 -0800",
	"Message-ID: <reply@croton.test>",
	"In-Reply-To: <root@croton.test>",
	"References: <root@croton.test> <middle@croton.test>",
)

func TestMessageHeadersAdapter(t *testing.T) {
	t.Run("identity_and_headers", func(t *testing.T) {
		session := startHeaderSession(t, headerScript(` FLAGS (\Seen)`, replyHeader))
		adapter, opened := newHeaderAdapter(1, headerTestLimit, session)
		identifier := headerTestID(t, adapter, headerTestUIDValidity, headerTestUID)

		result, err := callGetMessageHeaders(t, adapter, identifier)
		if err != nil {
			t.Fatalf("GetMessageHeaders: %v", err)
		}

		metadata := result.Metadata
		if metadata.ID != identifier || metadata.Mailbox != "INBOX" || metadata.UIDValidity != headerTestUIDValidity || metadata.UID != headerTestUID || metadata.uid != headerTestUID {
			t.Fatalf("metadata identity = %q %q %d %d", metadata.ID, metadata.Mailbox, metadata.UIDValidity, metadata.UID)
		}
		if metadata.Size != 300<<10 || metadata.ReadState != ReadStateRead || metadata.Subject != "Re: selected headers" || metadata.From != "Réply Sender <reply@croton.test>" || metadata.Date != "2026-11-03T17:15:00Z" || metadata.DateSource != DateSourceHeader || metadata.MetadataTruncated {
			t.Fatalf("metadata = %+v", metadata)
		}

		want := CanonicalHeaders{
			Date:       "Tue, 03 Nov 2026 09:15:00 -0800",
			From:       "Réply Sender <reply@croton.test>",
			To:         "Reader <reader@croton.test>",
			Subject:    "Re: selected headers",
			MessageID:  "<reply@croton.test>",
			InReplyTo:  "<root@croton.test>",
			References: []string{"<root@croton.test>", "<middle@croton.test>"},
		}
		if !reflect.DeepEqual(result.Headers, want) || result.Truncation.Any() {
			t.Fatalf("headers = %+v truncation %+v, want %+v", result.Headers, result.Truncation, want)
		}

		encoded, err := json.Marshal(result)
		if err != nil {
			t.Fatalf("marshal result: %v", err)
		}
		if strings.Contains(string(encoded), headerBodySentinel) || session.examines != 1 || *opened != 1 {
			t.Fatalf("result %s examines %d opened %d", encoded, session.examines, *opened)
		}
		requireHeaderOnlyFetches(t, session)
	})

	t.Run("invalidation_and_capability", func(t *testing.T) {
		stale := startHeaderSession(t, headerScript(" FLAGS ()", replyHeader))
		stale.uidValidity = headerTestUIDValidity + 1
		adapter, _ := newHeaderAdapter(2, headerTestLimit, stale)
		result, err := callGetMessageHeaders(t, adapter, headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
		requireHeaderRefusal(t, "stale UIDVALIDITY", result, err, CodeStaleMessageID)
		if len(stale.peer.fetches()) != 0 || stale.bodies != 0 {
			t.Fatalf("stale UIDVALIDITY reached FETCH %q bodies %d", stale.peer.fetches(), stale.bodies)
		}

		missing := startHeaderSession(t, headerScript(" FLAGS ()", replyHeader))
		missing.present = false
		adapter, _ = newHeaderAdapter(3, headerTestLimit, missing)
		result, err = callGetMessageHeaders(t, adapter, headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
		requireHeaderRefusal(t, "missing UID", result, err, CodeStaleMessageID)
		if len(missing.peer.fetches()) != 0 || missing.bodies != 0 {
			t.Fatalf("missing UID reached FETCH %q bodies %d", missing.peer.fetches(), missing.bodies)
		}

		before, _ := newHeaderAdapter(4, headerTestLimit)
		restarted, opened := newHeaderAdapter(5, headerTestLimit)
		result, err = callGetMessageHeaders(t, restarted, headerTestID(t, before, headerTestUIDValidity, headerTestUID))
		requireHeaderRefusal(t, "old ID after restart", result, err, CodeStaleMessageID)
		if *opened != 0 {
			t.Fatalf("old ID opened %d sessions", *opened)
		}

		firstScript := headerScript(` FLAGS (\Seen)`, headerBlock("Subject: first attempt", "Message-ID: <first@croton.test>"))
		firstScript.drop = true
		first := startHeaderSession(t, firstScript)
		second := startHeaderSession(t, headerScript(" FLAGS ()", replyHeader))
		adapter, opened = newHeaderAdapter(6, headerTestLimit, first, second)
		identifier := headerTestID(t, adapter, headerTestUIDValidity, headerTestUID)
		result, err = callGetMessageHeaders(t, adapter, identifier)
		if err != nil {
			t.Fatalf("retried GetMessageHeaders: %v", err)
		}
		if *opened != 2 || first.aborts == 0 || len(first.peer.fetches()) != 1 || first.examines != 1 || second.examines != 1 {
			t.Fatalf("retry opened %d first aborts %d fetches %q examines %d/%d", *opened, first.aborts, first.peer.fetches(), first.examines, second.examines)
		}
		if result.Metadata.ID != identifier || result.Metadata.UID != headerTestUID || result.Metadata.ReadState != ReadStateUnread || result.Metadata.Subject != "Re: selected headers" || result.Headers.MessageID != "<reply@croton.test>" {
			t.Fatalf("retry kept first-attempt data: %+v", result)
		}
		requireHeaderOnlyFetches(t, second)

		legacy := &replaySearchSession{snapshot: mailboxSnapshot{UIDNext: 201, UIDValidity: headerTestUIDValidity}}
		adapter, opened = newHeaderAdapter(7, headerTestLimit, legacy)
		result, err = callGetMessageHeaders(t, adapter, headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
		requireHeaderRefusal(t, "absent capability", result, err, CodeUnsupported)
		if *opened != 1 || legacy.searchCalls != 0 || legacy.metadataCalls != 0 {
			t.Fatalf("absent capability opened %d searched %d fetched metadata %d", *opened, legacy.searchCalls, legacy.metadataCalls)
		}

		// The refusal does not invalidate the session, and metadata reads on
		// a session without the capability keep working.
		legacy.searchResults = [][]uint32{{headerTestUID}}
		legacy.metadataResults = []replaySearchResponse{{metadata: []MessageMetadata{{uid: headerTestUID}}}}
		metadata, err := adapter.GetMessageMetadata(context.Background(), headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
		if err != nil || metadata.UID != headerTestUID || *opened != 1 {
			t.Fatalf("legacy GetMessageMetadata = %+v, %v, opened %d", metadata, err, *opened)
		}
	})

	t.Run("bounds_and_response", func(t *testing.T) {
		run := func(t *testing.T, key byte, script headerFetchScript) (selectedHeaderResult, error, *headerTestSession) {
			t.Helper()

			session := startHeaderSession(t, script)
			adapter, _ := newHeaderAdapter(key, headerTestLimit, session)
			result, err := callGetMessageHeaders(t, adapter, headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
			return result, err, session
		}

		atCap := paddedHeader(t, headerTestLimit, "Subject: at cap", "Message-ID: <cap@croton.test>")
		result, err, session := run(t, 10, headerScript(" FLAGS ()", atCap))
		if err != nil || result.Headers.Subject != "at cap" || result.Headers.MessageID != "<cap@croton.test>" || result.Truncation.Any() {
			t.Fatalf("header at cap = %+v, %v", result, err)
		}
		requireHeaderOnlyFetches(t, session)

		overCap := paddedHeader(t, headerTestLimit+1, "Subject: over cap")
		result, err, session = run(t, 11, headerScript(" FLAGS ()", overCap))
		requireHeaderRefusal(t, "header cap+1", result, err, CodeBoundsExceeded)
		if session.aborts == 0 {
			t.Fatal("header cap+1 did not abort the session")
		}

		fields := func(count int, last string) string {
			lines := []string{"Subject: counted", "Message-ID: <count@croton.test>"}
			for len(lines) < count-1 {
				lines = append(lines, fmt.Sprintf("X-Field-%03d: value", len(lines)))
			}
			return headerBlock(append(lines, last)...)
		}
		result, err, _ = run(t, 12, headerScript(" FLAGS ()", fields(100, "In-Reply-To: <parent@croton.test>")))
		if err != nil || result.Headers.InReplyTo != "<parent@croton.test>" || result.Truncation.HeaderCount {
			t.Fatalf("100 header fields = %+v, %v", result, err)
		}
		result, err, _ = run(t, 13, headerScript(" FLAGS ()", fields(101, "In-Reply-To: <parent@croton.test>")))
		if err != nil || result.Headers.InReplyTo != "" || result.Headers.Subject != "counted" || !result.Truncation.HeaderCount {
			t.Fatalf("101 header fields = %+v, %v", result, err)
		}

		references := func(count int) (string, []string) {
			identifiers := make([]string, count)
			for index := range identifiers {
				identifiers[index] = fmt.Sprintf("<ref-%02d@croton.test>", index)
			}
			return headerBlock("Message-ID: <refs@croton.test>", "References: "+strings.Join(identifiers, " ")), identifiers
		}
		header, identifiers := references(50)
		result, err, _ = run(t, 14, headerScript(" FLAGS ()", header))
		if err != nil || !reflect.DeepEqual(result.Headers.References, identifiers) || result.Truncation.References {
			t.Fatalf("50 references = %d %+v, %v", len(result.Headers.References), result.Truncation, err)
		}
		header, identifiers = references(51)
		result, err, _ = run(t, 15, headerScript(" FLAGS ()", header))
		if err != nil || !reflect.DeepEqual(result.Headers.References, identifiers[1:]) || !result.Truncation.References {
			t.Fatalf("51 references = %d %+v, %v", len(result.Headers.References), result.Truncation, err)
		}

		structural := []struct {
			name   string
			script func() headerFetchScript
		}{
			{name: "wrong UID", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.uidItems = fmt.Sprintf("UID %d", headerTestUID+1)
				return script
			}},
			{name: "duplicate UID", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.uidItems = fmt.Sprintf("UID %d UID %d", headerTestUID, headerTestUID)
				return script
			}},
			{name: "wrong section", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.sections = []string{"[TEXT]"}
				return script
			}},
			{name: "duplicate section", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.sections = []string{"[HEADER]", "[HEADER]"}
				return script
			}},
			{name: "duplicate FLAGS", script: func() headerFetchScript {
				return headerScript(" FLAGS () FLAGS (\\Seen)", replyHeader)
			}},
			{name: "missing section", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.sections = nil
				return script
			}},
			{name: "missing response", script: func() headerFetchScript {
				script := headerScript(" FLAGS ()", replyHeader)
				script.omit = true
				return script
			}},
			{name: "incomplete header literal", script: func() headerFetchScript {
				return headerScript(" FLAGS ()", "Subject: incomplete\r\nMessage-ID: <partial@croton.test>\r\n")
			}},
		}
		for index, test := range structural {
			result, err, session := run(t, byte(20+index), test.script())
			requireHeaderRefusal(t, test.name, result, err, CodeIMAPProtocol)
			if session.aborts == 0 || session.bodies != 0 {
				t.Fatalf("%s: aborts %d bodies %d, want safe abort without body read", test.name, session.aborts, session.bodies)
			}
		}

		// Existing literal, metadata and header limits are unchanged.
		if defaultMaxHeaderBytes != 16<<10 || maximumMaxHeaderBytes != 64<<10 || defaultMaxHeaderCount != 100 || defaultMaxReferenceCount != 50 || fetchInputBudget(headerTestLimit, 1) != headerTestLimit+fetchResponseOverheadBytes {
			t.Fatal("header, reference or fetch input limits changed")
		}
	})
}

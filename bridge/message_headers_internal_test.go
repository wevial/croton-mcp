package bridge

import (
	"bufio"
	"context"
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
)

// selectedHeaderResult mirrors the expected MessageHeaders fields using only
// pre-existing types, so these tests compile before the operation exists.
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
	if methodType.NumIn() != 2 || methodType.In(0) != reflect.TypeFor[context.Context]() || methodType.In(1).Kind() != reflect.String || methodType.NumOut() != 2 || methodType.Out(1) != reflect.TypeFor[error]() {
		t.Fatalf("Adapter.GetMessageHeaders has signature %s", methodType)
	}

	resultType := methodType.Out(0)
	if resultType.Name() != "MessageHeaders" || resultType.Kind() != reflect.Struct || resultType.NumField() != 3 {
		t.Fatalf("GetMessageHeaders result type %s does not hold exactly three fields", resultType)
	}

	outputs := method.Call([]reflect.Value{reflect.ValueOf(context.Background()), reflect.ValueOf(identifier)})
	result := selectedHeaderResult{}
	target := reflect.ValueOf(&result).Elem()
	for _, name := range []string{"Metadata", "Headers", "Truncation"} {
		member := outputs[0].FieldByName(name)
		if !member.IsValid() || member.Type() != target.FieldByName(name).Type() {
			t.Fatalf("MessageHeaders.%s is missing or has the wrong type", name)
		}
		target.FieldByName(name).Set(member)
	}

	err, _ := outputs[1].Interface().(error)
	return result, err
}

// headerFetchScript is the canned response to one header FETCH.
type headerFetchScript struct {
	uidItems string
	flags    string
	sections []string
	header   string
	omit     bool
	drop     bool
}

func headerScript(flags, header string) headerFetchScript {
	return headerFetchScript{uidItems: fmt.Sprintf("UID %d", headerTestUID), flags: flags, sections: []string{"[HEADER]"}, header: header}
}

func (script headerFetchScript) render(tag string) string {
	if script.omit {
		return tag + " OK FETCH completed\r\n"
	}

	response := fmt.Sprintf("* 1 FETCH (%s%s RFC822.SIZE %d", script.uidItems, script.flags, 300<<10)
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

func (session *headerTestSession) UIDFetchBody(context.Context, uint32, []int, int) ([]byte, error) {
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
func newHeaderAdapter(key byte, headerLimit int, sessions ...bridgeSession) (*Adapter, *int) {
	opened := 0
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 2000},
			Bounds: Bounds{MaxHeaderBytes: headerLimit, MaxBodyBytes: defaultMaxBodyBytes, MaxOutputBytes: defaultMaxOutputBytes},
		},
		gate: make(chan struct{}, 1),
	}
	adapter.idKey[0] = key
	adapter.factory = func(context.Context) (bridgeSession, error) {
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

// paddedHeader returns a header block of exactly size bytes ending in the
// header terminator, padding with one X-Pad field.
func paddedHeader(size int, fields ...string) string {
	base := metadataHeader(append(fields, "X-Pad: ")...)
	return metadataHeader(append(fields, "X-Pad: "+strings.Repeat("p", size-len(base)))...)
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

var replyHeader = metadataHeader(
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

		if session.examines != 1 || *opened != 1 {
			t.Fatalf("examines %d opened %d", session.examines, *opened)
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

		firstScript := headerScript(` FLAGS (\Seen)`, metadataHeader("Subject: first attempt", "Message-ID: <first@croton.test>"))
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

	})

	t.Run("bounds_and_response", func(t *testing.T) {
		run := func(t *testing.T, key byte, script headerFetchScript) (selectedHeaderResult, *headerTestSession, error) {
			t.Helper()

			session := startHeaderSession(t, script)
			adapter, _ := newHeaderAdapter(key, headerTestLimit, session)
			result, err := callGetMessageHeaders(t, adapter, headerTestID(t, adapter, headerTestUIDValidity, headerTestUID))
			return result, session, err
		}

		atCap := paddedHeader(headerTestLimit, "Subject: at cap", "Message-ID: <cap@croton.test>")
		result, session, err := run(t, 10, headerScript(" FLAGS ()", atCap))
		if err != nil || result.Headers.Subject != "at cap" || result.Headers.MessageID != "<cap@croton.test>" || result.Truncation.Any() {
			t.Fatalf("header at cap = %+v, %v", result, err)
		}
		requireHeaderOnlyFetches(t, session)

		overCap := paddedHeader(headerTestLimit+1, "Subject: over cap")
		result, session, err = run(t, 11, headerScript(" FLAGS ()", overCap))
		requireHeaderRefusal(t, "header cap+1", result, err, CodeBoundsExceeded)
		if session.aborts == 0 {
			t.Fatal("header cap+1 did not abort the session")
		}

		fields := func(count int) []string {
			lines := []string{"Subject: counted", "Message-ID: <count@croton.test>"}
			for len(lines) < count-1 {
				lines = append(lines, fmt.Sprintf("X-Field-%03d: value", len(lines)))
			}
			return append(lines, "In-Reply-To: <parent@croton.test>")
		}
		result, _, err = run(t, 12, headerScript(" FLAGS ()", metadataHeader(fields(100)...)))
		if err != nil || result.Headers.InReplyTo != "<parent@croton.test>" || result.Truncation.HeaderCount {
			t.Fatalf("100 header fields = %+v, %v", result, err)
		}
		result, _, err = run(t, 13, headerScript(" FLAGS ()", metadataHeader(fields(101)...)))
		if err != nil || result.Headers.InReplyTo != "<parent@croton.test>" || result.Headers.Subject != "counted" || !result.Truncation.HeaderCount {
			t.Fatalf("101 header fields = %+v, %v", result, err)
		}
		malformed := append(fields(101)[:100], "not-a-header")
		result, _, err = run(t, 16, headerScript(" FLAGS ()", metadataHeader(malformed...)))
		if err != nil || result.Headers.Subject != "counted" || result.Headers.MessageID != "<count@croton.test>" || !result.Truncation.HeaderCount {
			t.Fatalf("malformed omitted field = %+v, %v", result, err)
		}

		references := make([]string, 51)
		for index := range references {
			references[index] = fmt.Sprintf("<ref-%02d@croton.test>", index)
		}
		result, _, err = run(t, 14, headerScript(" FLAGS ()", metadataHeader("Message-ID: <refs@croton.test>", "References: "+strings.Join(references[:50], " "))))
		if err != nil || !reflect.DeepEqual(result.Headers.References, references[:50]) || result.Truncation.References {
			t.Fatalf("50 references = %d %+v, %v", len(result.Headers.References), result.Truncation, err)
		}
		result, _, err = run(t, 15, headerScript(" FLAGS ()", metadataHeader("Message-ID: <refs@croton.test>", "References: "+strings.Join(references, " "))))
		if err != nil || !reflect.DeepEqual(result.Headers.References, references[1:]) || !result.Truncation.References {
			t.Fatalf("51 references = %d %+v, %v", len(result.Headers.References), result.Truncation, err)
		}

		structural := []struct {
			name string
			edit func(*headerFetchScript)
		}{
			{name: "wrong UID", edit: func(script *headerFetchScript) { script.uidItems = fmt.Sprintf("UID %d", headerTestUID+1) }},
			{name: "duplicate UID", edit: func(script *headerFetchScript) {
				script.uidItems = fmt.Sprintf("UID %d UID %d", headerTestUID, headerTestUID)
			}},
			{name: "wrong section", edit: func(script *headerFetchScript) { script.sections = []string{"[TEXT]"} }},
			{name: "duplicate section", edit: func(script *headerFetchScript) { script.sections = []string{"[HEADER]", "[HEADER]"} }},
			{name: "duplicate FLAGS", edit: func(script *headerFetchScript) { script.flags = " FLAGS () FLAGS (\\Seen)" }},
			{name: "missing section", edit: func(script *headerFetchScript) { script.sections = nil }},
			{name: "missing response", edit: func(script *headerFetchScript) { script.omit = true }},
			{name: "incomplete header literal", edit: func(script *headerFetchScript) {
				script.header = "Subject: incomplete\r\nMessage-ID: <partial@croton.test>\r\n"
			}},
			{name: "trailing payload", edit: func(script *headerFetchScript) {
				script.header = "Subject: hi\r\n\r\nunexpected body\r\n\r\n"
			}},
			{name: "trailing payload after mixed separator", edit: func(script *headerFetchScript) {
				script.header = "Subject: hi\n\r\nunexpected body\r\n\r\n"
			}},
		}
		for index, test := range structural {
			script := headerScript(" FLAGS ()", replyHeader)
			test.edit(&script)
			result, session, err := run(t, byte(20+index), script)
			requireHeaderRefusal(t, test.name, result, err, CodeIMAPProtocol)
			if session.aborts == 0 || session.bodies != 0 {
				t.Fatalf("%s: aborts %d bodies %d, want safe abort without body read", test.name, session.aborts, session.bodies)
			}
		}
	})
}

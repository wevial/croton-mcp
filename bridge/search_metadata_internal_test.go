package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// senderByteBound is the ticket's From cap, stated independently of the
// production constant.
const senderByteBound = 512

// sourcedMetadata is the serialized view of the sourced metadata fields, so
// assertions inspect what a caller would marshal rather than private state.
type sourcedMetadata struct {
	ID                string
	Mailbox           string
	UIDValidity       uint32
	UID               uint32
	Subject           string
	From              *string
	Date              *string
	DateSource        *string
	ReadState         *string
	MetadataTruncated *bool
}

func serializedMetadata(t *testing.T, metadata MessageMetadata) sourcedMetadata {
	t.Helper()

	encoded, err := json.Marshal(metadata)
	if err != nil {
		t.Fatalf("marshal metadata: %v", err)
	}

	var decoded sourcedMetadata
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode serialized metadata: %v", err)
	}
	if decoded.From == nil || decoded.Date == nil || decoded.DateSource == nil || decoded.ReadState == nil || decoded.MetadataTruncated == nil {
		t.Fatalf("serialized metadata %s lacks sourced sender/date/read-state fields", encoded)
	}

	return decoded
}

// scriptedFetch is one canned untagged FETCH response for the closed IMAP
// script. Flags is the raw item text including its leading space, or empty
// when the item is omitted.
type scriptedFetch struct {
	uid    uint32
	flags  string
	header string
}

func (fetch scriptedFetch) line(sequence int) string {
	return fmt.Sprintf("* %d FETCH (UID %d%s RFC822.SIZE %d BODY[HEADER] {%d}\r\n%s)\r\n",
		sequence, fetch.uid, fetch.flags, len(fetch.header)+16, len(fetch.header), fetch.header)
}

// metadataScript is a closed, in-process IMAP peer. It answers only the
// greeting, UID FETCH and LOGOUT, and records every command it receives.
type metadataScript struct {
	mu       sync.Mutex
	commands []string
}

func (script *metadataScript) recorded() []string {
	script.mu.Lock()
	defer script.mu.Unlock()

	return append([]string(nil), script.commands...)
}

func (script *metadataScript) serve(connection net.Conn, fetches []scriptedFetch) {
	defer connection.Close()

	reader := bufio.NewReader(connection)
	if _, err := connection.Write([]byte("* OK [CAPABILITY IMAP4rev1] closed metadata script\r\n")); err != nil {
		return
	}

	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}

		line = strings.TrimRight(line, "\r\n")
		script.mu.Lock()
		script.commands = append(script.commands, line)
		script.mu.Unlock()

		tag, command, _ := strings.Cut(line, " ")
		var response strings.Builder
		switch {
		case strings.HasPrefix(strings.ToUpper(command), "UID FETCH "):
			for index, fetch := range fetches {
				response.WriteString(fetch.line(index + 1))
			}
			response.WriteString(tag + " OK FETCH completed\r\n")
		case strings.EqualFold(command, "LOGOUT"):
			response.WriteString("* BYE closing\r\n" + tag + " OK LOGOUT completed\r\n")
		default:
			response.WriteString(tag + " BAD unsupported by script\r\n")
		}
		if _, err := connection.Write([]byte(response.String())); err != nil {
			return
		}
	}
}

// scriptedMetadataSession uses the real go-imap metadata FETCH parser while
// stubbing mailbox selection and search, which are outside this ticket.
type scriptedMetadataSession struct {
	*imapSession
	uids []uint32
}

func (session *scriptedMetadataSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return mailboxSnapshot{UIDValidity: 9001, UIDNext: 103}, nil
}

func (session *scriptedMetadataSession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, _ int) ([]uint32, error) {
	var uids []uint32
	for _, uid := range session.uids {
		if uid >= window.Start && uid <= window.End {
			uids = append(uids, uid)
		}
	}

	return uids, nil
}

func startScriptedMetadataSession(t *testing.T, fetches []scriptedFetch) (*scriptedMetadataSession, *metadataScript) {
	t.Helper()

	client, server := net.Pipe()
	script := &metadataScript{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		script.serve(server, fetches)
	}()

	session, err := newIMAPSessionOver(context.Background(), client, false)
	if err != nil {
		t.Fatalf("newIMAPSessionOver: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Abort()
		<-done
	})

	uids := make([]uint32, 0, len(fetches))
	for _, fetch := range fetches {
		uids = append(uids, fetch.uid)
	}

	return &scriptedMetadataSession{imapSession: session, uids: uids}, script
}

func newScriptedMetadataAdapter(session readSession) *Adapter {
	adapter := &Adapter{
		config: ValidatedConfig{
			IMAP:   IMAPConfig{CommandTimeoutMs: 5000},
			Bounds: Bounds{MaxSearchResults: 10, MaxHeaderBytes: defaultMaxHeaderBytes, MaxOutputBytes: defaultMaxOutputBytes},
		},
		factory: func(context.Context) (readSession, error) { return session, nil },
		gate:    make(chan struct{}, 1),
	}
	adapter.gate <- struct{}{}

	return adapter
}

func metadataHeader(fields ...string) string {
	return strings.Join(fields, "\r\n") + "\r\n\r\n"
}

func fetchScriptedMetadata(t *testing.T, fetches []scriptedFetch) ([]MessageMetadata, error) {
	t.Helper()

	session, _ := startScriptedMetadataSession(t, fetches)

	return session.UIDFetchMetadata(context.Background(), "INBOX", session.uids, defaultMaxHeaderBytes)
}

func fetchOneSerialized(t *testing.T, name string, fetch scriptedFetch) sourcedMetadata {
	t.Helper()

	results, err := fetchScriptedMetadata(t, []scriptedFetch{fetch})
	if err != nil || len(results) != 1 {
		t.Fatalf("%s: UIDFetchMetadata = %d results, %v", name, len(results), err)
	}

	return serializedMetadata(t, results[0])
}

func TestSearchMetadataAdapter(t *testing.T) {
	t.Run("sourced_fields", func(t *testing.T) {
		fetches := []scriptedFetch{
			{uid: 101, flags: ` FLAGS (\Seen)`, header: metadataHeader(
				"From: Seen Sender <seen@croton.test>",
				"Date: Tue, 03 Nov 2026 09:15:00 -0800",
				"Subject: First sourced",
			)},
			{uid: 102, flags: ` FLAGS (\Flagged)`, header: metadataHeader(
				"From: =?utf-8?q?R=C3=A9sum=C3=A9_Sender?= <resume@croton.test>",
				"Date: Wed, 04 Nov 2026 23:30:00 +0100",
				"Subject: Second sourced",
			)},
		}
		want := map[uint32]struct {
			subject, from, date, readState string
		}{
			101: {subject: "First sourced", from: "Seen Sender <seen@croton.test>", date: "2026-11-03T17:15:00Z", readState: "read"},
			102: {subject: "Second sourced", from: "Résumé Sender <resume@croton.test>", date: "2026-11-04T22:30:00Z", readState: "unread"},
		}

		session, script := startScriptedMetadataSession(t, fetches)
		adapter := newScriptedMetadataAdapter(session)
		page, err := adapter.SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
		if err != nil {
			t.Fatalf("SearchMailPage: %v", err)
		}
		if len(page.Messages) != 2 {
			t.Fatalf("SearchMailPage messages = %d, want 2", len(page.Messages))
		}

		for _, message := range page.Messages {
			got := serializedMetadata(t, message)
			expected, ok := want[got.UID]
			if !ok {
				t.Fatalf("unexpected UID %d", got.UID)
			}

			payload, err := adapter.decodeMessageID(got.ID)
			if err != nil {
				t.Fatalf("decode opaque ID for UID %d: %v", got.UID, err)
			}
			if payload != (messageIDPayload{Mailbox: "INBOX", UIDValidity: 9001, UID: got.UID}) || got.UIDValidity != 9001 || got.Mailbox != "INBOX" {
				t.Fatalf("UID %d identity = %+v bound %+v", got.UID, got, payload)
			}

			if got.Subject != expected.subject || *got.From != expected.from || *got.Date != expected.date || *got.DateSource != "header" || *got.ReadState != expected.readState || *got.MetadataTruncated {
				t.Fatalf("UID %d sourced metadata = subject %q from %q date %q source %q read %q truncated %t, want %+v",
					got.UID, got.Subject, *got.From, *got.Date, *got.DateSource, *got.ReadState, *got.MetadataTruncated, expected)
			}
			if len(*got.From) > senderByteBound || !utf8.ValidString(*got.From) {
				t.Fatalf("UID %d From is not bounded valid UTF-8: %q", got.UID, *got.From)
			}
		}

		var fetchCommand string
		for _, command := range script.recorded() {
			if strings.Contains(strings.ToUpper(command), "UID FETCH") {
				fetchCommand = strings.ToUpper(command)
			}
		}
		if !strings.Contains(fetchCommand, " FLAGS") || !strings.Contains(fetchCommand, "BODY.PEEK[HEADER]") || strings.Contains(fetchCommand, "INTERNALDATE") || strings.Contains(strings.ReplaceAll(fetchCommand, "BODY.PEEK[", ""), "BODY[") {
			t.Fatalf("metadata FETCH = %q, want FLAGS and BODY.PEEK[HEADER] only", fetchCommand)
		}
	})

	t.Run("unknown_and_duplicate", func(t *testing.T) {
		cases := []struct {
			name       string
			fetch      scriptedFetch
			date       string
			dateSource string
			readState  string
		}{
			{
				name:       "absent date and absent flags",
				fetch:      scriptedFetch{uid: 101, header: metadataHeader("From: A <a@croton.test>", "Subject: no date")},
				dateSource: "unknown",
				readState:  "unknown",
			},
			{
				name:       "invalid date and present empty flags",
				fetch:      scriptedFetch{uid: 101, flags: " FLAGS ()", header: metadataHeader("From: A <a@croton.test>", "Date: yesterday-ish", "Subject: bad date")},
				dateSource: "unknown",
				readState:  "unread",
			},
			{
				name:       "repeated date and non-seen flags",
				fetch:      scriptedFetch{uid: 101, flags: ` FLAGS (\Answered $Label)`, header: metadataHeader("Date: Tue, 03 Nov 2026 09:15:00 +0000", "Date: Wed, 04 Nov 2026 09:15:00 +0000")},
				dateSource: "unknown",
				readState:  "unread",
			},
			{
				name:       "valid date and seen flags",
				fetch:      scriptedFetch{uid: 101, flags: ` FLAGS (\Seen \Answered)`, header: metadataHeader("Date: 3 Nov 2026 09:15:00 +0000")},
				date:       "2026-11-03T09:15:00Z",
				dateSource: "header",
				readState:  "read",
			},
		}

		for _, test := range cases {
			got := fetchOneSerialized(t, test.name, test.fetch)
			if *got.Date != test.date || *got.DateSource != test.dateSource || *got.ReadState != test.readState {
				t.Fatalf("%s: date %q source %q read %q, want %q %q %q", test.name, *got.Date, *got.DateSource, *got.ReadState, test.date, test.dateSource, test.readState)
			}
		}

		// The zero value never claims unread or a header date.
		zero := serializedMetadata(t, MessageMetadata{})
		if *zero.ReadState != "unknown" || *zero.DateSource != "unknown" || *zero.Date != "" || *zero.From != "" {
			t.Fatalf("zero metadata = %+v, want explicit unknown state", zero)
		}

		duplicate := scriptedFetch{uid: 101, flags: ` FLAGS (\Seen) FLAGS (\Seen)`, header: metadataHeader("From: Leaky <leak-marker@croton.test>", "Subject: leak-marker")}
		results, err := fetchScriptedMetadata(t, []scriptedFetch{duplicate})
		if CodeOf(err) != CodeIMAPProtocol || results != nil {
			t.Fatalf("duplicate FLAGS = %#v, %v, want nil and %q", results, err, CodeIMAPProtocol)
		}
		if err.Error() != CodeIMAPProtocol {
			t.Fatalf("duplicate FLAGS error text = %q, want code only", err.Error())
		}
	})

	t.Run("sender_bounds", func(t *testing.T) {
		multibyte := strings.Repeat("a", senderByteBound-1) + "é"
		expanding := "=?iso-8859-1?b?" + base64.StdEncoding.EncodeToString([]byte(strings.Repeat("\xe9", 360))) + "?="
		cases := []struct {
			name      string
			from      string
			want      string
			truncated bool
		}{
			{name: "512 bytes", from: strings.Repeat("a", senderByteBound), want: strings.Repeat("a", senderByteBound)},
			{name: "513 bytes", from: strings.Repeat("a", senderByteBound+1), want: strings.Repeat("a", senderByteBound), truncated: true},
			{name: "rune crossing boundary", from: multibyte, want: strings.Repeat("a", senderByteBound-1), truncated: true},
			{name: "decoded expansion", from: expanding, want: strings.Repeat("é", senderByteBound/2), truncated: true},
			{name: "far beyond cap", from: strings.Repeat("b", 10<<10), want: strings.Repeat("b", senderByteBound), truncated: true},
			{name: "invalid UTF-8", from: "Bad \xff\xfe <bad@croton.test>"},
			{name: "malformed encoded word", from: "=?utf-8?b?!!not-base64!!?= <bad@croton.test>"},
			{name: "unknown charset", from: "=?x-croton-unknown?q?decoder-marker?= <bad@croton.test>"},
			{name: "encoded control", from: "=?utf-8?q?line=0D=0Abreak?= <bad@croton.test>"},
			{name: "encoded invalid UTF-8", from: "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + "?="},
			{name: "blank", from: "   "},
		}

		for _, test := range cases {
			got := fetchOneSerialized(t, test.name, scriptedFetch{uid: 101, flags: " FLAGS ()", header: metadataHeader("From: "+test.from, "Subject: bounds")})
			if *got.From != test.want || *got.MetadataTruncated != test.truncated {
				t.Fatalf("%s: From %q (%d bytes) truncated %t, want %q truncated %t", test.name, *got.From, len(*got.From), *got.MetadataTruncated, test.want, test.truncated)
			}
			if len(*got.From) > senderByteBound || !utf8.ValidString(*got.From) || strings.Contains(*got.From, "marker") {
				t.Fatalf("%s: From %q is not bounded, valid and decoder-free", test.name, *got.From)
			}
		}

		// Missing and repeated From are omitted, not fabricated.
		for name, header := range map[string]string{
			"missing":  metadataHeader("Subject: no sender"),
			"repeated": metadataHeader("From: One <one@croton.test>", "From: Two <two@croton.test>"),
		} {
			if got := fetchOneSerialized(t, name, scriptedFetch{uid: 101, flags: " FLAGS ()", header: header}); *got.From != "" || *got.MetadataTruncated {
				t.Fatalf("%s From = %q truncated %t, want omitted", name, *got.From, *got.MetadataTruncated)
			}
		}

		// Header, aggregate and literal limits are unchanged, and an oversized
		// header literal is still refused instead of looking complete.
		if defaultMaxHeaderBytes != 16<<10 || maximumMaxHeaderBytes != 64<<10 || defaultMaxOutputBytes != 100000 || hardMaxOutputBytes != 400000 {
			t.Fatal("header or aggregate limits changed")
		}
		oversized := scriptedFetch{uid: 101, flags: " FLAGS ()", header: metadataHeader("From: "+strings.Repeat("c", defaultMaxHeaderBytes), "Subject: oversized")}
		results, err := fetchScriptedMetadata(t, []scriptedFetch{oversized})
		if CodeOf(err) != CodeBoundsExceeded || results != nil {
			t.Fatalf("oversized header literal = %d results, %v, want %q", len(results), err, CodeBoundsExceeded)
		}
	})
}

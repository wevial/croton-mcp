package bridge

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
)

// senderByteBound is the From cap, stated independently of the production
// constant.
const senderByteBound = 512

// sourcedMetadata is the serialized view of the sourced metadata fields, so
// assertions inspect what a caller would marshal rather than private state.
type sourcedMetadata struct {
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

// scriptedFetch is the canned untagged FETCH response for scriptedUID. Flags
// is the raw item text including its leading space.
type scriptedFetch struct {
	flags  string
	header string
}

const scriptedUID = 101

// serveMetadataScript is a closed, in-process IMAP peer. It answers only the
// greeting, UID FETCH and LOGOUT.
func serveMetadataScript(connection net.Conn, fetch scriptedFetch) {
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

		tag, command, _ := strings.Cut(strings.TrimRight(line, "\r\n"), " ")
		var response string
		switch {
		case strings.HasPrefix(strings.ToUpper(command), "UID FETCH "):
			response = fmt.Sprintf("* 1 FETCH (UID %d%s RFC822.SIZE %d BODY[HEADER] {%d}\r\n%s)\r\n",
				scriptedUID, fetch.flags, len(fetch.header)+16, len(fetch.header), fetch.header) + tag + " OK FETCH completed\r\n"
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

// scriptedMetadataSession uses the real go-imap metadata FETCH parser while
// stubbing mailbox selection and search.
type scriptedMetadataSession struct {
	*imapSession
}

func (session *scriptedMetadataSession) Examine(context.Context, string) (mailboxSnapshot, error) {
	return mailboxSnapshot{UIDValidity: 9001, UIDNext: scriptedUID + 1}, nil
}

func (session *scriptedMetadataSession) UIDSearchWindow(_ context.Context, _ SearchQuery, window uidWindow, _ int) ([]uint32, error) {
	if scriptedUID < window.Start || scriptedUID > window.End {
		return nil, nil
	}

	return []uint32{scriptedUID}, nil
}

func startScriptedMetadataSession(t *testing.T, fetch scriptedFetch) *scriptedMetadataSession {
	t.Helper()

	client, server := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveMetadataScript(server, fetch)
	}()

	session, err := newIMAPSessionOver(context.Background(), client, false)
	if err != nil {
		t.Fatalf("newIMAPSessionOver: %v", err)
	}
	t.Cleanup(func() {
		_ = session.Abort()
		<-done
	})

	return &scriptedMetadataSession{imapSession: session}
}

func metadataHeader(fields ...string) string {
	return strings.Join(fields, "\r\n") + "\r\n\r\n"
}

func fetchScriptedMetadata(t *testing.T, fetch scriptedFetch) ([]MessageMetadata, error) {
	t.Helper()

	session := startScriptedMetadataSession(t, fetch)

	return session.UIDFetchMetadata(context.Background(), "INBOX", []uint32{scriptedUID}, defaultMaxHeaderBytes)
}

func fetchOneSerialized(t *testing.T, name string, fetch scriptedFetch) sourcedMetadata {
	t.Helper()

	results, err := fetchScriptedMetadata(t, fetch)
	if err != nil || len(results) != 1 {
		t.Fatalf("%s: UIDFetchMetadata = %d results, %v", name, len(results), err)
	}

	return serializedMetadata(t, results[0])
}

func TestSearchMetadataAdapter(t *testing.T) {
	t.Run("sourced_fields", func(t *testing.T) {
		session := startScriptedMetadataSession(t, scriptedFetch{flags: ` FLAGS (\Flagged)`, header: metadataHeader(
			"From: =?utf-8?q?R=C3=A9sum=C3=A9_Sender?= <resume@croton.test>",
			"Date: Wed, 04 Nov 2026 23:30:00 +0100",
		)})
		page, err := newBoundedSearchAdapter(session, defaultMaxOutputBytes).SearchMailPage(context.Background(), SearchQuery{Mailbox: "INBOX"})
		if err != nil || len(page.Messages) != 1 {
			t.Fatalf("SearchMailPage = %d messages, %v, want 1", len(page.Messages), err)
		}

		got := serializedMetadata(t, page.Messages[0])
		if *got.From != "Résumé Sender <resume@croton.test>" || *got.Date != "2026-11-04T22:30:00Z" || *got.DateSource != "header" || *got.ReadState != "unread" || *got.MetadataTruncated {
			t.Fatalf("sourced metadata = from %q date %q source %q read %q truncated %t",
				*got.From, *got.Date, *got.DateSource, *got.ReadState, *got.MetadataTruncated)
		}

		zones := []struct {
			date       string
			want       string
			dateSource string
		}{
			{date: "Thu, 05 Nov 2026 10:00:00 PST", want: "2026-11-05T18:00:00Z", dateSource: "header"},
			{date: "Thu, 05 Nov 2026 10:00:00 EDT", want: "2026-11-05T14:00:00Z", dateSource: "header"},
			{date: "Thu, 05 Nov 2026 10:00:00 GMT", want: "2026-11-05T10:00:00Z", dateSource: "header"},
			{date: "Thu, 05 Nov 2026 10:00:00 Z", dateSource: "unknown"},
		}

		for _, zone := range zones {
			got := fetchOneSerialized(t, zone.date, scriptedFetch{flags: " FLAGS ()", header: metadataHeader("Date: " + zone.date)})
			if *got.Date != zone.want || *got.DateSource != zone.dateSource {
				t.Fatalf("%s: date %q source %q, want %q %q", zone.date, *got.Date, *got.DateSource, zone.want, zone.dateSource)
			}
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
				name:       "invalid date and present empty flags",
				fetch:      scriptedFetch{flags: " FLAGS ()", header: metadataHeader("From: A <a@croton.test>", "Date: yesterday-ish", "Subject: bad date")},
				dateSource: "unknown",
				readState:  "unread",
			},
			{
				name:       "named PDT zone with comment",
				fetch:      scriptedFetch{flags: " FLAGS ()", header: metadataHeader("Date: Sun, 01 Nov 2026 01:30:00 PDT (Pacific-Daylight)")},
				date:       "2026-11-01T08:30:00Z",
				dateSource: "header",
				readState:  "unread",
			},
			{
				name:       "named zone with unterminated comment",
				fetch:      scriptedFetch{flags: " FLAGS ()", header: metadataHeader("Date: Thu, 05 Nov 2026 10:00:00 PST (unterminated")},
				dateSource: "unknown",
				readState:  "unread",
			},
			{
				name:       "numeric zone with named comment",
				fetch:      scriptedFetch{flags: " FLAGS ()", header: metadataHeader("Date: Tue, 03 Nov 2026 09:15:00 -0800 (PST)")},
				date:       "2026-11-03T17:15:00Z",
				dateSource: "header",
				readState:  "unread",
			},
			{
				name:       "repeated date and non-seen flags",
				fetch:      scriptedFetch{flags: ` FLAGS (\Answered $Label)`, header: metadataHeader("Date: Tue, 03 Nov 2026 09:15:00 +0000", "Date: Wed, 04 Nov 2026 09:15:00 +0000")},
				dateSource: "unknown",
				readState:  "unread",
			},
		}

		for _, test := range cases {
			got := fetchOneSerialized(t, test.name, test.fetch)
			if *got.Date != test.date || *got.DateSource != test.dateSource || *got.ReadState != test.readState {
				t.Fatalf("%s: date %q source %q read %q, want %q %q %q", test.name, *got.Date, *got.DateSource, *got.ReadState, test.date, test.dateSource, test.readState)
			}
		}
	})

	t.Run("sender_bounds", func(t *testing.T) {
		multibyte := strings.Repeat("a", senderByteBound-1) + "é"
		manyWords := strings.TrimSuffix(strings.Repeat("=?utf-8?q?"+strings.Repeat("a", 50)+"?= ", 9), " ") + " <a@croton.test>"
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
			{name: "encoded words beyond raw cap", from: manyWords, want: strings.Repeat("a", 450) + " <a@croton.test>"},
			{name: "malformed Q escape", from: "=?utf-8?q?bad=GG?= <a@croton.test>"},
			{name: "truncated Q escape", from: "=?utf-8?q?bad=4?= <a@croton.test>"},
			{name: "unterminated encoded word", from: "=?utf-8?q?decoder-marker"},
			{name: "unknown encoding", from: "=?utf-8?x?decoder-marker?="},
			{name: "empty charset", from: "=??q?decoder-marker?= <a@croton.test>"},
			{name: "decodes to encoded-word text", from: "=?utf-8?q?=3D=3Futf-8=3Fq=3Fx=3F=3D?= <a@croton.test>"},
			{name: "invalid UTF-8", from: "Bad \xff\xfe <bad@croton.test>"},
			{name: "malformed encoded word", from: "=?utf-8?b?!!not-base64!!?= <bad@croton.test>"},
			{name: "unknown charset", from: "=?x-croton-unknown?q?decoder-marker?= <bad@croton.test>"},
			{name: "encoded control", from: "=?utf-8?q?line=0D=0Abreak?= <bad@croton.test>"},
			{name: "encoded invalid UTF-8", from: "=?utf-8?b?" + base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe}) + "?="},
		}

		for _, test := range cases {
			got := fetchOneSerialized(t, test.name, scriptedFetch{flags: " FLAGS ()", header: metadataHeader("From: "+test.from, "Subject: bounds")})
			if *got.From != test.want || *got.MetadataTruncated != test.truncated {
				t.Fatalf("%s: From %q (%d bytes) truncated %t, want %q truncated %t", test.name, *got.From, len(*got.From), *got.MetadataTruncated, test.want, test.truncated)
			}
		}

		// Missing and repeated From are omitted, not fabricated.
		for name, header := range map[string]string{
			"missing":  metadataHeader("Subject: no sender"),
			"repeated": metadataHeader("From: One <one@croton.test>", "From: Two <two@croton.test>"),
		} {
			if got := fetchOneSerialized(t, name, scriptedFetch{flags: " FLAGS ()", header: header}); *got.From != "" || *got.MetadataTruncated {
				t.Fatalf("%s From = %q truncated %t, want omitted", name, *got.From, *got.MetadataTruncated)
			}
		}

		// Header, aggregate and literal limits are unchanged, and an oversized
		// header literal is still refused instead of looking complete.
		if defaultMaxHeaderBytes != 16<<10 || maximumMaxHeaderBytes != 64<<10 || defaultMaxOutputBytes != 100000 || hardMaxOutputBytes != 400000 {
			t.Fatal("header or aggregate limits changed")
		}
		oversized := scriptedFetch{flags: " FLAGS ()", header: metadataHeader("From: "+strings.Repeat("c", defaultMaxHeaderBytes), "Subject: oversized")}
		results, err := fetchScriptedMetadata(t, oversized)
		if CodeOf(err) != CodeBoundsExceeded || results != nil {
			t.Fatalf("oversized header literal = %d results, %v, want %q", len(results), err, CodeBoundsExceeded)
		}
	})
}

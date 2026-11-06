package bridge

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// threadFiller returns count ordinary generic header fields.
func threadFiller(count int) string {
	var generic strings.Builder
	for index := range count {
		fmt.Fprintf(&generic, "X-Filler-%03d: %d\r\n", index, index)
	}

	return generic.String()
}

// threadReferences returns a References field naming count synthetic parents.
func threadReferences(count int) string {
	identifiers := make([]string, 0, count)
	for index := range count {
		identifiers = append(identifiers, fmt.Sprintf("<parent-%03d@example.test>", index))
	}

	return "References: " + strings.Join(identifiers, " ") + "\r\n"
}

func requireMalformedHeader(t *testing.T, name string, result MessageHeaders, err error) {
	t.Helper()

	var normalizeError *NormalizeError
	if !errors.As(err, &normalizeError) || normalizeError.Code != errorCodeMalformedHeader || err.Error() != errorCodeMalformedHeader {
		t.Fatalf("%s: error %v, want code-only %q", name, err, errorCodeMalformedHeader)
	}
	if result.Metadata != (MessageMetadata{}) || result.Headers.Subject != "" || result.Truncation != (Truncation{}) {
		t.Fatalf("%s: refused read returned %+v", name, result)
	}
}

func TestSelectedThreadFields(t *testing.T) {
	t.Run("late_fields", func(t *testing.T) {
		threading := "Subject: Late thread\r\n" +
			"Message-ID: <reply@example.test>\r\n" +
			"In-Reply-To: <root@example.test>\r\n" +
			"References: <root@example.test>\r\n"

		early, err := selectedHeaderRecord(MessageMetadata{}, []byte(threading+threadFiller(100)+"\r\n"), headerTestLimit*4)
		if err != nil {
			t.Fatalf("early selectedHeaderRecord: %v", err)
		}
		if early.Metadata.Subject != "Late thread" || early.Headers.MessageID == "" {
			t.Fatalf("early control lost threading fields: %+v", early)
		}

		late, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+threading+"\r\n"), headerTestLimit*4)
		if err != nil {
			t.Fatalf("late selectedHeaderRecord: %v", err)
		}

		if late.Metadata.Subject != early.Metadata.Subject {
			t.Errorf("Metadata.Subject = %q, want %q", late.Metadata.Subject, early.Metadata.Subject)
		}
		if late.Headers.Subject != early.Headers.Subject || late.Headers.MessageID != early.Headers.MessageID || late.Headers.InReplyTo != early.Headers.InReplyTo || strings.Join(late.Headers.References, " ") != strings.Join(early.Headers.References, " ") {
			t.Errorf("late threading headers = %+v, want %+v", late.Headers, early.Headers)
		}
		if !late.Truncation.HeaderCount {
			t.Error("HeaderCount truncation was not reported for omitted generic fields")
		}
	})

	t.Run("late_folded_case_insensitive", func(t *testing.T) {
		threading := "sUbJeCt: Folded\r\n late subject\r\n" +
			"MESSAGE-ID:\r\n <reply@example.test>\r\n" +
			"in-reply-to: <root@example.test>\r\n" +
			"REFERENCES: <root@example.test>\r\n\t<middle@example.test>\r\n"
		for _, position := range []int{0, 99, 100, 150} {
			header := threadFiller(position) + threading + threadFiller(120-position) + "\r\n"
			result, err := selectedHeaderRecord(MessageMetadata{}, []byte(header), headerTestLimit*4)
			if err != nil {
				t.Fatalf("position %d: %v", position, err)
			}

			if result.Metadata.Subject != "Folded late subject" || result.Headers.Subject != "Folded late subject" {
				t.Errorf("position %d: subject metadata %q canonical %q", position, result.Metadata.Subject, result.Headers.Subject)
			}
			if result.Headers.MessageID != "<reply@example.test>" || result.Headers.InReplyTo != "<root@example.test>" || strings.Join(result.Headers.References, " ") != "<root@example.test> <middle@example.test>" {
				t.Errorf("position %d: headers %+v", position, result.Headers)
			}
			if !result.Truncation.HeaderCount || result.Truncation.Headers || result.Truncation.References {
				t.Errorf("position %d: truncation %+v", position, result.Truncation)
			}
		}
	})

	t.Run("limits_and_compatibility", func(t *testing.T) {
		limit := headerTestLimit * 4
		subject := "Subject: =?utf-8?q?Caf=C3=A9?= thread\r\n"

		exact, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(99)+subject+"\r\n"), limit)
		if err != nil || exact.Truncation.HeaderCount || exact.Metadata.Subject != "=?utf-8?q?Caf=C3=A9?= thread" || exact.Headers.Subject != "Café thread" {
			t.Fatalf("100 fields: %+v, %v", exact, err)
		}

		over, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+subject+"\r\n"), limit)
		if err != nil || !over.Truncation.HeaderCount || over.Metadata.Subject != exact.Metadata.Subject || over.Headers.Subject != exact.Headers.Subject {
			t.Fatalf("101 fields: %+v, %v", over, err)
		}

		fifty, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+threadReferences(50)+"\r\n"), limit)
		if err != nil || len(fifty.Headers.References) != 50 || fifty.Truncation.References || !fifty.Truncation.HeaderCount {
			t.Fatalf("50 late references: %+v, %v", fifty.Truncation, err)
		}

		fiftyOne, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+threadReferences(51)+"\r\n"), limit)
		if err != nil || len(fiftyOne.Headers.References) != 50 || fiftyOne.Headers.References[0] != "<parent-001@example.test>" || !fiftyOne.Truncation.References || !fiftyOne.Truncation.HeaderCount {
			t.Fatalf("51 late references: %+v, %v", fiftyOne.Truncation, err)
		}

		longSubject := "Subject: " + strings.Repeat("s", headerTestLimit+10) + "\r\n"
		long, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+longSubject+"\r\n"), headerTestLimit)
		if err != nil || len(long.Headers.Subject) != headerTestLimit || !long.Truncation.Headers || !long.Truncation.HeaderCount {
			t.Fatalf("over-limit late subject: %d bytes, %+v, %v", len(long.Headers.Subject), long.Truncation, err)
		}

		atLimit := threadFiller(100) + "Subject: bounded\r\n"
		atLimit += "X-Pad: " + strings.Repeat("p", headerTestLimit-len(atLimit)-len("X-Pad: \r\n\r\n")) + "\r\n\r\n"
		bounded, err := selectedHeaderRecord(MessageMetadata{}, []byte(atLimit), headerTestLimit)
		if err != nil || len(atLimit) != headerTestLimit || bounded.Metadata.Subject != "bounded" || bounded.Truncation.Headers || !bounded.Truncation.HeaderCount {
			t.Fatalf("complete header at byte limit: %d bytes, %+v, %v", len(atLimit), bounded, err)
		}

		duplicates := threadFiller(50) + "Subject: first\r\nMessage-ID: <first@example.test>\r\n" + threadFiller(60) + "Subject: second\r\nMessage-ID: <second@example.test>\r\nIn-Reply-To: <late-first@example.test>\r\nIn-Reply-To: <late-second@example.test>\r\n\r\n"
		duplicate, err := selectedHeaderRecord(MessageMetadata{}, []byte(duplicates), limit)
		if err != nil || duplicate.Metadata.Subject != "first" || duplicate.Headers.Subject != "first" || duplicate.Headers.MessageID != "<first@example.test>" || duplicate.Headers.InReplyTo != "<late-first@example.test>" {
			t.Fatalf("duplicate fields: %+v, %v", duplicate, err)
		}

		absent, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(101)+"Message-ID: <alone@example.test>\r\n\r\n"), limit)
		if err != nil || absent.Metadata.Subject != "" || absent.Headers.Subject != "" || absent.Headers.MessageID != "<alone@example.test>" || !absent.Truncation.HeaderCount {
			t.Fatalf("absent subject: %+v, %v", absent, err)
		}

		retainedMalformed, err := selectedHeaderRecord(MessageMetadata{}, []byte("Subject: =?utf-8?B?***?=\r\n"+threadFiller(100)+"\r\n"), limit)
		requireMalformedHeader(t, "retained malformed subject", retainedMalformed, err)

		lateMalformed, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+"Subject: =?utf-8?B?***?=\r\n\r\n"), limit)
		requireMalformedHeader(t, "late malformed subject", lateMalformed, err)

		for name, header := range map[string]string{
			"missing terminator":    threadFiller(101) + "Subject: open\r\n",
			"data after terminator": threadFiller(101) + "Subject: body\r\n\r\nbody",
			"retained no colon":     "Not a field\r\n" + threadFiller(101) + "Subject: late\r\n\r\n",
		} {
			result, err := selectedHeaderRecord(MessageMetadata{}, []byte(header), limit)
			if CodeOf(err) != CodeIMAPProtocol || err.Error() != CodeIMAPProtocol || result.Metadata != (MessageMetadata{}) {
				t.Errorf("%s: %+v, %v", name, result, err)
			}
		}

		mixed := "X-Early: one\r\n" + threadFiller(100) + "X-Late: =?bogus\r\n continued\r\nreferences: <a@example.test>\r\nSubject: kept\r\nSubject: dropped\r\nX-Other: two\r\n\r\n"
		block := string(threadFieldBlock([]byte(mixed)))
		if block != "Subject: kept\r\nreferences: <a@example.test>\r\n\r\n" || len(block) > len(mixed) {
			t.Fatalf("thread field block = %q", block)
		}
	})
}

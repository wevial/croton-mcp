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

		sender := "From: Sender <sender@example.test>\r\nDate: Tue, 03 Nov 2026 09:15:00 -0800\r\n"
		mixed, err := selectedHeaderRecord(MessageMetadata{}, []byte(sender+threadFiller(98)+threading+"\r\n"), headerTestLimit*4)
		if err != nil {
			t.Fatalf("mixed selectedHeaderRecord: %v", err)
		}

		if mixed.Metadata.From != "Sender <sender@example.test>" || mixed.Metadata.Date != "2026-11-03T17:15:00Z" || mixed.Metadata.DateSource != DateSourceHeader {
			t.Errorf("early From/Date metadata = %+v, want values from the bounded header", mixed.Metadata)
		}
		if mixed.Headers.From != "Sender <sender@example.test>" || mixed.Headers.Date != "Tue, 03 Nov 2026 09:15:00 -0800" {
			t.Errorf("early From/Date headers = %+v, want values from the bounded header", mixed.Headers)
		}
		if mixed.Metadata.Subject != early.Metadata.Subject || mixed.Headers.InReplyTo != early.Headers.InReplyTo || !mixed.Truncation.HeaderCount {
			t.Errorf("mixed record = %+v, want late threading fields with HeaderCount", mixed)
		}
	})

	t.Run("late_folded_case_insensitive", func(t *testing.T) {
		threading := "sUbJeCt: Folded\r\n late subject\r\n" +
			"MESSAGE-ID:\r\n <reply@example.test>\r\n" +
			"in-reply-to: <root@example.test>\r\n" +
			"REFERENCES: <root@example.test>\r\n\t<middle@example.test>\r\n"
		result, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+threading+"\r\n"), headerTestLimit*4)
		if err != nil {
			t.Fatalf("selectedHeaderRecord: %v", err)
		}

		if result.Metadata.Subject != "Folded late subject" || result.Headers.Subject != "Folded late subject" {
			t.Errorf("subject metadata %q canonical %q", result.Metadata.Subject, result.Headers.Subject)
		}
		if result.Headers.MessageID != "<reply@example.test>" || result.Headers.InReplyTo != "<root@example.test>" || strings.Join(result.Headers.References, " ") != "<root@example.test> <middle@example.test>" {
			t.Errorf("headers %+v", result.Headers)
		}
		if !result.Truncation.HeaderCount || result.Truncation.Headers || result.Truncation.References {
			t.Errorf("truncation %+v", result.Truncation)
		}
	})

	t.Run("limits_and_compatibility", func(t *testing.T) {
		limit := headerTestLimit * 4

		over, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+"Subject: =?utf-8?q?Caf=C3=A9?= thread\r\n\r\n"), limit)
		if err != nil || !over.Truncation.HeaderCount || over.Metadata.Subject != "=?utf-8?q?Caf=C3=A9?= thread" || over.Headers.Subject != "Café thread" {
			t.Fatalf("101 fields: %+v, %v", over, err)
		}

		identifiers := make([]string, 0, 51)
		for index := range 51 {
			identifiers = append(identifiers, fmt.Sprintf("<parent-%03d@example.test>", index))
		}
		fiftyOne, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+"References: "+strings.Join(identifiers, " ")+"\r\n\r\n"), limit)
		if err != nil || len(fiftyOne.Headers.References) != 50 || fiftyOne.Headers.References[0] != "<parent-001@example.test>" || !fiftyOne.Truncation.References || !fiftyOne.Truncation.HeaderCount {
			t.Fatalf("51 late references: %+v, %v", fiftyOne.Truncation, err)
		}

		longSubject := "Subject: " + strings.Repeat("s", headerTestLimit+10) + "\r\n"
		long, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+longSubject+"\r\n"), headerTestLimit)
		if err != nil || len(long.Headers.Subject) != headerTestLimit || !long.Truncation.Headers || !long.Truncation.HeaderCount {
			t.Fatalf("over-limit late subject: %d bytes, %+v, %v", len(long.Headers.Subject), long.Truncation, err)
		}

		malformed, err := selectedHeaderRecord(MessageMetadata{}, []byte(threadFiller(100)+"Subject: =?utf-8?B?***?=\r\n\r\n"), limit)
		var normalizeError *NormalizeError
		if !errors.As(err, &normalizeError) || normalizeError.Code != errorCodeMalformedHeader || err.Error() != errorCodeMalformedHeader {
			t.Fatalf("late malformed subject: error %v, want code-only %q", err, errorCodeMalformedHeader)
		}
		if malformed.Metadata != (MessageMetadata{}) || malformed.Headers.Subject != "" || malformed.Truncation != (Truncation{}) {
			t.Fatalf("late malformed subject: refused read returned %+v", malformed)
		}

		noColon, err := selectedHeaderRecord(MessageMetadata{}, []byte("Not a field\r\n"+threadFiller(101)+"Subject: late\r\n\r\n"), limit)
		if CodeOf(err) != CodeIMAPProtocol || err.Error() != CodeIMAPProtocol || noColon.Metadata != (MessageMetadata{}) {
			t.Fatalf("retained no colon: %+v, %v", noColon, err)
		}

		mixed := "X-Early: one\r\n" + threadFiller(100) + "X-Late: =?bogus\r\n continued\r\nreferences: <a@example.test>\r\nSubject: kept\r\nSubject: dropped\r\nX-Other: two\r\n\r\n"
		block := string(threadFieldBlock([]byte(mixed)))
		if block != "Subject: kept\r\nreferences: <a@example.test>\r\n\r\n" {
			t.Fatalf("thread field block = %q", block)
		}
	})
}

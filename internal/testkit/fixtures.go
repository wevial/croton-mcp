package testkit

import (
	_ "embed"
	"encoding/base64"
	"fmt"
	"time"
)

// Synthetic attachment sentinels make content disclosure observable without live mail.
const (
	SyntheticAttachmentBody     = "SYNTHETIC_MULTIPART_BODY_SENTINEL"
	SyntheticAttachmentDecoded  = "SYNTHETIC_PDF_PAYLOAD_SENTINEL"
	SyntheticAttachmentFreeBody = "SYNTHETIC_ATTACHMENT_FREE_BODY_SENTINEL"
)

// SyntheticAttachmentMessages returns a multipart report and attachment-free mail.
// It is opt-in and does not alter the default fixture messages.
func SyntheticAttachmentMessages() []string {
	headers := "From: Fixture <fixture@croton.test>\r\n" +
		"To: Reader <reader@croton.test>\r\n" +
		"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
		"MIME-Version: 1.0\r\n"

	return []string{
		headers + "Subject: Synthetic attachment report\r\n" +
			"Message-ID: <attachment-report@croton.test>\r\n" +
			"Content-Type: multipart/mixed; boundary=synthetic-report\r\n\r\n" +
			"--synthetic-report\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" +
			SyntheticAttachmentBody + "\r\n" +
			"--synthetic-report\r\nContent-Type: application/pdf; name=report.pdf\r\n" +
			"Content-Disposition: attachment; filename=report.pdf\r\n" +
			"Content-Transfer-Encoding: base64\r\n\r\n" +
			base64.StdEncoding.EncodeToString([]byte(SyntheticAttachmentDecoded)) + "\r\n" +
			"--synthetic-report--\r\n",
		headers + "Subject: Synthetic attachment-free message\r\n" +
			"Message-ID: <attachment-free@croton.test>\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" +
			SyntheticAttachmentFreeBody + "\r\n",
	}
}

var (
	//go:embed testdata/welcome.eml
	welcomeMIME string

	//go:embed testdata/status-report.eml
	statusReportMIME string
)

// SyntheticMIMESeeds returns only invented MIME messages addressed within reserved .test domains.
func SyntheticMIMESeeds() []string {
	return []string{welcomeMIME, statusReportMIME}
}

// SyntheticLinkedThread returns a root, its reply, and unrelated same-subject
// mail. All identities and content are invented; UIDs are assigned from 101.
func SyntheticLinkedThread() []string {
	messages := make([]string, 0, 3)
	for index, name := range []string{"root", "reply", "unrelated"} {
		subject, references := "Synthetic planning", ""
		if name == "reply" {
			subject = "Re: Synthetic planning"
			references = "References: <root@croton.test>\r\n"
		}

		messages = append(messages, fmt.Sprintf("From: Fixture <fixture@croton.test>\r\n"+
			"To: Reader <reader@croton.test>\r\nSubject: %s\r\n"+
			"Date: Thu, 01 Jan 2026 00:00:0%d +0000\r\n"+
			"Message-ID: <%s@croton.test>\r\n%s\r\nSynthetic %s message.\r\n",
			subject, index, name, references, name))
	}

	return messages
}

// Synthetic digest sentinels are distinct from all header metadata.
const (
	SyntheticDigestUnreadFirstBody  = "SYNTHETIC_DIGEST_UNREAD_FIRST_BODY_SENTINEL"
	SyntheticDigestUnreadSecondBody = "SYNTHETIC_DIGEST_UNREAD_SECOND_BODY_SENTINEL"
	SyntheticDigestReadBody         = "SYNTHETIC_DIGEST_READ_BODY_SENTINEL"
)

// SyntheticDigestMixedMailbox supplies two unread messages and one read message.
// Dates are 24–26 hours old, safely inside a 72-hour digest window even after
// IMAP's day-level date rounding. Existing default fixtures are unaffected.
func SyntheticDigestMixedMailbox(now time.Time) Options {
	options := Options{Seen: []bool{false, false, true}}
	for index, message := range []struct{ subject, body string }{
		{"Synthetic digest unread first", SyntheticDigestUnreadFirstBody},
		{"Synthetic digest unread second", SyntheticDigestUnreadSecondBody},
		{"Synthetic digest read", SyntheticDigestReadBody},
	} {
		date := now.UTC().Add(-time.Duration(24+index) * time.Hour)
		options.Messages = append(options.Messages, fmt.Sprintf("From: Fixture <fixture@croton.test>\r\n"+
			"To: Reader <reader@croton.test>\r\nSubject: %s\r\nDate: %s\r\n"+
			"Message-ID: <digest-%d@croton.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n",
			message.subject, date.Format(time.RFC1123Z), index, message.body))
	}

	return options
}

// SyntheticDigestAllReadMailbox is a separate mailbox with no unread matches.
func SyntheticDigestAllReadMailbox(now time.Time) Options {
	options := SyntheticDigestMixedMailbox(now)
	options.Messages = options.Messages[2:]
	options.Seen = []bool{true}

	return options
}

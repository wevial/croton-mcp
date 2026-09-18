package testkit

import (
	_ "embed"
	"fmt"
)

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

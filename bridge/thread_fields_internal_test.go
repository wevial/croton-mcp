package bridge

import (
	"fmt"
	"strings"
	"testing"
)

func TestSelectedThreadFields(t *testing.T) {
	t.Run("late_fields", func(t *testing.T) {
		threading := "Subject: Late thread\r\n" +
			"Message-ID: <reply@example.test>\r\n" +
			"In-Reply-To: <root@example.test>\r\n" +
			"References: <root@example.test>\r\n"
		var generic strings.Builder
		for index := range 100 {
			fmt.Fprintf(&generic, "X-Filler-%03d: %d\r\n", index, index)
		}

		early, err := selectedHeaderRecord(MessageMetadata{}, []byte(threading+generic.String()+"\r\n"), headerTestLimit*4)
		if err != nil {
			t.Fatalf("early selectedHeaderRecord: %v", err)
		}
		if early.Metadata.Subject != "Late thread" || early.Headers.MessageID == "" {
			t.Fatalf("early control lost threading fields: %+v", early)
		}

		late, err := selectedHeaderRecord(MessageMetadata{}, []byte(generic.String()+threading+"\r\n"), headerTestLimit*4)
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
}

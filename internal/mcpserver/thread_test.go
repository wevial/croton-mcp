package mcpserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

const threadRootMIME = "From: Croton Fixture <fixture@croton.test>\r\n" +
	"To: Reader <reader@croton.test>\r\n" +
	"Subject: Planning\r\n" +
	"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
	"Message-ID: <root@croton.test>\r\n" +
	"\r\n" +
	"Root message.\r\n"

const threadReplyMIME = "From: Reader <reader@croton.test>\r\n" +
	"To: Croton Fixture <fixture@croton.test>\r\n" +
	"Subject: Re: Planning\r\n" +
	"Date: Fri, 02 Jan 2026 00:00:00 +0000\r\n" +
	"Message-ID: <reply@croton.test>\r\n" +
	"In-Reply-To: <root@croton.test>\r\n" +
	"References: <root@croton.test>\r\n" +
	"\r\n" +
	"Reply message.\r\n"

type getThreadDecoded struct {
	ID      string `json:"id"`
	Mailbox string `json:"mailbox"`
	Nodes   []struct {
		Key       string `json:"key"`
		MessageID string `json:"messageId"`
		ParentKey string `json:"parentKey"`
		Depth     int    `json:"depth"`
		Subject   string `json:"subject"`
		From      string `json:"from"`
		Date      string `json:"date"`
	} `json:"nodes"`
	Truncated bool `json:"truncated"`
}

func threadFake() *fakeMail {
	return &fakeMail{
		metadataByID: map[string]bridge.MessageMetadata{
			"id-root":  {ID: "id-root", Mailbox: "INBOX", Subject: "Planning", Size: int64(len(threadRootMIME))},
			"id-reply": {ID: "id-reply", Mailbox: "INBOX", Subject: "Re: Planning", Size: int64(len(threadReplyMIME))},
		},
		bodyByID: map[string][]byte{
			"id-root":  []byte(threadRootMIME),
			"id-reply": []byte(threadReplyMIME),
		},
		searched: []bridge.MessageMetadata{
			{ID: "id-reply", Mailbox: "INBOX", Subject: "Re: Planning"},
			{ID: "id-root", Mailbox: "INBOX", Subject: "Planning"},
		},
	}
}

func TestGetThreadLinksRepliesThroughReferences(t *testing.T) {
	t.Parallel()

	mail := threadFake()
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_thread", map[string]any{"messageId": "id-reply"})

	var decoded getThreadDecoded
	decodeResult(t, result, &decoded)
	if decoded.ID != "id-reply" || decoded.Mailbox != "INBOX" {
		t.Fatalf("unexpected identity: %+v", decoded)
	}
	if len(decoded.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2: %+v", len(decoded.Nodes), decoded.Nodes)
	}
	byKey := map[string]int{}
	for index, node := range decoded.Nodes {
		byKey[node.Key] = index
	}
	rootIndex, ok := byKey["id-root"]
	if !ok {
		t.Fatalf("root node missing: %+v", decoded.Nodes)
	}
	replyIndex, ok := byKey["id-reply"]
	if !ok {
		t.Fatalf("reply node missing: %+v", decoded.Nodes)
	}
	if decoded.Nodes[rootIndex].Depth != 0 || decoded.Nodes[rootIndex].ParentKey != "" {
		t.Fatalf("unexpected root node: %+v", decoded.Nodes[rootIndex])
	}
	if decoded.Nodes[replyIndex].ParentKey != "id-root" || decoded.Nodes[replyIndex].Depth != 1 {
		t.Fatalf("unexpected reply node: %+v", decoded.Nodes[replyIndex])
	}

	// The search must strip the reply prefix so all thread siblings match.
	if len(mail.searchCalls) != 1 || mail.searchCalls[0].Subject != "Planning" || mail.searchCalls[0].Mailbox != "INBOX" {
		t.Fatalf("unexpected sibling search: %+v", mail.searchCalls)
	}

	// The thread must not include message body text.
	if strings.Contains(resultText(t, result), "Root message.") {
		t.Fatal("thread result leaked message body text")
	}
}

func TestGetThreadBoundsFetchesWithMaxMessages(t *testing.T) {
	t.Parallel()

	mail := threadFake()
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_thread", map[string]any{"messageId": "id-reply", "maxMessages": 1})

	var decoded getThreadDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Nodes) != 1 || decoded.Nodes[0].Key != "id-reply" {
		t.Fatalf("nodes = %+v, want only the target", decoded.Nodes)
	}
	if !decoded.Truncated {
		t.Fatal("truncation not reported when siblings were dropped")
	}
	if len(mail.bodyCalls) != 1 {
		t.Fatalf("body fetches = %v, want only the target fetch", mail.bodyCalls)
	}
}

func TestGetThreadDoesNotReportTruncationWhenNothingWasDropped(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadataByID: map[string]bridge.MessageMetadata{
			"target": {ID: "target", Mailbox: "INBOX", Subject: "Standalone"},
		},
		bodyByID: map[string][]byte{
			"target": []byte("Subject: Standalone\r\nMessage-ID: <standalone@croton.test>\r\n\r\nbody\r\n"),
		},
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_thread", map[string]any{"messageId": "target", "maxMessages": 1})

	var decoded getThreadDecoded
	decodeResult(t, result, &decoded)
	if decoded.Truncated {
		t.Fatal("complete one-message thread reported truncated=true")
	}
	if len(mail.bodyCalls) != 1 {
		t.Fatalf("body fetches = %v, want only target", mail.bodyCalls)
	}
}

func TestGetThreadPropagatesAdapterTruncation(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadataByID: map[string]bridge.MessageMetadata{
			"target": {ID: "target", Mailbox: "INBOX", Subject: "Standalone"},
		},
		bodyByID: map[string][]byte{
			"target": []byte("Subject: Standalone\r\nMessage-ID: <standalone@croton.test>\r\n\r\nbody\r\n"),
		},
		searchTruncated: true,
	}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_thread", map[string]any{"messageId": "target"})

	var decoded getThreadDecoded
	decodeResult(t, result, &decoded)
	if !decoded.Truncated {
		t.Fatal("adapter-truncated sibling search reported a complete thread")
	}
}

func TestGetThreadValidatesArguments(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments map[string]any
	}{
		{"missing id", map[string]any{}},
		{"oversize maxMessages type", map[string]any{"messageId": "id-reply", "maxMessages": "many"}},
		{"zero maxMessages", map[string]any{"messageId": "id-reply", "maxMessages": 0}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			mail := threadFake()
			session := connectTestClient(t, Options{Mail: mail})

			result := callTool(t, session, "get_thread", testCase.arguments)

			requireToolError(t, result, errInvalidArgument)
			if len(mail.metaCalls)+len(mail.bodyCalls)+len(mail.searchCalls) != 0 {
				t.Fatal("adapter reached despite invalid arguments")
			}
		})
	}
}

func TestGetThreadClampsOversizedMaxMessages(t *testing.T) {
	t.Parallel()

	mail := threadFake()
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "get_thread", map[string]any{"messageId": "id-reply", "maxMessages": 1000000})

	var decoded getThreadDecoded
	decodeResult(t, result, &decoded)
	if len(decoded.Nodes) != 2 {
		t.Fatalf("nodes = %d, want 2", len(decoded.Nodes))
	}
	if len(mail.bodyCalls) > maxThreadMessages {
		t.Fatalf("body fetches = %d, exceeds bound %d", len(mail.bodyCalls), maxThreadMessages)
	}
}

func TestGetThreadBoundsAttemptedSiblingFetches(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		metadataByID: map[string]bridge.MessageMetadata{
			"target": {ID: "target", Mailbox: "INBOX", Subject: "Planning"},
		},
		bodyByID: map[string][]byte{
			"target": []byte(threadRootMIME),
		},
	}
	for index := range 30 {
		mail.searched = append(mail.searched, bridge.MessageMetadata{
			ID:      fmt.Sprintf("stale-%d", index),
			Mailbox: "INBOX",
			Subject: "Planning",
		})
	}

	_, _, code := appendThreadSiblings(
		context.Background(),
		Options{Mail: mail},
		[]bridge.ThreadMessage{{Key: "target"}},
		bridge.MessageMetadata{ID: "target", Mailbox: "INBOX", Subject: "Planning"},
		3,
	)
	if code != "" {
		t.Fatalf("code = %q", code)
	}
	if got := len(mail.metaCalls); got > 2 {
		t.Fatalf("sibling metadata attempts = %d, want <= 2", got)
	}
}

func TestGetThreadDeduplicatesAndRejectsSubjectFalsePositives(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{
		searched: []bridge.MessageMetadata{
			{ID: "match", Mailbox: "INBOX", Subject: "Re: Planning"},
			{ID: "match", Mailbox: "INBOX", Subject: "Re: Planning"},
			{ID: "false", Mailbox: "INBOX", Subject: "Planning committee"},
		},
		metadataByID: map[string]bridge.MessageMetadata{
			"match": {ID: "match", Mailbox: "INBOX", Subject: "Re: Planning"},
			"false": {ID: "false", Mailbox: "INBOX", Subject: "Planning committee"},
		},
		bodyByID: map[string][]byte{
			"match": []byte(threadReplyMIME),
			"false": []byte(strings.ReplaceAll(threadReplyMIME, "Re: Planning", "Planning committee")),
		},
	}

	members, _, code := appendThreadSiblings(
		context.Background(),
		Options{Mail: mail},
		[]bridge.ThreadMessage{{Key: "target"}},
		bridge.MessageMetadata{ID: "target", Mailbox: "INBOX", Subject: "Planning"},
		10,
	)
	if code != "" {
		t.Fatalf("code = %q", code)
	}
	if len(members) != 2 {
		keys := make([]string, 0, len(members))
		for _, member := range members {
			keys = append(keys, member.Key)
		}
		t.Fatalf("members = %d, want target plus one exact unique sibling: %v", len(members), keys)
	}
}

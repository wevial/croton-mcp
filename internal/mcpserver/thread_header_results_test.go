package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// threadAggregateKeys is the closed key set of one get_thread result.
var threadAggregateKeys = map[string]bool{"id": true, "mailbox": true, "nodes": true, "truncated": true, "unavailableMembers": true}

// threadNodeTruncationKeys is the closed key set of one node's truncation.
var threadNodeTruncationKeys = map[string]bool{"headers": true, "headerCount": true, "references": true}

// truncatingHeaderMail sets each member's header truncation on top of the
// fake's selected header read.
type truncatingHeaderMail struct {
	*fakeMail

	truncation map[string]bridge.Truncation
}

func (mail *truncatingHeaderMail) GetMessageHeaders(ctx context.Context, identifier string) (bridge.MessageHeaders, error) {
	headers, err := mail.fakeMail.GetMessageHeaders(ctx, identifier)
	if err != nil {
		return bridge.MessageHeaders{}, err
	}

	headers.Truncation = mail.truncation[identifier]
	return headers, nil
}

// threadSiblingFake holds target id-reply, sibling id-root and an unlinked
// sibling id-other, in that candidate order.
func threadSiblingFake() *fakeMail {
	mail := threadFake()
	mail.metadataByID["id-other"] = bridge.MessageMetadata{ID: "id-other", Mailbox: "INBOX", UIDValidity: 9001, UID: 103, Subject: "Planning"}
	mail.bodyByID["id-other"] = []byte(strings.ReplaceAll(threadRootMIME, "<root@", "<other@"))
	mail.searched = append(mail.searched, bridge.MessageMetadata{ID: "id-other", Mailbox: "INBOX", Subject: "Planning"})
	return mail
}

// requireThreadSchema pins the closed aggregate, node, truncation and
// unavailable-member shapes and returns the nodes and reasons.
func requireThreadSchema(t *testing.T, thread map[string]any) ([]map[string]any, []string) {
	t.Helper()

	for key := range thread {
		if !threadAggregateKeys[key] {
			t.Fatalf("thread aggregate gained field %q: %v", key, thread)
		}
	}
	requireNoIdentityKeys(t, "get_thread aggregate", thread)

	nodes := rawItems(t, thread, "nodes")
	for _, node := range nodes {
		for key := range node {
			if !threadNodeKeys[key] {
				t.Fatalf("thread node gained field %q: %v", key, node)
			}
		}
		truncation, ok := node["truncation"].(map[string]any)
		if !ok {
			t.Fatalf("thread node lacks truncation object: %v", node)
		}
		for key, value := range truncation {
			if !threadNodeTruncationKeys[key] || value != true {
				t.Fatalf("node truncation carries %q=%v: %v", key, value, node)
			}
		}
	}

	var reasons []string
	if _, present := thread["unavailableMembers"]; present {
		records := rawItems(t, thread, "unavailableMembers")
		if len(records) == 0 || len(records) > maxThreadMessages-1 {
			t.Fatalf("unavailableMembers has %d records, want 1..%d", len(records), maxThreadMessages-1)
		}
		for _, record := range records {
			reason, _ := record["reason"].(string)
			if len(record) != 1 || (reason != "stale_id" && reason != "invalid_identity" && reason != "malformed_header") {
				t.Fatalf("unavailable member is not one closed reason: %v", record)
			}
			reasons = append(reasons, reason)
		}
	}

	return nodes, reasons
}

func nodeKeys(nodes []map[string]any) []string {
	keys := make([]string, 0, len(nodes))
	for _, node := range nodes {
		key, _ := node["key"].(string)
		keys = append(keys, key)
	}
	return keys
}

func TestThreadHeaderResults(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	t.Run("closed_classifier", func(t *testing.T) {
		typed := &bridge.NormalizeError{Code: "malformed_header"}

		t.Run("local sibling omission", func(t *testing.T) {
			for _, local := range []struct {
				name string
				err  error
			}{
				{"typed", typed},
				{"wrapped", fmt.Errorf("selected header: %w", typed)},
				{"joined_local", errors.Join(errors.New("context-free"), typed)},
			} {
				mail := threadSiblingFake()
				mail.headerErrByID = map[string]error{"id-root": local.err}
				result := callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"})

				thread := rawObject(t, result)
				nodes, reasons := requireThreadSchema(t, thread)
				if got := nodeKeys(nodes); !reflect.DeepEqual(got, []string{"id-other", "id-reply"}) {
					t.Fatalf("%s nodes = %v, want healthy id-other and id-reply only", local.name, got)
				}
				for _, node := range nodes {
					if parent, _ := node["parentKey"].(string); parent != "" || fmt.Sprint(node["depth"]) != "0" {
						t.Fatalf("%s omitted parent was manufactured: %v", local.name, node)
					}
				}
				if !reflect.DeepEqual(reasons, []string{"malformed_header"}) {
					t.Fatalf("%s reasons = %v", local.name, reasons)
				}
				if truncated, _ := thread["truncated"].(bool); !truncated {
					t.Fatalf("%s omission not reported truncated: %v", local.name, thread)
				}
				if strings.Contains(resultText(t, result), "id-root") || strings.Contains(resultText(t, result), "context-free") {
					t.Fatalf("%s record exposed the failed candidate or error text: %s", local.name, resultText(t, result))
				}
				if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply", "id-root", "id-other"}) || len(mail.searchCalls) != 1 || len(mail.bodyCalls)+len(mail.metaCalls) != 0 {
					t.Fatalf("%s operations header=%v search=%d body=%v metadata=%v", local.name, mail.headerCalls, len(mail.searchCalls), mail.bodyCalls, mail.metaCalls)
				}
			}
		})

		t.Run("controls abort whole call", func(t *testing.T) {
			for _, control := range []struct {
				name string
				err  error
				want string
			}{
				{"plain_text_equivalent", errors.New("malformed_header"), errInternal},
				{"unknown_code", &bridge.NormalizeError{Code: "malformed_headers"}, errInternal},
				{"bridge_joined_with_local", errors.Join(typed, &bridge.Error{Code: bridge.CodeBridgeUnreachable}), errUnavailable},
				{"canceled_joined_with_local", errors.Join(context.Canceled, typed), errInternal},
				{"deadline_wrapping_local", fmt.Errorf("%w: %w", context.DeadlineExceeded, typed), errInternal},
			} {
				mail := threadSiblingFake()
				mail.headerErrByID = map[string]error{"id-root": control.err}
				result := callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"})

				requireRefusedWithoutTuple(t, result, control.want)
				if strings.Contains(resultText(t, result), "nodes") {
					t.Fatalf("%s returned a partial graph: %s", control.name, resultText(t, result))
				}
				if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply", "id-root"}) {
					t.Fatalf("%s did not stop reads: %v", control.name, mail.headerCalls)
				}
			}
		})

		t.Run("get_message and global mapping unchanged", func(t *testing.T) {
			malformed := strings.Replace(threadRootMIME, "From: Croton Fixture <fixture@croton.test>", "From: =?UTF-8?B?***?= <fixture@croton.test>", 1)
			mail := &fakeMail{metadata: withReadIdentity(bridge.MessageMetadata{ID: "id-bad", Mailbox: "INBOX", Subject: "Planning"}, 9001, 101), body: []byte(malformed)}
			requireRefusedWithoutTuple(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_message", map[string]any{"messageId": "id-bad"}), errInternal)
		})
	})

	t.Run("result_schema", func(t *testing.T) {
		t.Run("omissions and header limits", func(t *testing.T) {
			fake := &fakeMail{
				metadataByID: map[string]bridge.MessageMetadata{},
				bodyByID:     map[string][]byte{},
			}
			add := func(id, subject string, uid uint32) {
				fake.metadataByID[id] = withReadIdentity(bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: subject}, 9001, uid)
				fake.bodyByID[id] = fakeThreadBody(subject, "<"+id+"@croton.test>", "")
				fake.searched = append(fake.searched, bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: subject})
			}
			add("target", "Re: Budget", 500)
			add("headers", "Budget", 501)
			fake.searched = append(fake.searched, bridge.MessageMetadata{ID: "stale", Mailbox: "INBOX", Subject: "Budget"})
			add("count", "Budget", 502)
			add("zero", "Budget", 0)
			add("refs", "Budget", 503)
			add("bad", "Budget", 504)
			fake.headerErrByID = map[string]error{"bad": &bridge.NormalizeError{Code: "malformed_header"}}
			mail := &truncatingHeaderMail{fakeMail: fake, truncation: map[string]bridge.Truncation{
				"headers": {Headers: true},
				"count":   {HeaderCount: true},
				"refs":    {References: true},
			}}

			result := callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "target"})
			thread := rawObject(t, result)
			nodes, reasons := requireThreadSchema(t, thread)

			if !reflect.DeepEqual(reasons, []string{"stale_id", "invalid_identity", "malformed_header"}) {
				t.Fatalf("reasons = %v, want one per omission in attempt order", reasons)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("omissions not reported truncated: %v", thread)
			}
			wantTruncation := map[string]map[string]any{
				"target":  {},
				"headers": {"headers": true},
				"count":   {"headerCount": true},
				"refs":    {"references": true},
			}
			if len(nodes) != len(wantTruncation) {
				t.Fatalf("nodes = %v, want %d retained members", nodeKeys(nodes), len(wantTruncation))
			}
			for _, node := range nodes {
				key, _ := node["key"].(string)
				want, ok := wantTruncation[key]
				if !ok || !reflect.DeepEqual(node["truncation"], want) {
					t.Fatalf("node %q truncation = %v, want %v", key, node["truncation"], want)
				}
				if flatTuple(t, node) != (readTuple{Mailbox: "INBOX", UIDValidity: 9001, UID: fake.metadataByID[key].UID}) {
					t.Fatalf("node %q borrowed a tuple: %v", key, node)
				}
			}
			text := resultText(t, result)
			for _, leaked := range []string{`"stale"`, `"zero"`, `"bad"`, "<bad@", "504"} {
				if strings.Contains(text, leaked) {
					t.Fatalf("result exposed an omitted candidate %q: %s", leaked, text)
				}
			}
		})

		t.Run("stale omission alone marks truncated", func(t *testing.T) {
			mail := threadSiblingFake()
			mail.searched = append(mail.searched, bridge.MessageMetadata{ID: "id-gone", Mailbox: "INBOX", Subject: "Planning"})
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"}))
			nodes, reasons := requireThreadSchema(t, thread)

			if !reflect.DeepEqual(reasons, []string{"stale_id"}) || len(nodes) != 3 {
				t.Fatalf("stale-only thread = %v", thread)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("stale omission not reported truncated: %v", thread)
			}
			for _, node := range nodes {
				if truncation, _ := node["truncation"].(map[string]any); len(truncation) != 0 {
					t.Fatalf("stale-only thread has header-limited node: %v", node)
				}
			}
		})

		t.Run("header limits alone mark truncated", func(t *testing.T) {
			mail := &truncatingHeaderMail{fakeMail: threadSiblingFake(), truncation: map[string]bridge.Truncation{"id-other": {HeaderCount: true}}}
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"}))
			_, reasons := requireThreadSchema(t, thread)

			if truncated, _ := thread["truncated"].(bool); !truncated || len(reasons) != 0 {
				t.Fatalf("header-limited thread = %v", thread)
			}
		})
	})

	t.Run("reachable_header_failure", func(t *testing.T) {
		header := func(from, subject, messageID, extra string) string {
			return "From: " + from + "\r\nTo: Reader <reader@croton.test>\r\nSubject: " + subject + "\r\n" +
				"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\nMessage-ID: " + messageID + "\r\n" + extra + "\r\nSynthetic body.\r\n"
		}
		healthyRoot := header("Root <root@croton.test>", "Planning", "<root@croton.test>", "")
		healthyReply := header("Reply <reply@croton.test>", "Re: Planning", "<reply@croton.test>", "In-Reply-To: <root@croton.test>\r\nReferences: <root@croton.test>\r\n")
		malformed := header("=?UTF-8?B?***?= <bad@croton.test>", "Planning", "<bad@croton.test>", "")
		orphan := header("Orphan <orphan@croton.test>", "Re: Planning", "<orphan@croton.test>", "In-Reply-To: <bad@croton.test>\r\nReferences: <bad@croton.test>\r\n")

		t.Run("malformed encoded word is sibling-local", func(t *testing.T) {
			server, adapter := startIntegrationAdapterWithMessages(t, []string{healthyRoot, healthyReply, malformed, orphan})
			session := connectTestClient(t, Options{Mail: adapter})

			searched := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "subject": "Planning"})), "results")
			byUID := make(map[uint32]string)
			tuples := make(map[string]readTuple)
			for _, item := range searched {
				id, _ := item["id"].(string)
				tuple := flatTuple(t, item)
				byUID[tuple.UID] = id
				tuples[id] = tuple
			}
			rootID, replyID, badID, orphanID := byUID[101], byUID[102], byUID[103], byUID[104]
			if rootID == "" || replyID == "" || badID == "" || orphanID == "" {
				t.Fatalf("fixture UIDs = %v", byUID)
			}

			before := len(server.Commands())
			result := callTool(t, session, "get_thread", map[string]any{"messageId": replyID})
			thread := rawObject(t, result)
			nodes, reasons := requireThreadSchema(t, thread)

			if !reflect.DeepEqual(reasons, []string{"malformed_header"}) {
				t.Fatalf("reasons = %v, want only the malformed sibling", reasons)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("malformed sibling omission not truncated: %v", thread)
			}
			want := map[string]struct{ messageID, parent, depth string }{
				rootID:   {"<root@croton.test>", "", "0"},
				replyID:  {"<reply@croton.test>", rootID, "1"},
				orphanID: {"<orphan@croton.test>", "", "0"},
			}
			if len(nodes) != len(want) {
				t.Fatalf("nodes = %v, want root, reply and orphan", nodes)
			}
			for _, node := range nodes {
				key, _ := node["key"].(string)
				expected, ok := want[key]
				parent, _ := node["parentKey"].(string)
				if !ok || node["messageId"] != expected.messageID || parent != expected.parent || fmt.Sprint(node["depth"]) != expected.depth {
					t.Fatalf("node %v, want %+v", node, expected)
				}
				if flatTuple(t, node) != tuples[key] {
					t.Fatalf("node %q tuple is not its own", key)
				}
				if truncation, _ := node["truncation"].(map[string]any); len(truncation) != 0 {
					t.Fatalf("healthy node reports truncation: %v", node)
				}
			}
			if text := resultText(t, result); strings.Contains(text, badID) || strings.Contains(text, "bad@croton.test") {
				t.Fatalf("result exposed the malformed sibling: %s", text)
			}
			requireHeaderOnlyThreadTranscript(t, server.Commands()[before:], []uint32{101, 102, 103, 104})

			before = len(server.Commands())
			requireRefusedWithoutTuple(t, callTool(t, session, "get_thread", map[string]any{"messageId": badID}), errUnavailable)
			for _, command := range server.Commands()[before:] {
				if upper := strings.ToUpper(command.Raw); strings.Contains(upper, "SEARCH") && strings.Contains(upper, "SUBJECT") {
					t.Fatalf("malformed target scanned siblings: %q", command.Raw)
				}
			}
			requireHeaderOnlyThreadTranscript(t, server.Commands()[before:], []uint32{103})

			requireReadOnlyTranscript(t, server)
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
		})

		t.Run("syntax and bounds still abort", func(t *testing.T) {
			for _, systemic := range []struct {
				name    string
				message string
				want    string
			}{
				{"malformed_syntax", "From: Bad <bad@croton.test>\r\nSubject: Planning\r\nnot a header field\r\nMessage-ID: <bad@croton.test>\r\n\r\nbody\r\n", errUnavailable},
				{"over_bound", header("Bad <bad@croton.test>", "Planning", "<bad@croton.test>", "X-Pad: "+strings.Repeat("p", 20<<10)+"\r\n"), errBoundsExceeded},
			} {
				server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{
					Messages: []string{healthyRoot, healthyReply, systemic.message},
					Seen:     []bool{true, false, true},
				})
				session := connectTestClient(t, Options{Mail: adapter})

				unread := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true})), "results")
				if len(unread) != 1 || flatTuple(t, unread[0]).UID != 102 {
					t.Fatalf("%s fixture unread search = %v", systemic.name, unread)
				}
				replyID, _ := unread[0]["id"].(string)

				result := callTool(t, session, "get_thread", map[string]any{"messageId": replyID})
				requireRefusedWithoutTuple(t, result, systemic.want)
				if text := resultText(t, result); strings.Contains(text, "nodes") || strings.Contains(text, "unavailableMembers") {
					t.Fatalf("%s returned a partial graph: %s", systemic.name, text)
				}
				requireNoBodyFetch(t, server.Commands())
				requireReadOnlyTranscript(t, server)
			}
		})
	})

	t.Run("output_and_docs", func(t *testing.T) {
		requireThreadOutputControls(t)

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		for _, statement := range []string{
			"The `get_thread` result keys are `id`, `mailbox`, `nodes`, `truncated` and `unavailableMembers`.",
			"Each node carries `key`, `mailbox`, `uidvalidity`, `uid`, `messageId`, `parentKey`, `depth`, `subject`, `from`, `date` and `truncation`.",
			"A node's `truncation` object carries only the true reasons among `headers`, `headerCount` and `references`, and is empty when the node's headers are complete.",
			"Each `unavailableMembers` record is `{\"reason\": ...}` with one of `stale_id`, `invalid_identity` or `malformed_header`, at most 19 records.",
			"Records are nonselectable failed-attempt records, not a mailbox inventory:",
			"A target whose own header has a malformed encoded word fails `get_thread` with `unavailable`, without a sibling scan, while `get_message` still maps it to `internal`.",
			"Malformed header syntax and over-bound header blocks still fail the whole call",
		} {
			if !strings.Contains(string(contents), statement) {
				t.Errorf("Mail documentation missing contract statement: %q", statement)
			}
		}
	})
}

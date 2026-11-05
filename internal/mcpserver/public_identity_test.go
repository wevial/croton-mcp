package mcpserver

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/mail"
	"os"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// readTuple is one mailbox generation and UID observed independently of the
// MCP surface.
type readTuple struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

var (
	observedUIDValidity = regexp.MustCompile(`\[UIDVALIDITY ([0-9]+)\]`)
	observedFetchUID    = regexp.MustCompile(`^\* [0-9]+ FETCH \(UID ([0-9]+) `)
	observedLiteral     = regexp.MustCompile(`\{([0-9]+)\}\r\n$`)
)

// observeFixtureTuples reads the legacy fixture's INBOX generation and UIDs
// over its own verified loopback TLS connection, keyed by header, without
// any adapter or opaque identifier.
func observeFixtureTuples(t *testing.T, server *testkit.Server, header string) map[string]readTuple {
	t.Helper()

	connection, err := tls.Dial("tcp", server.Addr(), server.ClientTLSConfig())
	if err != nil {
		t.Fatalf("observer TLS dial: %v", err)
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	reader := bufio.NewReader(connection)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatalf("observer greeting: %v", err)
	}

	exchange := func(tag, command string) ([]string, [][]byte) {
		if _, err := fmt.Fprintf(connection, "%s %s\r\n", tag, command); err != nil {
			t.Fatalf("observer write: %v", err)
		}
		var lines []string
		var literals [][]byte
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatalf("observer read: %v", err)
			}
			if match := observedLiteral.FindStringSubmatch(line); match != nil {
				size, _ := strconv.Atoi(match[1])
				literal := make([]byte, size)
				if _, err := io.ReadFull(reader, literal); err != nil {
					t.Fatalf("observer literal: %v", err)
				}
				literals = append(literals, literal)
				if _, err := reader.ReadString('\n'); err != nil {
					t.Fatalf("observer literal tail: %v", err)
				}
			}
			lines = append(lines, line)
			if strings.HasPrefix(line, tag+" ") {
				if !strings.HasPrefix(line, tag+" OK") {
					t.Fatalf("observer %s failed: %q", command, line)
				}
				return lines, literals
			}
		}
	}

	exchange("o1", "AUTHENTICATE PLAIN "+base64.StdEncoding.EncodeToString([]byte("\x00observer\x00synthetic")))
	examined, _ := exchange("o2", "EXAMINE INBOX")
	var uidValidity uint64
	for _, line := range examined {
		if match := observedUIDValidity.FindStringSubmatch(line); match != nil {
			uidValidity, _ = strconv.ParseUint(match[1], 10, 32)
		}
	}
	fetched, headers := exchange("o3", "UID FETCH 1:4294967295 (UID BODY.PEEK[HEADER])")

	tuples := make(map[string]readTuple)
	headerIndex := 0
	for _, line := range fetched {
		match := observedFetchUID.FindStringSubmatch(line)
		if match == nil {
			continue
		}
		uid, _ := strconv.ParseUint(match[1], 10, 32)
		parsed, err := mail.ReadMessage(bytes.NewReader(headers[headerIndex]))
		headerIndex++
		if err != nil {
			t.Fatalf("observer header: %v", err)
		}
		tuples[parsed.Header.Get(header)] = readTuple{Mailbox: "INBOX", UIDValidity: uint32(uidValidity), UID: uint32(uid)}
	}
	if uidValidity == 0 || len(tuples) == 0 {
		t.Fatalf("observer saw no fixture tuples: %q", fetched)
	}

	return tuples
}

// rawObject decodes one successful tool result with exact numbers.
func rawObject(t *testing.T, result *mcp.CallToolResult) map[string]any {
	t.Helper()

	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, result))
	}
	decoder := json.NewDecoder(strings.NewReader(resultText(t, result)))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("decode tool result: %v", err)
	}
	return object
}

func rawItems(t *testing.T, object map[string]any, key string) []map[string]any {
	t.Helper()

	list, ok := object[key].([]any)
	if !ok {
		t.Fatalf("result field %q is not a list: %v", key, object)
	}
	items := make([]map[string]any, 0, len(list))
	for _, entry := range list {
		item, ok := entry.(map[string]any)
		if !ok {
			t.Fatalf("result field %q has a non-object entry: %v", key, entry)
		}
		items = append(items, item)
	}
	return items
}

// flatTuple returns the item's mailbox, uidvalidity and uid, requiring both
// numbers to be present JSON integers in 1..4294967295.
func flatTuple(t *testing.T, item map[string]any) readTuple {
	t.Helper()

	mailbox, _ := item["mailbox"].(string)
	tuple := readTuple{Mailbox: mailbox}
	for _, field := range []struct {
		name   string
		target *uint32
	}{{"uidvalidity", &tuple.UIDValidity}, {"uid", &tuple.UID}} {
		number, ok := item[field.name].(json.Number)
		if !ok {
			t.Fatalf("flat result lacks numeric %q: %v", field.name, item)
		}
		value, err := strconv.ParseUint(number.String(), 10, 64)
		if err != nil || value < 1 || value > math.MaxUint32 {
			t.Fatalf("flat result %q = %s, want an integer in 1..4294967295", field.name, number)
		}
		*field.target = uint32(value)
	}
	return tuple
}

func requireNoIdentityKeys(t *testing.T, label string, item map[string]any) {
	t.Helper()

	for _, key := range []string{"uidvalidity", "uid", "uidValidity", "UID", "UIDValidity"} {
		if _, ok := item[key]; ok {
			t.Fatalf("%s carries identity key %q: %v", label, key, item)
		}
	}
}

func requireNoBodyFetch(t *testing.T, commands []testkit.Command) {
	t.Helper()

	for _, command := range commands {
		upper := strings.ToUpper(command.Raw)
		if strings.Contains(upper, "FETCH") && (strings.Contains(upper, "BODY.PEEK[]") || strings.Contains(upper, "BODY[]") || strings.Contains(upper, "BODY.PEEK[TEXT]")) {
			t.Fatalf("metadata tool fetched a body: %q", command.Raw)
		}
	}
}

func requireRefusedWithoutTuple(t *testing.T, result *mcp.CallToolResult, code string) {
	t.Helper()

	requireToolError(t, result, code)
	text := resultText(t, result)
	if strings.Contains(text, "uidvalidity") || strings.Contains(text, `"uid"`) || strings.Contains(text, `"id"`) {
		t.Fatalf("refusal carried an identity: %s", text)
	}
}

func publicIdentityStateful() *testkit.StatefulOptions {
	messages := testkit.SyntheticAttachmentMessages()
	return &testkit.StatefulOptions{Mailboxes: []testkit.MailboxSeed{{
		Name:        "INBOX",
		UIDValidity: math.MaxUint32,
		Messages: []testkit.MessageSeed{
			{UID: 1, Body: messages[0]},
			{UID: 7, Body: messages[1]},
		},
	}}}
}

func TestPublicReadIdentityFlatMCP(t *testing.T) {
	t.Parallel()

	t.Run("search_digest", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapterWithOptions(t, testkit.SyntheticDigestMixedMailbox(time.Now()))
		observed := observeFixtureTuples(t, server, "Subject")
		session := connectTestClient(t, Options{Mail: adapter})
		before := len(server.Commands())

		search := rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true}))
		digest := rawObject(t, callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"}))
		requireNoBodyFetch(t, server.Commands()[before:])
		requireReadOnlyTranscript(t, server)

		if truncated, _ := search["truncated"].(bool); truncated {
			t.Fatalf("search result unexpectedly truncated: %v", search)
		}
		if truncated, _ := digest["truncated"].(bool); truncated {
			t.Fatalf("digest result unexpectedly truncated: %v", digest)
		}
		requireNoIdentityKeys(t, "digest summary", digest)

		idsBySubject := make(map[string]string)
		for _, shape := range []struct {
			name  string
			items []map[string]any
		}{
			{"search_mail results", rawItems(t, search, "results")},
			{"select_digest_candidates candidates", rawItems(t, digest, "candidates")},
		} {
			if len(shape.items) != 2 {
				t.Fatalf("%s = %v, want two fixture UIDs", shape.name, shape.items)
			}
			uids := make(map[uint32]bool)
			for _, item := range shape.items {
				subject, _ := item["subject"].(string)
				id, _ := item["id"].(string)
				want, ok := observed[subject]
				if !ok || id == "" {
					t.Fatalf("%s item is not an observed fixture message: %v", shape.name, item)
				}
				if got := flatTuple(t, item); got != want {
					t.Fatalf("%s tuple for %q = %+v, want observed %+v", shape.name, subject, got, want)
				}
				if previous, seen := idsBySubject[subject]; seen && previous != id {
					t.Fatalf("%s opaque id for %q differs from search", shape.name, subject)
				}
				idsBySubject[subject] = id
				uids[want.UID] = true
			}
			if len(uids) != 2 {
				t.Fatalf("%s did not return two distinct fixture UIDs", shape.name)
			}
		}
		if idsBySubject["Synthetic digest unread first"] == idsBySubject["Synthetic digest unread second"] {
			t.Fatal("distinct fixture messages share an opaque id")
		}

		// Each public id must select the very message its tuple was observed
		// for; swapped ids would select the other fixture subject and tuple.
		for _, shape := range []struct {
			name  string
			items []map[string]any
		}{
			{"search_mail results", rawItems(t, search, "results")},
			{"select_digest_candidates candidates", rawItems(t, digest, "candidates")},
		} {
			for _, item := range shape.items {
				id, _ := item["id"].(string)
				listed := flatTuple(t, item)
				selected := rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": id}))
				headers, _ := selected["headers"].(map[string]any)
				subject, _ := headers["subject"].(string)
				want, ok := observed[subject]
				if !ok || selected["id"] != id || flatTuple(t, selected) != want || listed != want {
					t.Fatalf("%s id selects %q %+v, listed with %+v", shape.name, subject, flatTuple(t, selected), listed)
				}
			}
		}
	})

	t.Run("selected_and_attachment", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{Stateful: publicIdentityStateful()})
		state, ok := server.MailboxSnapshot("INBOX")
		if !ok || len(state.Messages) != 2 {
			t.Fatalf("fixture snapshot = %+v", state)
		}
		observed := map[string]readTuple{
			"Synthetic attachment report":       {Mailbox: "INBOX", UIDValidity: state.UIDValidity, UID: state.Messages[0].UID},
			"Synthetic attachment-free message": {Mailbox: "INBOX", UIDValidity: state.UIDValidity, UID: state.Messages[1].UID},
		}
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		search := rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}))
		results := rawItems(t, search, "results")
		if len(results) != 2 {
			t.Fatalf("search results = %v, want both fixture messages", results)
		}

		sentinels := []string{testkit.SyntheticAttachmentDecoded, base64.StdEncoding.EncodeToString([]byte(testkit.SyntheticAttachmentDecoded))}
		for _, item := range results {
			subject, _ := item["subject"].(string)
			id, _ := item["id"].(string)
			searched := flatTuple(t, item)
			if searched != observed[subject] {
				t.Fatalf("search tuple for %q = %+v, want fixture %+v", subject, searched, observed[subject])
			}

			messageResult := callTool(t, session, "get_message", map[string]any{"messageId": id})
			message := rawObject(t, messageResult)
			if message["id"] != id || flatTuple(t, message) != searched {
				t.Fatalf("get_message parent = %v, want id and tuple %+v from search", message, searched)
			}
			headers, _ := message["headers"].(map[string]any)
			if headers["subject"] != subject || message["textSource"] != "plain" {
				t.Fatalf("get_message content boundary changed: %v", message)
			}
			for _, attachment := range rawItems(t, mapOrEmptyList(message, "attachments"), "attachments") {
				requireNoIdentityKeys(t, "get_message attachment", attachment)
			}

			attachmentsResult := callTool(t, session, "list_attachments", map[string]any{"messageId": id})
			attachments := rawObject(t, attachmentsResult)
			if attachments["id"] != id || flatTuple(t, attachments) != searched {
				t.Fatalf("list_attachments parent = %v, want id and tuple %+v from search", attachments, searched)
			}
			if _, ok := attachments["text"]; ok {
				t.Fatalf("list_attachments returned message text: %v", attachments)
			}
			parts := rawItems(t, attachments, "attachments")
			wantParts := 0
			if subject == "Synthetic attachment report" {
				wantParts = 1
			}
			if len(parts) != wantParts {
				t.Fatalf("list_attachments parts = %v, want %d", parts, wantParts)
			}
			for _, part := range parts {
				requireNoIdentityKeys(t, "attachment part", part)
			}

			for _, result := range []*mcp.CallToolResult{messageResult, attachmentsResult} {
				raw, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("marshal SDK result: %v", err)
				}
				for _, sentinel := range sentinels {
					if bytes.Contains(raw, []byte(sentinel)) {
						t.Fatal("attachment bytes leaked into a flat result")
					}
				}
			}
		}

		requireReadOnlyTranscript(t, server)
		for _, event := range auditLines(t, &audit) {
			requireAllowlistedKeys(t, event)
		}
	})

	t.Run("maximum_tuple_serialization", func(t *testing.T) {
		t.Parallel()

		// The adapter's selected-metadata maximum is witnessed in the bridge
		// suite; this pins the public JSON numbers without any UIDNEXT.
		maximum := withReadIdentity(bridge.MessageMetadata{ID: "opaque-maximum", Mailbox: "INBOX", Subject: "Maximum identity fixture"}, math.MaxUint32, math.MaxUint32)
		mail := &fakeMail{
			status:   bridge.MailboxStatus{Messages: 1, Unseen: 1},
			searched: []bridge.MessageMetadata{maximum},
			metadata: maximum,
			body:     []byte("From: Fixture <fixture@croton.test>\r\nSubject: Maximum identity fixture\r\n\r\nmaximum identity body\r\n"),
		}
		session := connectTestClient(t, Options{Mail: mail})
		want := readTuple{Mailbox: "INBOX", UIDValidity: math.MaxUint32, UID: math.MaxUint32}

		items := append(rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results"),
			rawItems(t, rawObject(t, callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"})), "candidates")...)
		items = append(items,
			rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": "opaque-maximum"})),
			rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-maximum"})))
		if len(items) != 4 {
			t.Fatalf("flat shapes = %d, want 4", len(items))
		}
		for _, item := range items {
			if got := flatTuple(t, item); got != want {
				t.Fatalf("flat tuple = %+v, want %+v", got, want)
			}
			if item["uidvalidity"] != json.Number("4294967295") || item["uid"] != json.Number("4294967295") {
				t.Fatalf("flat result did not serialize the maximum exactly: %v", item)
			}
		}
	})

	t.Run("invalid_identity", func(t *testing.T) {
		t.Parallel()

		t.Run("changed UIDVALIDITY", func(t *testing.T) {
			server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{Stateful: publicIdentityStateful()})
			session := connectTestClient(t, Options{Mail: adapter})
			results := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
			if len(results) == 0 {
				t.Fatal("search returned no fixture messages")
			}
			id, _ := results[0]["id"].(string)
			if err := server.ReplaceUIDValidity("INBOX", 4242); err != nil {
				t.Fatalf("replace fixture generation: %v", err)
			}

			for _, tool := range []string{"get_message", "list_attachments"} {
				requireRefusedWithoutTuple(t, callTool(t, session, tool, map[string]any{"messageId": id}), errStaleID)
			}
			for _, item := range rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results") {
				if tuple := flatTuple(t, item); tuple.UIDValidity != 4242 || item["id"] == id {
					t.Fatalf("fresh search kept the old generation: %v", item)
				}
			}
			requireReadOnlyTranscript(t, server)
		})

		t.Run("missing UID", func(t *testing.T) {
			server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{
				Messages: testkit.SyntheticAttachmentMessages(),
				Scenario: testkit.Scenario{OmitExactUIDSearchResponse: true},
			})
			session := connectTestClient(t, Options{Mail: adapter})
			results := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
			if len(results) != 2 {
				t.Fatalf("search results = %v", results)
			}
			id, _ := results[0]["id"].(string)

			for _, tool := range []string{"get_message", "list_attachments"} {
				requireRefusedWithoutTuple(t, callTool(t, session, tool, map[string]any{"messageId": id}), errStaleID)
			}
			requireReadOnlyTranscript(t, server)
		})

		t.Run("new adapter key after restart", func(t *testing.T) {
			server, first := startIntegrationAdapterWithOptions(t, testkit.Options{Messages: testkit.SyntheticAttachmentMessages()})
			results := rawItems(t, rawObject(t, callTool(t, connectTestClient(t, Options{Mail: first}), "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
			if len(results) == 0 {
				t.Fatal("search returned no fixture messages")
			}
			id, _ := results[0]["id"].(string)
			if err := first.Close(); err != nil {
				t.Fatalf("close first adapter: %v", err)
			}

			_, restarted := startIntegrationAdapterWithOptions(t, testkit.Options{Messages: testkit.SyntheticAttachmentMessages()})
			restartedSession := connectTestClient(t, Options{Mail: restarted})
			for _, tool := range []string{"get_message", "list_attachments"} {
				requireRefusedWithoutTuple(t, callTool(t, restartedSession, tool, map[string]any{"messageId": id}), errStaleID)
			}
			requireReadOnlyTranscript(t, server)
		})

		t.Run("zero or absent adapter identity", func(t *testing.T) {
			body := []byte("From: Fixture <fixture@croton.test>\r\nSubject: Zero identity fixture\r\n\r\nzero identity body\r\n")
			zero := bridge.MessageMetadata{ID: "opaque-zero", Mailbox: "INBOX", Subject: "Zero identity fixture"}
			for _, metadata := range []bridge.MessageMetadata{
				withReadIdentity(zero, 0, 0),
				withReadIdentity(zero, 9001, 0),
				withReadIdentity(zero, 0, 101),
			} {
				valid := withReadIdentity(bridge.MessageMetadata{ID: "opaque-valid", Mailbox: "INBOX", Subject: "Valid identity fixture"}, 9001, 102)
				mail := &fakeMail{
					status:   bridge.MailboxStatus{Messages: 2, Unseen: 2},
					searched: []bridge.MessageMetadata{valid, metadata},
					metadata: metadata,
					body:     body,
				}
				session := connectTestClient(t, Options{Mail: mail})

				requireRefusedWithoutTuple(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}), errUnavailable)
				requireRefusedWithoutTuple(t, callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"}), errUnavailable)
				requireRefusedWithoutTuple(t, callTool(t, session, "get_message", map[string]any{"messageId": "opaque-zero"}), errUnavailable)
				requireRefusedWithoutTuple(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-zero"}), errUnavailable)
			}
		})
	})

	t.Run("contract_guards", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapterWithMessages(t, testkit.SyntheticLinkedThread())
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		readInputs := map[string][]string{
			"list_folders":             {},
			"search_mail":              {"before", "limit", "mailbox", "sender", "since", "subject", "unreadOnly"},
			"get_message":              {"messageId"},
			"get_thread":               {"maxMessages", "messageId"},
			"list_attachments":         {"messageId"},
			"select_digest_candidates": {"limit", "mailbox", "sinceHours", "unreadOnly"},
		}
		triageInputs := map[string][]string{
			"mark_read":    {"mailbox", "uids", "uidvalidity"},
			"mark_unread":  {"mailbox", "uids", "uidvalidity"},
			"move_mail":    {"destination", "mailbox", "uids", "uidvalidity"},
			"archive_mail": {"mailbox", "uids", "uidvalidity"},
			"trash_mail":   {"mailbox", "uids", "uidvalidity"},
		}
		optedIn := make(map[string][]string, len(readInputs)+len(triageInputs))
		for name, inputs := range readInputs {
			optedIn[name] = inputs
		}
		for name, inputs := range triageInputs {
			optedIn[name] = inputs
		}
		requireCatalog(t, session, readInputs, triageInputs)
		requireCatalog(t, connectTestClient(t, Options{Mail: adapter, Triage: adapter}), optedIn, triageInputs)

		search := rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}))
		results := rawItems(t, search, "results")
		if len(results) == 0 {
			t.Fatal("search returned no fixture messages")
		}
		sensitive := []string{}
		for _, item := range results {
			tuple := flatTuple(t, item)
			id, _ := item["id"].(string)
			subject, _ := item["subject"].(string)
			sensitive = append(sensitive, id, subject, `"uidvalidity"`, `"uid"`, strconv.FormatUint(uint64(tuple.UIDValidity), 10), strconv.FormatUint(uint64(tuple.UID), 10))
		}
		id, _ := results[0]["id"].(string)
		digest := rawObject(t, callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false, "sinceHours": 720}))
		for _, candidate := range rawItems(t, digest, "candidates") {
			flatTuple(t, candidate)
		}
		message := rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": id}))
		flatTuple(t, message)
		text, _ := message["text"].(string)
		headers, _ := message["headers"].(map[string]any)
		from, _ := headers["from"].(string)
		messageID, _ := headers["messageId"].(string)
		if !strings.Contains(text, "Synthetic") || from == "" || messageID == "" {
			t.Fatalf("get_message sentinels missing: %v", message)
		}
		sensitive = append(sensitive, strings.TrimSpace(text), from, messageID, "croton.test")
		flatTuple(t, rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": id})))

		// The get_thread aggregate stays context without a UID; each node
		// carries the exact tuple search listed for its own opaque key.
		searched := make(map[string]readTuple, len(results))
		for _, item := range results {
			key, _ := item["id"].(string)
			searched[key] = flatTuple(t, item)
		}
		thread := rawObject(t, callTool(t, session, "get_thread", map[string]any{"messageId": id}))
		requireNoIdentityKeys(t, "get_thread result", thread)
		nodes := rawItems(t, thread, "nodes")
		if len(nodes) == 0 {
			t.Fatal("get_thread returned no nodes")
		}
		for _, node := range nodes {
			key, _ := node["key"].(string)
			want, ok := searched[key]
			if !ok || flatTuple(t, node) != want {
				t.Fatalf("get_thread node %v lacks its own searched tuple %+v", node, want)
			}
		}

		requireReadOnlyTranscript(t, server)
		for _, command := range server.Commands() {
			if !command.TLS {
				t.Fatalf("fixture command used plaintext transport: %q", command.Raw)
			}
		}

		events := auditLines(t, &audit)
		if len(events) != 5 {
			t.Fatalf("audit events = %d, want 5: %s", len(events), audit.String())
		}
		for _, event := range events {
			requireAllowlistedKeys(t, event)
			if event["outcome"] != "ok" {
				t.Fatalf("unexpected audit event: %v", event)
			}
		}
		for _, value := range sensitive {
			if value != "" && strings.Contains(audit.String(), value) {
				t.Fatalf("audit recorded identity or content %q: %s", value, audit.String())
			}
		}

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		documentation := strings.Join(strings.Fields(string(contents)), " ")
		for _, statement := range []string{
			"Four flat read shapes carry numeric `uidvalidity` and `uid` beside the opaque `id` and the exact `mailbox`: each `search_mail` result, each `select_digest_candidates` candidate, and the parent message of `get_message` and `list_attachments`.",
			"Both values are positive 32-bit integers, 1 to 4294967295.",
			"They come from the same successful read-only EXAMINE and UID FETCH generation that the bridge bound into that `id`, so clients never decode the opaque id.",
			"Attachment entries, digest counters and the `get_thread` aggregate carry no UID.",
			"Account continuity is unavailable. The write pilot remains blocked.",
			"[Mail principal selection record](design/0007-mail-principal-selection.md)",
			"Each `get_thread` node carries its own `mailbox`, `uidvalidity` and `uid`, taken from the metadata already fetched for that member and associated by its opaque `key`.",
			"Bounded output keeps every retained identity exact.",
			"A truncated or fallback result lists fewer items, but every item it retains is a complete selectable identity.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing flat identity statement: %q", statement)
			}
		}
	})
}

// withReadIdentity sets the adapter tuple by reflection so this file compiles,
// and its assertions fail, while the exported fields are absent.
func withReadIdentity(metadata bridge.MessageMetadata, uidValidity, uid uint32) bridge.MessageMetadata {
	value := reflect.ValueOf(&metadata).Elem()
	if field := value.FieldByName("UIDValidity"); field.IsValid() {
		field.SetUint(uint64(uidValidity))
	}
	if field := value.FieldByName("UID"); field.IsValid() {
		field.SetUint(uint64(uid))
	}
	return metadata
}

// mapOrEmptyList lets an omitted optional list decode as empty.
func mapOrEmptyList(object map[string]any, key string) map[string]any {
	if _, ok := object[key]; ok {
		return object
	}
	return map[string]any{key: []any{}}
}

// requireCatalog pins the exact tool names and closed input properties, and
// keeps every new identity name out of read inputs.
func requireCatalog(t *testing.T, session *mcp.ClientSession, want map[string][]string, triage map[string][]string) {
	t.Helper()

	listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
	if err != nil {
		t.Fatalf("list tools: %v", err)
	}
	if len(listed.Tools) != len(want) {
		t.Fatalf("catalog has %d tools, want %d", len(listed.Tools), len(want))
	}

	for _, tool := range listed.Tools {
		inputs, ok := want[tool.Name]
		if !ok {
			t.Fatalf("unexpected tool %q", tool.Name)
		}
		schema, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal %s schema: %v", tool.Name, err)
		}
		var decoded struct {
			Properties           map[string]json.RawMessage `json:"properties"`
			AdditionalProperties *bool                      `json:"additionalProperties"`
		}
		if err := json.Unmarshal(schema, &decoded); err != nil {
			t.Fatalf("decode %s schema: %v", tool.Name, err)
		}
		properties := make([]string, 0, len(decoded.Properties))
		for name := range decoded.Properties {
			properties = append(properties, name)
		}
		slices.Sort(properties)
		if !slices.Equal(properties, inputs) || decoded.AdditionalProperties == nil || *decoded.AdditionalProperties {
			t.Fatalf("%s inputs = %v (additionalProperties %v), want closed %v", tool.Name, properties, decoded.AdditionalProperties, inputs)
		}

		_, isTriage := triage[tool.Name]
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint == isTriage {
			t.Fatalf("%s readOnlyHint changed: %+v", tool.Name, tool.Annotations)
		}
		lowered := strings.ToLower(string(schema))
		for _, forbidden := range []string{"epoch", "principal", "continuity", "account"} {
			if strings.Contains(lowered, forbidden) {
				t.Fatalf("%s schema gained %q: %s", tool.Name, forbidden, schema)
			}
		}
	}
}

// threadNodeKeys is the closed key set of one get_thread node.
var threadNodeKeys = map[string]bool{
	"key": true, "mailbox": true, "uidvalidity": true, "uid": true, "messageId": true,
	"parentKey": true, "depth": true, "subject": true, "from": true, "date": true,
}

func fakeThreadBody(subject, messageID, references string) []byte {
	return []byte("From: Fixture <fixture@croton.test>\r\nSubject: " + subject + "\r\nMessage-ID: " + messageID + "\r\n" + references + "\r\nSynthetic thread body.\r\n")
}

// decodeBounded decodes encodeBounded output with exact numbers and pins the
// byte cap and JSON validity.
func decodeBounded(t *testing.T, encoded []byte) map[string]any {
	t.Helper()

	if len(encoded) > maxToolResultBytes || !json.Valid(encoded) {
		t.Fatalf("bounded output is %d bytes or invalid JSON", len(encoded))
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var object map[string]any
	if err := decoder.Decode(&object); err != nil {
		t.Fatalf("decode bounded output: %v", err)
	}
	if truncated, _ := object["truncated"].(bool); !truncated {
		t.Fatalf("bounded output not marked truncated: %.200s", encoded)
	}
	return object
}

// requireRetainedIdentities checks each retained item against the source
// identity of the same opaque key, exactly and whole.
func requireRetainedIdentities(t *testing.T, label, keyField string, items []map[string]any, source map[string]readTuple, total int) {
	t.Helper()

	if len(items) == 0 || len(items) >= total {
		t.Fatalf("%s retained %d of %d items, want a whole-item shrink", label, len(items), total)
	}
	seen := make(map[string]bool, len(items))
	for _, item := range items {
		key, _ := item[keyField].(string)
		want, ok := source[key]
		if !ok || seen[key] {
			t.Fatalf("%s retained an unknown, duplicate or shortened %s %.80q", label, keyField, key)
		}
		seen[key] = true
		if got := flatTuple(t, item); got != want {
			t.Fatalf("%s identity for %.40q = %.80v, want exact %.80v", label, key, got, want)
		}
	}
}

func TestPublicReadIdentityThreadMCP(t *testing.T) {
	t.Parallel()

	t.Run("thread_members", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapterWithMessages(t, testkit.SyntheticLinkedThread())
		observed := observeFixtureTuples(t, server, "Message-ID")
		session := connectTestClient(t, Options{Mail: adapter})

		results := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "subject": "Synthetic planning"})), "results")
		if len(results) != 3 {
			t.Fatalf("fixture search = %v, want three messages", results)
		}
		ids := make(map[string]string)
		for _, item := range results {
			id, _ := item["id"].(string)
			message := rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": id}))
			headers, _ := message["headers"].(map[string]any)
			messageID, _ := headers["messageId"].(string)
			ids[messageID] = id
		}

		thread := rawObject(t, callTool(t, session, "get_thread", map[string]any{"messageId": ids["<reply@croton.test>"]}))
		requireNoIdentityKeys(t, "get_thread aggregate", thread)
		if truncated, _ := thread["truncated"].(bool); truncated {
			t.Fatalf("complete thread reported truncated: %v", thread)
		}

		nodes := rawItems(t, thread, "nodes")
		if len(nodes) != 3 {
			t.Fatalf("nodes = %v, want root, reply and subject sibling", nodes)
		}
		for _, node := range nodes {
			for key := range node {
				if !threadNodeKeys[key] {
					t.Fatalf("thread node gained field %q: %v", key, node)
				}
			}
			key, _ := node["key"].(string)
			messageID, _ := node["messageId"].(string)
			want, ok := observed[messageID]
			if !ok || ids[messageID] != key {
				t.Fatalf("node key %q does not belong to %q", key, messageID)
			}
			if got := flatTuple(t, node); got != want {
				t.Fatalf("node %s tuple = %+v, want its own fixture tuple %+v", messageID, got, want)
			}
		}
		requireReadOnlyTranscript(t, server)

		// A fake pins the operation budget and association by opaque key, even
		// when two members share one RFC Message-ID and search order differs
		// from node order.
		fake := &fakeMail{
			metadataByID: map[string]bridge.MessageMetadata{
				"opaque-target":    withReadIdentity(bridge.MessageMetadata{ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"}, 77, 500),
				"opaque-sibling":   withReadIdentity(bridge.MessageMetadata{ID: "opaque-sibling", Mailbox: "INBOX", Subject: "Budget"}, 77, 9),
				"opaque-duplicate": withReadIdentity(bridge.MessageMetadata{ID: "opaque-duplicate", Mailbox: "INBOX", Subject: "Budget"}, 77, 4294967295),
			},
			bodyByID: map[string][]byte{
				"opaque-target":    fakeThreadBody("Re: Budget", "<budget-reply@croton.test>", "References: <budget@croton.test>\r\n"),
				"opaque-sibling":   fakeThreadBody("Budget", "<budget@croton.test>", ""),
				"opaque-duplicate": fakeThreadBody("Budget", "<budget@croton.test>", ""),
			},
			searched: []bridge.MessageMetadata{
				{ID: "opaque-duplicate", Mailbox: "INBOX", Subject: "Budget"},
				{ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"},
				{ID: "opaque-sibling", Mailbox: "INBOX", Subject: "Budget"},
			},
		}
		fakeNodes := rawItems(t, rawObject(t, callTool(t, connectTestClient(t, Options{Mail: fake}), "get_thread", map[string]any{"messageId": "opaque-target"})), "nodes")
		if len(fakeNodes) != 3 {
			t.Fatalf("fake nodes = %v", fakeNodes)
		}
		for _, node := range fakeNodes {
			key, _ := node["key"].(string)
			source := fake.metadataByID[key]
			if got := flatTuple(t, node); got != (readTuple{Mailbox: source.Mailbox, UIDValidity: source.UIDValidity, UID: source.UID}) {
				t.Fatalf("node %q tuple = %+v, want its own %+v", key, got, source)
			}
		}
		if len(fake.searchCalls) != 1 || len(fake.metaCalls) != 3 || len(fake.bodyCalls) != 3 {
			t.Fatalf("operation budget changed: search=%d metadata=%d body=%d", len(fake.searchCalls), len(fake.metaCalls), len(fake.bodyCalls))
		}
	})

	t.Run("bounded_identity", func(t *testing.T) {
		t.Parallel()

		longMailbox := "Folders/" + strings.Repeat("m", 2000)
		longID := strings.Repeat("i", maxMessageIDArgumentBytes)
		bigSubject := strings.Repeat("s", 4000)

		t.Run("search and digest shrink whole items", func(t *testing.T) {
			var searched []bridge.MessageMetadata
			source := make(map[string]readTuple)
			for index := range maxSearchLimit {
				id := fmt.Sprintf("opaque-%03d-%s", index, strings.Repeat("k", 300))
				metadata := withReadIdentity(bridge.MessageMetadata{ID: id, Mailbox: longMailbox, Subject: bigSubject}, math.MaxUint32-uint32(index), uint32(index)+1)
				searched = append(searched, metadata)
				source[id] = readTuple{Mailbox: longMailbox, UIDValidity: metadata.UIDValidity, UID: metadata.UID}
			}
			session := connectTestClient(t, Options{Mail: &fakeMail{status: bridge.MailboxStatus{Messages: 250, Unseen: 250}, searched: searched}})

			for _, shape := range []struct {
				tool, field string
				arguments   map[string]any
				total       int
			}{
				{"search_mail", "results", map[string]any{"mailbox": "INBOX", "limit": maxSearchLimit}, maxSearchLimit},
				{"select_digest_candidates", "candidates", map[string]any{"mailbox": "INBOX", "limit": maxDigestLimit}, maxDigestLimit},
			} {
				result := callTool(t, session, shape.tool, shape.arguments)
				if text := resultText(t, result); len(text) > maxToolResultBytes {
					t.Fatalf("%s result = %d bytes", shape.tool, len(text))
				}
				object := rawObject(t, result)
				if truncated, _ := object["truncated"].(bool); !truncated {
					t.Fatalf("%s shrink not marked truncated", shape.tool)
				}
				requireRetainedIdentities(t, shape.tool, "id", rawItems(t, object, shape.field), source, shape.total)
			}
		})

		t.Run("message and attachment fallback keep exact identity", func(t *testing.T) {
			message := &getMessageResult{
				ID: longID, Mailbox: longMailbox, UIDValidity: math.MaxUint32, UID: 1, Size: 9,
				Headers:    bridge.CanonicalHeaders{Subject: strings.Repeat("h", maxToolResultBytes)},
				Text:       strings.Repeat("t", maxToolResultBytes),
				TextSource: "plain",
			}
			encoded, truncated, err := encodeBounded(message)
			if err != nil || !truncated {
				t.Fatalf("message fallback: truncated=%v err=%v", truncated, err)
			}
			object := decodeBounded(t, encoded)
			if object["id"] != longID || flatTuple(t, object) != (readTuple{Mailbox: longMailbox, UIDValidity: math.MaxUint32, UID: 1}) {
				t.Fatalf("message fallback altered identity: %.200s", encoded)
			}

			attachments := &listAttachmentsResult{ID: longID, Mailbox: longMailbox, UIDValidity: 2, UID: math.MaxUint32}
			for index := range 400 {
				attachments.Attachments = append(attachments.Attachments, attachmentResult{Filename: fmt.Sprintf("%03d-%s", index, strings.Repeat("f", 500))})
			}
			encoded, truncated, err = encodeBounded(attachments)
			if err != nil || !truncated {
				t.Fatalf("attachment shrink: truncated=%v err=%v", truncated, err)
			}
			object = decodeBounded(t, encoded)
			if object["id"] != longID || flatTuple(t, object) != (readTuple{Mailbox: longMailbox, UIDValidity: 2, UID: math.MaxUint32}) {
				t.Fatalf("attachment shrink altered identity: %.200s", encoded)
			}
			if parts := rawItems(t, object, "attachments"); len(parts) == 0 || len(parts) >= 400 {
				t.Fatalf("attachment shrink retained %d of 400", len(parts))
			}
		})

		t.Run("thread nodes shrink whole", func(t *testing.T) {
			thread := &getThreadResult{ID: longID, Mailbox: longMailbox}
			source := make(map[string]readTuple)
			for index := range maxThreadMessages {
				key := fmt.Sprintf("opaque-node-%02d-%s", index, strings.Repeat("n", 1000))
				tuple := readTuple{Mailbox: longMailbox, UIDValidity: 3001, UID: uint32(1000 + index*7)}
				thread.Nodes = append(thread.Nodes, threadNodeResult{Key: key, Mailbox: tuple.Mailbox, UIDValidity: tuple.UIDValidity, UID: tuple.UID, Subject: strings.Repeat("s", 8000)})
				source[key] = tuple
			}
			encoded, truncated, err := encodeBounded(thread)
			if err != nil || !truncated {
				t.Fatalf("thread shrink: truncated=%v err=%v", truncated, err)
			}
			object := decodeBounded(t, encoded)
			requireRetainedIdentities(t, "thread nodes", "key", rawItems(t, object, "nodes"), source, maxThreadMessages)
		})

		t.Run("unretainable identity is a static error", func(t *testing.T) {
			huge := "Folders/" + strings.Repeat("x", maxToolResultBytes)
			var audit bytes.Buffer
			metadata := withReadIdentity(bridge.MessageMetadata{ID: "opaque-huge", Mailbox: huge, Subject: "Huge identity fixture"}, 9, 9)
			session := connectTestClient(t, Options{Mail: &fakeMail{metadata: metadata, body: fakeThreadBody("Huge identity fixture", "<huge@croton.test>", "")}, Audit: NewAuditor(&audit)})
			for _, tool := range []string{"get_message", "list_attachments"} {
				result := callTool(t, session, tool, map[string]any{"messageId": "opaque-huge"})
				requireRefusedWithoutTuple(t, result, errBoundsExceeded)
				if strings.Contains(resultText(t, result), "Folders/") {
					t.Fatalf("%s refusal carried a mailbox", tool)
				}
			}
			for _, event := range auditLines(t, &audit) {
				requireAllowlistedKeys(t, event)
				if event["outcome"] != "error" || event["code"] != errBoundsExceeded {
					t.Fatalf("unexpected audit event: %v", event)
				}
			}
			if strings.Contains(audit.String(), "opaque-huge") || strings.Contains(audit.String(), "Folders/") {
				t.Fatalf("audit recorded the refused identity: %.200s", audit.String())
			}
		})
	})

	t.Run("invalid_thread_identity", func(t *testing.T) {
		t.Parallel()

		t.Run("changed UIDVALIDITY", func(t *testing.T) {
			server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{Stateful: publicIdentityStateful()})
			session := connectTestClient(t, Options{Mail: adapter})
			results := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
			if len(results) != 2 {
				t.Fatalf("search results = %v", results)
			}
			id, _ := results[0]["id"].(string)
			if err := server.ReplaceUIDValidity("INBOX", 4242); err != nil {
				t.Fatalf("replace fixture generation: %v", err)
			}

			before := len(server.Commands())
			requireRefusedWithoutTuple(t, callTool(t, session, "get_thread", map[string]any{"messageId": id}), errStaleID)
			requireNoBodyFetch(t, server.Commands()[before:])
			requireReadOnlyTranscript(t, server)
		})

		t.Run("stale sibling", func(t *testing.T) {
			// A sibling whose UID vanished is omitted without truncation; the
			// target keeps only its own tuple.
			fake := &fakeMail{
				metadataByID: map[string]bridge.MessageMetadata{
					"opaque-target": withReadIdentity(bridge.MessageMetadata{ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"}, 77, 500),
				},
				bodyByID: map[string][]byte{
					"opaque-target": fakeThreadBody("Re: Budget", "<budget-reply@croton.test>", ""),
				},
				searched: []bridge.MessageMetadata{
					{ID: "opaque-gone", Mailbox: "INBOX", Subject: "Budget"},
					{ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"},
				},
			}
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: fake}), "get_thread", map[string]any{"messageId": "opaque-target"}))
			nodes := rawItems(t, thread, "nodes")
			if len(nodes) != 1 || nodes[0]["key"] != "opaque-target" || flatTuple(t, nodes[0]) != (readTuple{Mailbox: "INBOX", UIDValidity: 77, UID: 500}) {
				t.Fatalf("stale sibling handling changed: %v", thread)
			}
			if truncated, _ := thread["truncated"].(bool); truncated {
				t.Fatalf("stale sibling omission now reports truncation: %v", thread)
			}
			if len(fake.metaCalls) != 2 || len(fake.bodyCalls) != 1 {
				t.Fatalf("stale sibling triggered extra reads: metadata=%v body=%v", fake.metaCalls, fake.bodyCalls)
			}
		})

		t.Run("zero tuple from test adapter", func(t *testing.T) {
			zeroThread := func() *fakeMail {
				return &fakeMail{
					metadataByID: map[string]bridge.MessageMetadata{
						"opaque-target": withReadIdentity(bridge.MessageMetadata{ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"}, 77, 500),
						"opaque-zero":   {ID: "opaque-zero", Mailbox: "INBOX", Subject: "Budget"},
					},
					bodyByID: map[string][]byte{
						"opaque-target": fakeThreadBody("Re: Budget", "<budget-reply@croton.test>", "References: <budget@croton.test>\r\n"),
						"opaque-zero":   fakeThreadBody("Budget", "<budget@croton.test>", ""),
					},
					searched: []bridge.MessageMetadata{{ID: "opaque-zero", Mailbox: "INBOX", Subject: "Budget"}, {ID: "opaque-target", Mailbox: "INBOX", Subject: "Re: Budget"}},
				}
			}

			target := zeroThread()
			requireRefusedWithoutTuple(t, callTool(t, connectTestClient(t, Options{Mail: target}), "get_thread", map[string]any{"messageId": "opaque-zero"}), errUnavailable)
			if len(target.searchCalls) != 0 {
				t.Fatalf("zero target still searched for siblings: %v", target.searchCalls)
			}

			sibling := zeroThread()
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: sibling}), "get_thread", map[string]any{"messageId": "opaque-target"}))
			nodes := rawItems(t, thread, "nodes")
			if len(nodes) != 1 || nodes[0]["key"] != "opaque-target" || flatTuple(t, nodes[0]) != (readTuple{Mailbox: "INBOX", UIDValidity: 77, UID: 500}) {
				t.Fatalf("zero-tuple sibling retained or borrowed a tuple: %v", thread)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("omitted zero-tuple sibling not reported truncated: %v", thread)
			}
			if len(sibling.metaCalls) != 2 || len(sibling.bodyCalls) != 2 || len(sibling.searchCalls) != 1 {
				t.Fatalf("zero-tuple sibling triggered extra reads: metadata=%v body=%v", sibling.metaCalls, sibling.bodyCalls)
			}
		})
	})

	t.Run("thread_contract_guards", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapterWithMessages(t, testkit.SyntheticLinkedThread())
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		results := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
		if len(results) != 3 {
			t.Fatalf("search results = %v", results)
		}
		sensitive := []string{`"uidvalidity"`, `"uid"`, "croton.test", "Synthetic planning"}
		for _, item := range results {
			id, _ := item["id"].(string)
			sensitive = append(sensitive, id)
			for _, node := range rawItems(t, rawObject(t, callTool(t, session, "get_thread", map[string]any{"messageId": id})), "nodes") {
				tuple := flatTuple(t, node)
				messageID, _ := node["messageId"].(string)
				sensitive = append(sensitive, messageID, strconv.FormatUint(uint64(tuple.UIDValidity), 10), strconv.FormatUint(uint64(tuple.UID), 10))
			}
		}
		requireReadOnlyTranscript(t, server)

		events := auditLines(t, &audit)
		if len(events) != 4 {
			t.Fatalf("audit events = %d, want 4: %s", len(events), audit.String())
		}
		for _, event := range events {
			requireAllowlistedKeys(t, event)
			if event["outcome"] != "ok" {
				t.Fatalf("unexpected audit event: %v", event)
			}
		}
		for _, value := range sensitive {
			if value != "" && strings.Contains(audit.String(), value) {
				t.Fatalf("audit recorded identity or content %q: %s", value, audit.String())
			}
		}

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		documentation := strings.Join(strings.Fields(string(contents)), " ")
		for _, statement := range []string{
			"Each `get_thread` node carries its own `mailbox`, `uidvalidity` and `uid`, taken from the metadata already fetched for that member and associated by its opaque `key`.",
			"A node never borrows the target's tuple, an ordinal or its RFC `messageId`, and no extra read reconstructs a missing tuple.",
			"The thread's top-level `id` and `mailbox` are context and carry no UID.",
			"A stale sibling is omitted as before, and a sibling without a positive tuple is omitted and the result is marked truncated.",
			"Oversized search results, digest candidates and thread nodes are removed whole, so each retained item keeps its exact tuple, mailbox and opaque id.",
			"when even that cannot fit, the call fails with `bounds_exceeded` instead of returning a shortened identity.",
			"An empty fallback list contains no selectable item",
			"They are not account authentication or approval, grant no write authority, and different accounts may share the same tuple.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing thread identity statement: %q", statement)
			}
		}
		for _, superseded := range []string{"are deferred", "not complete selection snapshots"} {
			if strings.Contains(documentation, superseded) {
				t.Errorf("Mail documentation keeps the superseded interim clause %q", superseded)
			}
		}
	})
}

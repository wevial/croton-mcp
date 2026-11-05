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
// over its own verified loopback TLS connection, keyed by subject, without
// any adapter or opaque identifier.
func observeFixtureTuples(t *testing.T, server *testkit.Server) map[string]readTuple {
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
		tuples[parsed.Header.Get("Subject")] = readTuple{Mailbox: "INBOX", UIDValidity: uint32(uidValidity), UID: uint32(uid)}
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
		observed := observeFixtureTuples(t, server)
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
		flatTuple(t, rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": id})))
		flatTuple(t, rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": id})))

		// get_thread keeps its existing meaning: no node, root or parent gains
		// a UID in this interim change.
		thread := rawObject(t, callTool(t, session, "get_thread", map[string]any{"messageId": id}))
		requireNoIdentityKeys(t, "get_thread result", thread)
		for _, node := range rawItems(t, thread, "nodes") {
			requireNoIdentityKeys(t, "get_thread node", node)
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
			"Attachment entries, digest counters and `get_thread` results carry no UID.",
			"Account continuity is unavailable. The write pilot remains blocked.",
			"[Mail principal selection record](design/0007-mail-principal-selection.md)",
			"Thread identities and exact bounded-output identities are deferred.",
			"Truncated and fallback results are not complete selection snapshots.",
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

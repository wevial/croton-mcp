package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// wireItem is one search result or digest candidate with its closed
// dateSource and readState tokens kept as text.
type wireItem struct {
	ID                string `json:"id"`
	Mailbox           string `json:"mailbox"`
	UIDValidity       uint32 `json:"uidvalidity"`
	UID               uint32 `json:"uid"`
	Subject           string `json:"subject"`
	Size              int64  `json:"size"`
	From              string `json:"from"`
	Date              string `json:"date"`
	DateSource        string `json:"dateSource"`
	ReadState         string `json:"readState"`
	MetadataTruncated bool   `json:"metadataTruncated"`
}

type wireSearch struct {
	Results    []wireItem `json:"results"`
	Truncated  bool       `json:"truncated"`
	NextCursor string     `json:"nextCursor"`
}

type wireDigest struct {
	Mailbox       string     `json:"mailbox"`
	Candidates    []wireItem `json:"candidates"`
	TotalMessages int        `json:"totalMessages"`
	UnseenCount   int        `json:"unseenCount"`
	Truncated     bool       `json:"truncated"`
}

// The fixture mailbox: the linked thread at UIDs 101-103 (root read, reply,
// unrelated same-subject message), then the attachment report at 104 and
// the attachment-free message at 105, all in UIDVALIDITY 9001.
const (
	uidRoot       = 101
	uidReply      = 102
	uidUnrelated  = 103
	uidReport     = 104
	uidAttachFree = 105
)

func wireFixture() testkit.Options {
	text := func() *testkit.BodyPart {
		return &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 35, Lines: 1}
	}
	report := &testkit.BodyPart{
		Kind: testkit.BodyPartMultipart, Subtype: "mixed", Params: []testkit.BodyParam{{Name: "boundary", Value: "synthetic-report"}},
		Children: []*testkit.BodyPart{text(), {
			Kind: testkit.BodyPartBasic, Type: "application", Subtype: "pdf",
			Params: []testkit.BodyParam{{Name: "name", Value: "report.pdf"}}, Encoding: "base64", Size: 40,
			Disposition: &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: "report.pdf"}}},
		}},
	}

	return testkit.Options{
		Messages:       append(testkit.SyntheticLinkedThread(), testkit.SyntheticAttachmentMessages()...),
		Seen:           []bool{true},
		BodyStructures: []*testkit.BodyPart{text(), text(), text(), report, text()},
	}
}

func startIntegrationAdapter(t *testing.T, options testkit.Options, bounds ...bridge.BoundsPatch) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	options.Mode = testkit.ImplicitTLS
	server, err := testkit.Start(options)
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	var patch bridge.BoundsPatch
	if len(bounds) > 0 {
		patch = bounds[0]
	}
	adapter, err := bridge.NewAdapter(bridge.Config{Bounds: patch, IMAP: bridge.IMAPConfig{
		Host:              host,
		Port:              port,
		TLSMode:           bridge.TLSModeImplicit,
		CredentialCommand: []string{executable, "-test.run=TestMCPCredentialHelperProcess", "--", "valid"},
		TLS:               bridge.TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  2000,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

func TestMCPCredentialHelperProcess(t *testing.T) {
	if !strings.Contains(strings.Join(os.Args, " "), "TestMCPCredentialHelperProcess") {
		return
	}
	for index, value := range os.Args {
		if value == "--" && index+1 < len(os.Args) && os.Args[index+1] == "valid" {
			fmt.Fprint(os.Stdout, `{"username":"fixture-user","password":"fixture-password"}`)
			// Exit before the test framework can print PASS to stdout: the
			// credential parser strictly rejects trailing output.
			os.Exit(0)
		}
	}
}

// requireWire checks every command since before: TLS only, read-only, and a
// UID FETCH that reads body or part bytes only when bodyAllowed. It returns the
// commands for further checks.
func requireWire(t *testing.T, server *testkit.Server, before int, bodyAllowed bool) []testkit.Command {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatal(err)
	}

	commands := server.Commands()[before:]
	for _, command := range commands {
		upper := strings.ToUpper(command.Raw)
		if !command.TLS {
			t.Fatalf("command used plaintext transport: %q", command.Raw)
		}
		for _, field := range strings.Fields(upper) {
			switch field {
			case "STORE", "APPEND", "EXPUNGE", "DELETE", "RENAME", "CREATE", "COPY", "MOVE", "SUBSCRIBE", "UNSUBSCRIBE":
				t.Fatalf("mutating IMAP command reached the wire: %q", command.Raw)
			}
		}
		if strings.Contains(upper, "FETCH") && !bodyAllowed && fetchesContent(upper) {
			t.Fatalf("metadata-only read fetched message bytes: %q", command.Raw)
		}
	}

	return commands
}

var (
	fetchSection   = regexp.MustCompile(`(?:BODY|BINARY)(?:\.PEEK)?\[([^\]]*)\]`)
	headerSection  = regexp.MustCompile(`^(?:\d+\.)*(?:HEADER|MIME)`)
	wholeOrTextRFC = regexp.MustCompile(`(?:^|[ (])RFC822(?:\.TEXT)?(?:[ )]|$)`)
)

// fetchesContent reports whether an upper-cased FETCH asks for message or
// part bytes: any section other than a header or MIME header, or RFC822 and
// RFC822.TEXT.
func fetchesContent(upper string) bool {
	for _, match := range fetchSection.FindAllStringSubmatch(upper, -1) {
		if !headerSection.MatchString(match[1]) {
			return true
		}
	}

	return wholeOrTextRFC.MatchString(upper)
}

func callJSON(t *testing.T, session *mcp.ClientSession, name string, arguments map[string]any, target any) []byte {
	t.Helper()

	result := callTool(t, session, name, arguments)
	decodeResult(t, result, target)

	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal SDK result: %v", err)
	}
	return raw
}

func requireItems(t *testing.T, label string, items []wireItem, uids ...uint32) map[uint32]string {
	t.Helper()

	if items == nil || len(items) != len(uids) {
		t.Fatalf("%s = %+v, want UIDs %v", label, items, uids)
	}
	ids := make(map[uint32]string, len(items))
	for index, item := range items {
		if item.ID == "" || item.Mailbox != "INBOX" || item.UIDValidity != 9001 || item.UID != uids[index] || item.Size <= 0 {
			t.Fatalf("%s[%d] identity = %+v, want INBOX/9001/%d", label, index, item, uids[index])
		}
		if item.From != "Fixture <fixture@croton.test>" || item.DateSource != "header" || !strings.HasPrefix(item.Date, "2026-01-01T00:00:0") || item.MetadataTruncated {
			t.Fatalf("%s[%d] metadata = %+v", label, index, item)
		}
		wantState := "unread"
		if item.UID == uidRoot {
			wantState = "read"
		}
		if item.ReadState != wantState {
			t.Fatalf("%s[%d] readState = %q, want %q", label, index, item.ReadState, wantState)
		}
		ids[item.UID] = item.ID
	}
	return ids
}

func TestReadToolsOverTheWire(t *testing.T) {
	t.Parallel()

	server, adapter := startIntegrationAdapter(t, wireFixture())
	var audit bytes.Buffer
	session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})
	privateSentinels := []string{
		testkit.SyntheticAttachmentDecoded,
		base64.StdEncoding.EncodeToString([]byte(testkit.SyntheticAttachmentDecoded)),
		"fixture-user", "fixture-password",
	}
	bodySentinels := append([]string{testkit.SyntheticAttachmentBody, testkit.SyntheticAttachmentFreeBody, "Synthetic reply message."}, privateSentinels...)
	requireAbsent := func(t *testing.T, label string, raw []byte, sentinels []string) {
		t.Helper()
		for _, sentinel := range sentinels {
			if bytes.Contains(raw, []byte(sentinel)) {
				t.Fatalf("%s leaked %q", label, sentinel)
			}
		}
	}

	var folders listFoldersResult
	callJSON(t, session, "list_folders", map[string]any{}, &folders)
	if len(folders.Folders) != 1 || folders.Folders[0].Name != "INBOX" || folders.Truncated {
		t.Fatalf("list_folders = %+v", folders)
	}

	var ids map[uint32]string
	t.Run("search_mail", func(t *testing.T) {
		before := len(server.Commands())

		var all, unread, bounded wireSearch
		requireAbsent(t, "search_mail", callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &all), bodySentinels)
		callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true}, &unread)
		callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 2}, &bounded)
		requireWire(t, server, before, false)

		ids = requireItems(t, "search", all.Results, 105, 104, 103, 102, 101)
		requireItems(t, "unread search", unread.Results, 105, 104, 103, 102)
		requireItems(t, "bounded search", bounded.Results, 105, 104)
		if all.Truncated || unread.Truncated || !bounded.Truncated {
			t.Fatalf("search truncation = %v/%v/%v, want false/false/true", all.Truncated, unread.Truncated, bounded.Truncated)
		}
	})
	if ids == nil {
		t.FailNow()
	}

	t.Run("select_digest_candidates", func(t *testing.T) {
		before := len(server.Commands())

		var all, unread, bounded wireDigest
		requireAbsent(t, "digest", callJSON(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false, "sinceHours": 168}, &all), bodySentinels)
		callJSON(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"}, &unread)
		callJSON(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false, "limit": 1}, &bounded)
		commands := requireWire(t, server, before, false)

		requireItems(t, "digest", all.Candidates, 105, 104, 103, 102, 101)
		requireItems(t, "unread digest", unread.Candidates, 105, 104, 103, 102)
		requireItems(t, "bounded digest", bounded.Candidates, 105)
		for _, digest := range []wireDigest{all, unread, bounded} {
			if digest.Mailbox != "INBOX" || digest.TotalMessages != 5 || digest.UnseenCount != 4 {
				t.Fatalf("digest summary = %+v", digest)
			}
		}
		if all.Truncated || unread.Truncated || !bounded.Truncated {
			t.Fatalf("digest truncation = %v/%v/%v, want false/false/true", all.Truncated, unread.Truncated, bounded.Truncated)
		}
		statuses := 0
		for _, command := range commands {
			upper := strings.ToUpper(command.Raw)
			if command.Name == "STATUS" {
				statuses++
			}
			if strings.Contains(upper, "UID SEARCH") && (!strings.Contains(upper, " SINCE ") || !strings.Contains(upper, " BEFORE ")) {
				t.Fatalf("digest search lacks its date window: %q", command.Raw)
			}
		}
		if statuses != 3 {
			t.Fatalf("digest STATUS commands = %d, want 3", statuses)
		}
	})

	t.Run("get_message", func(t *testing.T) {
		before := len(server.Commands())

		var message getMessageResult
		requireAbsent(t, "get_message", callJSON(t, session, "get_message", map[string]any{"messageId": ids[uidReport]}, &message), privateSentinels)
		requireWire(t, server, before, true)

		if message.ID != ids[uidReport] || message.Mailbox != "INBOX" || message.UIDValidity != 9001 || message.UID != uidReport || message.Truncated {
			t.Fatalf("get_message identity = %+v", message)
		}
		if message.Headers.Subject != "Synthetic attachment report" || message.Headers.MessageID != "<attachment-report@croton.test>" || !strings.Contains(message.Text, testkit.SyntheticAttachmentBody) {
			t.Fatalf("get_message content = %+v", message)
		}
		if len(message.Attachments) != 1 || message.Attachments[0].Filename != "report.pdf" || message.Attachments[0].ContentType != "application/pdf" {
			t.Fatalf("get_message attachments = %+v", message.Attachments)
		}
	})

	t.Run("get_thread", func(t *testing.T) {
		for _, limit := range []int{10, 1} {
			before := len(server.Commands())

			var thread getThreadResult
			requireAbsent(t, "get_thread", callJSON(t, session, "get_thread", map[string]any{"messageId": ids[uidReply], "maxMessages": limit}, &thread), bodySentinels)
			commands := requireWire(t, server, before, false)

			if thread.ID != ids[uidReply] || thread.Mailbox != "INBOX" || thread.UnavailableMembers != nil {
				t.Fatalf("thread identity = %+v", thread)
			}
			parents := map[string]string{}
			for _, node := range thread.Nodes {
				if node.Mailbox != "INBOX" || node.UIDValidity != 9001 || ids[node.UID] != node.Key {
					t.Fatalf("thread node identity = %+v", node)
				}
				parents[node.Key] = node.ParentKey
			}
			want := map[string]string{ids[uidRoot]: "", ids[uidReply]: ids[uidRoot], ids[uidUnrelated]: ""}
			if limit == 1 {
				want = map[string]string{ids[uidReply]: ""}
			}
			if fmt.Sprint(parents) != fmt.Sprint(want) || thread.Truncated != (limit == 1) {
				t.Fatalf("maxMessages=%d thread = %+v, want parents %v", limit, thread, want)
			}

			headerReads := 0
			for _, command := range commands {
				upper := strings.ToUpper(command.Raw)
				if strings.Contains(upper, "UID FETCH") && strings.Contains(upper, "BODY.PEEK[HEADER]") && !strings.ContainsAny(strings.Fields(upper)[3], ":,*") {
					headerReads++
				}
			}
			if headerReads == 0 || headerReads > limit {
				t.Fatalf("maxMessages=%d made %d selected header reads", limit, headerReads)
			}
		}
	})

	t.Run("list_attachments", func(t *testing.T) {
		for _, tc := range []struct {
			uid   uint32
			count int
		}{{uidReport, 1}, {uidAttachFree, 0}} {
			before := len(server.Commands())

			var listed listAttachmentsResult
			requireAbsent(t, "list_attachments", callJSON(t, session, "list_attachments", map[string]any{"messageId": ids[tc.uid]}, &listed), bodySentinels)
			commands := requireWire(t, server, before, false)

			if listed.ID != ids[tc.uid] || listed.Mailbox != "INBOX" || listed.UIDValidity != 9001 || listed.UID != tc.uid || listed.Truncated {
				t.Fatalf("list_attachments identity = %+v", listed)
			}
			if listed.Attachments == nil || len(listed.Attachments) != tc.count {
				t.Fatalf("list_attachments(%d) = %+v, want %d entries", tc.uid, listed.Attachments, tc.count)
			}
			if tc.count == 1 && (listed.Attachments[0].Filename != "report.pdf" || listed.Attachments[0].Disposition != "attachment" || listed.Attachments[0].HasDeclaredSize) {
				t.Fatalf("attachment metadata = %+v", listed.Attachments[0])
			}
			structure := false
			for _, command := range commands {
				structure = structure || strings.Contains(strings.ToUpper(command.Raw), "BODYSTRUCTURE")
			}
			if !structure {
				t.Fatal("list_attachments did not read BODYSTRUCTURE")
			}
		}
	})

	requireAbsent(t, "audit", audit.Bytes(), append(bodySentinels, "INBOX", "Synthetic"))
	for _, event := range auditLines(t, &audit) {
		requireAllowlistedKeys(t, event)
		if event["event"] != "tool_call" || event["outcome"] != "ok" {
			t.Fatalf("unexpected audit event: %v", event)
		}
	}
}

func TestReadToolsEmptyOverTheWire(t *testing.T) {
	t.Parallel()

	// The fixture LIST always names INBOX, so an empty folder list comes
	// from an adapter that reports none.
	var folders listFoldersResult
	empty := connectTestClient(t, Options{Mail: &fakeMail{}})
	if raw := callJSON(t, empty, "list_folders", map[string]any{}, &folders); !bytes.Contains(raw, []byte(`{\"folders\":[]}`)) {
		t.Fatalf("empty folder list is not an empty array: %s", raw)
	}

	options := wireFixture()
	options.Messages, options.BodyStructures = options.Messages[4:], options.BodyStructures[4:]
	server, adapter := startIntegrationAdapter(t, options)
	session := connectTestClient(t, Options{Mail: adapter})

	var all, unread wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &all)
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true}, &unread)
	if len(all.Results) != 1 || unread.Results == nil || len(unread.Results) != 0 || unread.Truncated {
		t.Fatalf("search = %+v, unread = %+v; want one message and an empty unread page", all, unread)
	}
	id := all.Results[0].ID

	var digest wireDigest
	callJSON(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"}, &digest)
	if digest.Candidates == nil || len(digest.Candidates) != 0 || digest.TotalMessages != 1 || digest.UnseenCount != 0 || digest.Truncated {
		t.Fatalf("empty digest = %+v", digest)
	}

	var thread getThreadResult
	callJSON(t, session, "get_thread", map[string]any{"messageId": id}, &thread)
	if len(thread.Nodes) != 1 || thread.Nodes[0].Key != id || thread.Truncated {
		t.Fatalf("single-message thread = %+v", thread)
	}

	var message getMessageResult
	callJSON(t, session, "get_message", map[string]any{"messageId": id}, &message)
	if message.Attachments != nil || message.Truncated || message.TextSource != "plain" {
		t.Fatalf("attachment-free message = %+v", message)
	}

	var listed listAttachmentsResult
	if raw := callJSON(t, session, "list_attachments", map[string]any{"messageId": id}, &listed); !bytes.Contains(raw, []byte(`\"attachments\":[]`)) {
		t.Fatalf("empty attachment list is not an empty array: %s", raw)
	}

	requireWire(t, server, 0, true)
}

func TestReadToolsBoundsOverTheWire(t *testing.T) {
	t.Parallel()

	// UID 101 fits the 256 KiB body budget but not the result budget; UID
	// 102 exceeds the body budget.
	message := func(kib int) string {
		return "From: Fixture <fixture@croton.test>\r\nSubject: Synthetic oversize\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\n" +
			"Content-Type: text/plain; charset=utf-8\r\n\r\n" + strings.Repeat("Synthetic oversize line of plain text.\r\n", (kib<<10)/40)
	}
	server, adapter := startIntegrationAdapter(t, testkit.Options{Messages: []string{message(150), message(300)}, Scenario: testkit.Scenario{ListResponseCount: 201}})
	session := connectTestClient(t, Options{Mail: adapter})

	requireToolError(t, callTool(t, session, "list_folders", map[string]any{}), errBoundsExceeded)

	var search wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &search)
	if len(search.Results) != 2 || search.Results[1].UID != 101 {
		t.Fatalf("search = %+v", search)
	}

	result := callTool(t, session, "get_message", map[string]any{"messageId": search.Results[1].ID})
	var decoded getMessageResult
	decodeResult(t, result, &decoded)
	if len(resultText(t, result)) > maxToolResultBytes || !decoded.Truncated || !decoded.Truncation.Text && !decoded.Truncation.Output || decoded.UID != 101 {
		t.Fatalf("oversize message: %d bytes, truncation %+v", len(resultText(t, result)), decoded.Truncation)
	}
	requireToolError(t, callTool(t, session, "get_message", map[string]any{"messageId": search.Results[0].ID}), errBoundsExceeded)

	for _, tc := range []struct {
		tool      string
		arguments map[string]any
	}{
		{"search_mail", map[string]any{"mailbox": "INBOX", "limit": 0}},
		{"search_mail", map[string]any{"mailbox": strings.Repeat("m", maxMailboxArgumentBytes+1)}},
		{"search_mail", map[string]any{"mailbox": "INBOX", "subject": strings.Repeat("s", maxSearchTermBytes+1)}},
		{"search_mail", map[string]any{"mailbox": "INBOX", "since": "yesterday"}},
		{"select_digest_candidates", map[string]any{"mailbox": "INBOX", "sinceHours": -1}},
		{"get_message", map[string]any{"messageId": strings.Repeat("x", maxMessageIDArgumentBytes+1)}},
		{"get_thread", map[string]any{"messageId": search.Results[0].ID, "maxMessages": 0}},
		{"list_attachments", map[string]any{"messageId": ""}},
		{"list_folders", map[string]any{"pattern": "*"}},
	} {
		requireToolError(t, callTool(t, session, tc.tool, tc.arguments), errInvalidArgument)
	}

	requireWire(t, server, 0, true)

	// A sender-built structure with 21 attachments keeps the first 20 and
	// names the attachment bound.
	parts := []*testkit.BodyPart{{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 4, Lines: 1}}
	for index := range 21 {
		name := []testkit.BodyParam{{Name: "filename", Value: fmt.Sprintf("part-%02d.bin", index)}}
		parts = append(parts, &testkit.BodyPart{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "octet-stream", Size: 4, Disposition: &testkit.BodyDisposition{Value: "attachment", Params: name}})
	}
	_, adapter = startIntegrationAdapter(t, testkit.Options{
		Messages:       []string{"From: Fixture <fixture@croton.test>\r\nSubject: Synthetic many parts\r\nContent-Type: multipart/mixed; boundary=many\r\n\r\n--many--\r\n"},
		BodyStructures: []*testkit.BodyPart{{Kind: testkit.BodyPartMultipart, Subtype: "mixed", Params: []testkit.BodyParam{{Name: "boundary", Value: "many"}}, Children: parts}},
	})
	session = connectTestClient(t, Options{Mail: adapter})
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &search)
	if len(search.Results) != 1 {
		t.Fatalf("many-part search = %+v", search)
	}
	var listed listAttachmentsResult
	callJSON(t, session, "list_attachments", map[string]any{"messageId": search.Results[0].ID}, &listed)
	if len(listed.Attachments) != 20 || listed.Attachments[19].Filename != "part-19.bin" || listed.Truncation != (attachmentListTruncation{Attachments: true}) || !listed.Truncated {
		t.Fatalf("21 attachments = %d entries, truncation %+v (truncated %v)", len(listed.Attachments), listed.Truncation, listed.Truncated)
	}
}

func TestSearchMailCursorPages(t *testing.T) {
	t.Parallel()

	// 120 matching messages take UIDs 101-220.
	messages := make([]string, 120)
	for index := range messages {
		messages[index] = fmt.Sprintf("From: Fixture <fixture@croton.test>\r\nSubject: Synthetic page %d\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\n\r\nSynthetic page body.\r\n", index)
	}
	server, adapter := startIntegrationAdapter(t, testkit.Options{Messages: messages})
	session := connectTestClient(t, Options{Mail: adapter})

	var pages []wireSearch
	next := uint32(220)
	arguments := map[string]any{"mailbox": "INBOX", "limit": 50}
	for index, size := range []int{50, 50, 20} {
		var page wireSearch
		callJSON(t, session, "search_mail", arguments, &page)
		if len(page.Results) != size {
			t.Fatalf("page %d has %d results, want %d", index, len(page.Results), size)
		}
		for _, item := range page.Results {
			if item.UID != next {
				t.Fatalf("page %d UID %d, want %d", index, item.UID, next)
			}
			next--
		}
		last := index == 2
		if page.Truncated == last || (page.NextCursor == "") != last {
			t.Fatalf("page %d truncated %v, nextCursor %q", index, page.Truncated, page.NextCursor)
		}
		pages = append(pages, page)
		arguments = map[string]any{"mailbox": "INBOX", "limit": 50, "cursor": page.NextCursor}
	}

	cursor := pages[0].NextCursor
	tampered := []byte(cursor)
	tampered[len(tampered)/2] ^= 1
	before := len(server.Commands())
	for _, arguments := range []map[string]any{
		{"mailbox": "Archive", "cursor": cursor},
		{"mailbox": "INBOX", "unreadOnly": true, "cursor": cursor},
		{"mailbox": "INBOX", "subject": "Synthetic", "cursor": cursor},
		{"mailbox": "INBOX", "cursor": string(tampered)},
		{"mailbox": "INBOX", "cursor": pages[0].Results[0].ID},
	} {
		requireToolError(t, callTool(t, session, "search_mail", arguments), errInvalidArgument)
	}
	if commands := server.Commands()[before:]; len(commands) != 0 {
		t.Fatalf("rejected cursors reached the wire: %+v", commands)
	}
	requireWire(t, server, 0, false)

	// The generation changes after the first page's EXAMINE.
	_, adapter = startIntegrationAdapter(t, testkit.Options{Messages: messages, Scenario: testkit.Scenario{UIDValidityAfterExamine: 9002}})
	session = connectTestClient(t, Options{Mail: adapter})
	var first wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 50}, &first)
	requireToolError(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 50, "cursor": first.NextCursor}), errStaleID)

	// Pages the output budget shrinks still resume below their last result.
	for index := range messages {
		messages[index] = fmt.Sprintf("From: Fixture <fixture@croton.test>\r\nSubject: Synthetic page %d %s\r\n\r\nSynthetic page body.\r\n", index, strings.Repeat("s", 3000))
	}
	_, adapter = startIntegrationAdapter(t, testkit.Options{Messages: messages}, bridge.BoundsPatch{MaxOutputBytes: bridge.Int(400000)})
	session = connectTestClient(t, Options{Mail: adapter})
	next, shrunk := uint32(220), false
	arguments = map[string]any{"mailbox": "INBOX", "limit": 50}
	for calls := 0; ; calls++ {
		if calls == 20 {
			t.Fatal("shrunk pages did not finish")
		}
		var page wireSearch
		callJSON(t, session, "search_mail", arguments, &page)
		shrunk = shrunk || len(page.Results) < 50 && page.NextCursor != ""
		for _, item := range page.Results {
			if item.UID != next {
				t.Fatalf("shrunk page UID %d, want %d", item.UID, next)
			}
			next--
		}
		if page.NextCursor == "" {
			break
		}
		arguments = map[string]any{"mailbox": "INBOX", "limit": 50, "cursor": page.NextCursor}
	}
	if next != 100 || !shrunk {
		t.Fatalf("shrunk pages ended at UID %d (shrunk %v), want every match", next+1, shrunk)
	}

	// A single result over the output budget keeps its identity and cursor.
	oversized := []string{messages[0], "From: Fixture <fixture@croton.test>\r\nSubject: " + strings.Repeat("<", 20000) + "\r\n\r\nSynthetic page body.\r\n"}
	_, adapter = startIntegrationAdapter(t, testkit.Options{Messages: oversized}, bridge.BoundsPatch{MaxHeaderBytes: bridge.Int(65536), MaxOutputBytes: bridge.Int(400000)})
	session = connectTestClient(t, Options{Mail: adapter})
	var single, rest wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 1}, &single)
	if len(single.Results) != 1 || single.Results[0].UID != 102 || !single.Truncated || single.NextCursor == "" {
		t.Fatalf("oversized single result = %d results, truncated %v, cursor %q", len(single.Results), single.Truncated, single.NextCursor)
	}
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 1, "cursor": single.NextCursor}, &rest)
	if len(rest.Results) != 1 || rest.Results[0].UID != 101 {
		t.Fatalf("page after oversized result has %d results", len(rest.Results))
	}

	// An oversized terminal result is cut, not the page.
	_, adapter = startIntegrationAdapter(t, testkit.Options{Messages: oversized[1:]}, bridge.BoundsPatch{MaxHeaderBytes: bridge.Int(65536), MaxOutputBytes: bridge.Int(400000)})
	session = connectTestClient(t, Options{Mail: adapter})
	var terminal wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &terminal)
	if len(terminal.Results) != 1 || !terminal.Results[0].MetadataTruncated || terminal.Truncated || terminal.NextCursor != "" {
		t.Fatalf("oversized terminal result = %d results, truncated %v, cursor %q", len(terminal.Results), terminal.Truncated, terminal.NextCursor)
	}

	// A limit below the configured bound still examines every window, so a
	// lone match ends the search.
	seenFlags := make([]bool, 100)
	for index := range 99 {
		seenFlags[index] = true
	}
	_, adapter = startIntegrationAdapter(t, testkit.Options{Messages: messages[:100], Seen: seenFlags})
	session = connectTestClient(t, Options{Mail: adapter})
	var lone wireSearch
	callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true, "limit": 1}, &lone)
	if len(lone.Results) != 1 || lone.Results[0].UID != 200 || lone.Truncated || lone.NextCursor != "" {
		t.Fatalf("lone match = %d results, truncated %v, cursor %q", len(lone.Results), lone.Truncated, lone.NextCursor)
	}
}

func TestReadToolsUnavailableOverTheWire(t *testing.T) {
	t.Parallel()

	t.Run("stale_id", func(t *testing.T) {
		t.Parallel()

		// The generation changes after the search's EXAMINE; every selected
		// read refuses before any FETCH, so no BODYSTRUCTURE is needed.
		options := wireFixture()
		options.BodyStructures, options.Scenario.UIDValidityAfterExamine = nil, 9002
		_, adapter := startIntegrationAdapter(t, options)
		session := connectTestClient(t, Options{Mail: adapter})

		var search wireSearch
		callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &search)
		if len(search.Results) == 0 {
			t.Fatal("fixture search is empty")
		}
		for _, tool := range []string{"get_message", "get_thread", "list_attachments"} {
			requireToolError(t, callTool(t, session, tool, map[string]any{"messageId": search.Results[0].ID}), errStaleID)
		}
	})

	t.Run("bridge_gone", func(t *testing.T) {
		t.Parallel()

		server, adapter := startIntegrationAdapter(t, wireFixture())
		session := connectTestClient(t, Options{Mail: adapter})
		var search wireSearch
		callJSON(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}, &search)
		if len(search.Results) == 0 {
			t.Fatal("fixture search is empty")
		}
		if err := server.Close(); err != nil {
			t.Fatalf("close fake server: %v", err)
		}

		for tool, arguments := range readCalls {
			if arguments["messageId"] != nil {
				arguments = map[string]any{"messageId": search.Results[0].ID}
			}
			requireToolError(t, callTool(t, session, tool, arguments), errUnavailable)
		}
	})
}

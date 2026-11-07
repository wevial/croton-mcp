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
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAttachmentsSyntheticWire(t *testing.T) {
	t.Parallel()

	// The helper uses only loopback test TLS and the test executable's
	// synthetic credential helper; all mail is supplied by this named fixture
	// and its explicit, independently written BODYSTRUCTURE descriptors.
	server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{
		Messages:       testkit.SyntheticAttachmentMessages(),
		BodyStructures: syntheticAttachmentDescriptors(),
	})
	var audit bytes.Buffer
	session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

	var search searchMailDecoded
	decodeResult(t, callTool(t, session, "search_mail", map[string]any{
		"mailbox": "INBOX", "subject": "Synthetic attachment",
	}), &search)
	if len(search.Results) != 2 || search.Truncated {
		t.Fatalf("want both synthetic messages, got %+v", search)
	}

	ids := make(map[string]string)
	for _, candidate := range search.Results {
		if candidate.ID == "" || ids[candidate.Subject] != "" {
			t.Fatal("missing opaque ID or duplicate fixture subject")
		}
		ids[candidate.Subject] = candidate.ID
	}
	if ids["Synthetic attachment report"] == ids["Synthetic attachment-free message"] {
		t.Fatal("fixture messages must have distinct opaque IDs")
	}

	sentinels := []string{
		testkit.SyntheticAttachmentBody,
		testkit.SyntheticAttachmentFreeBody,
		testkit.SyntheticAttachmentDecoded,
		base64.StdEncoding.EncodeToString([]byte(testkit.SyntheticAttachmentDecoded)),
	}
	for _, tc := range []struct {
		subject string
		count   int
	}{
		{"Synthetic attachment report", 1},
		{"Synthetic attachment-free message", 0},
	} {
		t.Run(tc.subject, func(t *testing.T) {
			id := ids[tc.subject]
			if id == "" {
				t.Fatal("search did not return fixture message")
			}

			before := len(server.Commands())
			result := callTool(t, session, "list_attachments", map[string]any{"messageId": id})
			// Inspect the entire SDK result, including any structured content.
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal SDK result: %v", err)
			}
			for _, sentinel := range sentinels {
				if bytes.Contains(raw, []byte(sentinel)) {
					t.Fatal("synthetic content leaked into attachment result")
				}
			}

			var decoded struct {
				ID          string             `json:"id"`
				Mailbox     string             `json:"mailbox"`
				Attachments []attachmentResult `json:"attachments"`
				Truncated   bool               `json:"truncated"`
			}
			decodeResult(t, result, &decoded)
			if decoded.ID != id || decoded.Mailbox != "INBOX" || decoded.Truncated {
				t.Fatalf("unexpected attachment result identity or truncation: %+v", decoded)
			}
			if decoded.Attachments == nil || len(decoded.Attachments) != tc.count {
				t.Fatalf("attachments = %+v, want collection of length %d", decoded.Attachments, tc.count)
			}
			if tc.count == 1 {
				attachment := decoded.Attachments[0]
				if attachment.Filename != "report.pdf" || attachment.ContentType != "application/pdf" || attachment.Disposition != "attachment" {
					t.Fatalf("unexpected attachment metadata: %+v", attachment)
				}
			}

			sawStructure := false
			for _, command := range server.Commands()[before:] {
				upper := strings.ToUpper(command.Raw)
				if strings.Contains(upper, "UID FETCH") && strings.Contains(upper, "BODYSTRUCTURE") {
					sawStructure = true
				}
				if strings.Contains(upper, "FETCH") && (strings.Contains(upper, "BODY.PEEK[") || strings.Contains(upper, "BODY[") || strings.Contains(upper, "RFC822")) {
					t.Fatalf("attachment listing fetched message bytes: %q", command.Raw)
				}
			}
			if !sawStructure {
				t.Fatal("attachment listing did not request UID BODYSTRUCTURE")
			}
		})
	}

	for _, sentinel := range sentinels {
		if strings.Contains(audit.String(), sentinel) {
			t.Fatal("synthetic content leaked into audit output")
		}
	}
	attachmentEvents := 0
	for _, event := range auditLines(t, &audit) {
		requireAllowlistedKeys(t, event)
		if event["tool"] == "list_attachments" {
			attachmentEvents++
			if event["event"] != "tool_call" || event["outcome"] != "ok" {
				t.Fatalf("unexpected attachment audit event: %v", event)
			}
		}
	}
	if attachmentEvents != 2 {
		t.Fatalf("attachment audit events = %d, want 2", attachmentEvents)
	}

	requireReadOnlyTranscript(t, server)
	for _, command := range server.Commands() {
		if !command.TLS {
			t.Error("attachment test command used plaintext transport")
		}
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatal(err)
	}
}

// mutatingIMAPCommands is the vocabulary that must never appear in any
// transcript produced through the MCP tool surface.
var mutatingIMAPCommands = []string{
	"STORE", "APPEND", "EXPUNGE", "DELETE", "RENAME", "CREATE", "COPY", "MOVE", "SETACL", "SETQUOTA", "SUBSCRIBE", "UNSUBSCRIBE",
}

// syntheticAttachmentDescriptors are explicit typed BODYSTRUCTURE trees for
// testkit.SyntheticAttachmentMessages: a multipart report with one PDF
// attachment, then an attachment-free plain message.
func syntheticAttachmentDescriptors() []*testkit.BodyPart {
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

	return []*testkit.BodyPart{report, text()}
}

func startIntegrationAdapter(t *testing.T) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	return startIntegrationAdapterWithMessages(t, nil)
}

func startIntegrationAdapterWithMessages(t *testing.T, messages []string) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	return startIntegrationAdapterWithOptions(t, testkit.Options{Messages: messages})
}

func startIntegrationAdapterWithOptions(t *testing.T, options testkit.Options) (*testkit.Server, *bridge.Adapter) {
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

	adapter, err := bridge.NewAdapter(bridge.Config{IMAP: bridge.IMAPConfig{
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

// selectedUIDFetch reports whether a UID FETCH names exactly one UID, as a
// selected-message read does, rather than a search metadata range.
func selectedUIDFetch(raw string) bool {
	fields := strings.Fields(strings.ToUpper(raw))
	for index, field := range fields {
		if field == "FETCH" && index > 0 && fields[index-1] == "UID" && index+1 < len(fields) {
			return !strings.ContainsAny(fields[index+1], ":,*")
		}
	}

	return false
}

func requireReadOnlyTranscript(t *testing.T, server *testkit.Server) {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatalf("read-only transcript: %v", err)
	}
	for _, command := range server.Commands() {
		upper := strings.ToUpper(command.Raw)
		for _, mutation := range mutatingIMAPCommands {
			for _, field := range strings.Fields(upper) {
				if field == mutation {
					t.Fatalf("mutating IMAP command %q reached the wire: %q", mutation, command.Raw)
				}
			}
		}
	}
}

func TestSelectDigestCandidatesComposesOnlyReadOnlyOperationsOnTheWire(t *testing.T) {
	t.Parallel()

	server, adapter := startIntegrationAdapter(t)
	session := connectTestClient(t, Options{Mail: adapter})

	result := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false, "sinceHours": 100})

	var decoded digestDecoded
	decodeResult(t, result, &decoded)
	if decoded.Mailbox != "INBOX" || decoded.TotalMessages != 2 {
		t.Fatalf("unexpected digest summary: %+v", decoded)
	}
	if len(decoded.Candidates) != 1 || decoded.Candidates[0].Subject != "Welcome to Croton" {
		t.Fatalf("unexpected candidates: %+v", decoded.Candidates)
	}
	if decoded.Candidates[0].ID == "" {
		t.Fatal("candidate lacks opaque id")
	}

	transcript := server.Commands()
	sawStatus, sawSearch, sawFetchBody := false, false, false
	for _, command := range transcript {
		upper := strings.ToUpper(command.Raw)
		if strings.Contains(upper, "STATUS") {
			sawStatus = true
		}
		if strings.Contains(upper, "UID SEARCH") {
			sawSearch = true
		}
		if strings.Contains(upper, "BODY.PEEK[]") && !strings.Contains(upper, "HEADER") {
			sawFetchBody = true
		}
	}
	if !sawStatus || !sawSearch {
		t.Fatalf("digest transcript missing composed read operations (status=%v search=%v)", sawStatus, sawSearch)
	}
	if sawFetchBody {
		t.Fatal("metadata-first digest fetched a message body")
	}

	requireReadOnlyTranscript(t, server)
}

func TestAllSixToolsProduceReadOnlyWireTranscript(t *testing.T) {
	t.Parallel()

	// The default fixture does not answer BODYSTRUCTURE, so this transcript
	// uses one custom welcome message with an explicit plain-text descriptor.
	welcome := "From: Croton Fixture <fixture@croton.test>\r\nTo: Reader <reader@croton.test>\r\n" +
		"Subject: Welcome to Croton\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\nMessage-ID: <welcome@croton.test>\r\n\r\n" +
		"Welcome to Croton. This synthetic message exists only for adapter tests.\r\n"
	server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{
		Messages:       []string{welcome},
		BodyStructures: []*testkit.BodyPart{{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 74, Lines: 1}},
	})
	session := connectTestClient(t, Options{Mail: adapter})

	callTool(t, session, "list_folders", map[string]any{})

	searchResult := callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "subject": "Welcome"})
	var search searchMailDecoded
	decodeResult(t, searchResult, &search)
	if len(search.Results) != 1 {
		t.Fatalf("search results = %+v", search)
	}
	identifier := search.Results[0].ID

	message := callTool(t, session, "get_message", map[string]any{"messageId": identifier})
	var decodedMessage getMessageDecoded
	decodeResult(t, message, &decodedMessage)
	if decodedMessage.Headers.Subject != "Welcome to Croton" {
		t.Fatalf("unexpected message: %+v", decodedMessage)
	}

	thread := callTool(t, session, "get_thread", map[string]any{"messageId": identifier})
	var decodedThread getThreadDecoded
	decodeResult(t, thread, &decodedThread)
	if len(decodedThread.Nodes) == 0 {
		t.Fatalf("unexpected thread: %+v", decodedThread)
	}

	attachments := callTool(t, session, "list_attachments", map[string]any{"messageId": identifier})
	var decodedAttachments struct {
		ID          string             `json:"id"`
		Attachments []attachmentResult `json:"attachments"`
		Truncated   bool               `json:"truncated"`
	}
	decodeResult(t, attachments, &decodedAttachments)
	if decodedAttachments.ID != identifier || decodedAttachments.Attachments == nil || len(decodedAttachments.Attachments) != 0 || decodedAttachments.Truncated {
		t.Fatalf("unexpected attachments: %+v", decodedAttachments)
	}

	digest := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false})
	var decodedDigest digestDecoded
	decodeResult(t, digest, &decodedDigest)

	requireReadOnlyTranscript(t, server)
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatalf("insecure authentication: %v", err)
	}
}

func TestGetThreadSyntheticWire(t *testing.T) {
	t.Parallel()

	server, adapter := startIntegrationAdapterWithMessages(t, testkit.SyntheticLinkedThread())
	session := connectTestClient(t, Options{Mail: adapter})

	var search searchMailDecoded
	decodeResult(t, callTool(t, session, "search_mail", map[string]any{
		"mailbox": "INBOX", "subject": "Synthetic planning",
	}), &search)
	if len(search.Results) != 3 || search.Truncated {
		t.Fatalf("fixture search = %+v, want all three messages", search)
	}

	// Resolve each opaque ID through MCP, independently of search ordering.
	ids := make(map[string]string)
	seen := make(map[string]bool)
	for _, candidate := range search.Results {
		if candidate.ID == "" || seen[candidate.ID] {
			t.Fatalf("missing or duplicate opaque ID: %q", candidate.ID)
		}
		seen[candidate.ID] = true

		var message getMessageDecoded
		decodeResult(t, callTool(t, session, "get_message", map[string]any{"messageId": candidate.ID}), &message)
		ids[message.Headers.MessageID] = candidate.ID
	}
	for _, name := range []string{"root", "reply", "unrelated"} {
		if ids["<"+name+"@croton.test>"] == "" {
			t.Fatalf("missing synthetic %s ID: %v", name, ids)
		}
	}

	rootID, replyID, unrelatedID := ids["<root@croton.test>"], ids["<reply@croton.test>"], ids["<unrelated@croton.test>"]
	for _, limit := range []int{10, 1} {
		t.Run(fmt.Sprintf("maxMessages=%d", limit), func(t *testing.T) {
			before := len(server.Commands())

			var thread getThreadDecoded
			decodeResult(t, callTool(t, session, "get_thread", map[string]any{
				"messageId": replyID, "maxMessages": limit,
			}), &thread)
			if thread.ID != replyID || thread.Mailbox != "INBOX" {
				t.Errorf("unexpected thread identity: %+v", thread)
			}
			if limit == 1 {
				if len(thread.Nodes) != 1 || thread.Nodes[0].Key != replyID || !thread.Truncated {
					t.Errorf("bounded thread must retain target and report truncation: %+v", thread)
				}
			} else {
				if len(thread.Nodes) != 3 || thread.Truncated {
					t.Errorf("want three nodes without truncation, got %+v", thread)
				}
				foundRoot, foundReply, foundSibling := false, false, false
				for _, node := range thread.Nodes {
					switch node.Key {
					case rootID:
						foundRoot = true
						if node.MessageID != "<root@croton.test>" || node.ParentKey != "" || node.Depth != 0 {
							t.Errorf("unexpected root: %+v", node)
						}
					case replyID:
						foundReply = true
						if node.MessageID != "<reply@croton.test>" || node.ParentKey != rootID || node.Depth != 1 {
							t.Errorf("unexpected reply: %+v", node)
						}
					case unrelatedID:
						foundSibling = true
						if node.MessageID != "<unrelated@croton.test>" || node.ParentKey != "" || node.Depth != 0 {
							t.Errorf("unexpected independent subject sibling: %+v", node)
						}
					default:
						t.Errorf("unexpected node: %+v", node)
					}
				}
				if !foundRoot || !foundReply || !foundSibling {
					t.Errorf("thread membership missing: root=%v reply=%v sibling=%v", foundRoot, foundReply, foundSibling)
				}
			}

			headerFetches := 0
			for _, command := range server.Commands()[before:] {
				if !command.TLS {
					t.Error("thread command used plaintext transport")
				}
				upper := strings.ToUpper(command.Raw)
				if !strings.Contains(upper, "UID FETCH") {
					continue
				}
				if !strings.Contains(upper, "BODY.PEEK[HEADER]") || strings.Contains(upper, "BODY.PEEK[]") {
					t.Errorf("thread FETCH %q is not header-only", command.Raw)
				}
				if selectedUIDFetch(command.Raw) {
					headerFetches++
				}
			}
			if headerFetches == 0 || headerFetches > limit {
				t.Errorf("selected header PEEK fetches = %d, want 1..%d", headerFetches, limit)
			}

			requireReadOnlyTranscript(t, server)
		})
	}

	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatal(err)
	}
}

func TestDigestSyntheticWire(t *testing.T) {
	t.Parallel()

	now := time.Now()
	sentinels := []string{
		testkit.SyntheticDigestUnreadFirstBody,
		testkit.SyntheticDigestUnreadSecondBody,
		testkit.SyntheticDigestReadBody,
	}
	boundedUID := regexp.MustCompile(`\bUID [1-9][0-9]*(?::[1-9][0-9]*)? `)
	dateBounds := regexp.MustCompile(`\bSINCE "[0-9]{1,2}-[A-Z]{3}-[0-9]{4}" BEFORE "[0-9]{1,2}-[A-Z]{3}-[0-9]{4}"`)

	for _, fixture := range []struct {
		name    string
		options testkit.Options
		total   int
		unseen  int
	}{
		{"mixed", testkit.SyntheticDigestMixedMailbox(now), 3, 2},
		{"all_read", testkit.SyntheticDigestAllReadMailbox(now), 1, 0},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			server, adapter := startIntegrationAdapterWithOptions(t, fixture.options)
			var audit bytes.Buffer
			session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

			cases := []struct {
				name      string
				unread    bool
				limit     int
				subjects  []string
				truncated bool
			}{
				{"unread", true, 10, []string{"Synthetic digest unread first", "Synthetic digest unread second"}, false},
				{"all", false, 10, []string{"Synthetic digest unread first", "Synthetic digest unread second", "Synthetic digest read"}, false},
				{"bounded", true, 1, []string{"Synthetic digest unread first", "Synthetic digest unread second"}, true},
			}
			if fixture.unseen == 0 {
				cases = cases[:1]
				cases[0].subjects = nil
			}

			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					before := len(server.Commands())
					result := callTool(t, session, "select_digest_candidates", map[string]any{
						"mailbox": "INBOX", "sinceHours": 72, "unreadOnly": tc.unread, "limit": tc.limit,
					})
					raw, err := json.Marshal(result)
					if err != nil {
						t.Fatalf("marshal SDK result: %v", err)
					}
					for _, sentinel := range sentinels {
						if bytes.Contains(raw, []byte(sentinel)) {
							t.Fatal("synthetic body leaked into digest result")
						}
					}

					var decoded digestDecoded
					decodeResult(t, result, &decoded)
					if decoded.Mailbox != "INBOX" || decoded.TotalMessages != fixture.total || decoded.UnseenCount != fixture.unseen {
						t.Fatalf("unexpected digest summary: %+v", decoded)
					}
					wantCount := min(len(tc.subjects), tc.limit)
					if decoded.Candidates == nil || len(decoded.Candidates) != wantCount || decoded.Truncated != tc.truncated {
						t.Fatalf("want %d candidates, truncated=%v: %+v", wantCount, tc.truncated, decoded)
					}

					allowed := make(map[string]bool)
					for _, subject := range tc.subjects {
						allowed[subject] = true
					}
					ids := make(map[string]bool)
					for _, candidate := range decoded.Candidates {
						if !allowed[candidate.Subject] || candidate.ID == "" || ids[candidate.ID] || candidate.Mailbox != "INBOX" || candidate.Size <= 0 {
							t.Fatalf("unexpected or duplicate candidate metadata: %+v", candidate)
						}
						delete(allowed, candidate.Subject)
						ids[candidate.ID] = true
					}

					sawStatus, sawSearch, fetches := false, false, 0
					for _, command := range server.Commands()[before:] {
						upper := strings.ToUpper(command.Raw)
						if !command.TLS {
							t.Error("digest command used plaintext transport")
						}
						if command.Name == "STATUS" {
							sawStatus = true
							if !strings.Contains(upper, "MESSAGES") || !strings.Contains(upper, "UNSEEN") {
								t.Fatalf("STATUS omitted summary fields: %s", command.Raw)
							}
						}
						if strings.Contains(upper, "UID SEARCH ") {
							sawSearch = true
							if !boundedUID.MatchString(upper) || !dateBounds.MatchString(upper) || strings.Contains(upper, " UNSEEN") != tc.unread {
								t.Fatalf("unexpected digest search bounds or unread criterion: %s", command.Raw)
							}
						}
						if strings.Contains(upper, " FETCH ") {
							fetches++
							// Permit only UID, FLAGS, size and a non-mutating header read. A
							// body read fails even if combined with a permitted header read.
							if !strings.Contains(upper, "UID FETCH ") || !strings.Contains(upper, "BODY.PEEK[HEADER]") {
								t.Fatalf("unexpected metadata fetch: %s", command.Raw)
							}
							_, items, _ := strings.Cut(upper, "(")
							for _, item := range strings.Fields(strings.TrimSuffix(items, ")")) {
								if item != "UID" && item != "FLAGS" && item != "RFC822.SIZE" && item != "BODY.PEEK[HEADER]" {
									t.Fatalf("digest requested non-metadata fetch item: %s", command.Raw)
								}
							}
						}
					}
					if !sawStatus || !sawSearch || (fetches > 0) != (wantCount > 0) {
						t.Fatalf("unexpected wire operations: status=%v search=%v fetches=%d", sawStatus, sawSearch, fetches)
					}

					requireReadOnlyTranscript(t, server)
				})
			}

			for _, sentinel := range sentinels {
				if strings.Contains(audit.String(), sentinel) {
					t.Fatal("synthetic body leaked into audit output")
				}
			}
			events := auditLines(t, &audit)
			if len(events) != len(cases) {
				t.Fatalf("audit events = %d, want %d", len(events), len(cases))
			}
			for _, event := range events {
				requireAllowlistedKeys(t, event)
				if event["tool"] != "select_digest_candidates" || event["event"] != "tool_call" || event["outcome"] != "ok" {
					t.Fatalf("unexpected digest audit event: %v", event)
				}
			}
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

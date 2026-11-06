package mcpserver

import (
	"context"
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const threadHeaderBodySentinel = "croton-thread-header-body-sentinel"

// threadHeaderMessages holds a linked root and reply plus an independent
// same-subject root. The reply carries a 300KiB body behind small readable
// headers, beyond the default body bound.
func threadHeaderMessages() []string {
	line := threadHeaderBodySentinel + " " + strings.Repeat("b", 62) + "\r\n"
	body := strings.Repeat(line, (300<<10)/len(line)+1)

	return []string{
		"From: Root <root@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Large planning\r\n" +
			"Date: Thu, 01 Jan 2026 00:00:00 +0000\r\nMessage-ID: <large-root@croton.test>\r\n\r\nroot body\r\n",
		"From: Reply <reply@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Re: Large planning\r\n" +
			"Date: Thu, 01 Jan 2026 00:00:01 +0000\r\nMessage-ID: <large-reply@croton.test>\r\n" +
			"In-Reply-To: <large-root@croton.test>\r\nReferences: <large-root@croton.test>\r\n\r\n" + body,
		"From: Other <other@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Large planning\r\n" +
			"Date: Thu, 01 Jan 2026 00:00:02 +0000\r\nMessage-ID: <large-independent@croton.test>\r\n\r\nindependent body\r\n",
	}
}

// countingHeaderMail delegates to a header-capable adapter and counts each
// public adapter operation separately.
type countingHeaderMail struct {
	Mail
	headers MessageHeaderReader

	mu          sync.Mutex
	headerCalls int
	metaCalls   int
	bodyCalls   int
	searchCalls int
}

func (mail *countingHeaderMail) GetMessageHeaders(ctx context.Context, identifier string) (bridge.MessageHeaders, error) {
	mail.mu.Lock()
	mail.headerCalls++
	mail.mu.Unlock()

	return mail.headers.GetMessageHeaders(ctx, identifier)
}

func (mail *countingHeaderMail) GetMessageMetadata(ctx context.Context, identifier string) (bridge.MessageMetadata, error) {
	mail.mu.Lock()
	mail.metaCalls++
	mail.mu.Unlock()

	return mail.Mail.GetMessageMetadata(ctx, identifier)
}

func (mail *countingHeaderMail) GetMessageBody(ctx context.Context, identifier string) ([]byte, error) {
	mail.mu.Lock()
	mail.bodyCalls++
	mail.mu.Unlock()

	return mail.Mail.GetMessageBody(ctx, identifier)
}

func (mail *countingHeaderMail) SearchMailPage(ctx context.Context, query bridge.SearchQuery) (bridge.SearchPage, error) {
	mail.mu.Lock()
	mail.searchCalls++
	mail.mu.Unlock()

	return mail.Mail.SearchMailPage(ctx, query)
}

func (mail *countingHeaderMail) reset() {
	mail.mu.Lock()
	defer mail.mu.Unlock()

	mail.headerCalls, mail.metaCalls, mail.bodyCalls, mail.searchCalls = 0, 0, 0, 0
}

// mailWithoutHeaders exposes only the Mail surface, hiding any optional
// header-only read of the wrapped adapter.
type mailWithoutHeaders struct {
	Mail
}

func startThreadHeaderAdapter(t *testing.T, mode testkit.TLSMode) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: mode, Messages: threadHeaderMessages()})
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

	tlsMode := bridge.TLSModeStartTLS
	if mode == testkit.ImplicitTLS {
		tlsMode = bridge.TLSModeImplicit
	}
	adapter, err := bridge.NewAdapter(bridge.Config{IMAP: bridge.IMAPConfig{
		Host:              host,
		Port:              port,
		TLSMode:           tlsMode,
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

// requireHeaderOnlyThreadTranscript requires a TLS-only, read-only transcript
// whose every FETCH is header-only, with exactly one selected header FETCH per
// wanted UID and no separate selected metadata FETCH.
func requireHeaderOnlyThreadTranscript(t *testing.T, commands []testkit.Command, wantUIDs []uint32) {
	t.Helper()

	selected := make(map[string]int)
	for _, command := range commands {
		upper := strings.ToUpper(command.Raw)
		if !command.TLS && command.Name != "CAPABILITY" && command.Name != "STARTTLS" {
			t.Fatalf("command %q ran without TLS", command.Name)
		}
		if !strings.Contains(upper, " FETCH ") {
			continue
		}
		if !strings.Contains(upper, "BODY.PEEK[HEADER]") || strings.Contains(upper, "BODY.PEEK[]") ||
			strings.Contains(upper, "BODY[]") || strings.Contains(upper, "[TEXT]") || strings.Contains(upper, "BINARY") {
			t.Fatalf("FETCH %q is not header-only", command.Raw)
		}
		if selectedUIDFetch(command.Raw) {
			fields := strings.Fields(upper)
			for index, field := range fields {
				if field == "FETCH" && index+1 < len(fields) {
					selected[fields[index+1]]++
				}
			}
		}
	}

	want := make(map[string]int, len(wantUIDs))
	for _, uid := range wantUIDs {
		want[strconv.FormatUint(uint64(uid), 10)] = 1
	}
	if !reflect.DeepEqual(selected, want) {
		t.Fatalf("selected header FETCHes by UID = %v, want exactly one each %v", selected, want)
	}
}

func TestThreadHeaderTransport(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	t.Run("header_only_tls", func(t *testing.T) {
		for _, mode := range []struct {
			name string
			mode testkit.TLSMode
		}{
			{name: "starttls", mode: testkit.StartTLS},
			{name: "implicit_tls", mode: testkit.ImplicitTLS},
		} {
			t.Run(mode.name, func(t *testing.T) {
				server, adapter := startThreadHeaderAdapter(t, mode.mode)
				mail := &countingHeaderMail{Mail: adapter, headers: adapter}
				session := connectTestClient(t, Options{Mail: mail})

				searched := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "subject": "Large planning"})), "results")
				if len(searched) != 3 {
					t.Fatalf("fixture search = %v, want three messages", searched)
				}
				tuples := make(map[string]readTuple)
				byUID := make(map[uint32]string)
				for _, item := range searched {
					id, _ := item["id"].(string)
					tuple := flatTuple(t, item)
					tuples[id] = tuple
					byUID[tuple.UID] = id
				}
				rootID, replyID, independentID := byUID[101], byUID[102], byUID[103]
				if rootID == "" || replyID == "" || independentID == "" {
					t.Fatalf("fixture UIDs = %v", byUID)
				}

				for _, target := range []string{replyID, rootID} {
					mail.reset()
					before := len(server.Commands())

					result := callTool(t, session, "get_thread", map[string]any{"messageId": target})
					if strings.Contains(resultText(t, result), threadHeaderBodySentinel) {
						t.Fatal("thread result carried body text")
					}
					thread := rawObject(t, result)
					if thread["id"] != target || thread["mailbox"] != "INBOX" {
						t.Fatalf("thread context = %v", thread)
					}
					if truncated, _ := thread["truncated"].(bool); truncated {
						t.Fatalf("complete thread reported truncated: %v", thread)
					}

					nodes := rawItems(t, thread, "nodes")
					want := []struct {
						key, messageID, parent string
						depth                  string
					}{
						{rootID, "<large-root@croton.test>", "", "0"},
						{replyID, "<large-reply@croton.test>", rootID, "1"},
						{independentID, "<large-independent@croton.test>", "", "0"},
					}
					if len(nodes) != len(want) {
						t.Fatalf("nodes = %v, want %d", nodes, len(want))
					}
					for index, node := range nodes {
						parent, _ := node["parentKey"].(string)
						if node["key"] != want[index].key || node["messageId"] != want[index].messageID || parent != want[index].parent || fmt.Sprint(node["depth"]) != want[index].depth {
							t.Fatalf("node %d = %v, want %+v", index, node, want[index])
						}
						if got := flatTuple(t, node); got != tuples[want[index].key] {
							t.Fatalf("node %d tuple = %+v, want its own %+v", index, got, tuples[want[index].key])
						}
					}

					mail.mu.Lock()
					counts := [4]int{mail.headerCalls, mail.metaCalls, mail.bodyCalls, mail.searchCalls}
					mail.mu.Unlock()
					if counts != [4]int{3, 0, 0, 1} {
						t.Fatalf("operations header/metadata/body/search = %v, want [3 0 0 1]", counts)
					}
					requireHeaderOnlyThreadTranscript(t, server.Commands()[before:], []uint32{101, 102, 103})
				}

				if err := server.AssertReadOnlyCommands(); err != nil {
					t.Fatalf("read-only transcript: %v", err)
				}
				if err := server.AssertNoInsecureAuthentication(); err != nil {
					t.Fatalf("authentication transcript: %v", err)
				}
			})
		}
	})

	t.Run("capability_and_systemic", func(t *testing.T) {
		threeMemberFake := func() *fakeMail {
			mail := threadFake()
			mail.metadataByID["id-other"] = bridge.MessageMetadata{ID: "id-other", Mailbox: "INBOX", UIDValidity: 9001, UID: 103, Subject: "Planning"}
			mail.bodyByID["id-other"] = []byte(strings.ReplaceAll(strings.ReplaceAll(threadRootMIME, "<root@", "<other@"), "Root message.", "Other message."))
			mail.searched = append(mail.searched, bridge.MessageMetadata{ID: "id-other", Mailbox: "INBOX", Subject: "Planning"})
			return mail
		}
		requireNoBodyOrMetadata := func(t *testing.T, mail *fakeMail) {
			t.Helper()

			if len(mail.bodyCalls)+len(mail.metaCalls) != 0 {
				t.Fatalf("thread fell back to body or metadata reads: body=%v metadata=%v", mail.bodyCalls, mail.metaCalls)
			}
		}

		t.Run("absent capability", func(t *testing.T) {
			mail := threeMemberFake()
			result := callTool(t, connectTestClient(t, Options{Mail: mailWithoutHeaders{mail}}), "get_thread", map[string]any{"messageId": "id-reply"})

			requireRefusedWithoutTuple(t, result, errUnsupported)
			if len(mail.headerCalls)+len(mail.searchCalls) != 0 {
				t.Fatalf("unsupported adapter was read: header=%v search=%v", mail.headerCalls, mail.searchCalls)
			}
			requireNoBodyOrMetadata(t, mail)
		})

		failures := []struct {
			name string
			err  error
			want string
		}{
			{"auth", &bridge.Error{Code: bridge.CodeAuthentication}, errUnavailable},
			{"tls", &bridge.Error{Code: bridge.CodeTLSNegotiation}, errUnavailable},
			{"tls_mismatch", &bridge.Error{Code: bridge.CodeTLSMismatch}, errUnavailable},
			{"helper", &bridge.Error{Code: bridge.CodeCredentialCommand}, errUnavailable},
			{"transport", &bridge.Error{Code: bridge.CodeBridgeUnreachable}, errUnavailable},
			{"protocol", &bridge.Error{Code: bridge.CodeIMAPProtocol}, errUnavailable},
			{"command", &bridge.Error{Code: bridge.CodeIMAPCommand}, errUnavailable},
			{"bounds", &bridge.Error{Code: bridge.CodeBoundsExceeded}, errBoundsExceeded},
			{"timeout", &bridge.Error{Code: bridge.CodeCommandTimedOut}, errTimedOut},
			{"cancel", &bridge.Error{Code: bridge.CodeOperationCanceled}, errCanceled},
			{"closed_adapter", &bridge.Error{Code: bridge.CodeAdapterClosed}, errUnavailable},
			{"unsupported", &bridge.Error{Code: bridge.CodeUnsupported}, errUnsupported},
			{"invalid_request", &bridge.Error{Code: bridge.CodeInvalidRequest}, errInvalidArgument},
			{"missing_mailbox", &bridge.Error{Code: bridge.CodeMailboxNotFound}, errNotFound},
			{"malformed_header", &bridge.NormalizeError{Code: "malformed_header"}, errInternal},
		}

		t.Run("target failure", func(t *testing.T) {
			targetFailures := append(failures, struct {
				name string
				err  error
				want string
			}{"stale", &bridge.Error{Code: bridge.CodeStaleMessageID}, errStaleID})
			for _, failure := range targetFailures {
				mail := threeMemberFake()
				mail.headerErrByID = map[string]error{"id-reply": failure.err}
				result := callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"})

				requireRefusedWithoutTuple(t, result, failure.want)
				if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply"}) || len(mail.searchCalls) != 0 {
					t.Fatalf("%s target failure continued: header=%v search=%v", failure.name, mail.headerCalls, mail.searchCalls)
				}
				requireNoBodyOrMetadata(t, mail)
			}
		})

		t.Run("systemic sibling failure", func(t *testing.T) {
			for _, failure := range failures {
				mail := threeMemberFake()
				mail.headerErrByID = map[string]error{"id-root": failure.err}
				result := callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"})

				requireRefusedWithoutTuple(t, result, failure.want)
				if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply", "id-root"}) || len(mail.searchCalls) != 1 {
					t.Fatalf("%s sibling failure did not stop reads: header=%v search=%v", failure.name, mail.headerCalls, mail.searchCalls)
				}
				requireNoBodyOrMetadata(t, mail)
			}
		})

		t.Run("stale sibling", func(t *testing.T) {
			mail := threeMemberFake()
			mail.headerErrByID = map[string]error{"id-root": &bridge.Error{Code: bridge.CodeStaleMessageID}}
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"}))

			nodes := rawItems(t, thread, "nodes")
			keys := make([]string, 0, len(nodes))
			for _, node := range nodes {
				key, _ := node["key"].(string)
				keys = append(keys, key)
				source := mail.metadataByID[key]
				if got := flatTuple(t, node); got != (readTuple{Mailbox: source.Mailbox, UIDValidity: source.UIDValidity, UID: source.UID}) {
					t.Fatalf("node %q tuple = %+v", key, got)
				}
			}
			if len(keys) != 2 || !strings.Contains(strings.Join(keys, ","), "id-reply") || !strings.Contains(strings.Join(keys, ","), "id-other") {
				t.Fatalf("stale sibling handling changed: %v", thread)
			}
			if truncated, _ := thread["truncated"].(bool); truncated {
				t.Fatalf("stale sibling omission reports truncation: %v", thread)
			}
			if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply", "id-root", "id-other"}) {
				t.Fatalf("header reads = %v", mail.headerCalls)
			}
			requireNoBodyOrMetadata(t, mail)
		})

		t.Run("invalid sibling identity", func(t *testing.T) {
			mail := threeMemberFake()
			invalid := mail.metadataByID["id-root"]
			invalid.UID = 0
			mail.metadataByID["id-root"] = invalid
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", map[string]any{"messageId": "id-reply"}))

			nodes := rawItems(t, thread, "nodes")
			if len(nodes) != 2 {
				t.Fatalf("invalid sibling retained: %v", thread)
			}
			for _, node := range nodes {
				if node["key"] == "id-root" {
					t.Fatalf("invalid sibling retained or borrowed a tuple: %v", thread)
				}
				flatTuple(t, node)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("dropped invalid sibling not reported truncated: %v", thread)
			}
			if !reflect.DeepEqual(mail.headerCalls, []string{"id-reply", "id-root", "id-other"}) {
				t.Fatalf("header reads = %v", mail.headerCalls)
			}
			requireNoBodyOrMetadata(t, mail)
		})
	})

	t.Run("bounded_attempts", func(t *testing.T) {
		target := withReadIdentity(bridge.MessageMetadata{ID: "target", Mailbox: "INBOX", Subject: "Re: Budget"}, 77, 500)
		targetBody := fakeThreadBody("Re: Budget", "<budget-reply@croton.test>", "References: <budget@croton.test>\r\n")
		limits := []struct {
			name     string
			argument any
			limit    int
		}{
			{"max_messages_1", 1, 1},
			{"default_10", nil, defaultThreadMessages},
			{"max_20", maxThreadMessages, maxThreadMessages},
			{"clamped_20", 1000000, maxThreadMessages},
		}
		threadArguments := func(argument any) map[string]any {
			arguments := map[string]any{"messageId": "target"}
			if argument != nil {
				arguments["maxMessages"] = argument
			}
			return arguments
		}

		t.Run("failed attempts consume budget", func(t *testing.T) {
			for _, limit := range limits {
				mail := &fakeMail{
					metadataByID: map[string]bridge.MessageMetadata{"target": target},
					bodyByID:     map[string][]byte{"target": targetBody},
					searched:     []bridge.MessageMetadata{{ID: "target", Mailbox: "INBOX", Subject: "Re: Budget"}},
				}
				for index := range 5 {
					mail.searched = append(mail.searched, bridge.MessageMetadata{ID: fmt.Sprintf("false-%d", index), Mailbox: "INBOX", Subject: "Budget committee"})
				}
				for index := range 30 {
					id := fmt.Sprintf("stale-%02d", index)
					mail.searched = append(mail.searched,
						bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: "Budget"},
						bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: "Budget"})
				}
				mail.metadataByID["valid"] = withReadIdentity(bridge.MessageMetadata{ID: "valid", Mailbox: "INBOX", Subject: "Budget"}, 77, 9)
				mail.bodyByID["valid"] = fakeThreadBody("Budget", "<budget@croton.test>", "")
				mail.searched = append(mail.searched, bridge.MessageMetadata{ID: "valid", Mailbox: "INBOX", Subject: "Budget"})

				thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", threadArguments(limit.argument)))

				want := []string{"target"}
				for index := range limit.limit - 1 {
					want = append(want, fmt.Sprintf("stale-%02d", index))
				}
				if !reflect.DeepEqual(mail.headerCalls, want) {
					t.Fatalf("%s header attempts = %v, want %v", limit.name, mail.headerCalls, want)
				}
				if len(mail.searchCalls) != 1 || len(mail.metaCalls)+len(mail.bodyCalls) != 0 {
					t.Fatalf("%s operations search=%d metadata=%v body=%v", limit.name, len(mail.searchCalls), mail.metaCalls, mail.bodyCalls)
				}
				nodes := rawItems(t, thread, "nodes")
				if len(nodes) != 1 || nodes[0]["key"] != "target" || flatTuple(t, nodes[0]) != (readTuple{Mailbox: "INBOX", UIDValidity: 77, UID: 500}) {
					t.Fatalf("%s nodes = %v", limit.name, nodes)
				}
				if truncated, _ := thread["truncated"].(bool); !truncated {
					t.Fatalf("%s unattempted eligible siblings not reported truncated: %v", limit.name, thread)
				}
			}
		})

		t.Run("complete standalone is untruncated", func(t *testing.T) {
			for _, limit := range limits {
				mail := &fakeMail{
					metadataByID: map[string]bridge.MessageMetadata{"target": target},
					bodyByID:     map[string][]byte{"target": targetBody},
					searched: []bridge.MessageMetadata{
						{ID: "target", Mailbox: "INBOX", Subject: "Re: Budget"},
						{ID: "false", Mailbox: "INBOX", Subject: "Budget committee"},
					},
				}
				thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", threadArguments(limit.argument)))

				if truncated, _ := thread["truncated"].(bool); truncated {
					t.Fatalf("%s complete standalone reported truncated: %v", limit.name, thread)
				}
				if nodes := rawItems(t, thread, "nodes"); len(nodes) != 1 || nodes[0]["key"] != "target" {
					t.Fatalf("%s nodes = %v", limit.name, nodes)
				}
				if !reflect.DeepEqual(mail.headerCalls, []string{"target"}) || len(mail.searchCalls) != 1 || len(mail.metaCalls)+len(mail.bodyCalls) != 0 {
					t.Fatalf("%s operations header=%v search=%d metadata=%v body=%v", limit.name, mail.headerCalls, len(mail.searchCalls), mail.metaCalls, mail.bodyCalls)
				}
			}
		})

		t.Run("search truncation marker", func(t *testing.T) {
			mail := &fakeMail{
				metadataByID:    map[string]bridge.MessageMetadata{"target": target},
				bodyByID:        map[string][]byte{"target": targetBody},
				searchTruncated: true,
			}
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", threadArguments(nil)))

			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("truncated sibling search reported a complete thread: %v", thread)
			}
			if len(mail.headerCalls) != 1 || len(mail.searchCalls) != 1 {
				t.Fatalf("operations header=%v search=%d", mail.headerCalls, len(mail.searchCalls))
			}
		})

		t.Run("node-only output bound", func(t *testing.T) {
			sender := "From: Sender " + strings.Repeat("f", 6000) + " <sender@croton.test>\r\n"
			mail := &fakeMail{
				metadataByID: map[string]bridge.MessageMetadata{"target": target},
				bodyByID:     map[string][]byte{"target": targetBody},
			}
			for index := range 25 {
				id := fmt.Sprintf("sibling-%02d", index)
				mail.metadataByID[id] = withReadIdentity(bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: "Budget"}, 77, uint32(index)+1)
				mail.bodyByID[id] = []byte(sender + "Subject: Budget\r\nMessage-ID: <" + id + "@croton.test>\r\n\r\nbody\r\n")
				mail.searched = append(mail.searched, bridge.MessageMetadata{ID: id, Mailbox: "INBOX", Subject: "Budget"})
			}
			thread := rawObject(t, callTool(t, connectTestClient(t, Options{Mail: mail}), "get_thread", threadArguments(maxThreadMessages)))

			if len(mail.headerCalls) != maxThreadMessages || len(mail.searchCalls) != 1 || len(mail.metaCalls)+len(mail.bodyCalls) != 0 {
				t.Fatalf("operations header=%d search=%d metadata=%v body=%v", len(mail.headerCalls), len(mail.searchCalls), mail.metaCalls, mail.bodyCalls)
			}
			if truncated, _ := thread["truncated"].(bool); !truncated {
				t.Fatalf("bounded thread not reported truncated")
			}
			if thread["id"] != "target" || thread["mailbox"] != "INBOX" {
				t.Fatalf("thread context changed: id=%v mailbox=%v", thread["id"], thread["mailbox"])
			}
			nodes := rawItems(t, thread, "nodes")
			if len(nodes) == 0 || len(nodes) >= maxThreadMessages {
				t.Fatalf("output bound retained %d of %d nodes, want whole-node removal", len(nodes), maxThreadMessages)
			}
			for _, node := range nodes {
				key, _ := node["key"].(string)
				source := mail.metadataByID[key]
				if got := flatTuple(t, node); got != (readTuple{Mailbox: source.Mailbox, UIDValidity: source.UIDValidity, UID: source.UID}) {
					t.Fatalf("retained node %q tuple = %+v, want its own", key, got)
				}
				if from, _ := node["from"].(string); key != "target" && !strings.Contains(from, strings.Repeat("f", 6000)) {
					t.Fatalf("retained node %q was shortened rather than kept whole", key)
				}
			}
		})
	})
}

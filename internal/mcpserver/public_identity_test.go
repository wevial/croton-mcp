package mcpserver

import (
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// flatIdentity is one flat read result's identity. Decoding into uint32
// rejects non-integer, negative and out-of-range numbers.
type flatIdentity struct {
	ID          string `json:"id"`
	Mailbox     string `json:"mailbox"`
	UIDValidity uint32 `json:"uidvalidity"`
	UID         uint32 `json:"uid"`
	Subject     string `json:"subject"`
}

// readTuple is one mailbox generation and UID fixed by the fixture.
type readTuple struct {
	Mailbox     string
	UIDValidity uint32
	UID         uint32
}

func (item flatIdentity) tuple() readTuple {
	return readTuple{Mailbox: item.Mailbox, UIDValidity: item.UIDValidity, UID: item.UID}
}

func requireNoIdentityKeys(t *testing.T, label string, item map[string]any) {
	t.Helper()

	for _, key := range []string{"uidvalidity", "uid", "uidValidity", "UID", "UIDValidity"} {
		if _, ok := item[key]; ok {
			t.Fatalf("%s carries identity key %q: %v", label, key, item)
		}
	}
}

func TestPublicReadIdentityFlatMCP(t *testing.T) {
	t.Parallel()

	t.Run("search_digest", func(t *testing.T) {
		t.Parallel()

		// Custom testkit messages get UIDVALIDITY 9001 and UIDs from 101.
		_, adapter := startIntegrationAdapterWithOptions(t, testkit.SyntheticDigestMixedMailbox(time.Now()))
		session := connectTestClient(t, Options{Mail: adapter})
		fixture := map[string]readTuple{
			"Synthetic digest unread first":  {Mailbox: "INBOX", UIDValidity: 9001, UID: 101},
			"Synthetic digest unread second": {Mailbox: "INBOX", UIDValidity: 9001, UID: 102},
		}

		var search struct {
			Results []flatIdentity `json:"results"`
		}
		decodeResult(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "unreadOnly": true}), &search)
		digestResult := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"})
		var digest struct {
			Candidates []flatIdentity `json:"candidates"`
		}
		decodeResult(t, digestResult, &digest)
		var digestSummary map[string]any
		decodeResult(t, digestResult, &digestSummary)
		requireNoIdentityKeys(t, "digest summary", digestSummary)

		for name, items := range map[string][]flatIdentity{"search_mail": search.Results, "select_digest_candidates": digest.Candidates} {
			if len(items) != len(fixture) {
				t.Fatalf("%s = %+v, want both fixture UIDs", name, items)
			}
			for _, item := range items {
				if item.ID == "" || item.tuple() != fixture[item.Subject] {
					t.Fatalf("%s item %+v, want fixture tuple %+v", name, item, fixture[item.Subject])
				}
			}
		}
	})

	t.Run("selected_and_attachment", func(t *testing.T) {
		t.Parallel()

		messages := testkit.SyntheticAttachmentMessages()
		_, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{Stateful: &testkit.StatefulOptions{Mailboxes: []testkit.MailboxSeed{{
			Name:        "INBOX",
			UIDValidity: math.MaxUint32,
			Messages:    []testkit.MessageSeed{{UID: 1, Body: messages[0]}, {UID: 7, Body: messages[1]}},
		}}}})
		session := connectTestClient(t, Options{Mail: adapter})
		fixture := map[string]readTuple{
			"Synthetic attachment report":       {Mailbox: "INBOX", UIDValidity: math.MaxUint32, UID: 1},
			"Synthetic attachment-free message": {Mailbox: "INBOX", UIDValidity: math.MaxUint32, UID: 7},
		}

		var search struct {
			Results []flatIdentity `json:"results"`
		}
		decodeResult(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}), &search)
		if len(search.Results) != len(fixture) {
			t.Fatalf("search results = %+v, want both fixture messages", search.Results)
		}

		for _, item := range search.Results {
			if item.tuple() != fixture[item.Subject] {
				t.Fatalf("search tuple for %q = %+v, want fixture %+v", item.Subject, item.tuple(), fixture[item.Subject])
			}

			var message flatIdentity
			decodeResult(t, callTool(t, session, "get_message", map[string]any{"messageId": item.ID}), &message)
			if message.ID != item.ID || message.tuple() != item.tuple() {
				t.Fatalf("get_message parent = %+v, want id and tuple from search %+v", message, item)
			}

			attachmentsResult := callTool(t, session, "list_attachments", map[string]any{"messageId": item.ID})
			var attachments flatIdentity
			decodeResult(t, attachmentsResult, &attachments)
			if attachments.ID != item.ID || attachments.tuple() != item.tuple() {
				t.Fatalf("list_attachments parent = %+v, want id and tuple from search %+v", attachments, item)
			}
			var parts struct {
				Attachments []map[string]any `json:"attachments"`
			}
			decodeResult(t, attachmentsResult, &parts)
			for _, part := range parts.Attachments {
				requireNoIdentityKeys(t, "attachment part", part)
			}
		}
	})

	t.Run("invalid_identity", func(t *testing.T) {
		t.Parallel()

		t.Run("new adapter key after restart", func(t *testing.T) {
			_, first := startIntegrationAdapterWithOptions(t, testkit.Options{Messages: testkit.SyntheticAttachmentMessages()})
			var search struct {
				Results []flatIdentity `json:"results"`
			}
			decodeResult(t, callTool(t, connectTestClient(t, Options{Mail: first}), "search_mail", map[string]any{"mailbox": "INBOX"}), &search)
			if len(search.Results) == 0 {
				t.Fatal("search returned no fixture messages")
			}
			if err := first.Close(); err != nil {
				t.Fatalf("close first adapter: %v", err)
			}

			_, restarted := startIntegrationAdapterWithOptions(t, testkit.Options{Messages: testkit.SyntheticAttachmentMessages()})
			requireToolError(t, callTool(t, connectTestClient(t, Options{Mail: restarted}), "get_message", map[string]any{"messageId": search.Results[0].ID}), errStaleID)
		})

		t.Run("zero adapter identity", func(t *testing.T) {
			valid := bridge.MessageMetadata{ID: "opaque-valid", Mailbox: "INBOX", UIDValidity: 9001, UID: 102}
			for _, metadata := range []bridge.MessageMetadata{
				{ID: "opaque-zero", Mailbox: "INBOX", UIDValidity: 9001},
				{ID: "opaque-zero", Mailbox: "INBOX", UID: 101},
			} {
				mail := &fakeMail{
					searched: []bridge.MessageMetadata{valid, metadata},
					metadata: metadata,
					body:     []byte("From: Fixture <fixture@croton.test>\r\nSubject: Zero identity fixture\r\n\r\nzero identity body\r\n"),
				}
				session := connectTestClient(t, Options{Mail: mail})

				requireToolError(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}), errUnavailable)
				requireToolError(t, callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX"}), errUnavailable)
				requireToolError(t, callTool(t, session, "get_message", map[string]any{"messageId": "opaque-zero"}), errUnavailable)
				requireToolError(t, callTool(t, session, "list_attachments", map[string]any{"messageId": "opaque-zero"}), errUnavailable)
			}
		})
	})

	t.Run("contract_guards", func(t *testing.T) {
		t.Parallel()

		_, adapter := startIntegrationAdapterWithMessages(t, testkit.SyntheticLinkedThread())
		session := connectTestClient(t, Options{Mail: adapter})

		var search struct {
			Results []flatIdentity `json:"results"`
		}
		decodeResult(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"}), &search)
		if len(search.Results) == 0 {
			t.Fatal("search returned no fixture messages")
		}

		threadResult := callTool(t, session, "get_thread", map[string]any{"messageId": search.Results[0].ID})
		var thread map[string]any
		decodeResult(t, threadResult, &thread)
		requireNoIdentityKeys(t, "get_thread result", thread)
		var nodes struct {
			Nodes []map[string]any `json:"nodes"`
		}
		decodeResult(t, threadResult, &nodes)
		for _, node := range nodes.Nodes {
			requireNoIdentityKeys(t, "get_thread node", node)
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

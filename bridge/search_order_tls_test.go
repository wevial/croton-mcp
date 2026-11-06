package bridge_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const denseSearchResults = 10

func denseSyntheticMessages(count int) []string {
	messages := make([]string, 0, count)
	for index := range count {
		messages = append(messages, fmt.Sprintf("From: Dense Fixture <dense@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: %s\r\nMessage-ID: <dense-%d@croton.test>\r\n\r\nSynthetic dense window body.\r\n", denseSubject(uint32(101+index)), index))
	}
	return messages
}

func denseSubject(uid uint32) string {
	return fmt.Sprintf("Synthetic dense UID %d", uid)
}

func TestSearchOrderTLS(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name string
		fake testkit.TLSMode
		mode string
	}{
		{name: "starttls_dense", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS},
		{name: "implicit_tls_dense", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{Mode: test.fake, Messages: denseSyntheticMessages(100)})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			config := fakeServerConfig(t, server.Addr(), test.mode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
			config.Bounds = bridge.BoundsPatch{MaxSearchResults: bridge.Int(denseSearchResults)}
			adapter, err := bridge.NewAdapter(config)
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if err != nil {
				t.Fatalf("SearchMailPage on a dense window: %v", err)
			}

			if len(page.Messages) != denseSearchResults || !page.Truncated {
				t.Fatalf("page has %d messages truncated=%v, want %d and truncated=true", len(page.Messages), page.Truncated, denseSearchResults)
			}
			for index, message := range page.Messages {
				wantUID := uint32(200 - index)
				if message.UID != wantUID || message.UIDValidity == 0 || message.Mailbox != "INBOX" || message.ID == "" || message.Subject != denseSubject(wantUID) {
					t.Fatalf("message %d = UID %d subject %q, want highest bounded UID %d", index, message.UID, message.Subject, wantUID)
				}
			}

			searchCommands := 0
			for _, command := range server.Commands() {
				upper := strings.ToUpper(command.Raw)
				if strings.Contains(upper, " UID SEARCH ") {
					searchCommands++
				}
				if strings.Contains(upper, " UID FETCH ") && !strings.Contains(upper, "BODY.PEEK[HEADER]") {
					t.Fatalf("search issued a non-header FETCH: %q", command.Raw)
				}
				if strings.Contains(upper, " UID FETCH ") && !strings.Contains(upper, " UID FETCH 191:200 ") {
					t.Fatalf("search fetched UIDs beyond the bounded selection: %q", command.Raw)
				}
			}
			if searchCommands != 1 {
				t.Fatalf("UID SEARCH commands = %d, want one dense window", searchCommands)
			}

			metadata, err := adapter.GetMessageMetadata(context.Background(), page.Messages[0].ID)
			if err != nil {
				t.Fatalf("GetMessageMetadata after dense search: %v", err)
			}
			if metadata.UID != 200 || metadata.UIDValidity != page.Messages[0].UIDValidity || metadata.Subject != denseSubject(200) {
				t.Fatalf("subsequent bounded read = UID %d subject %q, want UID 200", metadata.UID, metadata.Subject)
			}
			for _, command := range server.Commands() {
				upper := strings.ToUpper(command.Raw)
				if strings.Contains(upper, " UID FETCH ") && !strings.Contains(upper, "BODY.PEEK[HEADER]") {
					t.Fatalf("dense search or follow-up read issued a non-header FETCH: %q", command.Raw)
				}
			}

			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
		})
	}

	runSearchOrderTLSRefusals(t)
}

// runSearchOrderTLSRefusals witnesses that an invalid duplicate ESEARCH ALL
// set behind a literal TAG, after an extension value carrying a nested ALL or
// after an extension value the observer cannot walk or after a correlator
// that directly follows the response name, and a duplicate SEARCH or ESEARCH
// behind a status whose text ends in a literal marker or a lone LF, including
// a status name directly followed by its text, or behind a quoted string
// whose literal marker spans lines, stays a code-only refusal in both TLS
// modes. So does an out-of-window UID that a later ESEARCH ALL, in another
// response or the same one, would replace before the final set is checked.
func runSearchOrderTLSRefusals(t *testing.T) {
	for _, test := range []struct {
		name     string
		fake     testkit.TLSMode
		mode     string
		response string
		code     string
	}{
		{name: "starttls_duplicate_literal_tag", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* ESEARCH (TAG {2}\r\n{TAG}) UID ALL 101,101"},
		{name: "implicit_tls_duplicate_literal_tag", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* ESEARCH (TAG {2}\r\n{TAG}) UID ALL 101,101"},
		{name: "starttls_duplicate_after_nested_all", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: `* ESEARCH (TAG "{TAG}") UID X (F ALL 101) ALL 101,101`},
		{name: "implicit_tls_duplicate_after_nested_all", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: `* ESEARCH (TAG "{TAG}") UID X (F ALL 101) ALL 101,101`},
		{name: "starttls_duplicate_after_unspaced_extension", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: `* ESEARCH (TAG "{TAG}") UID X(F ALL 101) ALL 101,101`},
		{name: "implicit_tls_duplicate_after_unspaced_extension", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: `* ESEARCH (TAG "{TAG}") UID X(F ALL 101) ALL 101,101`},
		{name: "starttls_duplicate_after_status_literal_marker", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* OK notice {18}\r\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "implicit_tls_duplicate_after_status_literal_marker", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* OK notice {18}\r\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "starttls_duplicate_after_unspaced_correlator", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* ESEARCH(TAG \"{TAG}\") UID ALL 101,101\r\n* SEARCH"},
		{name: "implicit_tls_duplicate_after_unspaced_correlator", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* ESEARCH(TAG \"{TAG}\") UID ALL 101,101\r\n* SEARCH"},
		{name: "starttls_duplicate_search_behind_lone_lf_status", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* OK notice\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "implicit_tls_duplicate_search_behind_lone_lf_status", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* OK notice\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "starttls_duplicate_esearch_behind_lone_lf_status", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* OK notice\n* ESEARCH (TAG \"{TAG}\") UID ALL 101,101\r\n* SEARCH"},
		{name: "implicit_tls_duplicate_esearch_behind_lone_lf_status", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* OK notice\n* ESEARCH (TAG \"{TAG}\") UID ALL 101,101\r\n* SEARCH"},
		{name: "starttls_duplicate_after_unspaced_status_literal_marker", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* OK(notice) {18}\r\n* SEARCH 101 101"},
		{name: "implicit_tls_duplicate_after_unspaced_status_literal_marker", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* OK(notice) {18}\r\n* SEARCH 101 101"},
		{name: "starttls_duplicate_after_quoted_literal_marker", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* LIST () \"/\" \"INBOX\" (\"X\" \"notice {18}\r\n\")\r\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "implicit_tls_duplicate_after_quoted_literal_marker", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* LIST () \"/\" \"INBOX\" (\"X\" \"notice {18}\r\n\")\r\n* SEARCH 101 101\r\n* SEARCH"},
		{name: "starttls_out_of_window_search_replaced_by_esearch", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: "* SEARCH 999\r\n* ESEARCH (TAG \"{TAG}\") UID ALL 101", code: bridge.CodeBoundsExceeded},
		{name: "implicit_tls_out_of_window_search_replaced_by_esearch", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: "* SEARCH 999\r\n* ESEARCH (TAG \"{TAG}\") UID ALL 101", code: bridge.CodeBoundsExceeded},
		{name: "starttls_out_of_window_all_replaced_by_later_all", fake: testkit.StartTLS, mode: bridge.TLSModeStartTLS, response: `* ESEARCH (TAG "{TAG}") UID ALL 999 ALL 101`, code: bridge.CodeBoundsExceeded},
		{name: "implicit_tls_out_of_window_all_replaced_by_later_all", fake: testkit.ImplicitTLS, mode: bridge.TLSModeImplicit, response: `* ESEARCH (TAG "{TAG}") UID ALL 999 ALL 101`, code: bridge.CodeBoundsExceeded},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			server, err := testkit.Start(testkit.Options{
				Mode:     test.fake,
				Scenario: testkit.Scenario{SearchResponse: test.response},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			config := fakeServerConfig(t, server.Addr(), test.mode, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
			config.Bounds = bridge.BoundsPatch{MaxSearchResults: bridge.Int(1)}
			adapter, err := bridge.NewAdapter(config)
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			page, err := adapter.SearchMailPage(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})

			want := test.code
			if want == "" {
				want = bridge.CodeIMAPProtocol
			}
			if bridge.CodeOf(err) != want || page.Messages != nil || page.Truncated {
				t.Fatalf("SearchMailPage invalid SEARCH response = %#v, %v, want %q and no partial page", page, err, want)
			}
			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
		})
	}
}

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
				if message.UID != wantUID || message.Subject != denseSubject(wantUID) {
					t.Fatalf("message %d = UID %d subject %q, want highest bounded UID %d", index, message.UID, message.Subject, wantUID)
				}
			}

			searchCommands := 0
			for _, command := range server.Commands() {
				upper := strings.ToUpper(command.Raw)
				if strings.Contains(upper, " UID SEARCH ") {
					searchCommands++
				}
				if strings.Contains(upper, " UID FETCH ") && !strings.Contains(upper, " UID FETCH 191:200 ") {
					t.Fatalf("search fetched UIDs beyond the bounded selection: %q", command.Raw)
				}
			}
			if searchCommands != 1 {
				t.Fatalf("UID SEARCH commands = %d, want one dense window", searchCommands)
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

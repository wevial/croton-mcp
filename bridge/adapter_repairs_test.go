package bridge_test

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestAdapterRejectsZeroMaxHeaderBytes(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxHeaderBytes = bridge.Int(0)

	if _, err := bridge.NewAdapter(config); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("NewAdapter zero maxHeaderBytes error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterRejectsMismatchedAndDuplicateFetchSections(t *testing.T) {
	for _, test := range []struct {
		name     string
		scenario testkit.Scenario
		body     bool
	}{
		{name: "metadata mismatched section", scenario: testkit.Scenario{FetchResponseSection: "[TEXT]"}},
		{name: "metadata duplicate section", scenario: testkit.Scenario{DuplicateFetchSection: true}},
		{name: "body mismatched section", scenario: testkit.Scenario{BodyFetchResponseSection: "[HEADER]"}, body: true},
		{name: "body duplicate section", scenario: testkit.Scenario{DuplicateBodyFetchSection: true}, body: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Scenario: test.scenario})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })

			adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
			if err != nil {
				t.Fatalf("NewAdapter: %v", err)
			}
			t.Cleanup(func() { _ = adapter.Close() })

			results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
			if !test.body {
				if bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
					t.Fatalf("SearchMail error = %v, want %q", err, bridge.CodeIMAPProtocol)
				}
				return
			}
			if err != nil || len(results) != 1 {
				t.Fatalf("SearchMail = %#v, %v", results, err)
			}
			if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
				t.Fatalf("GetMessageBody error = %v, want %q", err, bridge.CodeIMAPProtocol)
			}
		})
	}
}

func TestAdapterRejectsUnexpectedBinaryFetchLiteral(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{UnexpectedFetchBinaryLiteralBytes: 128 << 10},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("SearchMail unexpected binary literal error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterRejectsUnexpectedBinaryBodyFetchLiteral(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{UnexpectedBodyFetchBinaryLiteralBytes: 128 << 10},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxBodyBytes = bridge.Int(512)
	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(results) != 1 {
		t.Fatalf("SearchMail = %#v, %v", results, err)
	}

	if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("GetMessageBody unexpected binary literal error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
	if _, err := adapter.Status(context.Background(), "INBOX"); err != nil {
		t.Fatalf("Status after unexpected binary literal abort: %v", err)
	}
}

func TestAdapterRejectsLiteralDeclarationPayloadMismatch(t *testing.T) {
	const maxBodyBytes = 32

	server, err := testkit.Start(testkit.Options{
		Mode: testkit.ImplicitTLS,
		Scenario: testkit.Scenario{
			BodyFetchLiteralDeclaredBytes: maxBodyBytes,
			OversizedBodyLiteralBytes:     maxBodyBytes + 1,
		},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxBodyBytes = bridge.Int(maxBodyBytes)
	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(results) != 1 {
		t.Fatalf("SearchMail = %#v, %v", results, err)
	}
	if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeIMAPProtocol {
		t.Fatalf("GetMessageBody declaration/payload mismatch error = %v, want %q", err, bridge.CodeIMAPProtocol)
	}
}

func TestAdapterRejectsOversizedESearchRangeWithoutMaterializingIt(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{SearchResponse: "* ESEARCH (TAG \"{TAG}\") UID ALL 1:100000000"},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("SearchMail oversized ESEARCH range error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterReconnectsAfterBoundedAbort(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{OversizedBodyLiteralBytes: 33},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	config := fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()})
	config.Bounds.MaxBodyBytes = bridge.Int(32)
	adapter, err := bridge.NewAdapter(config)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(results) != 1 {
		t.Fatalf("SearchMail = %#v, %v", results, err)
	}
	if _, err := adapter.GetMessageBody(context.Background(), results[0].ID); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("GetMessageBody error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
	if _, err := adapter.Status(context.Background(), "INBOX"); err != nil {
		t.Fatalf("Status after bounded abort: %v", err)
	}

	var connections []int
	for _, command := range server.Commands() {
		if command.Name == "STATUS" {
			connections = append(connections, command.ConnectionID)
		}
	}
	if len(connections) != 1 || connections[0] < 2 {
		t.Fatalf("status did not use a post-abort connection: %+v", server.Commands())
	}
}

func TestAdapterMapsLoginRejectionToAuthentication(t *testing.T) {
	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Scenario: testkit.Scenario{RejectAuthentication: true}})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.ListFolders(context.Background()); bridge.CodeOf(err) != bridge.CodeAuthentication {
		t.Fatalf("ListFolders rejected login error = %v, want %q", err, bridge.CodeAuthentication)
	}
	if transcript := strings.Join(commandStrings(server.Commands()), "\n"); !strings.Contains(transcript, "LOGIN") {
		t.Fatalf("authentication test transcript = %q, want LOGIN", transcript)
	}
}

func TestAdapterReplaysLoginDisconnectOnce(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{DisconnectAfterCommand: 3},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	folders, err := adapter.ListFolders(context.Background())
	if err != nil || len(folders) != 1 {
		t.Fatalf("ListFolders after login disconnect = %#v, %v", folders, err)
	}

	logins := 0
	connections := make(map[int]struct{})
	for _, command := range server.Commands() {
		if command.Name != "LOGIN" {
			continue
		}

		logins++
		connections[command.ConnectionID] = struct{}{}
	}
	if logins != 2 || len(connections) != 2 {
		t.Fatalf("LOGIN replay transcript = %+v, want two LOGIN commands on distinct connections", server.Commands())
	}
}

func TestAdapterRejectsOversizedSearchResponseBudget(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{SearchResponse: "* SEARCH" + strings.Repeat(" 101", 1100)},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	if _, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"}); bridge.CodeOf(err) != bridge.CodeBoundsExceeded {
		t.Fatalf("SearchMail oversized SEARCH response error = %v, want %q", err, bridge.CodeBoundsExceeded)
	}
}

func TestAdapterSearchesAtMostEightDescendingWindows(t *testing.T) {
	server, err := testkit.Start(testkit.Options{
		Mode:     testkit.ImplicitTLS,
		Scenario: testkit.Scenario{UIDNext: 1000, SearchWindowResult: true},
	})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	adapter, err := bridge.NewAdapter(fakeServerConfig(t, server.Addr(), bridge.TLSModeImplicit, bridge.TLSConfig{SPKISHA256: server.SPKISHA256()}))
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	results, err := adapter.SearchMail(context.Background(), bridge.SearchQuery{Mailbox: "INBOX"})
	if err != nil || len(results) != 8 {
		t.Fatalf("SearchMail = %#v, %v", results, err)
	}

	var windows []string
	for _, command := range server.Commands() {
		if strings.Contains(command.Raw, "UID SEARCH") {
			windows = append(windows, command.Raw)
		}
	}
	if len(windows) != 8 {
		t.Fatalf("search windows = %q, want eight", windows)
	}
	for index, window := range windows {
		end := 999 - index*100
		start := end - 99
		fragment := "UID " + strconv.Itoa(start) + ":" + strconv.Itoa(end)
		if !strings.Contains(window, fragment) {
			t.Fatalf("window %d = %q, want %q", index, window, fragment)
		}
	}
}

func commandStrings(commands []testkit.Command) []string {
	result := make([]string, 0, len(commands))
	for _, command := range commands {
		result = append(result, command.Raw)
	}
	return result
}

package mcpserver

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// mutatingIMAPCommands is the vocabulary that must never appear in any
// transcript produced through the MCP tool surface.
var mutatingIMAPCommands = []string{
	"STORE", "APPEND", "EXPUNGE", "DELETE", "RENAME", "CREATE", "COPY", "MOVE", "SETACL", "SETQUOTA", "SUBSCRIBE", "UNSUBSCRIBE",
}

func startIntegrationAdapter(t *testing.T) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS})
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

	server, adapter := startIntegrationAdapter(t)
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
		Attachments []attachmentResult `json:"attachments"`
	}
	decodeResult(t, attachments, &decodedAttachments)

	digest := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false})
	var decodedDigest digestDecoded
	decodeResult(t, digest, &decodedDigest)

	requireReadOnlyTranscript(t, server)
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatalf("insecure authentication: %v", err)
	}
}

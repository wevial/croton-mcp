package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
)

func callTool(t *testing.T, session *mcp.ClientSession, name string, arguments any) *mcp.CallToolResult {
	t.Helper()

	result, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: arguments})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return result
}

func resultText(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()

	if len(result.Content) != 1 {
		t.Fatalf("content items = %d, want 1", len(result.Content))
	}
	text, ok := result.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatalf("content type = %T, want *mcp.TextContent", result.Content[0])
	}
	return text.Text
}

func decodeResult(t *testing.T, result *mcp.CallToolResult, target any) {
	t.Helper()

	if result.IsError {
		t.Fatalf("unexpected tool error: %s", resultText(t, result))
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), target); err != nil {
		t.Fatalf("tool result is not valid JSON: %v: %s", err, resultText(t, result))
	}
}

func requireToolError(t *testing.T, result *mcp.CallToolResult, wantCode string) {
	t.Helper()

	if !result.IsError {
		t.Fatalf("expected tool error %q, got success: %s", wantCode, resultText(t, result))
	}
	text := resultText(t, result)
	var envelope struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("error result is not valid JSON: %v: %s", err, text)
	}
	if envelope.Error.Code != wantCode {
		t.Fatalf("error code = %q, want %q (%s)", envelope.Error.Code, wantCode, text)
	}
}

func TestListFoldersReturnsBoundedFolderList(t *testing.T) {
	t.Parallel()

	mail := &fakeMail{folders: []bridge.Folder{
		{Name: "INBOX", Delimiter: "/"},
		{Name: "Folders/Receipts", Delimiter: "/"},
	}}
	session := connectTestClient(t, Options{Mail: mail})

	result := callTool(t, session, "list_folders", map[string]any{})

	var decoded struct {
		Folders []struct {
			Name      string `json:"name"`
			Delimiter string `json:"delimiter"`
		} `json:"folders"`
	}
	decodeResult(t, result, &decoded)
	if len(decoded.Folders) != 2 {
		t.Fatalf("folders = %d, want 2", len(decoded.Folders))
	}
	if decoded.Folders[0].Name != "INBOX" || decoded.Folders[1].Name != "Folders/Receipts" {
		t.Fatalf("unexpected folder names: %+v", decoded.Folders)
	}
	if decoded.Folders[0].Delimiter != "/" {
		t.Fatalf("unexpected delimiter: %+v", decoded.Folders[0])
	}
}

func TestListFoldersRejectsUnknownArguments(t *testing.T) {
	t.Parallel()

	session := connectTestClient(t, Options{Mail: &fakeMail{}})

	result := callTool(t, session, "list_folders", map[string]any{"pattern": "*"})

	requireToolError(t, result, errInvalidArgument)
}

func TestDecodeArgumentsRejectsNonObjectDuplicateAndOversizeJSON(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name      string
		arguments json.RawMessage
	}{
		{name: "null", arguments: json.RawMessage(`null`)},
		{name: "duplicate field", arguments: json.RawMessage(`{"mailbox":"INBOX","mailbox":"Archive"}`)},
		{name: "trailing value", arguments: json.RawMessage(`{"mailbox":"INBOX"} true`)},
		{name: "oversize raw arguments", arguments: json.RawMessage(strings.Repeat(" ", maxToolArgumentsBytes) + `{"mailbox":"INBOX"}`)},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			var input struct {
				Mailbox string `json:"mailbox"`
			}
			if decodeArguments(testCase.arguments, &input) {
				t.Fatalf("decodeArguments accepted %s", testCase.arguments)
			}
		})
	}
}

func TestBaseSubjectTruncatesOnUTF8Boundary(t *testing.T) {
	t.Parallel()

	subject := strings.Repeat("界", maxSearchTermBytes/len("界")+1)
	got := baseSubject(subject)
	if len(got) > maxSearchTermBytes {
		t.Fatalf("subject bytes = %d, want <= %d", len(got), maxSearchTermBytes)
	}
	if !utf8.ValidString(got) {
		t.Fatal("subject truncation split a UTF-8 code point")
	}
}

func TestDecodeArgumentsRejectsNullAndCaseFoldedAliases(t *testing.T) {
	t.Parallel()

	type input struct {
		Mailbox string `json:"mailbox"`
		Sender  string `json:"sender"`
		Subject string `json:"subject"`
		Limit   *int   `json:"limit"`
	}
	cases := []json.RawMessage{
		json.RawMessage(`{"mailbox":"INBOX","limit":null}`),
		json.RawMessage(`{"mailbox":"INBOX","Mailbox":"Archive"}`),
	}
	for _, arguments := range cases {
		var decoded input
		if decodeArguments(arguments, &decoded) {
			t.Fatalf("ambiguous schema-invalid arguments accepted: %s", arguments)
		}
	}
}

func TestDecodeArgumentsAcceptsSchemaBoundedEscapedStrings(t *testing.T) {
	t.Parallel()

	type input struct {
		Mailbox string `json:"mailbox"`
		Sender  string `json:"sender"`
		Subject string `json:"subject"`
	}
	arguments := json.RawMessage(`{"mailbox":"INBOX","sender":"` +
		strings.Repeat(`\u0073`, maxSearchTermBytes) +
		`","subject":"` + strings.Repeat(`\u0074`, maxSearchTermBytes) + `"}`)
	var decoded input
	if !decodeArguments(arguments, &decoded) {
		t.Fatalf("decoded-size-bounded arguments rejected at %d wire bytes", len(arguments))
	}
	if len(decoded.Sender) != maxSearchTermBytes || len(decoded.Subject) != maxSearchTermBytes {
		t.Fatalf("decoded lengths = sender:%d subject:%d", len(decoded.Sender), len(decoded.Subject))
	}
}

func TestToolErrorsMapBridgeCodesStably(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name       string
		underlying error
		wantCode   string
	}{
		{"mailbox not found", &bridge.Error{Code: bridge.CodeMailboxNotFound}, errNotFound},
		{"stale id", &bridge.Error{Code: bridge.CodeStaleMessageID}, errStaleID},
		{"bounds", &bridge.Error{Code: bridge.CodeBoundsExceeded}, errBoundsExceeded},
		{"timeout", &bridge.Error{Code: bridge.CodeCommandTimedOut}, errTimedOut},
		{"canceled", &bridge.Error{Code: bridge.CodeOperationCanceled}, errCanceled},
		{"unreachable", &bridge.Error{Code: bridge.CodeBridgeUnreachable}, errUnavailable},
		{"authentication", &bridge.Error{Code: bridge.CodeAuthentication}, errUnavailable},
		{"credential command", &bridge.Error{Code: bridge.CodeCredentialCommand}, errUnavailable},
		{"tls mismatch", &bridge.Error{Code: bridge.CodeTLSMismatch}, errUnavailable},
		{"imap protocol", &bridge.Error{Code: bridge.CodeIMAPProtocol}, errUnavailable},
		{"adapter closed", &bridge.Error{Code: bridge.CodeAdapterClosed}, errUnavailable},
		{"unknown error", errors.New("dial tcp 127.0.0.1:1143: secret-user@mail.test password hunter2"), errInternal},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()

			session := connectTestClient(t, Options{Mail: &fakeMail{err: testCase.underlying}})

			result := callTool(t, session, "list_folders", map[string]any{})

			requireToolError(t, result, testCase.wantCode)
			text := resultText(t, result)
			for _, fragment := range []string{"secret-user", "hunter2", "127.0.0.1", "dial tcp", "mail.test"} {
				if strings.Contains(text, fragment) {
					t.Fatalf("error result leaks %q: %s", fragment, text)
				}
			}
		})
	}
}

package mcpserver

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
)

// readCalls reaches every read tool with arguments that pass validation.
var readCalls = map[string]map[string]any{
	"list_folders":             {},
	"search_mail":              {"mailbox": "INBOX"},
	"select_digest_candidates": {"mailbox": "INBOX"},
	"get_message":              {"messageId": "opaque-target"},
	"get_thread":               {"messageId": "opaque-target"},
	"list_attachments":         {"messageId": "opaque-target"},
}

func identity(id string, uid uint32) bridge.MessageMetadata {
	return bridge.MessageMetadata{ID: id, Mailbox: "INBOX", UIDValidity: 3001, UID: uid, Subject: "Plan"}
}

func TestToolErrorsMapBridgeCodesStably(t *testing.T) {
	t.Parallel()

	for underlying, want := range map[error]string{
		&bridge.Error{Code: bridge.CodeMailboxNotFound}:                      errNotFound,
		&bridge.Error{Code: bridge.CodeInvalidRequest}:                       errInvalidArgument,
		&bridge.Error{Code: bridge.CodeUnsupported}:                          errUnsupported,
		&bridge.Error{Code: bridge.CodeStaleMessageID}:                       errStaleID,
		&bridge.Error{Code: bridge.CodeBoundsExceeded}:                       errBoundsExceeded,
		&bridge.Error{Code: bridge.CodeCommandTimedOut}:                      errTimedOut,
		&bridge.Error{Code: bridge.CodeOperationCanceled}:                    errCanceled,
		&bridge.Error{Code: bridge.CodeAuthentication}:                       errUnavailable,
		&bridge.Error{Code: bridge.CodeTLSMismatch}:                          errUnavailable,
		&bridge.Error{Code: "future hunter2"}:                                errInternal,
		errors.New("dial tcp 127.0.0.1:1143: secret-user@mail.test hunter2"): errInternal,
	} {
		session := connectTestClient(t, Options{Mail: &fakeMail{err: underlying}})
		for tool, arguments := range readCalls {
			requireToolError(t, callTool(t, session, tool, arguments), want)
		}
	}
}

func TestToolsRefuseMissingCapabilitiesAndPanics(t *testing.T) {
	t.Parallel()

	// Embedding only Mail hides the optional header and structure readers;
	// a nil embedded Mail panics on every call.
	readerless := connectTestClient(t, Options{Mail: struct{ Mail }{&fakeMail{}}})
	requireToolError(t, callTool(t, readerless, "get_thread", readCalls["get_thread"]), errUnsupported)
	requireToolError(t, callTool(t, readerless, "list_attachments", readCalls["list_attachments"]), errUnsupported)

	absent := connectTestClient(t, Options{})
	panicking := connectTestClient(t, Options{Mail: struct{ Mail }{}})
	for tool, arguments := range readCalls {
		requireToolError(t, callTool(t, absent, tool, arguments), errUnavailable)
		want := errInternal
		if tool == "get_thread" || tool == "list_attachments" {
			want = errUnsupported
		}
		requireToolError(t, callTool(t, panicking, tool, arguments), want)
	}
}

func TestToolsRefuseResultsWithoutReadIdentity(t *testing.T) {
	t.Parallel()

	missing := identity("opaque-target", 0)
	session := connectTestClient(t, Options{Mail: &fakeMail{
		searched:  []bridge.MessageMetadata{identity("opaque-a", 7), missing},
		metadata:  missing,
		body:      []byte("Subject: Plan\r\n\r\nbody\r\n"),
		headers:   map[string]bridge.MessageHeaders{"opaque-target": {Metadata: missing}},
		structure: bridge.MessageAttachmentStructure{ID: "opaque-other", Mailbox: "INBOX", UIDValidity: 3001, UID: 7},
	}})

	for tool, arguments := range readCalls {
		if tool != "list_folders" {
			requireToolError(t, callTool(t, session, tool, arguments), errUnavailable)
		}
	}
}

func TestGetThreadOmitsUnreadableSiblingsByReasonOnly(t *testing.T) {
	t.Parallel()

	headers := func(metadata bridge.MessageMetadata) bridge.MessageHeaders {
		return bridge.MessageHeaders{Metadata: metadata, Headers: bridge.CanonicalHeaders{Subject: "Re: Plan", MessageID: "<" + metadata.ID + "@croton.test>"}}
	}
	mail := &fakeMail{
		searched: []bridge.MessageMetadata{identity("opaque-stale", 4), identity("opaque-malformed", 3), identity("opaque-zero", 2), identity("opaque-ok", 1)},
		headers: map[string]bridge.MessageHeaders{
			"opaque-target": headers(identity("opaque-target", 5)),
			"opaque-zero":   headers(identity("opaque-zero", 0)),
			"opaque-ok":     headers(identity("opaque-ok", 1)),
		},
		headerErr: map[string]error{
			"opaque-stale":     &bridge.Error{Code: bridge.CodeStaleMessageID},
			"opaque-malformed": &bridge.NormalizeError{Code: threadUnavailableMalformedHeader},
		},
	}
	session := connectTestClient(t, Options{Mail: mail})

	var thread getThreadResult
	decodeResult(t, callTool(t, session, "get_thread", readCalls["get_thread"]), &thread)
	reasons, _ := json.Marshal(thread.UnavailableMembers)
	if len(thread.Nodes) != 2 || !thread.Truncated || string(reasons) != `[{"reason":"stale_id"},{"reason":"malformed_header"},{"reason":"invalid_identity"}]` {
		t.Fatalf("thread = %+v, unavailable %s", thread, reasons)
	}

	// Failed attempts spend the sibling budget: maxMessages=3 allows the
	// target read plus two sibling attempts, however many siblings are stale.
	stale := &fakeMail{headers: mail.headers, headerErr: map[string]error{}}
	for uid := uint32(100); uid < 130; uid++ {
		id := fmt.Sprintf("opaque-stale-%d", uid)
		stale.searched = append(stale.searched, identity(id, uid))
		stale.headerErr[id] = &bridge.Error{Code: bridge.CodeStaleMessageID}
	}
	decodeResult(t, callTool(t, connectTestClient(t, Options{Mail: stale}), "get_thread", map[string]any{"messageId": "opaque-target", "maxMessages": 3}), &thread)
	if stale.headerReads != 3 || len(thread.Nodes) != 1 || len(thread.UnavailableMembers) != 2 || !thread.Truncated {
		t.Fatalf("stale siblings: %d header reads, thread %+v; want 3 reads and two omissions", stale.headerReads, thread)
	}

	mail.headerErr["opaque-stale"] = &bridge.Error{Code: bridge.CodeCommandTimedOut}
	requireToolError(t, callTool(t, session, "get_thread", readCalls["get_thread"]), errTimedOut)
}

func TestFlatResultsPublishOnlyTrustedMetadata(t *testing.T) {
	t.Parallel()

	long := strings.Repeat("é", 300)
	items := []bridge.MessageMetadata{
		identity("opaque-1", 4), identity("opaque-2", 3), identity("opaque-3", 2), identity("opaque-4", 1),
	}
	items[0].Date, items[0].DateSource, items[0].ReadState, items[0].From = "2026-01-01T00:00:00Z", bridge.DateSourceHeader, bridge.ReadStateRead, "Known <known@croton.test>"
	items[1].Date, items[1].DateSource, items[1].From = "2026-01-01T02:00:00+02:00", bridge.DateSourceHeader, long
	items[2].Date, items[2].ReadState, items[2].From = "2026-01-01T00:00:00Z", bridge.ReadStateUnread, "\xff invalid"
	session := connectTestClient(t, Options{Mail: &fakeMail{searched: items}})

	var search wireSearch
	decodeResult(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 3}), &search)
	want := []wireItem{
		{ID: "opaque-1", Mailbox: "INBOX", UIDValidity: 3001, UID: 4, Subject: "Plan", From: "Known <known@croton.test>", Date: "2026-01-01T00:00:00Z", DateSource: "header", ReadState: "read"},
		{ID: "opaque-2", Mailbox: "INBOX", UIDValidity: 3001, UID: 3, Subject: "Plan", From: long[:512], DateSource: "unknown", ReadState: "unknown", MetadataTruncated: true},
		{ID: "opaque-3", Mailbox: "INBOX", UIDValidity: 3001, UID: 2, Subject: "Plan", DateSource: "unknown", ReadState: "unread"},
	}
	if !search.Truncated || len(search.Results) != len(want) {
		t.Fatalf("search = %+v, want three results and truncation", search)
	}
	for index := range want {
		if search.Results[index] != want[index] {
			t.Fatalf("result %d = %+v, want %+v", index, search.Results[index], want[index])
		}
	}
}

func TestDecodeArgumentsRejectsAmbiguousJSON(t *testing.T) {
	t.Parallel()

	for _, arguments := range []string{
		`null`,
		`{"mailbox":"INBOX","mailbox":"Archive"}`,
		`{"mailbox":"INBOX","Mailbox":"Archive"}`,
		`{"mailbox":"INBOX","limit":null}`,
		`{"mailbox":"INBOX"} true`,
		strings.Repeat(" ", maxToolArgumentsBytes) + `{"mailbox":"INBOX"}`,
	} {
		var input struct {
			Mailbox string `json:"mailbox"`
			Limit   *int   `json:"limit"`
		}
		if decodeArguments(json.RawMessage(arguments), &input) {
			t.Fatalf("decodeArguments accepted %.80s", arguments)
		}
	}
}

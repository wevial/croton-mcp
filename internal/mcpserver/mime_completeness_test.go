package mcpserver

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// The closed key sets of the two selected read results, their attachment
// entries and their truncation objects.
var (
	getMessageKeys = map[string]bool{
		"id": true, "mailbox": true, "uidvalidity": true, "uid": true, "size": true, "headers": true,
		"text": true, "textSource": true, "attachments": true, "truncation": true, "truncated": true,
	}
	listAttachmentsKeys = map[string]bool{
		"id": true, "mailbox": true, "uidvalidity": true, "uid": true, "attachments": true, "truncation": true, "truncated": true,
	}
	attachmentEntryKeys = map[string]bool{
		"filename": true, "contentType": true, "disposition": true, "hasDeclaredSize": true, "declaredSize": true,
	}
	getMessageReasons = map[string]bool{
		"headers": true, "headerCount": true, "text": true, "mimeParts": true, "mimeDepth": true,
		"attachments": true, "references": true, "output": true,
	}
	listAttachmentsReasons = map[string]bool{
		"headers": true, "headerCount": true, "mimeParts": true, "mimeDepth": true, "attachments": true, "output": true,
	}
)

var completenessTuple = readTuple{Mailbox: "INBOX", UIDValidity: 4242, UID: 77}

// completenessHeader is the root header block of a synthetic fixture message,
// without its Content-Type and blank separator line.
func completenessHeader(subject string) string {
	return "From: Croton Fixture <fixture@croton.test>\r\n" +
		"To: Reader <reader@croton.test>\r\n" +
		"Subject: " + subject + "\r\n" +
		"Message-ID: <completeness@croton.test>\r\n" +
		"MIME-Version: 1.0\r\n"
}

// multipartBody joins already-formed MIME parts under one boundary.
func multipartBody(boundary string, parts []string) string {
	var body strings.Builder
	for _, part := range parts {
		body.WriteString("--" + boundary + "\r\n" + part + "\r\n")
	}
	body.WriteString("--" + boundary + "--\r\n")
	return body.String()
}

func multipartMessage(subject string, parts []string) string {
	return completenessHeader(subject) +
		"Content-Type: multipart/mixed; boundary=\"root-boundary\"\r\n\r\n" +
		multipartBody("root-boundary", parts)
}

func harmlessPart(index int) string {
	return fmt.Sprintf("Content-Type: text/plain; charset=utf-8\r\n\r\nHarmless synthetic part %d.\r\n", index)
}

// attachmentPart declares one attachment; sizeParameter is appended verbatim
// to its Content-Disposition, so an empty value declares no size.
func attachmentPart(filename, sizeParameter, content string) string {
	return "Content-Type: application/octet-stream; name=\"" + filename + "\"\r\n" +
		"Content-Disposition: attachment; filename=\"" + filename + "\"" + sizeParameter + "\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" +
		base64.StdEncoding.EncodeToString([]byte(content)) + "\r\n"
}

func completenessSession(t *testing.T, body string) *mcp.ClientSession {
	t.Helper()

	metadata := bridge.MessageMetadata{ID: "opaque-completeness", Mailbox: completenessTuple.Mailbox, Subject: "Synthetic completeness"}
	mail := &fakeMail{metadata: withReadIdentity(metadata, completenessTuple.UIDValidity, completenessTuple.UID), body: []byte(body)}
	return connectTestClient(t, Options{Mail: mail})
}

// selectedReads calls get_message and list_attachments for one message id and
// pins both results' closed keys and exact parent identity.
func selectedReads(t *testing.T, session *mcp.ClientSession, id string, tuple readTuple) (map[string]any, map[string]any) {
	t.Helper()

	message := rawObject(t, callTool(t, session, "get_message", map[string]any{"messageId": id}))
	listing := rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": id}))
	for _, read := range []struct {
		label   string
		object  map[string]any
		allowed map[string]bool
	}{{"get_message", message, getMessageKeys}, {"list_attachments", listing, listAttachmentsKeys}} {
		requireClosedKeys(t, read.label, read.object, read.allowed)
		if read.object["id"] != id || flatTuple(t, read.object) != tuple {
			t.Fatalf("%s identity = %v/%+v, want %s/%+v", read.label, read.object["id"], flatTuple(t, read.object), id, tuple)
		}
	}
	return message, listing
}

// requireReasons pins the result's truncation object to exactly the wanted
// reasons, each serialized as true, and the aggregate marker to match.
func requireReasons(t *testing.T, label string, object map[string]any, allowed map[string]bool, want ...string) {
	t.Helper()

	truncation, ok := object["truncation"].(map[string]any)
	if !ok {
		t.Fatalf("%s truncation is not an object: %v", label, object["truncation"])
	}
	requireClosedKeys(t, label+" truncation", truncation, allowed)
	if len(truncation) != len(want) {
		t.Fatalf("%s truncation = %v, want exactly %v", label, truncation, want)
	}
	for _, reason := range want {
		if truncation[reason] != true {
			t.Fatalf("%s truncation = %v, want %q true", label, truncation, reason)
		}
	}
	requireTruncated(t, label, object, len(want) > 0)
}

type wantAttachment struct {
	filename string
	size     int64
	known    bool
}

func requireAttachmentEntries(t *testing.T, label string, object map[string]any, want []wantAttachment) {
	t.Helper()

	entries := rawItems(t, object, "attachments")
	if len(entries) != len(want) {
		t.Fatalf("%s attachments = %d, want %d", label, len(entries), len(want))
	}
	for index, entry := range entries {
		requireClosedKeys(t, label+" attachment", entry, attachmentEntryKeys)
		requireNoIdentityKeys(t, label+" attachment", entry)
		if entry["filename"] != want[index].filename || entry["contentType"] != "application/octet-stream" || entry["disposition"] != "attachment" {
			t.Fatalf("%s attachment %d = %v, want %s", label, index, entry, want[index].filename)
		}

		has, present := entry["hasDeclaredSize"]
		if !present || has != want[index].known {
			t.Fatalf("%s attachment %d hasDeclaredSize = %v (present %v), want explicit %v", label, index, has, present, want[index].known)
		}
		size, present := entry["declaredSize"]
		if present != want[index].known {
			t.Fatalf("%s attachment %d declaredSize = %v (present %v), want present %v", label, index, size, present, want[index].known)
		}
		if want[index].known && size != json.Number(fmt.Sprint(want[index].size)) {
			t.Fatalf("%s attachment %d declaredSize = %v, want %d", label, index, size, want[index].size)
		}
	}
}

func TestMIMECompletenessPublic(t *testing.T) {
	isolateFixtureEnvironment(t)

	t.Run("size_presence", func(t *testing.T) {
		t.Parallel()

		body := multipartMessage("Synthetic size presence", []string{
			harmlessPart(0),
			attachmentPart("positive.bin", "; size=1234", "positive"),
			attachmentPart("zero.bin", "; size=0", "zero"),
			attachmentPart("absent.bin", "", "absent"),
			attachmentPart("negative.bin", "; size=-7", "negative"),
		})
		want := []wantAttachment{
			{filename: "positive.bin", size: 1234, known: true},
			{filename: "zero.bin", size: 0, known: true},
			{filename: "absent.bin"},
			{filename: "negative.bin"},
		}

		message, listing := selectedReads(t, completenessSession(t, body), "opaque-completeness", completenessTuple)
		requireAttachmentEntries(t, "get_message", message, want)
		requireAttachmentEntries(t, "list_attachments", listing, want)
		requireReasons(t, "get_message", message, getMessageReasons)
		requireReasons(t, "list_attachments", listing, listAttachmentsReasons)

		// A defensive projection never turns an invalid value into a known size.
		encoded, err := json.Marshal(attachmentResults([]bridge.AttachmentMetadata{
			{Filename: "flagged-negative.bin", DeclaredSize: -3, HasDeclaredSize: true},
		}))
		if err != nil {
			t.Fatalf("marshal attachments: %v", err)
		}
		const wantJSON = `[{"filename":"flagged-negative.bin","hasDeclaredSize":false}]`
		if string(encoded) != wantJSON {
			t.Fatalf("projection = %s, want %s", encoded, wantJSON)
		}
	})

	t.Run("structural_omission", func(t *testing.T) {
		t.Parallel()

		manyParts := []string{}
		for index := range 49 {
			manyParts = append(manyParts, harmlessPart(index))
		}
		manyParts = append(manyParts, attachmentPart("beyond-parts.bin", "; size=9", "beyond"))

		// Ten nested multipart levels put the attachment at depth 11.
		deep := attachmentPart("too-deep.bin", "; size=9", "deep")
		for level := 10; level >= 1; level-- {
			boundary := fmt.Sprintf("depth-%d", level)
			deep = "Content-Type: multipart/mixed; boundary=\"" + boundary + "\"\r\n\r\n" + multipartBody(boundary, []string{deep})
		}

		// The multipart Content-Type is the 101st field, so the header count
		// bound hides the attachment it would have introduced.
		var headerCount strings.Builder
		headerCount.WriteString(completenessHeader("Synthetic header count"))
		for index := range 95 {
			fmt.Fprintf(&headerCount, "X-Synthetic-Filler-%d: filler\r\n", index)
		}
		headerCount.WriteString("Content-Type: multipart/mixed; boundary=\"root-boundary\"\r\n\r\n")
		headerCount.WriteString(multipartBody("root-boundary", []string{harmlessPart(0), attachmentPart("hidden.bin", "; size=9", "hidden")}))

		many := []string{}
		for index := range 21 {
			many = append(many, attachmentPart(fmt.Sprintf("file-%02d.bin", index), "", "many"))
		}
		twenty := make([]wantAttachment, 0, 20)
		for index := range 20 {
			twenty = append(twenty, wantAttachment{filename: fmt.Sprintf("file-%02d.bin", index)})
		}

		var references strings.Builder
		references.WriteString("References:")
		for index := range 51 {
			fmt.Fprintf(&references, " <reference-%d@croton.test>", index)
		}

		for _, tc := range []struct {
			name           string
			body           string
			attachments    []wantAttachment
			messageReasons []string
			listReasons    []string
		}{
			{
				name:           "mime_parts",
				body:           multipartMessage("Synthetic parts", manyParts),
				messageReasons: []string{"mimeParts"},
				listReasons:    []string{"mimeParts"},
			},
			{
				name:           "mime_depth",
				body:           completenessHeader("Synthetic depth") + "Content-Type: multipart/mixed; boundary=\"depth-0\"\r\n\r\n" + multipartBody("depth-0", []string{deep}),
				messageReasons: []string{"mimeDepth"},
				listReasons:    []string{"mimeDepth"},
			},
			{
				name:           "header_count",
				body:           headerCount.String(),
				messageReasons: []string{"headerCount"},
				listReasons:    []string{"headerCount"},
			},
			{
				name:           "attachments",
				body:           multipartMessage("Synthetic many", many),
				attachments:    twenty,
				messageReasons: []string{"attachments"},
				listReasons:    []string{"attachments"},
			},
			{
				// A reference-only limit marks the message but not its
				// attachment list.
				name:           "references",
				body:           completenessHeader("Synthetic references") + references.String() + "\r\nContent-Type: text/plain\r\n\r\nReferences only.\r\n",
				messageReasons: []string{"references"},
			},
		} {
			message, listing := selectedReads(t, completenessSession(t, tc.body), "opaque-completeness", completenessTuple)
			if _, present := message["attachments"]; len(tc.attachments) > 0 || present {
				requireAttachmentEntries(t, tc.name+" get_message", message, tc.attachments)
			}
			requireAttachmentEntries(t, tc.name+" list_attachments", listing, tc.attachments)
			requireReasons(t, tc.name+" get_message", message, getMessageReasons, tc.messageReasons...)
			requireReasons(t, tc.name+" list_attachments", listing, listAttachmentsReasons, tc.listReasons...)
		}
	})

	t.Run("output_propagation", func(t *testing.T) {
		t.Parallel()

		// Latin-1 text doubles when decoded, so the parser's text limit and the
		// output cap both apply to one public get_message call.
		latin := completenessHeader("Synthetic output pressure") + "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + strings.Repeat("\xe9", 200<<10) + "\r\n"
		result := callTool(t, completenessSession(t, latin), "get_message", map[string]any{"messageId": "opaque-completeness"})
		text := resultText(t, result)
		if len(text) > maxToolResultBytes || !utf8.ValidString(text) {
			t.Fatalf("public output is %d bytes or invalid UTF-8", len(text))
		}
		message := rawObject(t, result)
		requireClosedKeys(t, "public get_message", message, getMessageKeys)
		if message["id"] != "opaque-completeness" || flatTuple(t, message) != completenessTuple {
			t.Fatalf("public output identity changed: %v %+v", message["id"], flatTuple(t, message))
		}
		requireReasons(t, "public get_message", message, getMessageReasons, "text", "output")

		prior := messageTruncation{Headers: true, Text: true, References: true, MIMEParts: true}
		fallback := &getMessageResult{
			ID: "opaque-completeness", Mailbox: "INBOX", UIDValidity: 4242, UID: 77,
			Headers: bridge.CanonicalHeaders{Subject: strings.Repeat("s", maxToolResultBytes)}, TextSource: "plain", Truncation: prior, Truncated: true,
		}
		listPressure := make([]attachmentResult, 0, 600)
		for index := range 600 {
			listPressure = append(listPressure, attachmentResult{Filename: fmt.Sprintf("%03d-%s", index, strings.Repeat("b", 250)), ContentType: "application/octet-stream"})
		}
		listing := &listAttachmentsResult{
			ID: "opaque-completeness", Mailbox: "INBOX", UIDValidity: 4242, UID: 77,
			Attachments: listPressure, Truncation: attachmentListTruncation{MIMEDepth: true, HeaderCount: true}, Truncated: true,
		}
		listFallback := &listAttachmentsResult{
			ID: "opaque-completeness", Mailbox: "INBOX", UIDValidity: 4242, UID: 77,
			Attachments: []attachmentResult{}, Truncation: attachmentListTruncation{Attachments: true}, Truncated: true,
		}

		for _, tc := range []struct {
			name    string
			result  any
			allowed map[string]bool
			want    []string
		}{
			{"get_message_fallback", fallback, getMessageReasons, []string{"headers", "text", "references", "mimeParts", "output"}},
			{"list_attachments_shrink", listing, listAttachmentsReasons, []string{"mimeDepth", "headerCount", "output"}},
		} {
			encoded, truncated, err := encodeBounded(tc.result)
			if err != nil || !truncated || !utf8.Valid(encoded) {
				t.Fatalf("%s: encodeBounded err %v truncated %v utf8 %v", tc.name, err, truncated, utf8.Valid(encoded))
			}
			object := decodeBounded(t, encoded)
			if object["id"] != "opaque-completeness" || flatTuple(t, object) != completenessTuple {
				t.Fatalf("%s identity changed: %v %+v", tc.name, object["id"], flatTuple(t, object))
			}
			requireReasons(t, tc.name, object, tc.allowed, tc.want...)
		}

		listFallback.ID = strings.Repeat("i", maxToolResultBytes)
		unfittable := &getMessageResult{ID: listFallback.ID, Mailbox: "INBOX", UIDValidity: 4242, UID: 77, Truncation: prior}
		for _, oversized := range []any{listFallback, unfittable} {
			if encoded, _, err := encodeBounded(oversized); err != errUnretainableIdentity || encoded != nil {
				t.Fatalf("unfittable identity returned %d bytes, err %v", len(encoded), err)
			}
		}
	})

	t.Run("wire_and_privacy", func(t *testing.T) {
		t.Parallel()

		zeroContent := highEntropySentinel(t, "ZERO")
		absentContent := highEntropySentinel(t, "ABSENT")
		sizes := multipartMessage("Synthetic completeness sizes", []string{
			harmlessPart(0),
			attachmentPart("zero.bin", "; size=0", zeroContent),
			attachmentPart("absent.bin", "", absentContent),
		})
		server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{Messages: []string{sizes}})
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		_, results := searchItems(t, session, map[string]any{"mailbox": "INBOX", "subject": "Synthetic completeness"})
		if len(results) != 1 || flatTuple(t, results[0]).Mailbox != "INBOX" {
			t.Fatalf("search returned %v, want the one fixture message", results)
		}
		id, _ := results[0]["id"].(string)

		before := len(server.Commands())
		message, listing := selectedReads(t, session, id, flatTuple(t, results[0]))
		want := []wantAttachment{{filename: "zero.bin", size: 0, known: true}, {filename: "absent.bin"}}
		requireAttachmentEntries(t, "get_message", message, want)
		requireAttachmentEntries(t, "list_attachments", listing, want)
		requireReasons(t, "get_message", message, getMessageReasons)
		requireReasons(t, "list_attachments", listing, listAttachmentsReasons)

		for _, tool := range []string{"get_message", "list_attachments"} {
			raw, err := json.Marshal(callTool(t, session, tool, map[string]any{"messageId": id}))
			if err != nil {
				t.Fatalf("marshal SDK result: %v", err)
			}
			for _, sentinel := range []string{zeroContent, absentContent} {
				if bytes.Contains(raw, []byte(sentinel)) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString([]byte(sentinel))[:16])) {
					t.Fatalf("%s leaked attachment bytes", tool)
				}
			}
		}

		sawBodyPeek := false
		for _, command := range server.Commands()[before:] {
			upper := strings.ToUpper(command.Raw)
			if !command.TLS {
				t.Fatalf("read command used plaintext transport: %q", command.Raw)
			}
			if strings.Contains(upper, "FETCH") && strings.Contains(upper, "BODY.PEEK[]") {
				sawBodyPeek = true
			}
			if strings.Contains(upper, "FETCH") && strings.Contains(upper, "BODY[") {
				t.Fatalf("read used a non-PEEK body fetch: %q", command.Raw)
			}
		}
		if !sawBodyPeek {
			t.Fatal("selected reads did not use the existing BODY.PEEK[] retrieval")
		}
		requireReadOnlyTranscript(t, server)
		if err := server.AssertNoInsecureAuthentication(); err != nil {
			t.Fatal(err)
		}

		for _, sentinel := range []string{zeroContent, absentContent, "zero.bin", "absent.bin", "croton.test"} {
			if strings.Contains(audit.String(), sentinel) {
				t.Fatalf("audit recorded message content %q", sentinel)
			}
		}
		for _, event := range auditLines(t, &audit) {
			requireAllowlistedKeys(t, event)
			if event["outcome"] != "ok" {
				t.Fatalf("audit event = %v, want ok", event)
			}
		}

		// Oversized raw MIME or header blocks keep a code-only refusal rather
		// than an empty complete result.
		oversizedRaw := completenessHeader("Synthetic oversized raw") + "Content-Type: text/plain\r\n\r\n" + strings.Repeat("r", 300<<10) + "\r\n"
		oversizedHeader := completenessHeader("Synthetic oversized header") + "X-Synthetic-Filler: " + strings.Repeat("h", 20<<10) + "\r\nContent-Type: text/plain\r\n\r\nBody.\r\n"
		for _, body := range []string{oversizedRaw, oversizedHeader} {
			oversizedSession := completenessSession(t, body)
			for _, tool := range []string{"get_message", "list_attachments"} {
				result := callTool(t, oversizedSession, tool, map[string]any{"messageId": "opaque-completeness"})
				if !result.IsError {
					t.Fatalf("%s returned a normal result for oversized input: %.200s", tool, resultText(t, result))
				}
				requireRefusedWithoutTuple(t, result, errInternal)
			}
		}
	})

	t.Run("contract_guards", func(t *testing.T) {
		t.Parallel()

		complete := completenessHeader("Synthetic complete") + "Content-Type: text/plain\r\n\r\nComplete.\r\n"
		message, listing := selectedReads(t, completenessSession(t, complete), "opaque-completeness", completenessTuple)
		for label, object := range map[string]map[string]any{"get_message": message, "list_attachments": listing} {
			truncation, ok := object["truncation"].(map[string]any)
			if !ok || len(truncation) != 0 {
				t.Fatalf("%s complete truncation = %v, want an empty object", label, object["truncation"])
			}
			requireTruncated(t, label, object, false)
		}
		if attachments := rawItems(t, listing, "attachments"); len(attachments) != 0 {
			t.Fatalf("complete listing = %v, want an empty list", attachments)
		}

		// A list fallback serializes only the output reason from the closed set.
		oversized := &listAttachmentsResult{ID: "opaque", Mailbox: "INBOX", UIDValidity: 1, UID: 1, Attachments: []attachmentResult{{Filename: strings.Repeat("y", maxToolResultBytes)}}}
		encoded, _, err := encodeBounded(oversized)
		if err != nil {
			t.Fatalf("encodeBounded: %v", err)
		}
		requireReasons(t, "list_attachments fallback", decodeBounded(t, encoded), listAttachmentsReasons, "output")

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		documentation := strings.Join(strings.Fields(string(contents)), " ")
		for _, statement := range []string{
			"When it is `true`, `declaredSize` is present and is the nonnegative `size` parameter the sender declared in `Content-Disposition`, including a declared `0`.",
			"a missing or invalid declaration is never reported as zero.",
			"not a count of decoded bytes or of the transferred part.",
			"It is always present and holds only `true` keys, so a complete result has an empty `truncation` object and no `truncated` marker.",
			"`get_message` reasons are `headers`, `headerCount`, `text`, `mimeParts`, `mimeDepth`, `attachments`, `references` and `output`.",
			"`list_attachments` reasons are `headers`, `headerCount`, `mimeParts`, `mimeDepth`, `attachments` and `output`;",
			"An empty list with any reason is incomplete, not proof that the message has no attachments.",
			"Listing attachments still fetches and parses the bounded raw MIME message with `BODY.PEEK[]`; it is not a metadata-only transfer and never returns attachment bytes.",
			"A message whose raw MIME or header block exceeds the parser's bounds may be refused with an error code instead of a result.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing completeness statement: %q", statement)
			}
		}
	})
}

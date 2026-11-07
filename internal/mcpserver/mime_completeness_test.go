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
		"mimeParts": true, "mimeDepth": true, "attachments": true, "output": true,
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

// completenessSession serves get_message from the raw body and
// list_attachments from an independently arranged structure result with the
// same identity, so neither read derives the other's expectations.
// textDescriptor, octetDescriptor and mixedDescriptor build ordinary typed
// BODYSTRUCTURE descriptors; declaredSize is the Content-Disposition size
// parameter text, omitted when empty.
func textDescriptor() *testkit.BodyPart {
	return &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 32, Lines: 1}
}

func octetDescriptor(filename, declaredSize string) *testkit.BodyPart {
	disposition := &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: filename}}}
	if declaredSize != "" {
		disposition.Params = append(disposition.Params, testkit.BodyParam{Name: "size", Value: declaredSize})
	}

	return &testkit.BodyPart{
		Kind: testkit.BodyPartBasic, Type: "application", Subtype: "octet-stream",
		Params: []testkit.BodyParam{{Name: "name", Value: filename}}, Encoding: "base64", Size: 64, Disposition: disposition,
	}
}

func mixedDescriptor(children ...*testkit.BodyPart) *testkit.BodyPart {
	return &testkit.BodyPart{Kind: testkit.BodyPartMultipart, Subtype: "mixed", Params: []testkit.BodyParam{{Name: "boundary", Value: "root-boundary"}}, Children: children}
}

func completenessSession(t *testing.T, body string, truncation bridge.Truncation, attachments ...bridge.AttachmentMetadata) *mcp.ClientSession {
	t.Helper()

	metadata := bridge.MessageMetadata{ID: "opaque-completeness", Mailbox: completenessTuple.Mailbox, Subject: "Synthetic completeness"}
	structure := selectedStructure("opaque-completeness", completenessTuple, attachments...)
	structure.Truncation = truncation
	mail := &structureMail{
		fakeMail:  &fakeMail{metadata: withReadIdentity(metadata, completenessTuple.UIDValidity, completenessTuple.UID), body: []byte(body)},
		structure: structure,
	}
	return connectTestClient(t, Options{Mail: mail})
}

// declaredAttachment is arranged octet-stream attachment metadata with an
// optional declared size.
func declaredAttachment(filename string, size int64, known bool) bridge.AttachmentMetadata {
	attachment := octetAttachment(filename)
	attachment.HasDeclaredSize, attachment.DeclaredSize = known, size
	return attachment
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

		session := completenessSession(t, body, bridge.Truncation{},
			declaredAttachment("positive.bin", 1234, true),
			declaredAttachment("zero.bin", 0, true),
			declaredAttachment("absent.bin", 0, false),
			declaredAttachment("negative.bin", -7, true),
		)
		message, listing := selectedReads(t, session, "opaque-completeness", completenessTuple)
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
		arrangedTwenty := make([]bridge.AttachmentMetadata, 0, 20)
		for index := range 20 {
			twenty = append(twenty, wantAttachment{filename: fmt.Sprintf("file-%02d.bin", index)})
			arrangedTwenty = append(arrangedTwenty, octetAttachment(fmt.Sprintf("file-%02d.bin", index)))
		}

		var references strings.Builder
		references.WriteString("References:")
		for index := range 51 {
			fmt.Fprintf(&references, " <reference-%d@croton.test>", index)
		}

		// Each listing is an independently arranged structure result; raw and
		// structured omissions are not assumed to coincide.
		for _, tc := range []struct {
			name            string
			body            string
			attachments     []wantAttachment
			messageReasons  []string
			listStructure   []bridge.AttachmentMetadata
			listTruncation  bridge.Truncation
			listAttachments []wantAttachment
			listReasons     []string
		}{
			{
				name:           "mime_parts",
				body:           multipartMessage("Synthetic parts", manyParts),
				messageReasons: []string{"mimeParts"},
				listTruncation: bridge.Truncation{MIMEParts: true},
				listReasons:    []string{"mimeParts"},
			},
			{
				name:           "mime_depth",
				body:           completenessHeader("Synthetic depth") + "Content-Type: multipart/mixed; boundary=\"depth-0\"\r\n\r\n" + multipartBody("depth-0", []string{deep}),
				messageReasons: []string{"mimeDepth"},
				listTruncation: bridge.Truncation{MIMEDepth: true},
				listReasons:    []string{"mimeDepth"},
			},
			{
				// The header count hides the attachment from raw parsing,
				// while the structure still lists it completely.
				name:            "header_count",
				body:            headerCount.String(),
				messageReasons:  []string{"headerCount"},
				listStructure:   []bridge.AttachmentMetadata{declaredAttachment("hidden.bin", 9, true)},
				listAttachments: []wantAttachment{{filename: "hidden.bin", size: 9, known: true}},
			},
			{
				name:            "attachments",
				body:            multipartMessage("Synthetic many", many),
				attachments:     twenty,
				messageReasons:  []string{"attachments"},
				listStructure:   arrangedTwenty,
				listTruncation:  bridge.Truncation{Attachments: true},
				listAttachments: twenty,
				listReasons:     []string{"attachments"},
			},
			{
				// A reference-only limit marks the message but not its
				// attachment list.
				name:           "references",
				body:           completenessHeader("Synthetic references") + references.String() + "\r\nContent-Type: text/plain\r\n\r\nReferences only.\r\n",
				messageReasons: []string{"references"},
				listTruncation: bridge.Truncation{References: true},
			},
		} {
			session := completenessSession(t, tc.body, tc.listTruncation, tc.listStructure...)
			message, listing := selectedReads(t, session, "opaque-completeness", completenessTuple)
			if _, present := message["attachments"]; len(tc.attachments) > 0 || present {
				requireAttachmentEntries(t, tc.name+" get_message", message, tc.attachments)
			}
			requireAttachmentEntries(t, tc.name+" list_attachments", listing, tc.listAttachments)
			requireReasons(t, tc.name+" get_message", message, getMessageReasons, tc.messageReasons...)
			requireReasons(t, tc.name+" list_attachments", listing, listAttachmentsReasons, tc.listReasons...)
		}
	})

	t.Run("output_propagation", func(t *testing.T) {
		t.Parallel()

		// Latin-1 text doubles when decoded, so the parser's text limit and the
		// output cap both apply to one public get_message call.
		latin := completenessHeader("Synthetic output pressure") + "Content-Type: text/plain; charset=iso-8859-1\r\nContent-Transfer-Encoding: 8bit\r\n\r\n" + strings.Repeat("\xe9", 200<<10) + "\r\n"
		result := callTool(t, completenessSession(t, latin, bridge.Truncation{}), "get_message", map[string]any{"messageId": "opaque-completeness"})
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
			Attachments: listPressure, Truncation: attachmentListTruncation{MIMEDepth: true, Attachments: true}, Truncated: true,
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
			{"list_attachments_shrink", listing, listAttachmentsReasons, []string{"mimeDepth", "attachments", "output"}},
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
		// Twenty-one attachments exceed the 20-entry bound, so the second
		// message is a bounded omission served over the same verified TLS.
		omittedContent := highEntropySentinel(t, "OMITTED")
		omitted := []string{}
		for index := range 21 {
			omitted = append(omitted, attachmentPart(fmt.Sprintf("file-%02d.bin", index), "; size=4", omittedContent))
		}
		omission := multipartMessage("Synthetic completeness omission", omitted)

		// The typed descriptors are written independently of the raw bytes.
		omittedDescriptors := []*testkit.BodyPart{}
		for index := range 21 {
			omittedDescriptors = append(omittedDescriptors, octetDescriptor(fmt.Sprintf("file-%02d.bin", index), "4"))
		}
		server, adapter := startIntegrationAdapterWithOptions(t, testkit.Options{
			Messages: []string{sizes, omission},
			BodyStructures: []*testkit.BodyPart{
				mixedDescriptor(textDescriptor(), octetDescriptor("zero.bin", "0"), octetDescriptor("absent.bin", "")),
				mixedDescriptor(omittedDescriptors...),
			},
		})
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		_, results := searchItems(t, session, map[string]any{"mailbox": "INBOX", "subject": "Synthetic completeness"})
		if len(results) != 2 {
			t.Fatalf("search returned %d fixture messages, want 2", len(results))
		}
		ids := map[string]string{}
		tuples := map[string]readTuple{}
		for _, item := range results {
			subject, _ := item["subject"].(string)
			ids[subject], _ = item["id"].(string)
			tuples[subject] = flatTuple(t, item)
		}

		twenty := make([]wantAttachment, 0, 20)
		for index := range 20 {
			twenty = append(twenty, wantAttachment{filename: fmt.Sprintf("file-%02d.bin", index), size: 4, known: true})
		}
		before := len(server.Commands())
		for _, tc := range []struct {
			subject string
			want    []wantAttachment
			reasons []string
		}{
			{"Synthetic completeness sizes", []wantAttachment{{filename: "zero.bin", size: 0, known: true}, {filename: "absent.bin"}}, nil},
			{"Synthetic completeness omission", twenty, []string{"attachments"}},
		} {
			id := ids[tc.subject]
			if id == "" || tuples[tc.subject].Mailbox != "INBOX" {
				t.Fatalf("search did not return %q", tc.subject)
			}
			message, listing := selectedReads(t, session, id, tuples[tc.subject])
			requireAttachmentEntries(t, tc.subject+" get_message", message, tc.want)
			requireAttachmentEntries(t, tc.subject+" list_attachments", listing, tc.want)
			requireReasons(t, tc.subject+" get_message", message, getMessageReasons, tc.reasons...)
			requireReasons(t, tc.subject+" list_attachments", listing, listAttachmentsReasons, tc.reasons...)

			for _, tool := range []string{"get_message", "list_attachments"} {
				raw, err := json.Marshal(callTool(t, session, tool, map[string]any{"messageId": id}))
				if err != nil {
					t.Fatalf("marshal SDK result: %v", err)
				}
				for _, sentinel := range []string{zeroContent, absentContent, omittedContent} {
					if bytes.Contains(raw, []byte(sentinel)) || bytes.Contains(raw, []byte(base64.StdEncoding.EncodeToString([]byte(sentinel))[:16])) {
						t.Fatalf("%s leaked attachment bytes", tool)
					}
				}
			}
		}

		// Each listing alone sends a BODYSTRUCTURE fetch and no body or
		// header fetch; get_message keeps its BODY.PEEK[] retrieval.
		for subject, id := range ids {
			listingBefore := len(server.Commands())
			rawObject(t, callTool(t, session, "list_attachments", map[string]any{"messageId": id}))
			sawStructure := false
			for _, command := range server.Commands()[listingBefore:] {
				upper := strings.ToUpper(command.Raw)
				if !command.TLS {
					t.Fatalf("listing command used plaintext transport: %q", command.Raw)
				}
				if strings.Contains(upper, "FETCH") && strings.Contains(upper, "BODYSTRUCTURE") {
					sawStructure = true
				}
				if strings.Contains(upper, "FETCH") && (strings.Contains(upper, "BODY.PEEK[") || strings.Contains(upper, "BODY[") || strings.Contains(upper, "RFC822")) {
					t.Fatalf("%s listing fetched message bytes: %q", subject, command.Raw)
				}
			}
			if !sawStructure {
				t.Fatalf("%s listing did not request BODYSTRUCTURE", subject)
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
			t.Fatal("get_message did not use the existing BODY.PEEK[] retrieval")
		}
		requireReadOnlyTranscript(t, server)
		if err := server.AssertNoInsecureAuthentication(); err != nil {
			t.Fatal(err)
		}

		for _, sentinel := range []string{zeroContent, absentContent, omittedContent, "zero.bin", "absent.bin", "croton.test"} {
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

		// Oversized raw MIME or header blocks keep get_message's code-only
		// refusal rather than an empty complete result; the structure-only
		// listing of the same message is not refused for its raw size.
		oversizedRaw := completenessHeader("Synthetic oversized raw") + "Content-Type: text/plain\r\n\r\n" + strings.Repeat("r", 300<<10) + "\r\n"
		oversizedHeader := completenessHeader("Synthetic oversized header") + "X-Synthetic-Filler: " + strings.Repeat("h", 20<<10) + "\r\nContent-Type: text/plain\r\n\r\nBody.\r\n"
		for _, body := range []string{oversizedRaw, oversizedHeader} {
			oversizedSession := completenessSession(t, body, bridge.Truncation{}, declaredAttachment("listed.bin", 3, true))
			result := callTool(t, oversizedSession, "get_message", map[string]any{"messageId": "opaque-completeness"})
			if !result.IsError {
				t.Fatalf("get_message returned a normal result for oversized input: %.200s", resultText(t, result))
			}
			requireRefusedWithoutTuple(t, result, errInternal)

			listing := rawObject(t, callTool(t, oversizedSession, "list_attachments", map[string]any{"messageId": "opaque-completeness"}))
			requireClosedKeys(t, "oversized listing", listing, listAttachmentsKeys)
			if listing["id"] != "opaque-completeness" || flatTuple(t, listing) != completenessTuple {
				t.Fatalf("oversized listing identity = %v/%+v", listing["id"], flatTuple(t, listing))
			}
			requireAttachmentEntries(t, "oversized listing", listing, []wantAttachment{{filename: "listed.bin", size: 3, known: true}})
			requireReasons(t, "oversized listing", listing, listAttachmentsReasons)
		}
	})

	t.Run("contract_guards", func(t *testing.T) {
		t.Parallel()

		complete := completenessHeader("Synthetic complete") + "Content-Type: text/plain\r\n\r\nComplete.\r\n"
		message, listing := selectedReads(t, completenessSession(t, complete, bridge.Truncation{}), "opaque-completeness", completenessTuple)
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
			"`list_attachments` reasons are `mimeParts`, `mimeDepth`, `attachments` and `output`.",
			"An empty list with any reason is incomplete, not proof that the message has no attachments.",
			"Listing attachments requests UID BODYSTRUCTURE only; it does not fetch or parse the message body.",
			"`get_message` still fetches and parses the bounded raw MIME message with `BODY.PEEK[]`",
			"A message whose raw MIME or header block exceeds the parser's bounds may be refused by `get_message` with an error code instead of a result.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing completeness statement: %q", statement)
			}
		}
	})
}

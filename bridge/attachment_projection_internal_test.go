package bridge

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"
)

// projectionLeaf is a decoded single part with params and an optional
// disposition.
func projectionLeaf(kind, subtype string, size uint32, params map[string]string, disposition *imap.BodyStructureDisposition) *imap.BodyStructureSinglePart {
	return &imap.BodyStructureSinglePart{Type: kind, Subtype: subtype, Params: params, Encoding: "base64", Size: size, Extended: &imap.BodyStructureSinglePartExt{Disposition: disposition}}
}

// projectionMessage is a decoded embedded message of subtype around inner.
func projectionMessage(subtype string, size uint32, disposition *imap.BodyStructureDisposition, inner imap.BodyStructure) *imap.BodyStructureSinglePart {
	return &imap.BodyStructureSinglePart{
		Type: "message", Subtype: subtype, Encoding: "7bit", Size: size,
		MessageRFC822: &imap.BodyStructureMessageRFC822{Envelope: &imap.Envelope{Subject: "embedded"}, BodyStructure: inner, NumLines: 3},
		Extended:      &imap.BodyStructureSinglePartExt{Disposition: disposition},
	}
}

// projectionMultipart is a decoded multipart container with a boundary and
// an optional disposition.
func projectionMultipart(subtype string, disposition *imap.BodyStructureDisposition, children ...imap.BodyStructure) *imap.BodyStructureMultiPart {
	return &imap.BodyStructureMultiPart{Subtype: subtype, Children: children, Extended: &imap.BodyStructureMultiPartExt{Params: map[string]string{"boundary": "b-" + subtype}, Disposition: disposition}}
}

func projectionDisposition(value string, params map[string]string) *imap.BodyStructureDisposition {
	return &imap.BodyStructureDisposition{Value: value, Params: params}
}

// projectionText is an unnamed inline-less text part.
func projectionText() *imap.BodyStructureSinglePart {
	return &imap.BodyStructureSinglePart{Type: "text", Subtype: "plain", Encoding: "7bit", Size: 5, Text: &imap.BodyStructureText{NumLines: 1}}
}

// projectScripted runs the adapter operation over a dedicated read that
// returns structure, under bounds.
func projectScripted(t *testing.T, structure imap.BodyStructure, bounds Bounds) (attachmentStructureResult, error) {
	t.Helper()

	adapter, attempts := newScriptedStructureAdapter(t, func(int) (selectedStructure, error) {
		return selectedStructure{Structure: structure}, nil
	})
	adapter.config.Bounds = bounds

	result, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9001, 7))
	if len(attempts()) != 1 {
		t.Fatalf("projection made %d dedicated reads", len(attempts()))
	}
	requireGateReleased(t, adapter)

	return result, err
}

// projectionCase is one structure and its independently expected outcome.
type projectionCase struct {
	name       string
	structure  imap.BodyStructure
	bounds     Bounds
	want       []attachmentStructureView
	truncation Truncation
	malformed  bool
}

func runProjectionCases(t *testing.T, cases []projectionCase) {
	t.Helper()

	for _, testCase := range cases {
		result, err := projectScripted(t, testCase.structure, testCase.bounds)
		if testCase.malformed {
			var normalizeError *NormalizeError
			if !errors.As(err, &normalizeError) || normalizeError.Code != errorCodeMalformedMIME || err.Error() != errorCodeMalformedMIME {
				t.Fatalf("%s error = %v, want the fixed malformed_mime failure", testCase.name, err)
			}
			if !reflect.DeepEqual(result, attachmentStructureResult{}) {
				t.Fatalf("%s returned a result %+v beside its failure", testCase.name, result)
			}
			continue
		}

		if err != nil {
			t.Fatalf("%s error = %v", testCase.name, err)
		}
		if result.UID != 7 || result.UIDValidity != 9001 || result.Mailbox != "INBOX" || result.Attachments == nil {
			t.Fatalf("%s result = %+v", testCase.name, result)
		}
		if !reflect.DeepEqual(result.Attachments, testCase.want) {
			t.Fatalf("%s attachments = %+v, want %+v", testCase.name, result.Attachments, testCase.want)
		}
		if result.Truncation != testCase.truncation {
			t.Fatalf("%s truncation = %+v, want %+v", testCase.name, result.Truncation, testCase.truncation)
		}
	}
}

// attachmentView is an expected entry with known transfer octets.
func attachmentView(filename, contentType, disposition string, octets uint32) attachmentStructureView {
	return attachmentStructureView{Metadata: AttachmentMetadata{Filename: filename, ContentType: contentType, Disposition: disposition}, HasTransferOctets: true, TransferOctets: octets}
}

func TestAttachmentProjection(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	t.Run("parameters", func(t *testing.T) {
		attachment := func(params map[string]string) *imap.BodyStructureDisposition {
			return projectionDisposition("attachment", params)
		}

		runProjectionCases(t, []projectionCase{
			{
				name:      "plain_unicode_filename",
				structure: projectionLeaf("application", "pdf", 40, nil, attachment(map[string]string{"filename": "résumé.pdf"})),
				want:      []attachmentStructureView{attachmentView("résumé.pdf", "application/pdf", "attachment", 40)},
			},
			{
				name:      "extended_filename",
				structure: projectionLeaf("application", "pdf", 40, nil, attachment(map[string]string{"filename*": "utf-8''caf%C3%A9.pdf"})),
				want:      []attachmentStructureView{attachmentView("café.pdf", "application/pdf", "attachment", 40)},
			},
			{
				name:      "extended_name_without_disposition",
				structure: projectionLeaf("application", "octet-stream", 41, map[string]string{"name*": "UTF-8''r%C3%A9sum%C3%A9.pdf"}, nil),
				want:      []attachmentStructureView{attachmentView("résumé.pdf", "application/octet-stream", "", 41)},
			},
			{
				name:      "unstarred_continuations",
				structure: projectionLeaf("application", "pdf", 42, nil, attachment(map[string]string{"filename*0": "part-one", "filename*1": "-part-two", "filename*2": ".pdf"})),
				want:      []attachmentStructureView{attachmentView("part-one-part-two.pdf", "application/pdf", "attachment", 42)},
			},
			{
				name:      "starred_continuations",
				structure: projectionLeaf("application", "pdf", 43, map[string]string{"name*0*": "utf-8''caf%C3%A9", "name*1*": "%2Epdf"}, nil),
				want:      []attachmentStructureView{attachmentView("café.pdf", "application/pdf", "", 43)},
			},
			{
				name:      "mixed_starred_and_unstarred_continuations",
				structure: projectionLeaf("application", "pdf", 44, nil, attachment(map[string]string{"filename*0*": "utf-8''r%C3%A9sum%C3%A9", "filename*1": ".pdf"})),
				want:      []attachmentStructureView{attachmentView("résumé.pdf", "application/pdf", "attachment", 44)},
			},
			{
				name:      "valid_extended_before_plain",
				structure: projectionLeaf("application", "pdf", 45, nil, attachment(map[string]string{"filename": "plain.pdf", "filename*": "utf-8''caf%C3%A9.pdf"})),
				want:      []attachmentStructureView{attachmentView("café.pdf", "application/pdf", "attachment", 45)},
			},
			{
				name:      "invalid_extended_keeps_plain",
				structure: projectionLeaf("application", "pdf", 46, nil, attachment(map[string]string{"filename": "plain.pdf", "filename*": "koi8-r''x.pdf"})),
				want:      []attachmentStructureView{attachmentView("plain.pdf", "application/pdf", "attachment", 46)},
			},
			{
				name:      "disposition_filename_before_type_name",
				structure: projectionLeaf("application", "pdf", 47, map[string]string{"name": "type-name.pdf"}, projectionDisposition("INLINE", map[string]string{"filename*": "utf-8''caf%C3%A9.pdf"})),
				want:      []attachmentStructureView{attachmentView("café.pdf", "application/pdf", "inline", 47)},
			},
			{
				name:      "extended_type_name_fallback",
				structure: projectionLeaf("application", "pdf", 48, map[string]string{"name*": "utf-8''r%C3%A9sum%C3%A9.pdf"}, attachment(nil)),
				want:      []attachmentStructureView{attachmentView("résumé.pdf", "application/pdf", "attachment", 48)},
			},
			{
				// Quotes, backslashes and separators survive the rebuilt
				// header exactly.
				name:      "quoted_specials",
				structure: projectionLeaf("application", "pdf", 49, nil, attachment(map[string]string{"filename": `a "b"; c\d=e.pdf`})),
				want:      []attachmentStructureView{attachmentView(`a "b"; c\d=e.pdf`, "application/pdf", "attachment", 49)},
			},
			{
				name:      "attachment_disposition_without_filename",
				structure: projectionLeaf("application", "pdf", 50, nil, projectionDisposition("ATTACHMENT", nil)),
				want:      []attachmentStructureView{attachmentView("", "application/pdf", "attachment", 50)},
			},
			{
				name:      "inline_without_filename",
				structure: projectionLeaf("text", "plain", 51, map[string]string{"charset": "utf-8"}, projectionDisposition("inline", nil)),
				want:      []attachmentStructureView{},
			},
			{
				// An undecodable extended name with no plain fallback leaves no
				// filename, so the part is not an attachment.
				name:      "undecodable_extended_name_alone",
				structure: projectionLeaf("application", "pdf", 52, map[string]string{"name*": "no-charset-quotes"}, nil),
				want:      []attachmentStructureView{},
			},
			{name: "key_with_space", structure: projectionLeaf("application", "pdf", 9, map[string]string{"file name": "x.pdf"}, nil), malformed: true},
			{name: "value_with_line_break", structure: projectionLeaf("application", "pdf", 9, nil, attachment(map[string]string{"filename": "a\r\nb.pdf"})), malformed: true},
			{name: "type_with_space", structure: projectionLeaf("app lication", "pdf", 9, nil, nil), malformed: true},
			{name: "subtype_with_parameter", structure: projectionLeaf("application", "pdf;name=x.pdf", 9, nil, nil), malformed: true},
			{name: "disposition_with_space", structure: projectionLeaf("application", "pdf", 9, nil, projectionDisposition("attach ment", map[string]string{"filename": "x.pdf"})), malformed: true},
			{name: "empty_disposition", structure: projectionLeaf("application", "pdf", 9, nil, projectionDisposition("", map[string]string{"filename": "x.pdf"})), malformed: true},
			{
				// A malformed later child fails the whole projection rather
				// than returning the earlier attachment as complete.
				name: "malformed_child_after_attachment",
				structure: projectionMultipart("mixed", nil,
					projectionLeaf("application", "pdf", 9, nil, attachment(map[string]string{"filename": "first.pdf"})),
					projectionLeaf("application", "pdf", 9, map[string]string{"na\"me": "x"}, nil),
				),
				malformed: true,
			},
		})
	})

	t.Run("embedded_layouts", func(t *testing.T) {
		attachment := func(filename string) *imap.BodyStructureDisposition {
			if filename == "" {
				return projectionDisposition("attachment", nil)
			}
			return projectionDisposition("attachment", map[string]string{"filename": filename})
		}
		hidden := projectionMultipart("mixed", nil, projectionText(), projectionLeaf("application", "pdf", 30, nil, attachment("hidden.pdf")))

		runProjectionCases(t, []projectionCase{
			{
				// Server DFS order across nested containers, each qualified
				// outer entity once, and no enumeration inside any of them.
				name: "mixed_layout",
				structure: projectionMultipart("mixed", nil,
					projectionText(),
					projectionMultipart("related", nil,
						&imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Encoding: "7bit", Size: 30, Text: &imap.BodyStructureText{NumLines: 1}},
						projectionLeaf("image", "png", 64, map[string]string{"name": "logo.png"}, nil),
					),
					projectionMultipart("alternative", attachment("bundle.zip"), projectionText(), projectionLeaf("application", "pdf", 20, nil, attachment("inside-bundle.pdf"))),
					projectionMessage("rfc822", 900, attachment("fwd.eml"), hidden),
					projectionMessage("global", 0, attachment(""), hidden),
					projectionMessage("rfc822", 700, nil, hidden),
					projectionLeaf("application", "pdf", 4096, nil, attachment("last.pdf")),
				),
				want: []attachmentStructureView{
					attachmentView("logo.png", "image/png", "", 64),
					{Metadata: AttachmentMetadata{Filename: "bundle.zip", ContentType: "multipart/alternative", Disposition: "attachment"}},
					attachmentView("fwd.eml", "message/rfc822", "attachment", 900),
					attachmentView("", "message/global", "attachment", 0),
					attachmentView("last.pdf", "application/pdf", "attachment", 4096),
				},
				truncation: Truncation{MIMEDepth: true},
			},
			{
				// An unqualified message/global is omitted visibly, as rfc822 is.
				name:       "unqualified_global",
				structure:  projectionMultipart("mixed", nil, projectionText(), projectionMessage("global", 80, nil, hidden)),
				want:       []attachmentStructureView{},
				truncation: Truncation{MIMEDepth: true},
			},
			{
				name:       "unqualified_rfc822_root",
				structure:  projectionMessage("rfc822", 80, nil, hidden),
				want:       []attachmentStructureView{},
				truncation: Truncation{MIMEDepth: true},
			},
			{
				name:      "qualified_global_root_by_type_name",
				structure: &imap.BodyStructureSinglePart{Type: "MESSAGE", Subtype: "GLOBAL", Params: map[string]string{"name*": "utf-8''caf%C3%A9.eml"}, Size: 81, MessageRFC822: &imap.BodyStructureMessageRFC822{BodyStructure: hidden}},
				want:      []attachmentStructureView{attachmentView("café.eml", "message/global", "", 81)},
			},
			{
				// A qualified multipart root is one entry with unknown
				// transfer size; its declared size stays its own.
				name:      "qualified_multipart_root",
				structure: projectionMultipart("mixed", projectionDisposition("attachment", map[string]string{"filename": "all.zip", "size": "123"}), projectionLeaf("application", "pdf", 4000, nil, attachment("a.pdf"))),
				want:      []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "all.zip", ContentType: "multipart/mixed", Disposition: "attachment", DeclaredSize: 123, HasDeclaredSize: true}}},
			},
			{
				name:      "single_part_root",
				structure: projectionLeaf("application", "pdf", 0, nil, projectionDisposition("attachment", map[string]string{"filename": "root.pdf", "size": "0"})),
				want:      []attachmentStructureView{{Metadata: AttachmentMetadata{Filename: "root.pdf", ContentType: "application/pdf", Disposition: "attachment", HasDeclaredSize: true}, HasTransferOctets: true}},
			},
		})
	})

	t.Run("limits_and_sizes", func(t *testing.T) {
		named := func(filename string) *imap.BodyStructureSinglePart {
			return projectionLeaf("application", "pdf", 10, nil, projectionDisposition("attachment", map[string]string{"filename": filename}))
		}
		nested := func(levels int, leaf imap.BodyStructure) imap.BodyStructure {
			structure := leaf
			for range levels {
				structure = projectionMultipart("mixed", nil, structure)
			}
			return structure
		}
		hiddenWide := projectionMultipart("mixed", nil, named("h1"), named("h2"), named("h3"), named("h4"), named("h5"))
		longName := strings.Repeat("é", 2500)
		longEncoded := "utf-8''" + strings.Repeat("%C3%A9", 2500)
		exactName := strings.Repeat("n", defaultMaxFilenameBytes)
		exactType := strings.Repeat("s", defaultMaxContentTypeBytes-len("application/"))
		exactDisposition := strings.Repeat("d", defaultMaxDispositionBytes)

		runProjectionCases(t, []projectionCase{
			{
				name:      "parts_exact_boundary",
				structure: projectionMultipart("mixed", nil, named("a"), named("b")),
				bounds:    Bounds{MaxMimeParts: 3},
				want:      []attachmentStructureView{attachmentView("a", "application/pdf", "attachment", 10), attachmentView("b", "application/pdf", "attachment", 10)},
			},
			{
				name:       "parts_omitted",
				structure:  projectionMultipart("mixed", nil, named("a"), named("b"), named("c")),
				bounds:     Bounds{MaxMimeParts: 3},
				want:       []attachmentStructureView{attachmentView("a", "application/pdf", "attachment", 10), attachmentView("b", "application/pdf", "attachment", 10)},
				truncation: Truncation{MIMEParts: true},
			},
			{
				// Containers count as visited parts.
				name:       "parts_containers_counted",
				structure:  projectionMultipart("mixed", nil, projectionMultipart("mixed", nil, named("a")), named("b")),
				bounds:     Bounds{MaxMimeParts: 3},
				want:       []attachmentStructureView{attachmentView("a", "application/pdf", "attachment", 10)},
				truncation: Truncation{MIMEParts: true},
			},
			{
				name:       "empty_by_part_omission",
				structure:  projectionMultipart("mixed", nil, named("a")),
				bounds:     Bounds{MaxMimeParts: 1},
				want:       []attachmentStructureView{},
				truncation: Truncation{MIMEParts: true},
			},
			{
				// Children hidden inside one attached message are not counted.
				name:      "hidden_children_not_counted",
				structure: projectionMultipart("mixed", nil, projectionMessage("rfc822", 300, projectionDisposition("attachment", nil), hiddenWide)),
				bounds:    Bounds{MaxMimeParts: 2},
				want:      []attachmentStructureView{attachmentView("", "message/rfc822", "attachment", 300)},
			},
			{
				name:      "depth_exact_boundary",
				structure: nested(defaultMaxMIMEDepth, named("deep")),
				want:      []attachmentStructureView{attachmentView("deep", "application/pdf", "attachment", 10)},
			},
			{
				name:       "empty_by_depth_omission",
				structure:  nested(defaultMaxMIMEDepth+1, named("deeper")),
				want:       []attachmentStructureView{},
				truncation: Truncation{MIMEDepth: true},
			},
			{
				name:      "attachments_exact_boundary",
				structure: projectionMultipart("mixed", nil, named("a"), named("b")),
				bounds:    Bounds{MaxAttachmentCount: 2},
				want:      []attachmentStructureView{attachmentView("a", "application/pdf", "attachment", 10), attachmentView("b", "application/pdf", "attachment", 10)},
			},
			{
				name:       "attachments_omitted",
				structure:  projectionMultipart("mixed", nil, named("a"), named("b"), named("c")),
				bounds:     Bounds{MaxAttachmentCount: 2},
				want:       []attachmentStructureView{attachmentView("a", "application/pdf", "attachment", 10), attachmentView("b", "application/pdf", "attachment", 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				name:      "complete_empty",
				structure: projectionMultipart("alternative", nil, projectionText(), &imap.BodyStructureSinglePart{Type: "text", Subtype: "html", Size: 9}),
				want:      []attachmentStructureView{},
			},
			{
				name:      "fields_exact_boundary",
				structure: projectionLeaf("application", exactType, 10, nil, projectionDisposition(exactDisposition, map[string]string{"filename": exactName})),
				want:      []attachmentStructureView{attachmentView(exactName, "application/"+exactType, exactDisposition, 10)},
			},
			{
				// A decoded RFC 2231 name over 4096 bytes is cut on a rune
				// boundary within 255 bytes, not refused.
				name:       "long_extended_filename",
				structure:  projectionLeaf("application", "pdf", 10, nil, projectionDisposition("attachment", map[string]string{"filename*": longEncoded})),
				want:       []attachmentStructureView{attachmentView(longName[:254], "application/pdf", "attachment", 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				name:       "long_plain_name",
				structure:  projectionLeaf("application", "pdf", 10, map[string]string{"name": strings.Repeat("x", 5000)}, nil),
				want:       []attachmentStructureView{attachmentView(strings.Repeat("x", defaultMaxFilenameBytes), "application/pdf", "", 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				name:      "invalid_utf8_name_repaired",
				structure: projectionLeaf("application", "pdf", 10, map[string]string{"name": "\xffa.pdf"}, nil),
				want:      []attachmentStructureView{attachmentView("�a.pdf", "application/pdf", "", 10)},
			},
			{
				// truncateUTF8: a raw name within the limit is repaired
				// without the flag, even when repair grows it past the limit.
				name:      "invalid_utf8_name_at_limit_repaired",
				structure: projectionLeaf("application", "pdf", 10, map[string]string{"name": strings.Repeat("\xffn", 5) + strings.Repeat("n", defaultMaxFilenameBytes-10)}, nil),
				want:      []attachmentStructureView{attachmentView(strings.Repeat("�n", 5)+strings.Repeat("n", defaultMaxFilenameBytes-10), "application/pdf", "", 10)},
			},
			{
				// A longer raw name is cut to the limit first, then trimmed
				// back until valid UTF-8, and sets the flag.
				name:       "long_name_cut_then_trimmed",
				structure:  projectionLeaf("application", "pdf", 10, map[string]string{"name": strings.Repeat("a", 250) + "\xff" + strings.Repeat("b", 5000)}, nil),
				want:       []attachmentStructureView{attachmentView(strings.Repeat("a", 250), "application/pdf", "", 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				// Trimming back from the cut removes everything after an early
				// invalid byte; the part stays listed with the flag.
				name:       "long_name_early_invalid_trimmed_to_empty",
				structure:  projectionLeaf("application", "pdf", 10, map[string]string{"name": "\xff" + strings.Repeat("c", 5000)}, nil),
				want:       []attachmentStructureView{attachmentView("", "application/pdf", "", 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				name:       "long_content_type_and_disposition",
				structure:  projectionLeaf("application", exactType+"s", 10, nil, projectionDisposition(exactDisposition+"d", map[string]string{"filename": "x.pdf"})),
				want:       []attachmentStructureView{attachmentView("x.pdf", "application/"+exactType, exactDisposition, 10)},
				truncation: Truncation{Attachments: true},
			},
			{
				// Overlong fields of a part that is not an attachment are never
				// listed and set no flag.
				name: "long_fields_of_nonattachment",
				structure: projectionMultipart("mixed", nil,
					projectionLeaf("text", strings.Repeat("s", 400), 10, nil, nil),
					projectionLeaf("text", "plain", 10, nil, projectionDisposition(strings.Repeat("d", 400), nil)),
				),
				want: []attachmentStructureView{},
			},
			{
				// Single-part transfer octets stay separate from declared
				// sizes; a multipart has neither a sum nor a stand-in.
				name: "sizes",
				structure: projectionMultipart("mixed", nil,
					projectionLeaf("application", "pdf", 0, nil, projectionDisposition("attachment", map[string]string{"filename": "zero.pdf", "size": "0"})),
					projectionLeaf("application", "pdf", 4294967295, nil, projectionDisposition("attachment", map[string]string{"filename": "max.pdf", "size": "9223372036854775807"})),
					projectionLeaf("application", "pdf", 77, nil, projectionDisposition("attachment", map[string]string{"filename": "bad.pdf", "size": "-1"})),
					projectionMultipart("mixed", projectionDisposition("attachment", map[string]string{"filename": "box.zip"}), named("in-box")),
				),
				want: []attachmentStructureView{
					{Metadata: AttachmentMetadata{Filename: "zero.pdf", ContentType: "application/pdf", Disposition: "attachment", HasDeclaredSize: true}, HasTransferOctets: true},
					{Metadata: AttachmentMetadata{Filename: "max.pdf", ContentType: "application/pdf", Disposition: "attachment", DeclaredSize: 9223372036854775807, HasDeclaredSize: true}, HasTransferOctets: true, TransferOctets: 4294967295},
					attachmentView("bad.pdf", "application/pdf", "attachment", 77),
					{Metadata: AttachmentMetadata{Filename: "box.zip", ContentType: "multipart/mixed", Disposition: "attachment"}},
				},
			},
		})

		// A valid tree deeper than the unchanged wire guard allows is refused
		// as bounds before any projection, through the real dedicated read.
		for _, mode := range []struct {
			startTLS bool
			mode     string
		}{{startTLS: true, mode: TLSModeStartTLS}, {startTLS: false, mode: TLSModeImplicit}} {
			peer := startStructureTLSPeer(t, mode.startTLS, structurePeerScript{structure: nestedStructure(quarantineDepth + 6)})
			adapter := structurePeerAdapter(t, peer, mode.mode)
			result, err := callAttachmentStructure(t, context.Background(), adapter, headerTestID(t, adapter, 9001, structurePeerUID))
			requireStructureRefusal(t, "guard-refused tree", result, err, CodeBoundsExceeded)
			peer.wait(t)
		}
	})
}

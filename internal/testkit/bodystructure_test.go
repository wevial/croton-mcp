package testkit

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"net"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"
)

const (
	structureMixedBody = "From: Structure Fixture <structure@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Typed structure\r\nContent-Type: multipart/mixed; boundary=mixed-boundary\r\n\r\nSTRUCTURE_BODY_SENTINEL_MIXED\r\n"
	structurePlainBody = "From: Structure Fixture <structure@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Plain structure\r\n\r\nSTRUCTURE_BODY_SENTINEL_PLAIN\r\n"
	structureBasicBody = "From: Structure Fixture <structure@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Basic structure\r\n\r\nSTRUCTURE_BODY_SENTINEL_BASIC\r\n"
)

// Expected wire forms are written out by hand, independently of the renderer.
const (
	wantPlainWire = `("text" "plain" ("charset" "us-ascii") NIL NIL "7bit" 42 2 NIL NIL NIL NIL)`
	wantBasicWire = `("application" "octet-stream" ("name" "basic.bin") NIL NIL "binary" 64 NIL ("inline" NIL) NIL NIL)`
	wantMixedWire = `(` +
		`(("text" "plain" ("charset" "utf-8" "format" "flowed") NIL NIL "quoted-printable" 120 4 NIL NIL NIL NIL)` +
		`("text" "html" ("charset" "utf-8") NIL NIL "8bit" 340 9 NIL NIL NIL NIL)` +
		` "alternative" ("boundary" "alt-boundary") NIL NIL NIL)` +
		`("application" "octet-stream" ("name" "absent.bin") NIL NIL "base64" 4096 NIL ("attachment" ("filename" "absent.bin")) NIL NIL)` +
		`("application" "octet-stream" ("name" "zero.txt") NIL NIL "quoted-printable" 777 NIL ("attachment" ("filename" "zero.txt" "size" "0")) NIL NIL)` +
		`("application" "octet-stream" ("name" "positive.bin") NIL NIL "base64" 1368 NIL ("attachment" ("filename" "positive.bin" "size" "1000")) NIL NIL)` +
		` "mixed" ("boundary" "mixed-boundary") NIL NIL NIL)`
)

func plainStructure() *BodyPart {
	return &BodyPart{Kind: BodyPartText, Type: "text", Subtype: "plain", Params: []BodyParam{{"charset", "us-ascii"}}, Size: 42, Lines: 2}
}

func basicStructure() *BodyPart {
	return &BodyPart{
		Kind: BodyPartBasic, Type: "application", Subtype: "octet-stream", Params: []BodyParam{{"name", "basic.bin"}},
		Encoding: "binary", Size: 64, Disposition: &BodyDisposition{Value: "inline"},
	}
}

func mixedStructure() *BodyPart {
	attachment := func(name, encoding string, size uint32, dispositionSize ...string) *BodyPart {
		params := []BodyParam{{"filename", name}}
		for _, value := range dispositionSize {
			params = append(params, BodyParam{"size", value})
		}

		return &BodyPart{
			Kind: BodyPartBasic, Type: "application", Subtype: "octet-stream", Params: []BodyParam{{"name", name}},
			Encoding: encoding, Size: size, Disposition: &BodyDisposition{Value: "attachment", Params: params},
		}
	}

	return &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed", Params: []BodyParam{{"boundary", "mixed-boundary"}}, Children: []*BodyPart{
		{Kind: BodyPartMultipart, Type: "multipart", Subtype: "alternative", Params: []BodyParam{{"boundary", "alt-boundary"}}, Children: []*BodyPart{
			{Kind: BodyPartText, Type: "text", Subtype: "plain", Params: []BodyParam{{"charset", "utf-8"}, {"format", "flowed"}}, Encoding: "quoted-printable", Size: 120, Lines: 4},
			{Kind: BodyPartText, Type: "text", Subtype: "html", Params: []BodyParam{{"charset", "utf-8"}}, Encoding: "8bit", Size: 340, Lines: 9},
		}},
		attachment("absent.bin", "base64", 4096),
		attachment("zero.txt", "quoted-printable", 777, "0"),
		attachment("positive.bin", "base64", 1368, "1000"),
	}}
}

func TestBodyStructureFixture(t *testing.T) {
	t.Parallel()

	t.Run("typed_tls", func(t *testing.T) {
		t.Parallel()

		for _, mode := range []TLSMode{StartTLS, ImplicitTLS} {
			server := startStructureFixture(t, Options{
				Mode:           mode,
				Messages:       []string{structureMixedBody, structurePlainBody},
				BodyStructures: []*BodyPart{mixedStructure(), plainStructure()},
			})
			client, wire := structureSession(t, server, mode)
			options := &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}

			before := unseenUIDs(t, client)
			messages, command, served := fetchStructureWire(t, client, wire, imap.UIDSetNum(101), options)
			if command != "UID FETCH 101 (UID BODYSTRUCTURE)" {
				t.Fatalf("%s: requested %q", mode, command)
			}

			if served != "* 1 FETCH (UID 101 BODYSTRUCTURE "+wantMixedWire+")\r\n" {
				t.Fatalf("%s: served %q", mode, served)
			}

			if len(messages) != 1 || messages[0].UID != 101 || len(messages[0].BodySection) != 0 || len(messages[0].BinarySection) != 0 || messages[0].Flags != nil {
				t.Fatalf("%s: parsed messages = %+v, want only UID 101 structure", mode, messages)
			}
			assertMixedStructure(t, messages[0].BodyStructure)

			messages, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(102), options)
			if served != "* 2 FETCH (UID 102 BODYSTRUCTURE "+wantPlainWire+")\r\n" {
				t.Fatalf("%s: served %q", mode, served)
			}

			plain, ok := messages[0].BodyStructure.(*imap.BodyStructureSinglePart)
			if len(messages) != 1 || messages[0].UID != 102 || !ok || plain.MediaType() != "text/plain" || plain.Encoding != "7bit" || plain.Size != 42 || plain.Text == nil || plain.Text.NumLines != 2 {
				t.Fatalf("%s: parsed UID 102 structure = %+v", mode, messages[0].BodyStructure)
			}

			if after := unseenUIDs(t, client); !slices.Equal(before, []imap.UID{101, 102}) || !slices.Equal(after, before) {
				t.Fatalf("%s: UNSEEN before %v, after %v", mode, before, after)
			}

			assertReadOnlyTLSTranscript(t, server)
		}
	})

	t.Run("selective_fetch", func(t *testing.T) {
		t.Parallel()

		bodies := []string{structurePlainBody, structureMixedBody, structureBasicBody}
		server := startStructureFixture(t, Options{
			Mode:           ImplicitTLS,
			Messages:       bodies,
			Seen:           []bool{true, false},
			BodyStructures: []*BodyPart{plainStructure(), mixedStructure(), basicStructure()},
		})
		client, wire := structureSession(t, server, ImplicitTLS)
		structure := &imap.FetchItemBodyStructure{Extended: true}
		header := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
		before := unseenUIDs(t, client)

		// Structure only, on a noncontiguous selection.
		messages, _, served := fetchStructureWire(t, client, wire, imap.UIDSetNum(101, 103), &imap.FetchOptions{BodyStructure: structure})
		if served != "* 1 FETCH (UID 101 BODYSTRUCTURE "+wantPlainWire+")\r\n* 3 FETCH (UID 103 BODYSTRUCTURE "+wantBasicWire+")\r\n" {
			t.Fatalf("structure-only served %q", served)
		}

		if len(messages) != 2 || messages[0].UID != 101 || messages[1].UID != 103 || messages[0].BodyStructure.MediaType() != "text/plain" || messages[1].BodyStructure.MediaType() != "application/octet-stream" {
			t.Fatalf("structure-only parsed %+v", messages)
		}

		// UID, FLAGS and size plus structure. The client orders items itself.
		messages, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(101, 102, 103), &imap.FetchOptions{Flags: true, RFC822Size: true, BodyStructure: structure})
		lines := strings.Split(strings.TrimSuffix(served, "\r\n"), "\r\n")
		wantStructures := []string{wantPlainWire, wantMixedWire, wantBasicWire}
		wantFlags := []string{`FLAGS (\Seen)`, `FLAGS ()`, `FLAGS ()`}
		if len(lines) != 3 || len(messages) != 3 {
			t.Fatalf("flags fetch served %q", served)
		}
		for index, line := range lines {
			uid := imap.UID(101 + index)
			prefix := fmt.Sprintf("* %d FETCH (UID %d ", index+1, uid)
			if !strings.HasPrefix(line, prefix) || strings.Count(line, "BODYSTRUCTURE ") != 1 || !strings.Contains(line, "BODYSTRUCTURE "+wantStructures[index]) ||
				strings.Count(line, "FLAGS (") != 1 || !strings.Contains(line, wantFlags[index]) || !strings.Contains(line, fmt.Sprintf("RFC822.SIZE %d", len(bodies[index]))) ||
				strings.Contains(line, "BODY[") || strings.Contains(line, "BINARY") {
				t.Fatalf("UID %d served %q", uid, line)
			}

			message := messages[index]
			wantSeen := index == 0
			if message.UID != uid || slices.Contains(message.Flags, imap.FlagSeen) != wantSeen || message.RFC822Size != int64(len(bodies[index])) || message.BodyStructure == nil {
				t.Fatalf("UID %d parsed %+v", uid, message)
			}
		}

		// Structure plus header: exactly one structure and one header literal.
		messages, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(102), &imap.FetchOptions{BodyStructure: structure, BodySection: []*imap.FetchItemBodySection{header}})
		mixedHeader := metadataHeader(structureMixedBody)
		if strings.Count(served, "BODYSTRUCTURE "+wantMixedWire) != 1 || strings.Count(served, "BODYSTRUCTURE") != 1 ||
			strings.Count(served, fmt.Sprintf("BODY[HEADER] {%d}\r\n%s", len(mixedHeader), mixedHeader)) != 1 ||
			strings.Count(served, "BODY[") != 1 || strings.Contains(served, "STRUCTURE_BODY_SENTINEL") || strings.Contains(served, "FLAGS") {
			t.Fatalf("structure plus header served %q", served)
		}

		if len(messages) != 1 || string(messages[0].FindBodySection(header)) != mixedHeader {
			t.Fatalf("structure plus header parsed %+v", messages)
		}
		assertMixedStructure(t, messages[0].BodyStructure)

		// Header-only and body-only requests carry no structure.
		plainHeader := metadataHeader(structurePlainBody)
		_, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(101), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{header}})
		if served != fmt.Sprintf("* 1 FETCH (UID 101 BODY[HEADER] {%d}\r\n%s)\r\n", len(plainHeader), plainHeader) {
			t.Fatalf("header-only served %q", served)
		}

		_, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(103), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{{Peek: true}}})
		if served != fmt.Sprintf("* 3 FETCH (UID 103 BODY[] {%d}\r\n%s)\r\n", len(structureBasicBody), structureBasicBody) {
			t.Fatalf("body-only served %q", served)
		}

		if after := unseenUIDs(t, client); !slices.Equal(before, []imap.UID{102, 103}) || !slices.Equal(after, before) {
			t.Fatalf("UNSEEN before %v, after %v", before, after)
		}
		assertReadOnlyTLSTranscript(t, server)

		// UID is mandatory even when omitted from the item list; duplicate
		// structures and Seen-setting items are refused.
		raw := connectClient(t, server, ImplicitTLS)
		t.Cleanup(func() { _ = raw.Close() })
		raw.command(t, "s0 LOGIN fixture-user@client.test not-a-secret", "s0 OK LOGIN completed")
		raw.command(t, "s1 UID FETCH 102 (BODYSTRUCTURE)", "* 2 FETCH (UID 102 BODYSTRUCTURE "+wantMixedWire+")")
		if line := raw.readLine(t); line != "s1 OK FETCH completed" {
			t.Fatalf("UID-less FETCH completion = %q", line)
		}
		for _, refused := range []string{"UID FETCH 102 (UID BODYSTRUCTURE BODYSTRUCTURE)", "UID FETCH 102 (BODY[])", "UID FETCH 102 (ENVELOPE)", "UID FETCH * (BODYSTRUCTURE)"} {
			if _, err := fmt.Fprintf(raw.connection, "s2 %s\r\n", refused); err != nil {
				t.Fatalf("write %q: %v", refused, err)
			}

			if line := raw.readLine(t); !strings.HasPrefix(line, "s2 BAD ") {
				t.Fatalf("%q response = %q, want BAD", refused, line)
			}
		}
	})

	t.Run("bounded_configuration", func(t *testing.T) {
		t.Parallel()

		one := []string{structurePlainBody}
		leaf := func() *BodyPart {
			return &BodyPart{Kind: BodyPartBasic, Type: "application", Subtype: "octet-stream"}
		}
		withParams := func(content, disposition int) *BodyPart {
			part := leaf()
			for index := range content {
				part.Params = append(part.Params, BodyParam{fmt.Sprintf("p%d", index), "v"})
			}
			if disposition > 0 {
				part.Disposition = &BodyDisposition{Value: "attachment"}
				for index := range disposition {
					part.Disposition.Params = append(part.Disposition.Params, BodyParam{fmt.Sprintf("d%d", index), "v"})
				}
			}
			return part
		}
		wide := func(children int) *BodyPart {
			root := &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed"}
			for range children {
				root.Children = append(root.Children, leaf())
			}
			return root
		}
		deep := func(depth int) *BodyPart {
			root := leaf()
			for range depth - 1 {
				root = &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed", Children: []*BodyPart{root}}
			}
			return root
		}
		named := func(value string) *BodyPart {
			part := leaf()
			part.Disposition = &BodyDisposition{Value: "attachment", Params: []BodyParam{{"filename", value}}}
			return part
		}
		messages := func(count int) Options {
			options := Options{Mode: ImplicitTLS}
			for range count {
				options.Messages = append(options.Messages, structurePlainBody)
				options.BodyStructures = append(options.BodyStructures, plainStructure())
			}
			return options
		}
		structures := func(parts ...*BodyPart) Options {
			options := Options{Mode: ImplicitTLS, BodyStructures: parts}
			for range parts {
				options.Messages = append(options.Messages, structurePlainBody)
			}
			return options
		}
		self := &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed"}
		self.Children = []*BodyPart{self}
		outer := &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed"}
		inner := &BodyPart{Kind: BodyPartMultipart, Subtype: "alternative", Children: []*BodyPart{leaf(), outer}}
		outer.Children = []*BodyPart{inner}
		sized, sizedWire := sizedStructure(t, maxBodyStructureBytes)
		oversized, _ := sizedStructure(t, maxBodyStructureBytes+1)
		maxValue := strings.Repeat("é", maxBodyStructureValue/2)

		refusals := []struct {
			name    string
			options Options
			want    string
		}{
			{"count mismatch", Options{Messages: []string{structurePlainBody, structurePlainBody}, BodyStructures: []*BodyPart{plainStructure()}}, "1 BodyStructures for 2 Messages"},
			{"descriptors without messages", Options{BodyStructures: []*BodyPart{plainStructure()}}, "require custom Messages"},
			{"empty descriptors with messages", Options{Messages: one, BodyStructures: []*BodyPart{}}, "0 BodyStructures for 1 Messages"},
			{"nonordinary metadata flags", Options{Messages: one, BodyStructures: []*BodyPart{plainStructure()}, MetadataFlags: MetadataFlagsDuplicated}, "cannot be combined"},
			{"nonzero scenario", Options{Messages: one, BodyStructures: []*BodyPart{plainStructure()}, Scenario: Scenario{OmitMetadataFetchResponse: true}}, "cannot be combined"},
			{"stateful with messages", Options{Messages: one, BodyStructures: []*BodyPart{plainStructure()}, Stateful: &StatefulOptions{Mailboxes: []MailboxSeed{{Name: "INBOX"}}}}, "stateful mode cannot be combined"},
			{"stateful without messages", Options{BodyStructures: []*BodyPart{plainStructure()}, Stateful: &StatefulOptions{Mailboxes: []MailboxSeed{{Name: "INBOX"}}}}, "require custom Messages"},
			{"labels with messages", Options{Messages: one, BodyStructures: []*BodyPart{plainStructure()}, Labels: &LabelOptions{Views: []LabelViewSeed{{Name: "INBOX", Role: FolderRole}}}}, "label mode cannot be combined"},
			{"labels without messages", Options{BodyStructures: []*BodyPart{plainStructure()}, Labels: &LabelOptions{Views: []LabelViewSeed{{Name: "INBOX", Role: FolderRole}}}}, "require custom Messages"},
			{"nil root", structures(nil), "nil body part"},
			{"zero kind", structures(&BodyPart{Type: "text", Subtype: "plain"}), "unsupported body part kind"},
			{"unknown kind", structures(&BodyPart{Kind: BodyPartMultipart + 1, Type: "text", Subtype: "plain"}), "unsupported body part kind"},
			{"embedded message", structures(&BodyPart{Kind: BodyPartBasic, Type: "message", Subtype: "rfc822"}), "unsupported basic body part type"},
			{"text kind with basic type", structures(&BodyPart{Kind: BodyPartText, Type: "application", Subtype: "pdf"}), "text body part has type"},
			{"unsupported encoding", structures(&BodyPart{Kind: BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "uuencode"}), "unsupported transfer encoding"},
			{"empty multipart", structures(&BodyPart{Kind: BodyPartMultipart, Subtype: "mixed"}), "needs children"},
			{"self cycle", structures(self), "cyclic"},
			{"indirect cycle", structures(outer), "cyclic"},
			{"CR value", structures(named("a\rb.pdf")), "single-line UTF-8"},
			{"LF value", structures(named("a\nb.pdf")), "single-line UTF-8"},
			{"NUL value", structures(&BodyPart{Kind: BodyPartBasic, Type: "application", Subtype: "pd\x00f"}), "single-line UTF-8"},
			{"invalid UTF-8 value", structures(named("\xff.pdf")), "single-line UTF-8"},
			{"messages cap+1", messages(maxBodyStructureMessages + 1), "at most 64 messages"},
			{"nodes cap+1", structures(wide(maxBodyStructureNodes)), "more than 128 body parts"},
			{"depth cap+1", structures(deep(maxBodyStructureDepth + 1)), "depth exceeds 16"},
			{"params cap+1", structures(withParams(maxBodyStructureParams+1, 0)), "more than 8 parameters"},
			{"params cap+1 with disposition", structures(withParams(4, 5)), "more than 8 parameters"},
			{"value cap+1", structures(named(maxValue + "x")), "exceeds 512 bytes"},
			{"serialized cap+1", structures(oversized), "renders 65537 bytes"},
		}
		for _, refusal := range refusals {
			server, err := Start(refusal.options)
			if err == nil {
				_ = server.Close()
				t.Fatalf("%s: Start succeeded, want refusal", refusal.name)
			}

			if !strings.Contains(err.Error(), refusal.want) {
				t.Fatalf("%s: Start error %q, want %q", refusal.name, err, refusal.want)
			}
		}

		accepted := []struct {
			name    string
			options Options
			uid     int
			wire    string
		}{
			{name: "messages cap", options: messages(maxBodyStructureMessages), uid: 100 + maxBodyStructureMessages, wire: wantPlainWire},
			{name: "nodes cap", options: structures(wide(maxBodyStructureNodes - 1)), uid: 101},
			{name: "depth cap", options: structures(deep(maxBodyStructureDepth)), uid: 101},
			{name: "params cap", options: structures(withParams(maxBodyStructureParams, 0)), uid: 101},
			{name: "params cap with disposition", options: structures(withParams(4, 4)), uid: 101},
			{name: "value cap", options: structures(named(maxValue)), uid: 101},
			{name: "serialized cap", options: structures(sized), uid: 101, wire: sizedWire},
		}
		for _, test := range accepted {
			server := startStructureFixture(t, test.options)
			client, _ := structureSession(t, server, ImplicitTLS)
			messages, err := client.Fetch(imap.UIDSetNum(imap.UID(test.uid)), &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}}).Collect()
			if err != nil || len(messages) != 1 || messages[0].UID != imap.UID(test.uid) || messages[0].BodyStructure == nil {
				t.Fatalf("%s: FETCH = %+v, %v", test.name, messages, err)
			}

			if test.wire == "" {
				continue
			}

			raw := connectClient(t, server, ImplicitTLS)
			raw.command(t, "c0 LOGIN fixture-user@client.test not-a-secret", "c0 OK LOGIN completed")
			raw.command(t, fmt.Sprintf("c1 UID FETCH %d (BODYSTRUCTURE)", test.uid), fmt.Sprintf("* %d FETCH (UID %d BODYSTRUCTURE %s)", test.uid-100, test.uid, test.wire))
			_ = raw.Close()
		}
		if len(sizedWire) != maxBodyStructureBytes {
			t.Fatalf("serialized cap witness = %d bytes", len(sizedWire))
		}

		// Caller mutation after Start cannot alter accepted tree bytes.
		tree := mixedStructure()
		callerStructures := []*BodyPart{tree}
		server := startStructureFixture(t, Options{Mode: ImplicitTLS, Messages: []string{structureMixedBody}, BodyStructures: callerStructures})
		tree.Subtype = "related"
		tree.Params[0].Value = "mutated"
		tree.Children[0].Children[0].Lines = 99
		tree.Children[3].Disposition.Params[1].Value = "1"
		tree.Children[1].Disposition = nil
		tree.Children = tree.Children[:1]
		callerStructures[0] = plainStructure()

		raw := connectClient(t, server, ImplicitTLS)
		t.Cleanup(func() { _ = raw.Close() })
		raw.command(t, "c0 LOGIN fixture-user@client.test not-a-secret", "c0 OK LOGIN completed")
		raw.command(t, "c1 UID FETCH 101 (UID BODYSTRUCTURE)", "* 1 FETCH (UID 101 BODYSTRUCTURE "+wantMixedWire+")")
	})

	t.Run("framing_and_legacy", func(t *testing.T) {
		t.Parallel()

		const sentinel = "LARGE_RAW_BODY_SENTINEL_"
		const quoted, backslash, nonASCII = `quote "q".pdf`, `back\slash.bin`, "résumé-日本.pdf"
		largeHeader := "From: Large Fixture <large@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Large structure\r\n\r\n"
		large := largeHeader + strings.Repeat(sentinel, 300<<10/len(sentinel)+1)[:300<<10]
		attachment := func(name string, size uint32, dispositionSize string) *BodyPart {
			return &BodyPart{
				Kind: BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "base64", Size: size,
				Disposition: &BodyDisposition{Value: "attachment", Params: []BodyParam{{"filename", name}, {"size", dispositionSize}}},
			}
		}
		tree := &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed", Children: []*BodyPart{
			{Kind: BodyPartText, Type: "text", Subtype: "plain", Size: 64, Lines: 2},
			attachment(quoted, 409600, "307200"),
			attachment(backslash, 12, "8"),
			attachment(nonASCII, 16, "11"),
		}}
		wantWire := `(("text" "plain" NIL NIL NIL "7bit" 64 2 NIL NIL NIL NIL)` +
			`("application" "pdf" NIL NIL NIL "base64" 409600 NIL ("attachment" ("filename" "quote \"q\".pdf" "size" "307200")) NIL NIL)` +
			`("application" "pdf" NIL NIL NIL "base64" 12 NIL ("attachment" ("filename" "back\\slash.bin" "size" "8")) NIL NIL)` +
			`("application" "pdf" NIL NIL NIL "base64" 16 NIL ("attachment" ("filename" {` + strconv.Itoa(len(nonASCII)) + "}\r\n" + nonASCII + ` "size" "11")) NIL NIL)` +
			` "mixed" NIL NIL NIL NIL)`
		wantNames := []string{quoted, backslash, nonASCII}
		wantSizes := []uint32{409600, 12, 16}
		wantDispositionSizes := []string{"307200", "8", "11"}

		for _, mode := range []TLSMode{StartTLS, ImplicitTLS} {
			server := startStructureFixture(t, Options{Mode: mode, Messages: []string{large}, Seen: []bool{true}, BodyStructures: []*BodyPart{tree}})
			client, wire := structureSession(t, server, mode)

			messages, command, served := fetchStructureWire(t, client, wire, imap.UIDSetNum(101), &imap.FetchOptions{BodyStructure: &imap.FetchItemBodyStructure{Extended: true}})
			if command != "UID FETCH 101 (UID BODYSTRUCTURE)" || served != "* 1 FETCH (UID 101 BODYSTRUCTURE "+wantWire+")\r\n" {
				t.Fatalf("%s: requested %q, served %q", mode, command, served)
			}

			multipart, ok := messages[0].BodyStructure.(*imap.BodyStructureMultiPart)
			if len(messages) != 1 || !ok || len(multipart.Children) != 4 {
				t.Fatalf("%s: parsed %+v", mode, messages)
			}
			for index, child := range multipart.Children[1:] {
				part := child.(*imap.BodyStructureSinglePart)
				if part.Filename() != wantNames[index] || part.Size != wantSizes[index] || part.Disposition().Params["size"] != wantDispositionSizes[index] {
					t.Fatalf("%s: attachment %d parsed %+v", mode, index, part)
				}
			}

			messages, _, served = fetchStructureWire(t, client, wire, imap.UIDSetNum(101), &imap.FetchOptions{Flags: true, RFC822Size: true})
			if len(messages) != 1 || messages[0].RFC822Size != int64(len(large)) || !slices.Equal(messages[0].Flags, []imap.Flag{imap.FlagSeen}) {
				t.Fatalf("%s: size/flags parsed %+v", mode, messages)
			}
			if !strings.Contains(served, fmt.Sprintf("RFC822.SIZE %d", len(large))) || !strings.Contains(served, `FLAGS (\Seen)`) || strings.Contains(served, "BODYSTRUCTURE") || strings.Contains(served, sentinel) {
				t.Fatalf("%s: size/flags served %q", mode, served)
			}

			header := &imap.FetchItemBodySection{Specifier: imap.PartSpecifierHeader, Peek: true}
			messages, _, headerServed := fetchStructureWire(t, client, wire, imap.UIDSetNum(101), &imap.FetchOptions{BodySection: []*imap.FetchItemBodySection{header}})
			if headerServed != fmt.Sprintf("* 1 FETCH (UID 101 BODY[HEADER] {%d}\r\n%s)\r\n", len(largeHeader), largeHeader) || string(messages[0].FindBodySection(header)) != largeHeader {
				t.Fatalf("%s: header served %q", mode, headerServed)
			}

			if received := wire.received(); strings.Contains(received, sentinel) || len(received) > 8<<10 {
				t.Fatalf("%s: client received %d bytes including body sentinel=%t", mode, len(received), strings.Contains(received, sentinel))
			}
			for _, command := range server.Commands() {
				if strings.Contains(strings.ToUpper(command.Raw), "BODY.PEEK[]") || strings.Contains(strings.ToUpper(command.Raw), "BODY[") {
					t.Fatalf("%s: whole-body request %q", mode, command.Raw)
				}
			}
			assertReadOnlyTLSTranscript(t, server)

			closed := make(chan error, 1)
			go func() { closed <- server.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatalf("%s: close fixture: %v", mode, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatalf("%s: fixture close did not complete", mode)
			}
			if err := client.Noop().Wait(); err == nil {
				t.Fatalf("%s: client command succeeded after fixture shutdown", mode)
			}
		}
	})
}

// sizedStructure builds a multipart tree that renders to exactly target bytes,
// returning the tree and its independently computed wire form.
func sizedStructure(t *testing.T, target int) (*BodyPart, string) {
	t.Helper()

	const leaves = 40
	values := make([][]string, leaves)
	for index := range values {
		values[index] = slices.Repeat([]string{"v"}, maxBodyStructureParams)
	}
	render := func() string {
		var builder strings.Builder
		builder.WriteByte('(')
		for _, params := range values {
			builder.WriteString(`("text" "plain" (`)
			for index, value := range params {
				if index > 0 {
					builder.WriteByte(' ')
				}
				fmt.Fprintf(&builder, `"p%d" "%s"`, index, value)
			}
			builder.WriteString(`) NIL NIL "7bit" 0 0 NIL NIL NIL NIL)`)
		}
		builder.WriteString(` "mixed" NIL NIL NIL NIL)`)
		return builder.String()
	}

	gap := target - len(render())
	for leaf := range values {
		for param := range values[leaf] {
			grow := min(gap, maxBodyStructureValue-len(values[leaf][param]))
			values[leaf][param] += strings.Repeat("v", grow)
			gap -= grow
		}
	}
	wire := render()
	if len(wire) != target {
		t.Fatalf("sized structure renders %d bytes, want %d", len(wire), target)
	}

	root := &BodyPart{Kind: BodyPartMultipart, Subtype: "mixed"}
	for _, params := range values {
		part := &BodyPart{Kind: BodyPartText, Type: "text", Subtype: "plain"}
		for index, value := range params {
			part.Params = append(part.Params, BodyParam{fmt.Sprintf("p%d", index), value})
		}
		root.Children = append(root.Children, part)
	}

	return root, wire
}

func assertMixedStructure(t *testing.T, structure imap.BodyStructure) {
	t.Helper()

	mixed, ok := structure.(*imap.BodyStructureMultiPart)
	if !ok || mixed.Subtype != "mixed" || len(mixed.Children) != 4 || mixed.Extended == nil || mixed.Extended.Params["boundary"] != "mixed-boundary" {
		t.Fatalf("mixed structure = %+v", structure)
	}

	alternative, ok := mixed.Children[0].(*imap.BodyStructureMultiPart)
	if !ok || alternative.Subtype != "alternative" || len(alternative.Children) != 2 {
		t.Fatalf("alternative structure = %+v", mixed.Children[0])
	}

	texts := []struct {
		subtype, charset, encoding string
		size                       uint32
		lines                      int64
	}{{"plain", "utf-8", "quoted-printable", 120, 4}, {"html", "utf-8", "8bit", 340, 9}}
	for index, want := range texts {
		part, ok := alternative.Children[index].(*imap.BodyStructureSinglePart)
		if !ok || part.Type != "text" || part.Subtype != want.subtype || part.Params["charset"] != want.charset || part.Encoding != want.encoding ||
			part.Size != want.size || part.Text == nil || part.Text.NumLines != want.lines || part.Disposition() != nil {
			t.Fatalf("text part %d = %+v", index, alternative.Children[index])
		}
	}

	attachments := []struct {
		name, encoding  string
		size            uint32
		dispositionSize string
		hasSize         bool
	}{{"absent.bin", "base64", 4096, "", false}, {"zero.txt", "quoted-printable", 777, "0", true}, {"positive.bin", "base64", 1368, "1000", true}}
	for index, want := range attachments {
		part, ok := mixed.Children[index+1].(*imap.BodyStructureSinglePart)
		if !ok || part.MediaType() != "application/octet-stream" || part.Encoding != want.encoding || part.Size != want.size || part.Text != nil ||
			part.Disposition() == nil || part.Disposition().Value != "attachment" || part.Filename() != want.name {
			t.Fatalf("attachment %d = %+v", index, mixed.Children[index+1])
		}

		size, hasSize := part.Disposition().Params["size"]
		if hasSize != want.hasSize || size != want.dispositionSize {
			t.Fatalf("attachment %d disposition size = %q present=%t, want %q present=%t", index, size, hasSize, want.dispositionSize, want.hasSize)
		}
	}
}

func startStructureFixture(t *testing.T, options Options) *Server {
	t.Helper()

	server, err := Start(options)
	if err != nil {
		t.Fatalf("start structure fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close structure fixture: %v", err)
		}
	})

	return server
}

// wireLog records the client's raw post-TLS traffic split by direction. The
// client tees ingress through io.TeeReader and egress through io.MultiWriter
// into one DebugWriter, so each write is classified by its caller frame.
// Writes from neither path are counted as unclassified.
type wireLog struct {
	mu           sync.Mutex
	ingress      bytes.Buffer
	egress       bytes.Buffer
	unclassified int
}

func (log *wireLog) Write(data []byte) (int, error) {
	ingress, egress := debugWriteDirection()

	log.mu.Lock()
	defer log.mu.Unlock()

	switch {
	case ingress:
		return log.ingress.Write(data)
	case egress:
		return log.egress.Write(data)
	default:
		log.unclassified += len(data)
		return len(data), nil
	}
}

// debugWriteDirection reports whether the current DebugWriter call carries
// bytes read from the server or written by the client.
func debugWriteDirection() (bool, bool) {
	callers := make([]uintptr, 64)
	frames := runtime.CallersFrames(callers[:runtime.Callers(3, callers)])
	for {
		frame, more := frames.Next()
		switch frame.Function {
		case "io.(*teeReader).Read":
			return true, false
		case "io.(*multiWriter).Write", "io.(*multiWriter).WriteString":
			return false, true
		}

		if !more {
			return false, false
		}
	}
}

// marks returns the current ingress and egress lengths.
func (log *wireLog) marks() (int, int) {
	log.mu.Lock()
	defer log.mu.Unlock()

	return log.ingress.Len(), log.egress.Len()
}

// since returns ingress and egress bytes after the given marks and the
// number of unclassified bytes seen so far.
func (log *wireLog) since(ingress, egress int) (string, string, int) {
	log.mu.Lock()
	defer log.mu.Unlock()

	return log.ingress.String()[ingress:], log.egress.String()[egress:], log.unclassified
}

// received returns every byte the client read from the server.
func (log *wireLog) received() string {
	received, _, _ := log.since(0, 0)
	return received
}

// structureSession connects the pinned go-imap client over verified TLS,
// authenticates with synthetic credentials and examines INBOX.
func structureSession(t *testing.T, server *Server, mode TLSMode) (*imapclient.Client, *wireLog) {
	t.Helper()

	connection, err := net.DialTimeout("tcp", server.Addr(), statefulNetworkTimeout)
	if err != nil {
		t.Fatalf("dial %s: %v", mode, err)
	}
	if err := connection.SetDeadline(time.Now().Add(statefulNetworkTimeout)); err != nil {
		_ = connection.Close()
		t.Fatalf("set deadline: %v", err)
	}

	wire := &wireLog{}
	options := &imapclient.Options{TLSConfig: server.ClientTLSConfig(), DebugWriter: wire}
	var client *imapclient.Client
	if mode == StartTLS {
		client, err = imapclient.NewStartTLS(connection, options)
		if err != nil {
			_ = connection.Close()
			t.Fatalf("STARTTLS: %v", err)
		}
	} else {
		tlsConnection := tls.Client(connection, server.ClientTLSConfig())
		if err := tlsConnection.Handshake(); err != nil {
			_ = connection.Close()
			t.Fatalf("implicit TLS handshake: %v", err)
		}
		client = imapclient.New(tlsConnection, options)
	}
	t.Cleanup(func() { _ = client.Close() })

	if err := client.Login("fixture-user@client.test", "not-a-secret").Wait(); err != nil {
		t.Fatalf("login over %s: %v", mode, err)
	}

	if _, err := client.Select("INBOX", &imap.SelectOptions{ReadOnly: true}).Wait(); err != nil {
		t.Fatalf("examine over %s: %v", mode, err)
	}

	return client, wire
}

// fetchStructureWire runs one UID FETCH and returns the parsed messages, the
// exact command sent and the untagged FETCH responses served for its tag.
// Unrelated exchanges, such as the client's own CAPABILITY, are skipped.
func fetchStructureWire(t *testing.T, client *imapclient.Client, wire *wireLog, uids imap.UIDSet, options *imap.FetchOptions) ([]*imapclient.FetchMessageBuffer, string, string) {
	t.Helper()

	ingressMark, egressMark := wire.marks()
	messages, err := client.Fetch(uids, options).Collect()
	if err != nil {
		t.Fatalf("UID FETCH %s: %v", uids, err)
	}

	ingress, egress, unclassified := wire.since(ingressMark, egressMark)
	if unclassified != 0 {
		t.Fatalf("%d DebugWriter bytes had no direction", unclassified)
	}

	tag, command := "", ""
	for _, line := range strings.Split(egress, "\r\n") {
		if lineTag, rest, _ := strings.Cut(line, " "); strings.HasPrefix(rest, "UID FETCH ") {
			if tag != "" {
				t.Fatalf("more than one FETCH sent: %q", egress)
			}
			tag, command = lineTag, rest
		}
	}
	if tag == "" {
		t.Fatalf("no FETCH command in egress %q", egress)
	}

	served, ok := servedFetch(ingress, tag)
	if !ok {
		t.Fatalf("ingress %q lacks %s FETCH completion", ingress, tag)
	}

	return messages, command, served
}

var (
	literalSuffix = regexp.MustCompile(`\{(\d+)\}$`)
	untaggedFetch = regexp.MustCompile(`^\* \d+ FETCH \(`)
)

// servedFetch splits ingress into whole responses, following literals, and
// returns the untagged FETCH responses before the tagged OK for tag.
func servedFetch(ingress, tag string) (string, bool) {
	var served strings.Builder
	for ingress != "" {
		end := 0
		for {
			line := strings.Index(ingress[end:], "\r\n")
			if line < 0 {
				return "", false
			}
			line += end

			match := literalSuffix.FindStringSubmatch(ingress[end:line])
			if match == nil {
				end = line + 2
				break
			}

			size, err := strconv.Atoi(match[1])
			if err != nil || line+2+size > len(ingress) {
				return "", false
			}
			end = line + 2 + size
		}

		response := ingress[:end]
		ingress = ingress[end:]
		switch {
		case response == tag+" OK FETCH completed\r\n":
			return served.String(), true
		case strings.HasPrefix(response, tag+" "):
			return "", false
		case untaggedFetch.MatchString(response):
			served.WriteString(response)
		}
	}

	return "", false
}

func unseenUIDs(t *testing.T, client *imapclient.Client) []imap.UID {
	t.Helper()

	data, err := client.UIDSearch(&imap.SearchCriteria{NotFlag: []imap.Flag{imap.FlagSeen}}, nil).Wait()
	if err != nil {
		t.Fatalf("UID SEARCH UNSEEN: %v", err)
	}

	return data.AllUIDs()
}

// assertReadOnlyTLSTranscript checks that every recorded command is read-only,
// peek-only and, apart from STARTTLS and the client's own CAPABILITY probe,
// sent after verified TLS.
func assertReadOnlyTLSTranscript(t *testing.T, server *Server) {
	t.Helper()

	if err := server.AssertReadOnlyCommands(); err != nil {
		t.Fatal(err)
	}

	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatal(err)
	}

	transcript, err := ParseTranscript(server.Commands())
	if err != nil {
		t.Fatalf("parse transcript: %v", err)
	}
	for _, command := range transcript {
		if command.Store != "" || command.Destination != "" || !isReadOnlyCommand(command.Verb) {
			t.Fatalf("transcript records mutating %s", command.Verb)
		}

		if command.Verb != "STARTTLS" && command.Verb != "CAPABILITY" && !command.TLS {
			t.Fatalf("%s recorded before TLS", command.Verb)
		}
	}
}

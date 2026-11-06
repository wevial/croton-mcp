package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// searchMetadataItemKeys is the closed key set of one search result or
// digest candidate.
var searchMetadataItemKeys = map[string]bool{
	"id": true, "mailbox": true, "uidvalidity": true, "uid": true, "subject": true, "size": true,
	"from": true, "date": true, "dateSource": true, "readState": true, "metadataTruncated": true,
}

var (
	searchMetadataResultKeys = map[string]bool{"results": true, "truncated": true}
	digestMetadataResultKeys = map[string]bool{"mailbox": true, "candidates": true, "totalMessages": true, "unseenCount": true, "truncated": true}
)

// metadataSeed is one synthetic message whose sender, header Date and Seen
// state are known to the test independently of the MCP surface. An empty
// header value omits that header.
type metadataSeed struct {
	from    string
	date    string
	subject string
	seen    bool
}

// wantMetadata is the exact public metadata expected for one entry. An empty
// from or date means the key must be absent.
type wantMetadata struct {
	tuple      readTuple
	subject    string
	from       string
	date       string
	dateSource string
	readState  string
	truncated  bool
	size       int64
}

func metadataFixture(seeds []metadataSeed, bodies []string) testkit.Options {
	options := testkit.Options{}
	for index, seed := range seeds {
		var header strings.Builder
		if seed.from != "" {
			header.WriteString("From: " + seed.from + "\r\n")
		}
		header.WriteString("To: Reader <reader@croton.test>\r\nSubject: " + seed.subject + "\r\n")
		if seed.date != "" {
			header.WriteString("Date: " + seed.date + "\r\n")
		}
		body := fmt.Sprintf("Synthetic metadata body %d.", index)
		if index < len(bodies) {
			body = bodies[index]
		}
		fmt.Fprintf(&header, "Message-ID: <metadata-%d@croton.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s\r\n", index, body)

		options.Messages = append(options.Messages, header.String())
		options.Seen = append(options.Seen, seed.seen)
	}

	return options
}

func requireClosedKeys(t *testing.T, label string, object map[string]any, allowed map[string]bool) {
	t.Helper()

	for key := range object {
		if !allowed[key] {
			t.Fatalf("%s gained key %q: %v", label, key, object)
		}
	}
}

// requireMetadataItem compares one entry with its independently known
// identity and metadata, exactly and whole.
func requireMetadataItem(t *testing.T, label string, item map[string]any, want wantMetadata) {
	t.Helper()

	requireClosedKeys(t, label, item, searchMetadataItemKeys)
	if id, _ := item["id"].(string); id == "" {
		t.Fatalf("%s lacks its opaque id: %v", label, item)
	}
	if got := flatTuple(t, item); got != want.tuple {
		t.Fatalf("%s tuple = %+v, want %+v", label, got, want.tuple)
	}
	if item["subject"] != want.subject || item["dateSource"] != want.dateSource || item["readState"] != want.readState {
		t.Fatalf("%s subject/dateSource/readState = %.80q/%v/%v, want %.80q/%s/%s", label, item["subject"], item["dateSource"], item["readState"], want.subject, want.dateSource, want.readState)
	}

	for _, field := range []struct{ key, want string }{{"from", want.from}, {"date", want.date}} {
		value, present := item[field.key]
		if field.want == "" && present {
			t.Fatalf("%s carries %s %.80q, want it omitted", label, field.key, value)
		}
		if field.want != "" && value != field.want {
			t.Fatalf("%s %s = %.80q, want %.80q", label, field.key, value, field.want)
		}
	}

	truncated, present := item["metadataTruncated"]
	if want.truncated != present || (present && truncated != true) {
		t.Fatalf("%s metadataTruncated = %v (present %v), want %v and omitted when false", label, truncated, present, want.truncated)
	}
	if want.size > 0 && item["size"] != json.Number(strconv.FormatInt(want.size, 10)) {
		t.Fatalf("%s size = %v, want %d", label, item["size"], want.size)
	}
}

// requireDescendingUIDs pins strictly descending UID order.
func requireDescendingUIDs(t *testing.T, label string, items []map[string]any) []uint32 {
	t.Helper()

	uids := make([]uint32, 0, len(items))
	for index, item := range items {
		uid := flatTuple(t, item).UID
		if index > 0 && uid >= uids[index-1] {
			t.Fatalf("%s UID %d follows %d, want strictly descending UIDs", label, uid, uids[index-1])
		}
		uids = append(uids, uid)
	}

	return uids
}

// requireMetadataOnlyWire permits only TLS, read-only commands and metadata
// FETCH items: no body, INTERNALDATE or mutating command.
func requireMetadataOnlyWire(t *testing.T, server *testkit.Server, commands []testkit.Command) {
	t.Helper()

	requireReadOnlyTranscript(t, server)
	requireNoBodyFetch(t, commands)
	for _, command := range commands {
		upper := strings.ToUpper(command.Raw)
		if !command.TLS {
			t.Fatalf("metadata command used plaintext transport: %q", command.Raw)
		}
		if !strings.Contains(upper, " FETCH ") {
			continue
		}
		_, items, _ := strings.Cut(upper, "(")
		for _, item := range strings.Fields(strings.TrimSuffix(items, ")")) {
			if item != "UID" && item != "FLAGS" && item != "RFC822.SIZE" && item != "BODY.PEEK[HEADER]" {
				t.Fatalf("metadata call requested non-metadata FETCH item %q: %s", item, command.Raw)
			}
		}
	}
	if err := server.AssertNoInsecureAuthentication(); err != nil {
		t.Fatal(err)
	}
}

func searchItems(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (map[string]any, []map[string]any) {
	t.Helper()

	object := rawObject(t, callTool(t, session, "search_mail", arguments))
	requireClosedKeys(t, "search_mail result", object, searchMetadataResultKeys)
	return object, rawItems(t, object, "results")
}

func digestItems(t *testing.T, session *mcp.ClientSession, arguments map[string]any) (map[string]any, []map[string]any) {
	t.Helper()

	object := rawObject(t, callTool(t, session, "select_digest_candidates", arguments))
	requireClosedKeys(t, "select_digest_candidates result", object, digestMetadataResultKeys)
	requireNoIdentityKeys(t, "digest summary", object)
	return object, rawItems(t, object, "candidates")
}

func requireTruncated(t *testing.T, label string, object map[string]any, want bool) {
	t.Helper()

	value, present := object["truncated"]
	if want != present || (present && value != true) {
		t.Fatalf("%s truncated = %v (present %v), want %v", label, value, present, want)
	}
}

func requireCounter(t *testing.T, label string, object map[string]any, key string, want int) {
	t.Helper()

	if object[key] != json.Number(strconv.Itoa(want)) {
		t.Fatalf("%s %s = %v, want %d", label, key, object[key], want)
	}
}

func highEntropySentinel(t *testing.T, label string) string {
	t.Helper()

	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		t.Fatalf("generate sentinel: %v", err)
	}
	return label + hex.EncodeToString(random)
}

func TestSearchMetadataPublicMCP(t *testing.T) {
	t.Parallel()

	t.Run("search_and_digest", func(t *testing.T) {
		t.Parallel()

		seeds := []metadataSeed{
			{from: "Alpha Sender <alpha@croton.test>", date: "Mon, 02 Jan 2006 15:04:05 -0700", subject: "Synthetic metadata alpha", seen: true},
			{from: "=?UTF-8?Q?Beta_S=C3=A9nder?= <beta@croton.test>", date: "Tue, 03 Jan 2006 01:00:00 +0000", subject: "Synthetic metadata beta"},
			{from: "Gamma <gamma@croton.test>", date: "Wed, 04 Jan 2006 08:30:00 +0900", subject: "Synthetic metadata gamma"},
		}
		server, adapter := startIntegrationAdapterWithOptions(t, metadataFixture(seeds, nil))
		observed := observeFixtureTuples(t, server, "Subject")
		want := map[string]wantMetadata{
			"Synthetic metadata alpha": {from: "Alpha Sender <alpha@croton.test>", date: "2006-01-02T22:04:05Z", dateSource: "header", readState: "read"},
			"Synthetic metadata beta":  {from: "Beta Sénder <beta@croton.test>", date: "2006-01-03T01:00:00Z", dateSource: "header", readState: "unread"},
			"Synthetic metadata gamma": {from: "Gamma <gamma@croton.test>", date: "2006-01-03T23:30:00Z", dateSource: "header", readState: "unread"},
		}
		for subject, metadata := range want {
			metadata.tuple, metadata.subject = observed[subject], subject
			want[subject] = metadata
		}
		session := connectTestClient(t, Options{Mail: adapter})
		before := len(server.Commands())

		search, results := searchItems(t, session, map[string]any{"mailbox": "INBOX"})
		digest, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": false, "sinceHours": 168})
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		requireTruncated(t, "search_mail", search, false)
		requireTruncated(t, "select_digest_candidates", digest, false)
		requireCounter(t, "digest", digest, "totalMessages", 3)
		requireCounter(t, "digest", digest, "unseenCount", 2)

		idsBySubject := make(map[string]string)
		for _, shape := range []struct {
			name  string
			items []map[string]any
		}{{"search_mail result", results}, {"select_digest_candidates candidate", candidates}} {
			if len(shape.items) != len(seeds) {
				t.Fatalf("%s count = %d, want %d", shape.name, len(shape.items), len(seeds))
			}
			uids := requireDescendingUIDs(t, shape.name, shape.items)
			if uids[0] != observed["Synthetic metadata gamma"].UID {
				t.Fatalf("%s starts at UID %d, want the highest fixture UID", shape.name, uids[0])
			}

			for _, item := range shape.items {
				subject, _ := item["subject"].(string)
				expected, ok := want[subject]
				if !ok {
					t.Fatalf("%s is not a fixture message: %v", shape.name, item)
				}
				requireMetadataItem(t, shape.name+" "+subject, item, expected)

				from, _ := item["from"].(string)
				if len(from) > 512 || !utf8.ValidString(from) {
					t.Fatalf("%s discloses %d sender bytes, want at most 512 valid UTF-8 bytes", shape.name, len(from))
				}
				id, _ := item["id"].(string)
				if previous, seen := idsBySubject[subject]; seen && previous != id {
					t.Fatalf("%s opaque id for %q differs from search", shape.name, subject)
				}
				idsBySubject[subject] = id
			}
		}

		// The header Date is labeled by its source and never as an arrival,
		// delivery or INTERNALDATE time.
		for _, item := range append(results, candidates...) {
			for key := range item {
				lowered := strings.ToLower(key)
				for _, forbidden := range []string{"internal", "arriv", "receiv", "deliver"} {
					if strings.Contains(lowered, forbidden) {
						t.Fatalf("entry presents a header date as %q: %v", key, item)
					}
				}
			}
		}
		for _, command := range server.Commands()[before:] {
			if strings.Contains(strings.ToUpper(command.Raw), "INTERNALDATE") {
				t.Fatalf("metadata call requested INTERNALDATE: %q", command.Raw)
			}
		}
	})

	t.Run("unknown_and_truncated", func(t *testing.T) {
		t.Parallel()

		longSender := "Long " + strings.Repeat("sender", 100) + " <long@croton.test>"
		seeds := []metadataSeed{
			{from: longSender, subject: "Synthetic unknown long sender"},
			{date: "not a synthetic date", subject: "Synthetic unknown invalid date"},
			{from: "Known <known@croton.test>", date: "Thu, 05 Jan 2006 10:00:00 +0000", subject: "Synthetic unknown known header"},
		}
		known := map[string]wantMetadata{
			"Synthetic unknown long sender":  {from: longSender[:512], dateSource: "unknown", truncated: true},
			"Synthetic unknown invalid date": {dateSource: "unknown"},
			"Synthetic unknown known header": {from: "Known <known@croton.test>", date: "2006-01-05T10:00:00Z", dateSource: "header"},
		}

		for _, flags := range []struct {
			name      string
			mode      testkit.MetadataFlagsMode
			seen      []bool
			readState []string
		}{
			{name: "flags omitted", mode: testkit.MetadataFlagsOmitted, seen: []bool{true, false, true}, readState: []string{"unknown", "unknown", "unknown"}},
			{name: "flags present", mode: testkit.MetadataFlagsOrdinary, seen: []bool{true, false, true}, readState: []string{"read", "unread", "read"}},
		} {
			for index := range seeds {
				seeds[index].seen = flags.seen[index]
			}
			options := metadataFixture(seeds, nil)
			options.MetadataFlags = flags.mode
			server, adapter := startIntegrationAdapterWithOptions(t, options)
			observed := observeFixtureTuples(t, server, "Subject")
			session := connectTestClient(t, Options{Mail: adapter})
			before := len(server.Commands())

			search, results := searchItems(t, session, map[string]any{"mailbox": "INBOX"})
			digest, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": false})
			requireMetadataOnlyWire(t, server, server.Commands()[before:])
			requireTruncated(t, flags.name+" search", search, false)
			requireTruncated(t, flags.name+" digest", digest, false)

			for _, items := range [][]map[string]any{results, candidates} {
				if len(items) != len(seeds) {
					t.Fatalf("%s returned %d entries, want %d", flags.name, len(items), len(seeds))
				}
				requireDescendingUIDs(t, flags.name, items)
				for _, item := range items {
					subject, _ := item["subject"].(string)
					expected, ok := known[subject]
					if !ok {
						t.Fatalf("%s returned a non-fixture entry: %v", flags.name, item)
					}
					index := int(observed[subject].UID) - 101
					expected.tuple, expected.subject, expected.readState = observed[subject], subject, flags.readState[index]
					requireMetadataItem(t, flags.name+" "+subject, item, expected)
				}
			}
		}

		// Adapter-shaped metadata reaches the same mapping: zero values and
		// inconsistent provenance stay unknown, never fabricated unread.
		zero := withReadIdentity(bridge.MessageMetadata{ID: "opaque-zero-metadata", Mailbox: "INBOX", Subject: "Zero metadata"}, 9001, 105)
		inconsistent := []bridge.MessageMetadata{
			zero,
			withReadIdentity(bridge.MessageMetadata{ID: "opaque-empty-header-date", Mailbox: "INBOX", Subject: "Empty header date", DateSource: bridge.DateSourceHeader}, 9001, 104),
			withReadIdentity(bridge.MessageMetadata{ID: "opaque-offset-date", Mailbox: "INBOX", Subject: "Offset date", Date: "2006-01-02T15:04:05+02:00", DateSource: bridge.DateSourceHeader}, 9001, 103),
			withReadIdentity(bridge.MessageMetadata{ID: "opaque-unsourced-date", Mailbox: "INBOX", Subject: "Unsourced date", Date: "2006-01-02T15:04:05Z", ReadState: bridge.ReadState(9)}, 9001, 102),
			withReadIdentity(bridge.MessageMetadata{ID: "opaque-oversized-sender", Mailbox: "INBOX", Subject: "Oversized sender", From: strings.Repeat("é", 300), ReadState: bridge.ReadStateUnread}, 9001, 101),
		}
		mail := &fakeMail{status: bridge.MailboxStatus{Messages: 5, Unseen: 1}, searched: inconsistent}
		session := connectTestClient(t, Options{Mail: mail})
		_, results := searchItems(t, session, map[string]any{"mailbox": "INBOX"})
		_, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX"})
		for _, items := range [][]map[string]any{results, candidates} {
			if len(items) != len(inconsistent) {
				t.Fatalf("adapter-shaped entries = %d, want %d", len(items), len(inconsistent))
			}
			for index, item := range items {
				source := inconsistent[index]
				expected := wantMetadata{tuple: readTuple{Mailbox: "INBOX", UIDValidity: 9001, UID: source.UID}, subject: source.Subject, dateSource: "unknown", readState: "unknown"}
				if source.ReadState == bridge.ReadStateUnread {
					expected.readState = "unread"
					expected.from, expected.truncated = strings.Repeat("é", 256), true
				}
				requireMetadataItem(t, "adapter-shaped "+source.Subject, item, expected)
			}
		}
	})

	t.Run("order_and_unread", func(t *testing.T) {
		t.Parallel()

		// Header dates run backwards against UIDs, in equal pairs, so a date
		// sort and a UID sort disagree everywhere.
		const count = 100
		base := time.Date(2026, time.January, 31, 12, 0, 0, 0, time.UTC)
		seeds := make([]metadataSeed, 0, count)
		for index := range count {
			seeds = append(seeds, metadataSeed{
				from:    "Order Fixture <order@croton.test>",
				date:    base.Add(-time.Duration(index/2) * time.Hour).Format(time.RFC1123Z),
				subject: fmt.Sprintf("Synthetic order UID %d", 101+index),
				seen:    index%3 == 0,
			})
		}
		server, adapter := startIntegrationAdapterWithOptions(t, metadataFixture(seeds, nil))
		observed := observeFixtureTuples(t, server, "Subject")
		session := connectTestClient(t, Options{Mail: adapter})
		before := len(server.Commands())

		expect := func(uid uint32) wantMetadata {
			index := int(uid) - 101
			readState := "unread"
			if seeds[index].seen {
				readState = "read"
			}
			subject := seeds[index].subject
			return wantMetadata{
				tuple: observed[subject], subject: subject, from: seeds[index].from,
				date:       base.Add(-time.Duration(index/2) * time.Hour).Format(time.RFC3339),
				dateSource: "header", readState: readState,
			}
		}
		var unreadUIDs []uint32
		unseen := 0
		for uid := uint32(100 + count); uid > 100; uid-- {
			if !seeds[uid-101].seen {
				unreadUIDs = append(unreadUIDs, uid)
				unseen++
			}
		}
		requirePage := func(label string, items []map[string]any, want []uint32) {
			t.Helper()

			if len(items) != len(want) {
				t.Fatalf("%s returned %d entries, want %d", label, len(items), len(want))
			}
			uids := requireDescendingUIDs(t, label, items)
			for index, item := range items {
				if uids[index] != want[index] {
					t.Fatalf("%s UIDs = %v, want highest eligible %v", label, uids, want)
				}
				requireMetadataItem(t, fmt.Sprintf("%s UID %d", label, uids[index]), item, expect(uids[index]))
			}
		}

		search, results := searchItems(t, session, map[string]any{"mailbox": "INBOX", "limit": 7})
		requireTruncated(t, "dense search", search, true)
		requirePage("dense search", results, []uint32{200, 199, 198, 197, 196, 195, 194})
		equal, older := false, false
		for index := 1; index < len(results); index++ {
			previous, _ := results[index-1]["date"].(string)
			current, _ := results[index]["date"].(string)
			equal = equal || current == previous
			older = older || current > previous
		}
		if !equal || !older {
			t.Fatal("dense page does not witness UID order over equal and reversed header dates")
		}

		unread, unreadResults := searchItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": true, "limit": 5})
		requireTruncated(t, "unread search", unread, true)
		requirePage("unread search", unreadResults, unreadUIDs[:5])

		digest, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "limit": 4})
		requireTruncated(t, "unread digest", digest, true)
		requirePage("unread digest", candidates, unreadUIDs[:4])
		requireCounter(t, "unread digest", digest, "totalMessages", count)
		requireCounter(t, "unread digest", digest, "unseenCount", unseen)
		if unseen == len(candidates) {
			t.Fatal("digest counters coincide with the returned sample")
		}

		mixed, mixedCandidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": false, "limit": 3})
		requireTruncated(t, "mixed digest", mixed, true)
		requirePage("mixed digest", mixedCandidates, []uint32{200, 199, 198})

		// Reading metadata never changes Seen: the same unread page repeats.
		_, repeated := searchItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": true, "limit": 5})
		requirePage("repeated unread search", repeated, unreadUIDs[:5])
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		// A complete empty unread result is distinct from an incomplete one.
		allRead := []metadataSeed{
			{from: "Read <read@croton.test>", date: "Fri, 06 Jan 2006 09:00:00 +0000", subject: "Synthetic all read one", seen: true},
			{from: "Read <read@croton.test>", date: "Fri, 06 Jan 2006 09:00:00 +0000", subject: "Synthetic all read two", seen: true},
		}
		readServer, readAdapter := startIntegrationAdapterWithOptions(t, metadataFixture(allRead, nil))
		readSession := connectTestClient(t, Options{Mail: readAdapter})
		readBefore := len(readServer.Commands())

		empty, emptyResults := searchItems(t, readSession, map[string]any{"mailbox": "INBOX", "unreadOnly": true})
		requireTruncated(t, "complete empty search", empty, false)
		emptyDigest, emptyCandidates := digestItems(t, readSession, map[string]any{"mailbox": "INBOX"})
		requireTruncated(t, "complete empty digest", emptyDigest, false)
		if len(emptyResults) != 0 || len(emptyCandidates) != 0 {
			t.Fatalf("all-read unread queries returned %v and %v", emptyResults, emptyCandidates)
		}
		requireCounter(t, "complete empty digest", emptyDigest, "totalMessages", 2)
		requireCounter(t, "complete empty digest", emptyDigest, "unseenCount", 0)

		_, everything := searchItems(t, readSession, map[string]any{"mailbox": "INBOX"})
		if len(everything) != len(allRead) {
			t.Fatalf("all-read search returned %d entries, want %d", len(everything), len(allRead))
		}
		for _, item := range everything {
			if item["readState"] != "read" {
				t.Fatalf("all-read entry readState = %v, want read", item["readState"])
			}
		}
		requireMetadataOnlyWire(t, readServer, readServer.Commands()[readBefore:])

		description := ""
		listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		for _, tool := range listed.Tools {
			if tool.Name == "select_digest_candidates" {
				description = tool.Description
			}
		}
		if !strings.Contains(description, "counters do not count examined matches") {
			t.Fatalf("digest description does not separate counters from examined matches: %q", description)
		}
	})

	t.Run("output_boundary", func(t *testing.T) {
		t.Parallel()

		// The adapter's 100000-byte aggregate cap measures a longer encoding
		// of the same entries, so a real Bridge page cannot overflow the
		// public ceiling; adapter-shaped metadata supplies the pressure.
		const count = 250
		source := make([]bridge.MessageMetadata, 0, count)
		expected := make([]wantMetadata, 0, count)
		for index := range count {
			uid := uint32(1000 - index)
			readState, wantRead := []bridge.ReadState{bridge.ReadStateRead, bridge.ReadStateUnread, bridge.ReadStateUnknown}[index%3], []string{"read", "unread", "unknown"}[index%3]
			metadata := withReadIdentity(bridge.MessageMetadata{
				ID:                fmt.Sprintf("opaque-boundary-%03d", index),
				Mailbox:           "INBOX",
				Subject:           fmt.Sprintf("Boundary subject %03d ", index) + strings.Repeat("s", 4000),
				Size:              int64(4096 + index),
				From:              fmt.Sprintf("Boundary sender %03d ", index) + strings.Repeat("f", 512-20),
				Date:              time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(index) * time.Minute).Format(time.RFC3339),
				DateSource:        bridge.DateSourceHeader,
				ReadState:         readState,
				MetadataTruncated: true,
			}, 77, uid)
			source = append(source, metadata)
			expected = append(expected, wantMetadata{
				tuple: readTuple{Mailbox: "INBOX", UIDValidity: 77, UID: uid}, subject: metadata.Subject, from: metadata.From,
				date: metadata.Date, dateSource: "header", readState: wantRead, truncated: true, size: metadata.Size,
			})
		}
		var audit bytes.Buffer
		mail := &fakeMail{status: bridge.MailboxStatus{Messages: count, Unseen: 83}, searched: source}
		session := connectTestClient(t, Options{Mail: mail, Audit: NewAuditor(&audit)})

		for _, call := range []struct {
			tool      string
			key       string
			arguments map[string]any
			limit     int
		}{
			{"search_mail", "results", map[string]any{"mailbox": "INBOX", "limit": count}, count},
			{"select_digest_candidates", "candidates", map[string]any{"mailbox": "INBOX", "limit": 50}, 50},
		} {
			result := callTool(t, session, call.tool, call.arguments)
			text := resultText(t, result)
			if len(text) > maxToolResultBytes || !json.Valid([]byte(text)) {
				t.Fatalf("%s output is %d bytes or invalid JSON", call.tool, len(text))
			}
			object := rawObject(t, result)
			requireTruncated(t, call.tool, object, true)
			items := rawItems(t, object, call.key)
			if len(items) == 0 || len(items) >= call.limit {
				t.Fatalf("%s retained %d of %d entries, want a whole-entry shrink", call.tool, len(items), call.limit)
			}
			for index, item := range items {
				requireMetadataItem(t, fmt.Sprintf("%s retained entry %d", call.tool, index), item, expected[index])
				if item["id"] != source[index].ID {
					t.Fatalf("%s retained entry %d id = %v, want %s", call.tool, index, item["id"], source[index].ID)
				}
			}
		}

		// Complete empty, incomplete empty and fallback-empty stay distinct.
		oversized := withReadIdentity(bridge.MessageMetadata{ID: "opaque-oversized", Mailbox: "INBOX", Subject: strings.Repeat("o", 2*maxToolResultBytes), ReadState: bridge.ReadStateUnread}, 77, 1)
		for _, empty := range []struct {
			name      string
			searched  []bridge.MessageMetadata
			truncated bool
		}{
			{"complete empty", nil, false},
			{"incomplete empty", nil, true},
			{"fallback empty", []bridge.MessageMetadata{oversized}, true},
		} {
			mail := &fakeMail{status: bridge.MailboxStatus{Messages: 1, Unseen: 1}, searched: empty.searched, searchTruncated: empty.name == "incomplete empty"}
			emptySession := connectTestClient(t, Options{Mail: mail})
			search, results := searchItems(t, emptySession, map[string]any{"mailbox": "INBOX"})
			digest, candidates := digestItems(t, emptySession, map[string]any{"mailbox": "INBOX"})
			if len(results) != 0 || len(candidates) != 0 {
				t.Fatalf("%s returned entries: %v %v", empty.name, results, candidates)
			}
			requireTruncated(t, empty.name+" search", search, empty.truncated)
			requireTruncated(t, empty.name+" digest", digest, empty.truncated)
		}

		events := auditLines(t, &audit)
		if len(events) != 2 {
			t.Fatalf("audit events = %d, want 2", len(events))
		}
		for _, event := range events {
			requireAllowlistedKeys(t, event)
			if event["outcome"] != "ok" || event["truncated"] != true {
				t.Fatalf("output-bounded call audit = %v, want ok and truncated", event)
			}
		}
		if strings.Contains(audit.String(), "Boundary") || strings.Contains(audit.String(), "opaque-boundary") {
			t.Fatal("audit recorded bounded metadata")
		}
	})

	t.Run("contract_and_privacy", func(t *testing.T) {
		t.Parallel()

		bodySentinel := highEntropySentinel(t, "BODY")
		fromSentinel := highEntropySentinel(t, "FROM")
		subjectSentinel := highEntropySentinel(t, "SUBJECT")
		seeds := []metadataSeed{
			{from: "Privacy " + fromSentinel + " <privacy@croton.test>", date: "Sat, 07 Jan 2006 07:07:07 +0000", subject: "Synthetic privacy " + subjectSentinel, seen: true},
			{from: "Privacy " + fromSentinel + " <privacy@croton.test>", date: "Sat, 07 Jan 2006 08:08:08 +0000", subject: "Synthetic privacy second " + subjectSentinel},
		}
		server, adapter := startIntegrationAdapterWithOptions(t, metadataFixture(seeds, []string{bodySentinel, bodySentinel}))
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

		readInputs := map[string][]string{
			"list_folders":             {},
			"search_mail":              {"before", "limit", "mailbox", "sender", "since", "subject", "unreadOnly"},
			"get_message":              {"messageId"},
			"get_thread":               {"maxMessages", "messageId"},
			"list_attachments":         {"messageId"},
			"select_digest_candidates": {"limit", "mailbox", "sinceHours", "unreadOnly"},
		}
		triageInputs := map[string][]string{
			"mark_read":    {"mailbox", "uids", "uidvalidity"},
			"mark_unread":  {"mailbox", "uids", "uidvalidity"},
			"move_mail":    {"destination", "mailbox", "uids", "uidvalidity"},
			"archive_mail": {"mailbox", "uids", "uidvalidity"},
			"trash_mail":   {"mailbox", "uids", "uidvalidity"},
		}
		optedIn := make(map[string][]string, len(readInputs)+len(triageInputs))
		for name, inputs := range readInputs {
			optedIn[name] = inputs
		}
		for name, inputs := range triageInputs {
			optedIn[name] = inputs
		}
		requireCatalog(t, session, readInputs, triageInputs)
		requireCatalog(t, connectTestClient(t, Options{Mail: adapter, Triage: adapter}), optedIn, triageInputs)

		listed, err := session.ListTools(context.Background(), &mcp.ListToolsParams{})
		if err != nil {
			t.Fatalf("list tools: %v", err)
		}
		for _, tool := range listed.Tools {
			if tool.OutputSchema != nil {
				t.Fatalf("%s gained an output schema", tool.Name)
			}
			if tool.Name != "search_mail" && tool.Name != "select_digest_candidates" {
				continue
			}
			for _, phrase := range []string{"bounded sender", "header Date with its source", "read state", "unknown values are marked unknown", "descending UID order, not chronological order", "incomplete sample"} {
				if !strings.Contains(tool.Description, phrase) {
					t.Errorf("%s description lacks %q: %q", tool.Name, phrase, tool.Description)
				}
			}
		}

		before := len(server.Commands())
		searchResult := callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})
		digestResult := callTool(t, session, "select_digest_candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false})
		for _, result := range []*mcp.CallToolResult{searchResult, digestResult} {
			raw, err := json.Marshal(result)
			if err != nil {
				t.Fatalf("marshal SDK result: %v", err)
			}
			if bytes.Contains(raw, []byte(bodySentinel)) {
				t.Fatal("synthetic body leaked into a metadata result")
			}
			if !bytes.Contains(raw, []byte(fromSentinel)) || !bytes.Contains(raw, []byte(subjectSentinel)) {
				t.Fatal("metadata result does not disclose the documented sender and subject")
			}
			for _, forbidden := range []string{"http://", "https://", "cursor", "nextPage", "continuation"} {
				if bytes.Contains(raw, []byte(forbidden)) {
					t.Fatalf("metadata result carries %q", forbidden)
				}
			}
		}
		search := rawObject(t, searchResult)
		requireClosedKeys(t, "search_mail result", search, searchMetadataResultKeys)
		digest := rawObject(t, digestResult)
		requireClosedKeys(t, "select_digest_candidates result", digest, digestMetadataResultKeys)
		for _, item := range append(rawItems(t, search, "results"), rawItems(t, digest, "candidates")...) {
			requireClosedKeys(t, "metadata entry", item, searchMetadataItemKeys)
			if item["dateSource"] != "header" || (item["readState"] != "read" && item["readState"] != "unread") {
				t.Fatalf("privacy fixture entry lost its sourced metadata: %v", item)
			}
		}
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		negatives := []struct {
			tool      string
			arguments map[string]any
		}{
			{"search_mail", map[string]any{"mailbox": "INBOX", "cursor": subjectSentinel}},
			{"search_mail", map[string]any{"mailbox": "INBOX", "readState": "unread"}},
			{"search_mail", map[string]any{"mailbox": "INBOX", "sort": "date"}},
			{"search_mail", map[string]any{"mailbox": "INBOX", "includeBody": true}},
			{"search_mail", map[string]any{"mailbox": "INBOX", "url": "https://" + fromSentinel + ".test/"}},
			{"search_mail", map[string]any{"mailbox": "INBOX", "limit": 0}},
			{"select_digest_candidates", map[string]any{"mailbox": "INBOX", "cursor": subjectSentinel}},
			{"select_digest_candidates", map[string]any{"mailbox": "INBOX", "dateSource": "header"}},
			{"select_digest_candidates", map[string]any{"mailbox": "INBOX", "sinceHours": 0}},
		}
		commandsBefore := len(server.Commands())
		for _, negative := range negatives {
			result := callTool(t, session, negative.tool, negative.arguments)
			requireToolError(t, result, errInvalidArgument)
			if text := resultText(t, result); text != `{"error":{"code":"invalid_argument"}}` {
				t.Fatalf("%s refusal is not content-free: %s", negative.tool, text)
			}
		}
		if len(server.Commands()) != commandsBefore {
			t.Fatal("refused metadata arguments reached the wire")
		}

		events := auditLines(t, &audit)
		if len(events) != 2+len(negatives) {
			t.Fatalf("audit events = %d, want %d", len(events), 2+len(negatives))
		}
		for index, event := range events {
			requireAllowlistedKeys(t, event)
			want := "ok"
			if index >= 2 {
				want = "error"
			}
			if event["outcome"] != want {
				t.Fatalf("audit event %d = %v, want outcome %s", index, event, want)
			}
		}
		for _, value := range []string{bodySentinel, fromSentinel, subjectSentinel, "croton.test", "2006-01-07", "header", "unread"} {
			if strings.Contains(audit.String(), value) {
				t.Fatalf("audit recorded metadata %q: %s", value, audit.String())
			}
		}

		contents, err := os.ReadFile("../../docs/MCP.md")
		if err != nil {
			t.Fatalf("read Mail documentation: %v", err)
		}
		documentation := strings.Join(strings.Fields(string(contents)), " ")
		for _, statement := range []string{
			"`from` is the decoded `From` header text, capped at 512 UTF-8 bytes.",
			"It is untrusted, sender-controlled text and proves no account, address or sender identity.",
			"`date` is the message's own header `Date`, normalized to RFC 3339 UTC. It is sender-controlled and is not arrival, delivery or `INTERNALDATE` time.",
			"`readState` is `read` when `FLAGS` includes `\\Seen`, `unread` when `FLAGS` lacks it, and `unknown` when no `FLAGS` item was observed. Unread is never guessed.",
			"Returning sender text and header dates is a deliberate disclosure of private mail metadata to the MCP client",
			"Entries keep descending UID order. They are never sorted by `date`",
			"A result is a bounded sample, not a complete inventory",
			"The digest `totalMessages` and `unseenCount` are mailbox STATUS counters, not the number of matches examined or returned.",
			"whole entries are removed from the end and `truncated` is set; a retained entry keeps every field. There is no continuation cursor.",
		} {
			if !strings.Contains(documentation, statement) {
				t.Errorf("Mail documentation missing search metadata statement: %q", statement)
			}
		}
	})
}

package mcpserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

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

// isolateFixtureEnvironment points every home, config, cache and temporary
// directory, and the identity and locale variables the credential helper
// inherits, at one fixture-owned root. Setenv requires a sequential parent;
// its subtests may still run in parallel and are restored together.
func isolateFixtureEnvironment(t *testing.T) string {
	t.Helper()

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve fixture root: %v", err)
	}

	for _, key := range []string{"HOME", "TMPDIR", "TMP", "TEMP", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "XDG_RUNTIME_DIR"} {
		t.Setenv(key, root)
	}
	for key, value := range map[string]string{"USER": "fixture", "LOGNAME": "fixture", "LANG": "C", "LC_ALL": "C"} {
		t.Setenv(key, value)
	}

	if home, err := os.UserHomeDir(); err != nil || home != root || os.TempDir() != root {
		t.Fatalf("fixture environment not isolated: home %q temp %q", home, os.TempDir())
	}
	return root
}

func TestSearchMetadataPublicMCP(t *testing.T) {
	isolateFixtureEnvironment(t)

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

		_, results := searchItems(t, session, map[string]any{"mailbox": "INBOX"})
		_, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": false, "sinceHours": 168})
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		for _, shape := range []struct {
			name  string
			items []map[string]any
		}{{"search_mail result", results}, {"select_digest_candidates candidate", candidates}} {
			if len(shape.items) != len(seeds) {
				t.Fatalf("%s count = %d, want %d", shape.name, len(shape.items), len(seeds))
			}
			for _, item := range shape.items {
				subject, _ := item["subject"].(string)
				expected, ok := want[subject]
				if !ok {
					t.Fatalf("%s is not a fixture message: %v", shape.name, item)
				}
				requireMetadataItem(t, shape.name+" "+subject, item, expected)
			}
		}
	})

	t.Run("unknown_and_truncated", func(t *testing.T) {
		t.Parallel()

		longSender := "Long " + strings.Repeat("sender", 100) + " <long@croton.test>"
		seeds := []metadataSeed{
			{from: longSender, subject: "Synthetic unknown long sender", seen: true},
			{date: "not a synthetic date", subject: "Synthetic unknown invalid date"},
			{from: "Known <known@croton.test>", date: "Thu, 05 Jan 2006 10:00:00 +0000", subject: "Synthetic unknown known header", seen: true},
		}
		known := map[string]wantMetadata{
			"Synthetic unknown long sender":  {from: longSender[:512], dateSource: "unknown", readState: "unknown", truncated: true},
			"Synthetic unknown invalid date": {dateSource: "unknown", readState: "unknown"},
			"Synthetic unknown known header": {from: "Known <known@croton.test>", date: "2006-01-05T10:00:00Z", dateSource: "header", readState: "unknown"},
		}
		options := metadataFixture(seeds, nil)
		options.MetadataFlags = testkit.MetadataFlagsOmitted
		server, adapter := startIntegrationAdapterWithOptions(t, options)
		observed := observeFixtureTuples(t, server, "Subject")
		session := connectTestClient(t, Options{Mail: adapter})

		_, results := searchItems(t, session, map[string]any{"mailbox": "INBOX"})
		if len(results) != len(seeds) {
			t.Fatalf("flags omitted returned %d entries, want %d", len(results), len(seeds))
		}
		for _, item := range results {
			subject, _ := item["subject"].(string)
			expected, ok := known[subject]
			if !ok {
				t.Fatalf("flags omitted returned a non-fixture entry: %v", item)
			}
			expected.tuple, expected.subject = observed[subject], subject
			requireMetadataItem(t, "flags omitted "+subject, item, expected)
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
		_, results = searchItems(t, connectTestClient(t, Options{Mail: &fakeMail{searched: inconsistent}}), map[string]any{"mailbox": "INBOX"})
		if len(results) != len(inconsistent) {
			t.Fatalf("adapter-shaped entries = %d, want %d", len(results), len(inconsistent))
		}
		for index, item := range results {
			source := inconsistent[index]
			expected := wantMetadata{tuple: readTuple{Mailbox: "INBOX", UIDValidity: 9001, UID: source.UID}, subject: source.Subject, dateSource: "unknown", readState: "unknown"}
			if source.ReadState == bridge.ReadStateUnread {
				expected.readState = "unread"
				expected.from, expected.truncated = strings.Repeat("é", 256), true
			}
			requireMetadataItem(t, "adapter-shaped "+source.Subject, item, expected)
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
		for uid := uint32(100 + count); uid > 100; uid-- {
			if !seeds[uid-101].seen {
				unreadUIDs = append(unreadUIDs, uid)
			}
		}
		requirePage := func(label string, items []map[string]any, want []uint32) {
			t.Helper()

			if len(items) != len(want) {
				t.Fatalf("%s returned %d entries, want %d", label, len(items), len(want))
			}
			for index, item := range items {
				requireMetadataItem(t, fmt.Sprintf("%s entry %d", label, index), item, expect(want[index]))
			}
		}

		search, results := searchItems(t, session, map[string]any{"mailbox": "INBOX", "limit": 7})
		requireTruncated(t, "dense search", search, true)
		requirePage("dense search", results, []uint32{200, 199, 198, 197, 196, 195, 194})

		unread, unreadResults := searchItems(t, session, map[string]any{"mailbox": "INBOX", "unreadOnly": true, "limit": 5})
		requireTruncated(t, "unread search", unread, true)
		requirePage("unread search", unreadResults, unreadUIDs[:5])

		digest, candidates := digestItems(t, session, map[string]any{"mailbox": "INBOX", "limit": 4})
		requireTruncated(t, "unread digest", digest, true)
		requirePage("unread digest", candidates, unreadUIDs[:4])
		requireCounter(t, "unread digest", digest, "totalMessages", count)
		requireCounter(t, "unread digest", digest, "unseenCount", len(unreadUIDs))
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		// A complete empty unread result is distinct from an incomplete one.
		allRead := []metadataSeed{
			{subject: "Synthetic all read one", seen: true},
			{subject: "Synthetic all read two", seen: true},
		}
		_, readAdapter := startIntegrationAdapterWithOptions(t, metadataFixture(allRead, nil))
		readSession := connectTestClient(t, Options{Mail: readAdapter})

		empty, emptyResults := searchItems(t, readSession, map[string]any{"mailbox": "INBOX", "unreadOnly": true})
		requireTruncated(t, "complete empty search", empty, false)
		emptyDigest, emptyCandidates := digestItems(t, readSession, map[string]any{"mailbox": "INBOX"})
		requireTruncated(t, "complete empty digest", emptyDigest, false)
		if len(emptyResults) != 0 || len(emptyCandidates) != 0 {
			t.Fatalf("all-read unread queries returned %v and %v", emptyResults, emptyCandidates)
		}
	})

	t.Run("output_boundary", func(t *testing.T) {
		t.Parallel()

		// Over verified TLS: fully populated adapter and public entries encode
		// to equal lengths, so a page the adapter accepts within its unchanged
		// 100000-byte aggregate cap still overflows the public ceiling once the
		// result wrapper is added. A probe measures one fixed-length page and
		// the subjects are then padded until the adapter array lands in
		// (100000-29, 100000], above the public cap for search and digest.
		const pageSize, padding = 50, 1000
		boundarySeeds := func(extra int) []metadataSeed {
			seeds := make([]metadataSeed, 0, pageSize)
			for index := range pageSize {
				pad := padding + extra/pageSize
				if index < extra%pageSize {
					pad++
				}
				seeds = append(seeds, metadataSeed{
					from:    fmt.Sprintf("Boundary %03d ", index) + strings.Repeat("f", 600) + " <boundary@croton.test>",
					date:    time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(index) * time.Hour).Format(time.RFC1123Z),
					subject: fmt.Sprintf("Synthetic boundary %03d ", index) + strings.Repeat("s", pad),
					seen:    index%2 == 0,
				})
			}
			return seeds
		}
		const searchWrapper = len(`{"results":,"truncated":true}`)

		_, probe := startIntegrationAdapterWithOptions(t, metadataFixture(boundarySeeds(0), nil))
		probeText := resultText(t, callTool(t, connectTestClient(t, Options{Mail: probe}), "search_mail", map[string]any{"mailbox": "INBOX"}))
		if !strings.HasSuffix(probeText, `],"truncated":true}`) || strings.Count(probeText, `"metadataTruncated":true`) != pageSize {
			t.Fatalf("boundary probe is not one budget-filling, fully populated page of %d", pageSize)
		}
		extra := maxToolResultBytes - 5 - (len(probeText) - searchWrapper)
		if extra < 0 || extra >= pageSize*(10000-padding) {
			t.Fatalf("boundary probe array is %d bytes; padding cannot reach the cap", len(probeText)-searchWrapper)
		}

		seeds := boundarySeeds(extra)
		server, adapter := startIntegrationAdapterWithOptions(t, metadataFixture(seeds, nil))
		observed := observeFixtureTuples(t, server, "Subject")
		tlsSession := connectTestClient(t, Options{Mail: adapter})
		before := len(server.Commands())
		for _, call := range []struct {
			tool      string
			key       string
			arguments map[string]any
		}{
			{"search_mail", "results", map[string]any{"mailbox": "INBOX"}},
			{"select_digest_candidates", "candidates", map[string]any{"mailbox": "INBOX", "unreadOnly": false, "limit": pageSize}},
		} {
			result := callTool(t, tlsSession, call.tool, call.arguments)
			if text := resultText(t, result); len(text) > maxToolResultBytes {
				t.Fatalf("%s TLS output is %d bytes", call.tool, len(text))
			}
			object := rawObject(t, result)
			requireTruncated(t, call.tool+" over TLS", object, true)
			items := rawItems(t, object, call.key)
			if len(items) == 0 || len(items) >= pageSize {
				t.Fatalf("%s over TLS retained %d of %d entries, want a whole-entry shrink", call.tool, len(items), pageSize)
			}
			for position, item := range items {
				index := pageSize - 1 - position
				readState := "unread"
				if seeds[index].seen {
					readState = "read"
				}
				requireMetadataItem(t, fmt.Sprintf("%s TLS retained entry %d", call.tool, position), item, wantMetadata{
					tuple: observed[seeds[index].subject], subject: seeds[index].subject, from: seeds[index].from[:512],
					date:       time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(index) * time.Hour).Format(time.RFC3339),
					dateSource: "header", readState: readState, truncated: true,
				})
			}
		}
		requireMetadataOnlyWire(t, server, server.Commands()[before:])

		// Adapter-shaped metadata adds pressure beyond one adapter page.
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
		session := connectTestClient(t, Options{Mail: &fakeMail{searched: source}})

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
			if len(text) > maxToolResultBytes {
				t.Fatalf("%s output is %d bytes", call.tool, len(text))
			}
			object := rawObject(t, result)
			requireTruncated(t, call.tool, object, true)
			items := rawItems(t, object, call.key)
			if len(items) == 0 || len(items) >= call.limit {
				t.Fatalf("%s retained %d of %d entries, want a whole-entry shrink", call.tool, len(items), call.limit)
			}
			for index, item := range items {
				requireMetadataItem(t, fmt.Sprintf("%s retained entry %d", call.tool, index), item, expected[index])
			}
		}

		// An entry too large to retain falls back to an empty, truncated result.
		oversized := withReadIdentity(bridge.MessageMetadata{ID: "opaque-oversized", Mailbox: "INBOX", Subject: strings.Repeat("o", 2*maxToolResultBytes), ReadState: bridge.ReadStateUnread}, 77, 1)
		fallbackSession := connectTestClient(t, Options{Mail: &fakeMail{searched: []bridge.MessageMetadata{oversized}}})
		search, results := searchItems(t, fallbackSession, map[string]any{"mailbox": "INBOX"})
		digest, candidates := digestItems(t, fallbackSession, map[string]any{"mailbox": "INBOX"})
		if len(results) != 0 || len(candidates) != 0 {
			t.Fatalf("fallback returned entries: %v %v", results, candidates)
		}
		requireTruncated(t, "fallback search", search, true)
		requireTruncated(t, "fallback digest", digest, true)
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
		_, adapter := startIntegrationAdapterWithOptions(t, metadataFixture(seeds, []string{bodySentinel, bodySentinel}))
		var audit bytes.Buffer
		session := connectTestClient(t, Options{Mail: adapter, Audit: NewAuditor(&audit)})

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
			if tool.Name == "select_digest_candidates" && !strings.Contains(tool.Description, "counters do not count examined matches") {
				t.Errorf("digest description does not separate counters from examined matches: %q", tool.Description)
			}
		}

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
		}

		events := auditLines(t, &audit)
		if len(events) != 2 {
			t.Fatalf("audit events = %d, want 2", len(events))
		}
		for _, event := range events {
			requireAllowlistedKeys(t, event)
			if event["outcome"] != "ok" {
				t.Fatalf("audit event = %v, want outcome ok", event)
			}
		}
		for _, value := range []string{bodySentinel, fromSentinel, subjectSentinel, "croton.test", "2006-01-07", "header", "unread"} {
			if strings.Contains(audit.String(), value) {
				t.Fatalf("audit recorded metadata %q: %s", value, audit.String())
			}
		}
	})
}

package testkit

import (
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

const (
	metadataSeenBody   = "From: Seen Fixture <seen@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Seen metadata\r\n\r\nSeen body.\r\n"
	metadataUnseenBody = "From: Unseen Fixture <unseen@croton.test>\r\nTo: Reader <reader@croton.test>\r\nSubject: Unseen metadata\r\n\r\nUnseen body.\r\n"
)

var (
	metadataFetchPrefix = regexp.MustCompile(`^\* (\d+) FETCH \((.*) BODY\[([^\]]*)\] \{(\d+)\}$`)
	metadataUIDItem     = regexp.MustCompile(`(?:^| )UID (\d+)`)
	metadataFlagsItem   = regexp.MustCompile(`(?:^| )FLAGS \(([^)]*)\)`)
)

// metadataFetch is one untagged FETCH response split into its items.
type metadataFetch struct {
	items   string
	section string
	literal string
}

// values returns the argument of every occurrence of one item.
func (fetch metadataFetch) values(item *regexp.Regexp) []string {
	var values []string
	for _, match := range item.FindAllStringSubmatch(fetch.items, -1) {
		values = append(values, match[1])
	}

	return values
}

func TestSearchMetadataFixture(t *testing.T) {
	t.Parallel()

	messages := []string{metadataSeenBody, metadataUnseenBody}
	seen := []bool{true, false}
	headers := map[string]string{"101": metadataHeader(metadataSeenBody), "102": metadataHeader(metadataUnseenBody)}
	wantFlags := map[string]string{"101": `\Seen`, "102": ""}

	t.Run("seen_and_unseen", func(t *testing.T) {
		t.Parallel()

		server := startMetadataFixture(t, ImplicitTLS, messages, seen, MetadataFlagsOrdinary)
		client := metadataSession(t, server, ImplicitTLS)

		batch := client.fetchMetadata(t, "m2", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
		if len(batch) != 2 {
			t.Fatalf("batched FETCH responses = %d, want 2", len(batch))
		}
		for index, fetch := range batch {
			uid := strconv.Itoa(101 + index)
			if got := fetch.values(metadataUIDItem); len(got) != 1 || got[0] != uid {
				t.Fatalf("batched FETCH UID items = %v, want exactly [%s]", got, uid)
			}

			if got := fetch.values(metadataFlagsItem); len(got) != 1 || got[0] != wantFlags[uid] {
				t.Fatalf("batched UID %s FLAGS items = %q, want exactly [%q]", uid, got, wantFlags[uid])
			}

			if fetch.section != "HEADER" || fetch.literal != headers[uid] {
				t.Fatalf("batched UID %s header bytes changed", uid)
			}
		}
	})

	t.Run("not_requested_or_omitted", func(t *testing.T) {
		t.Parallel()

		cases := []struct {
			name    string
			mode    MetadataFlagsMode
			command string
		}{
			{name: "ordinary without FLAGS", mode: MetadataFlagsOrdinary, command: "UID FETCH 101:102 (UID BODY.PEEK[HEADER])"},
			{name: "ordinary with FLAGS inside header fields", mode: MetadataFlagsOrdinary, command: "UID FETCH 101:102 (UID BODY.PEEK[HEADER.FIELDS (FLAGS)])"},
			{name: "omitted with FLAGS", mode: MetadataFlagsOmitted, command: "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])"},
		}

		for _, test := range cases {
			server := startMetadataFixture(t, ImplicitTLS, messages, seen, test.mode)
			client := metadataSession(t, server, ImplicitTLS)

			fetches := client.fetchMetadata(t, "m2", test.command)
			if len(fetches) != 2 {
				t.Fatalf("%s: FETCH responses = %d, want 2", test.name, len(fetches))
			}
			for _, fetch := range fetches {
				if strings.Contains(fetch.items, "FLAGS") {
					t.Fatalf("%s: FETCH items = %q, want no FLAGS", test.name, fetch.items)
				}
			}
		}
	})

	t.Run("duplicate_flags", func(t *testing.T) {
		t.Parallel()

		server := startMetadataFixture(t, ImplicitTLS, messages, seen, MetadataFlagsDuplicated)
		client := metadataSession(t, server, ImplicitTLS)

		fetches := client.fetchMetadata(t, "m2", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
		if len(fetches) != 2 {
			t.Fatalf("FETCH responses = %d, want 2", len(fetches))
		}
		for index, fetch := range fetches {
			uid := strconv.Itoa(101 + index)
			if got := fetch.values(metadataFlagsItem); len(got) != 2 || got[0] != wantFlags[uid] || got[1] != wantFlags[uid] {
				t.Fatalf("UID %s FLAGS items = %q, want exactly two %q items", uid, got, wantFlags[uid])
			}
		}

		// The fault is scoped to metadata reads: a whole-body read stays unchanged.
		body := client.fetchMetadata(t, "m3", "UID FETCH 101 (UID FLAGS BODY.PEEK[])")
		if len(body) != 1 || strings.Contains(body[0].items, "FLAGS") {
			t.Fatal("whole-body FETCH changed under the duplicate metadata FLAGS mode")
		}
	})

	t.Run("compatibility_and_readonly", func(t *testing.T) {
		t.Parallel()

		server := startMetadataFixture(t, StartTLS, messages, seen, MetadataFlagsDuplicated)
		client := metadataSession(t, server, StartTLS)

		before := client.searchUnseen(t, "m2")
		client.fetchMetadata(t, "m3", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
		after := client.searchUnseen(t, "m4")
		if before != "* SEARCH 102" || after != before {
			t.Fatalf("UNSEEN before %q, after %q, want unchanged \"* SEARCH 102\"", before, after)
		}

		stateful := &StatefulOptions{Mailboxes: []MailboxSeed{{Name: "INBOX", Messages: []MessageSeed{{Body: metadataSeenBody}}}}}
		labels := &LabelOptions{
			Messages: []LabelMessage{{ID: "A", Body: metadataSeenBody}},
			Views:    []LabelViewSeed{{Name: "INBOX", Role: FolderRole, Members: []Membership{{Message: "A", UID: 101}}}},
		}
		refusals := []struct {
			name    string
			options Options
		}{
			{name: "stateful with messages", options: Options{Stateful: stateful, Messages: messages}},
			{name: "stateful with seen", options: Options{Stateful: stateful, Seen: seen}},
			{name: "stateful with scenario", options: Options{Stateful: stateful, Scenario: Scenario{OmitStatusResponse: true}}},
			{name: "labels with seen", options: Options{Labels: labels, Seen: seen}},
			{name: "omitted without messages", options: Options{MetadataFlags: MetadataFlagsOmitted}},
			{name: "unknown mode", options: Options{Messages: messages, MetadataFlags: MetadataFlagsDuplicated + 1}},
		}
		for _, refusal := range refusals {
			server, err := Start(refusal.options)
			if err == nil {
				_ = server.Close()
				t.Fatalf("%s: Start succeeded, want refusal", refusal.name)
			}
		}
	})
}

func metadataHeader(body string) string {
	header, _, _ := strings.Cut(body, "\r\n\r\n")
	return header + "\r\n\r\n"
}

func startMetadataFixture(t *testing.T, mode TLSMode, messages []string, seen []bool, flags MetadataFlagsMode) *Server {
	t.Helper()

	server, err := Start(Options{Mode: mode, Messages: messages, Seen: seen, MetadataFlags: flags})
	if err != nil {
		t.Fatalf("start metadata fixture: %v", err)
	}
	t.Cleanup(func() {
		if err := server.Close(); err != nil {
			t.Errorf("close metadata fixture: %v", err)
		}
	})

	return server
}

// metadataSession opens a verified-TLS session and examines INBOX.
func metadataSession(t *testing.T, server *Server, mode TLSMode) *imapClient {
	t.Helper()

	client := labelSession(t, server, mode, "m", "")
	requireExchange(t, client, "m0", "EXAMINE INBOX", "m0 OK [READ-ONLY] EXAMINE completed")

	return client
}

// fetchMetadata sends one FETCH and returns every untagged FETCH response,
// reading each literal by its declared byte count.
func (client *imapClient) fetchMetadata(t *testing.T, tag, command string) []metadataFetch {
	t.Helper()

	if _, err := fmt.Fprintf(client.connection, "%s %s\r\n", tag, command); err != nil {
		t.Fatalf("write FETCH: %v", err)
	}

	var fetches []metadataFetch
	for {
		line := client.readLine(t)
		if line == tag+" OK FETCH completed" {
			return fetches
		}

		match := metadataFetchPrefix.FindStringSubmatch(line)
		if match == nil {
			t.Fatalf("unexpected FETCH response line %q", line)
		}

		size, err := strconv.Atoi(match[4])
		if err != nil {
			t.Fatalf("parse literal size: %v", err)
		}
		literal := make([]byte, size)
		if _, err := io.ReadFull(client.reader, literal); err != nil {
			t.Fatalf("read FETCH literal: %v", err)
		}
		if tail := client.readLine(t); tail != ")" {
			t.Fatalf("FETCH response tail = %q, want \")\"", tail)
		}

		fetches = append(fetches, metadataFetch{items: match[2], section: match[3], literal: string(literal)})
	}
}

func (client *imapClient) searchUnseen(t *testing.T, tag string) string {
	t.Helper()

	if _, err := fmt.Fprintf(client.connection, "%s UID SEARCH UNSEEN\r\n", tag); err != nil {
		t.Fatalf("write SEARCH: %v", err)
	}

	result := client.readLine(t)
	if completion := client.readLine(t); completion != tag+" OK SEARCH completed" {
		t.Fatalf("SEARCH completion = %q", completion)
	}

	return result
}

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

func (fetch metadataFetch) uids() []string {
	var uids []string
	for _, match := range metadataUIDItem.FindAllStringSubmatch(fetch.items, -1) {
		uids = append(uids, match[1])
	}

	return uids
}

func (fetch metadataFetch) flags() []string {
	var flags []string
	for _, match := range metadataFlagsItem.FindAllStringSubmatch(fetch.items, -1) {
		flags = append(flags, match[1])
	}

	return flags
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
		defer client.Close()

		for _, uid := range []string{"101", "102"} {
			fetches := client.fetchMetadata(t, "m2", "UID FETCH "+uid+" (UID FLAGS BODY.PEEK[HEADER])")
			if len(fetches) != 1 {
				t.Fatalf("UID %s FETCH responses = %d, want 1", uid, len(fetches))
			}

			fetch := fetches[0]
			if got := fetch.uids(); len(got) != 1 || got[0] != uid {
				t.Fatalf("UID %s FETCH UID items = %v, want exactly [%s]", uid, got, uid)
			}

			if got := fetch.flags(); len(got) != 1 || got[0] != wantFlags[uid] {
				t.Fatalf("UID %s FETCH FLAGS items = %q, want exactly [%q]", uid, got, wantFlags[uid])
			}

			if fetch.section != "HEADER" || fetch.literal != headers[uid] {
				t.Fatalf("UID %s FETCH header bytes changed", uid)
			}
		}

		batch := client.fetchMetadata(t, "m3", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
		if len(batch) != 2 {
			t.Fatalf("batched FETCH responses = %d, want 2", len(batch))
		}
		for index, fetch := range batch {
			uid := strconv.Itoa(101 + index)
			if got := fetch.uids(); len(got) != 1 || got[0] != uid {
				t.Fatalf("batched FETCH UID items = %v, want exactly [%s]", got, uid)
			}

			if got := fetch.flags(); len(got) != 1 || got[0] != wantFlags[uid] {
				t.Fatalf("batched UID %s FLAGS items = %q, want exactly [%q]", uid, got, wantFlags[uid])
			}

			if fetch.literal != headers[uid] {
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
			_ = client.Close()

			if len(fetches) != 2 {
				t.Fatalf("%s: FETCH responses = %d, want 2", test.name, len(fetches))
			}
			for index, fetch := range fetches {
				uid := strconv.Itoa(101 + index)
				if got := fetch.flags(); len(got) != 0 || strings.Contains(fetch.items, "FLAGS") {
					t.Fatalf("%s: UID %s FLAGS items = %q, want none", test.name, uid, got)
				}

				if got := fetch.uids(); len(got) != 1 || got[0] != uid {
					t.Fatalf("%s: UID items = %v, want exactly [%s]", test.name, got, uid)
				}

				if fetch.literal != headers[uid] {
					t.Fatalf("%s: UID %s header bytes changed", test.name, uid)
				}
			}
		}
	})

	t.Run("duplicate_flags", func(t *testing.T) {
		t.Parallel()

		server := startMetadataFixture(t, ImplicitTLS, messages, seen, MetadataFlagsDuplicated)
		client := metadataSession(t, server, ImplicitTLS)
		defer client.Close()

		fetches := client.fetchMetadata(t, "m2", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
		if len(fetches) != 2 {
			t.Fatalf("FETCH responses = %d, want 2", len(fetches))
		}
		for index, fetch := range fetches {
			uid := strconv.Itoa(101 + index)
			if got := fetch.flags(); len(got) != 2 || got[0] != wantFlags[uid] || got[1] != wantFlags[uid] {
				t.Fatalf("UID %s FLAGS items = %q, want exactly two %q items", uid, got, wantFlags[uid])
			}

			if got := fetch.uids(); len(got) != 1 || got[0] != uid {
				t.Fatalf("UID items = %v, want exactly [%s]", got, uid)
			}

			if fetch.section != "HEADER" || fetch.literal != headers[uid] {
				t.Fatalf("UID %s header bytes changed", uid)
			}
		}

		// The fault is scoped to metadata reads: a whole-body read stays unchanged.
		body := client.fetchMetadata(t, "m3", "UID FETCH 101 (UID FLAGS BODY.PEEK[])")
		if len(body) != 1 || len(body[0].flags()) != 0 || body[0].section != "" || body[0].literal != metadataSeenBody {
			t.Fatal("whole-body FETCH changed under the duplicate metadata FLAGS mode")
		}
	})

	t.Run("compatibility_and_readonly", func(t *testing.T) {
		t.Parallel()

		modes := []MetadataFlagsMode{MetadataFlagsOrdinary, MetadataFlagsOmitted, MetadataFlagsDuplicated}
		for _, tlsMode := range []TLSMode{ImplicitTLS, StartTLS} {
			for _, mode := range modes {
				server := startMetadataFixture(t, tlsMode, messages, seen, mode)
				client := metadataSession(t, server, tlsMode)

				before := client.searchUnseen(t, "m2")
				client.fetchMetadata(t, "m3", "UID FETCH 101:102 (UID FLAGS BODY.PEEK[HEADER])")
				client.fetchMetadata(t, "m4", "UID FETCH 101 (UID BODY.PEEK[HEADER])")
				after := client.searchUnseen(t, "m5")
				_ = client.Close()

				if before != "* SEARCH 102" || after != before {
					t.Fatalf("%s mode %d: UNSEEN before %q, after %q, want unchanged \"* SEARCH 102\"", tlsMode, mode, before, after)
				}

				if err := server.AssertReadOnlyCommands(); err != nil {
					t.Fatalf("%s mode %d: %v", tlsMode, mode, err)
				}

				if err := server.AssertNoInsecureAuthentication(); err != nil {
					t.Fatalf("%s mode %d: %v", tlsMode, mode, err)
				}

				transcript, err := ParseTranscript(server.Commands())
				if err != nil {
					t.Fatalf("%s mode %d: parse transcript: %v", tlsMode, mode, err)
				}
				authenticated := false
				for _, command := range transcript {
					if command.Store != "" || command.Destination != "" || !isReadOnlyCommand(command.Verb) {
						t.Fatalf("%s mode %d: transcript records mutating %s", tlsMode, mode, command.Verb)
					}

					if command.Verb != "STARTTLS" && !command.TLS {
						t.Fatalf("%s mode %d: %s recorded before TLS", tlsMode, mode, command.Verb)
					}

					authenticated = authenticated || isAuthentication(command.Verb) && command.TLS
				}
				if !authenticated {
					t.Fatalf("%s mode %d: no authentication over verified TLS", tlsMode, mode)
				}
			}
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
			{name: "labels with stateful", options: Options{Labels: labels, Stateful: stateful}},
			{name: "labels with messages", options: Options{Labels: labels, Messages: messages}},
			{name: "labels with seen", options: Options{Labels: labels, Seen: seen}},
			{name: "omitted without messages", options: Options{MetadataFlags: MetadataFlagsOmitted}},
			{name: "duplicated with stateful", options: Options{Stateful: stateful, MetadataFlags: MetadataFlagsDuplicated}},
			{name: "duplicated with labels", options: Options{Labels: labels, MetadataFlags: MetadataFlagsDuplicated}},
			{name: "unknown mode", options: Options{Messages: messages, MetadataFlags: MetadataFlagsDuplicated + 1}},
		}
		for _, refusal := range refusals {
			server, err := Start(refusal.options)
			if err == nil {
				_ = server.Close()
				t.Fatalf("%s: Start succeeded, want refusal", refusal.name)
			}
		}

		for _, accepted := range []Options{{Stateful: stateful}, {Labels: labels}} {
			server, err := Start(accepted)
			if err != nil {
				t.Fatalf("exclusive mode with ordinary metadata FLAGS refused: %v", err)
			}
			_ = server.Close()
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

// metadataSession connects over verified TLS, authenticates with synthetic
// credentials and examines INBOX.
func metadataSession(t *testing.T, server *Server, mode TLSMode) *imapClient {
	t.Helper()

	client := connectClient(t, server, mode)
	client.command(t, "m0 LOGIN fixture-user@client.test not-a-secret", "m0 OK LOGIN completed")
	if got := client.exchange(t, "m1", "EXAMINE INBOX"); got != "m1 OK [READ-ONLY] EXAMINE completed" {
		t.Fatalf("EXAMINE completion = %q", got)
	}

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

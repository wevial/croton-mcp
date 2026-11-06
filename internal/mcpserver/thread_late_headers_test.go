package mcpserver

import (
	"fmt"
	"net"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/wevial/croton-mcp/bridge"
	"github.com/wevial/croton-mcp/internal/testkit"
)

const threadLateBodySentinel = "croton-thread-late-body-sentinel"

// threadLateFiller returns 100 ordinary fields, filling the default generic
// header count so any field after them is beyond the count bound.
func threadLateFiller() string {
	var filler strings.Builder
	for index := range 100 {
		fmt.Fprintf(&filler, "X-Filler-%03d: %d\r\n", index, index)
	}

	return filler.String()
}

// threadLateMessages holds a root and reply whose threading fields follow 100
// generic fields, an early-field equivalent pair whose threading fields lead,
// and a message with no Subject at all. UIDs run 101 to 105.
func threadLateMessages() []string {
	filler := threadLateFiller()
	body := "\r\n" + threadLateBodySentinel + "\r\n"

	return []string{
		"From: Root <root@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\n" + filler +
			"Subject: Late planning\r\nMessage-ID: <late-root@croton.test>\r\n" + body,
		"From: Reply <reply@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:01 +0000\r\n" + filler +
			"SUBJECT: Re: Late\r\n planning\r\nmessage-id: <late-reply@croton.test>\r\n" +
			"In-Reply-To: <late-root@croton.test>\r\nReferences:\r\n <late-root@croton.test>\r\n" + body,
		"Subject: Early planning\r\nMessage-ID: <early-root@croton.test>\r\n" +
			"From: Root <root@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:02 +0000\r\n" + filler + body,
		"Subject: Re: Early planning\r\nMessage-ID: <early-reply@croton.test>\r\n" +
			"In-Reply-To: <early-root@croton.test>\r\nReferences: <early-root@croton.test>\r\n" +
			"From: Reply <reply@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:03 +0000\r\n" + filler + body,
		"From: Plain <plain@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:04 +0000\r\n" + filler +
			"Message-ID: <absent-subject@croton.test>\r\n" + body,
	}
}

func startThreadLateAdapter(t *testing.T, mode testkit.TLSMode) (*testkit.Server, *bridge.Adapter) {
	t.Helper()

	server, err := testkit.Start(testkit.Options{Mode: mode, Messages: threadLateMessages()})
	if err != nil {
		t.Fatalf("start fake server: %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })

	host, portText, err := net.SplitHostPort(server.Addr())
	if err != nil {
		t.Fatalf("split fake address: %v", err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		t.Fatalf("parse fake port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	tlsMode := bridge.TLSModeStartTLS
	if mode == testkit.ImplicitTLS {
		tlsMode = bridge.TLSModeImplicit
	}
	adapter, err := bridge.NewAdapter(bridge.Config{IMAP: bridge.IMAPConfig{
		Host:              host,
		Port:              port,
		TLSMode:           tlsMode,
		CredentialCommand: []string{executable, "-test.run=TestMCPCredentialHelperProcess", "--", "valid"},
		TLS:               bridge.TLSConfig{SPKISHA256: server.SPKISHA256()},
		ConnectTimeoutMs:  2000,
	}})
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	t.Cleanup(func() { _ = adapter.Close() })

	return server, adapter
}

func TestThreadLateHeaders(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	for _, mode := range []struct {
		name string
		mode testkit.TLSMode
	}{
		{name: "starttls_late_fields", mode: testkit.StartTLS},
		{name: "implicit_tls_late_fields", mode: testkit.ImplicitTLS},
	} {
		t.Run(mode.name, func(t *testing.T) {
			server, adapter := startThreadLateAdapter(t, mode.mode)
			mail := &countingHeaderMail{Adapter: adapter}
			session := connectTestClient(t, Options{Mail: mail})

			searched := rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results")
			if len(searched) != 5 {
				t.Fatalf("fixture search = %v, want five messages", searched)
			}
			tuples := make(map[string]readTuple)
			byUID := make(map[uint32]string)
			for _, item := range searched {
				id, _ := item["id"].(string)
				tuple := flatTuple(t, item)
				tuples[id] = tuple
				byUID[tuple.UID] = id
			}
			for uid := uint32(101); uid <= 105; uid++ {
				if byUID[uid] == "" {
					t.Fatalf("fixture UIDs = %v", byUID)
				}
			}

			type wantNode struct {
				key, messageID, parent, depth string
			}
			for _, test := range []struct {
				name        string
				target      string
				nodes       []wantNode
				searches    int
				selectedUID []uint32
			}{
				{
					name:   "late",
					target: byUID[102],
					nodes: []wantNode{
						{byUID[101], "<late-root@croton.test>", "", "0"},
						{byUID[102], "<late-reply@croton.test>", byUID[101], "1"},
					},
					searches:    1,
					selectedUID: []uint32{101, 102},
				},
				{
					name:   "early",
					target: byUID[104],
					nodes: []wantNode{
						{byUID[103], "<early-root@croton.test>", "", "0"},
						{byUID[104], "<early-reply@croton.test>", byUID[103], "1"},
					},
					searches:    1,
					selectedUID: []uint32{103, 104},
				},
				{
					name:        "absent_subject",
					target:      byUID[105],
					nodes:       []wantNode{{byUID[105], "<absent-subject@croton.test>", "", "0"}},
					searches:    0,
					selectedUID: []uint32{105},
				},
			} {
				mail.reset()
				before := len(server.Commands())

				result := callTool(t, session, "get_thread", map[string]any{"messageId": test.target})
				if strings.Contains(resultText(t, result), threadLateBodySentinel) {
					t.Fatalf("%s: thread result carried body text", test.name)
				}
				thread := rawObject(t, result)
				if thread["id"] != test.target || thread["mailbox"] != "INBOX" {
					t.Fatalf("%s: thread context = %v", test.name, thread)
				}
				if truncated, _ := thread["truncated"].(bool); !truncated {
					t.Fatalf("%s: generic header count omission not reported truncated: %v", test.name, thread)
				}

				nodes := rawItems(t, thread, "nodes")
				if len(nodes) != len(test.nodes) {
					t.Fatalf("%s: nodes = %v, want %d", test.name, nodes, len(test.nodes))
				}
				for index, node := range nodes {
					want := test.nodes[index]
					parent, _ := node["parentKey"].(string)
					if node["key"] != want.key || node["messageId"] != want.messageID || parent != want.parent || fmt.Sprint(node["depth"]) != want.depth {
						t.Fatalf("%s: node %d = %v, want %+v", test.name, index, node, want)
					}
					if got := flatTuple(t, node); got != tuples[want.key] {
						t.Fatalf("%s: node %d tuple = %+v, want its own %+v", test.name, index, got, tuples[want.key])
					}
					if !reflect.DeepEqual(node["truncation"], map[string]any{"headerCount": true}) {
						t.Fatalf("%s: node %d truncation = %v, want only headerCount", test.name, index, node["truncation"])
					}
				}

				counts := [4]int{mail.headerCalls, mail.metaCalls, mail.bodyCalls, mail.searchCalls}
				if want := [4]int{len(test.selectedUID), 0, 0, test.searches}; counts != want {
					t.Fatalf("%s: operations header/metadata/body/search = %v, want %v", test.name, counts, want)
				}
				requireHeaderOnlyThreadTranscript(t, server.Commands()[before:], test.selectedUID)
			}

			requireReadOnlyTranscript(t, server)
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
		})
	}
}

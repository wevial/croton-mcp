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

// threadLateMessages holds a root and reply whose threading fields follow 100
// generic fields, and a message with no Subject at all. UIDs run 101 to 103.
func threadLateMessages() []string {
	var fields strings.Builder
	for index := range 100 {
		fmt.Fprintf(&fields, "X-Filler-%03d: %d\r\n", index, index)
	}
	filler := fields.String()
	body := "\r\nbody\r\n"

	return []string{
		"From: Root <root@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:00 +0000\r\n" + filler +
			"Subject: Late planning\r\nMessage-ID: <late-root@croton.test>\r\n" + body,
		"From: Reply <reply@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:01 +0000\r\n" + filler +
			"SUBJECT: Re: Late\r\n planning\r\nmessage-id: <late-reply@croton.test>\r\n" +
			"In-Reply-To: <late-root@croton.test>\r\nReferences:\r\n <late-root@croton.test>\r\n" + body,
		"From: Plain <plain@croton.test>\r\nDate: Thu, 01 Jan 2026 00:00:04 +0000\r\n" + filler +
			"Message-ID: <absent-subject@croton.test>\r\n" + body,
	}
}

func startThreadLateAdapter(t *testing.T, mode testkit.TLSMode) *bridge.Adapter {
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

	return adapter
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
			adapter := startThreadLateAdapter(t, mode.mode)
			mail := &countingHeaderMail{Adapter: adapter}
			session := connectTestClient(t, Options{Mail: mail})

			byUID := make(map[uint32]string)
			for _, item := range rawItems(t, rawObject(t, callTool(t, session, "search_mail", map[string]any{"mailbox": "INBOX"})), "results") {
				id, _ := item["id"].(string)
				byUID[flatTuple(t, item).UID] = id
			}

			type wantNode struct {
				key, messageID, parent, depth string
			}
			for _, test := range []struct {
				name     string
				target   string
				nodes    []wantNode
				searches int
			}{
				{
					name:   "late",
					target: byUID[102],
					nodes: []wantNode{
						{byUID[101], "<late-root@croton.test>", "", "0"},
						{byUID[102], "<late-reply@croton.test>", byUID[101], "1"},
					},
					searches: 1,
				},
				{
					name:     "absent_subject",
					target:   byUID[103],
					nodes:    []wantNode{{byUID[103], "<absent-subject@croton.test>", "", "0"}},
					searches: 0,
				},
			} {
				mail.reset()

				nodes := rawItems(t, rawObject(t, callTool(t, session, "get_thread", map[string]any{"messageId": test.target})), "nodes")
				if len(nodes) != len(test.nodes) {
					t.Fatalf("%s: nodes = %v, want %d", test.name, nodes, len(test.nodes))
				}
				for index, node := range nodes {
					want := test.nodes[index]
					parent, _ := node["parentKey"].(string)
					if node["key"] != want.key || node["messageId"] != want.messageID || parent != want.parent || fmt.Sprint(node["depth"]) != want.depth {
						t.Fatalf("%s: node %d = %v, want %+v", test.name, index, node, want)
					}
					if !reflect.DeepEqual(node["truncation"], map[string]any{"headerCount": true}) {
						t.Fatalf("%s: node %d truncation = %v, want only headerCount", test.name, index, node["truncation"])
					}
				}
				if mail.searchCalls != test.searches {
					t.Fatalf("%s: searches = %d, want %d", test.name, mail.searchCalls, test.searches)
				}
			}
		})
	}
}

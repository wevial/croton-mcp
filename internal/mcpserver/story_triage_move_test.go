// Draft frozen-contract Move witness with synthetic TLS race control.
package mcpserver_test

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

// TestStoryTriageMove exercises the compiled CLI/config/CommandTransport path
// provided by triageStart against synthetic mutable TLS IMAP state. It neither
// uses an in-memory MCP server nor reaches a real account. Missing config/tool
// support is an assertion RED on the pinned baseline, never a skip.
func TestStoryTriageMove(t *testing.T) {
	start := func(t *testing.T, options *testkit.StatefulOptions) *triageHarness {
		t.Helper()
		h := triageStart(t, options, map[string]any{"enabled": true})
		h.requireTools(t, "move_mail")
		return h
	}

	for _, setting := range []struct {
		name  string
		value any
	}{
		{"default-off", nil},
		{"explicit-disabled", map[string]any{"enabled": false}},
	} {
		t.Run(setting.name, func(t *testing.T) {
			h := triageStart(t, triageSeeds(), setting.value)
			before := h.fixture.Snapshot()
			catalog, err := h.session.ListTools(h.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			triageCatalog(t, catalog)
			mutation := map[string]bool{"mark_read": true, "mark_unread": true, "move_mail": true, "archive_mail": true, "trash_mail": true}
			for _, tool := range catalog.Tools {
				if mutation[tool.Name] {
					t.Errorf("default-off catalog exposes mutation tool %s", tool.Name)
				}
			}
			result, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: "move_mail", Arguments: triageMoveArgs([]uint32{101}, "Folders/Existing")})
			if err == nil && (result == nil || !result.IsError) {
				t.Error("disabled move_mail accepted a public call")
			}
			triageNoWrites(t, h, before)
		})
	}

	t.Run("explicit-opt-in", func(t *testing.T) {
		h := start(t, triageSeeds())
		before := h.fixture.Snapshot()
		catalog, err := h.session.ListTools(h.ctx, nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, tool := range catalog.Tools {
			if tool.Name != "move_mail" {
				continue
			}
			schema, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			if err := json.Unmarshal(schema, &root); err != nil {
				t.Fatal(err)
			}
			var properties map[string]json.RawMessage
			if err := json.Unmarshal(root["properties"], &properties); err != nil {
				t.Fatal("move_mail has no object properties")
			}
			for _, key := range []string{"mailbox", "uidvalidity", "uids", "destination"} {
				if _, ok := properties[key]; !ok {
					t.Errorf("move_mail schema missing %s", key)
				}
			}
			if _, ok := properties["approved"]; ok {
				t.Error("agent-supplied approved boolean must not manufacture consent")
			}
		}
		triageNoWrites(t, h, before)
	})

	t.Run("displayed-snapshot-first-three-after-changed-search", func(t *testing.T) {
		h := start(t, triageSeeds())
		original := triageDisplayed(t, h, false)
		if len(original) != 10 {
			t.Fatal("ten-message display missing")
		}
		selected := append([]uint32(nil), original[:3]...)
		later := triageDisplayed(t, h, true)
		if reflect.DeepEqual(original, later) {
			t.Fatal("later display did not change")
		}
		before := h.fixture.Snapshot()
		triageResults(t, h.call(t, "move_mail", triageMoveArgs(selected, "Folders/Existing")), selected, []string{"applied", "applied", "applied"}, "")
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", selected)
		triageMoveWire(t, h, selected, "Folders/Existing")
	})

	t.Run("selected-only-input-order", func(t *testing.T) {
		h := start(t, triageSeeds())
		before := h.fixture.Snapshot()
		uids := []uint32{103, 101, 102}
		result := h.call(t, "move_mail", triageMoveArgs(uids, "Folders/Existing"))
		triageResults(t, result, uids, []string{"applied", "applied", "applied"}, "")
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", uids)
		triageMoveWire(t, h, uids, "Folders/Existing")
	})

	t.Run("destination-disappeared-after-validation-definitive-refusal", func(t *testing.T) {
		var proxy *triageRaceProxy
		h := triageStartRoute(t, triageSeeds(), map[string]any{"enabled": true}, "", func(t *testing.T, f *testkit.Server) (string, string) {
			proxy = triageRaceStart(t, f, "Folders/Existing")
			return proxy.addr, proxy.pin
		})
		h.requireTools(t, "move_mail")
		before := h.fixture.Snapshot()
		triageResults(t, h.call(t, "move_mail", triageMoveArgs([]uint32{101}, "Folders/Existing")), []uint32{101}, []string{"refused"}, "")
		select {
		case err := <-proxy.removed:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("disappearance observer did not trigger")
		}
		var expected []testkit.MailboxState
		for _, b := range before {
			if b.Name != "Folders/Existing" {
				expected = append(expected, b)
			}
		}
		triageNoRaceMutation(t, h, expected)
		triageMoveWire(t, h, []uint32{101}, "Folders/Existing")
	})

	t.Run("missing-uid-mixed", func(t *testing.T) {
		h := start(t, triageSeeds())
		before := h.fixture.Snapshot()
		uids := []uint32{101, 999, 102, 103}
		result := h.call(t, "move_mail", triageMoveArgs(uids, "Folders/Existing"))
		triageResults(t, result, uids, []string{"applied", "refused", "applied", "applied"}, "")
		moved := []uint32{101, 102, 103}
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", moved)
		triageMoveWire(t, h, moved, "Folders/Existing")
	})

	for _, destination := range []struct{ name, mailbox string }{
		{"same-source", "INBOX"},
		{"same-source-case-folded", "inbox"},
		{"destination-missing", "Folders/Missing"},
		{"destination-nonselectable", "Folders/NoSelect"},
		{"destination-label", "Labels/Tag"},
		{"destination-all-mail", "All Mail"},
		{"destination-starred", "Starred"},
		{"destination-ambiguous-prefix", "Folders/"},
	} {
		t.Run(destination.name, func(t *testing.T) {
			h := start(t, triageSeeds())
			triageMoveRefused(t, h, triageMoveArgs([]uint32{103, 101, 102}, destination.mailbox), []uint32{103, 101, 102})
		})
	}

	t.Run("destination-nonexistent-attribute", func(t *testing.T) {
		options := triageSeeds()
		for i := range options.Mailboxes {
			if options.Mailboxes[i].Name == "Folders/Existing" {
				options.Mailboxes[i].Attributes = []string{`\NonExistent`}
			}
		}
		h := start(t, options)
		triageMoveRefused(t, h, triageMoveArgs([]uint32{101}, "Folders/Existing"), []uint32{101})
	})

	t.Run("missing-move-capability", func(t *testing.T) {
		options := triageSeeds()
		options.Move = false
		h := start(t, options)
		triageMoveRefused(t, h, triageMoveArgs([]uint32{101, 102}, "Folders/Existing"), []uint32{101, 102})
	})

	t.Run("stale-generation", func(t *testing.T) {
		h := start(t, triageSeeds())
		_ = triageDisplayed(t, h, false)
		if err := h.fixture.ReplaceUIDValidity("INBOX", 7999); err != nil {
			t.Fatal(err)
		}
		triageMoveRefused(t, h, triageMoveArgs([]uint32{101, 102}, "Folders/Existing"), []uint32{101, 102})
	})

	t.Run("wrong-source-identity", func(t *testing.T) {
		h := start(t, triageSeeds())
		args := triageMoveArgs([]uint32{101, 102}, "Folders/Existing")
		args["mailbox"] = "Archivage"
		triageMoveRefused(t, h, args, []uint32{101, 102})
	})

	t.Run("wrong-source-generation-with-colliding-uid", func(t *testing.T) {
		options := triageSeeds()
		for i := range options.Mailboxes {
			if options.Mailboxes[i].Name == "Archivage" {
				options.Mailboxes[i].UIDValidity = 8001
				options.Mailboxes[i].Messages = []testkit.MessageSeed{{UID: 101, Body: "Subject: synthetic collision\r\n\r\nwrong mailbox\r\n"}}
			}
		}
		h := start(t, options)
		args := triageMoveArgs([]uint32{101}, "Folders/Existing")
		args["mailbox"] = "Archivage"
		triageMoveRefused(t, h, args, []uint32{101})
	})

	t.Run("fresh-list-every-move", func(t *testing.T) {
		h := start(t, triageSeeds())
		before := h.fixture.Snapshot()
		for _, uid := range []uint32{101, 102} {
			result := h.call(t, "move_mail", triageMoveArgs([]uint32{uid}, "Folders/Existing"))
			triageResults(t, result, []uint32{uid}, []string{"applied"}, "")
		}
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", []uint32{101, 102})
		triageMoveWire(t, h, []uint32{101, 102}, "Folders/Existing")
	})

	for _, change := range []struct {
		name   string
		remove bool
	}{
		{"destination-disappeared-between-calls", true},
		{"destination-became-nonselectable-between-calls", false},
	} {
		t.Run(change.name, func(t *testing.T) {
			h := start(t, triageSeeds())
			before := h.fixture.Snapshot()
			result := h.call(t, "move_mail", triageMoveArgs([]uint32{101}, "Folders/Existing"))
			triageResults(t, result, []uint32{101}, []string{"applied"}, "")
			triageMoved(t, before, h.fixture.Snapshot(), "INBOX", "Folders/Existing", []uint32{101})
			triageMoveWire(t, h, []uint32{101}, "Folders/Existing")
			var err error
			if change.remove {
				err = h.fixture.RemoveMailbox("Folders/Existing")
			} else {
				err = h.fixture.SetMailboxAttributes("Folders/Existing", `\Noselect`)
			}
			if err != nil {
				t.Fatal(err)
			}
			settled := h.fixture.Snapshot()
			result = h.call(t, "move_mail", triageMoveArgs([]uint32{102}, "Folders/Existing"))
			triageResults(t, result, []uint32{102}, []string{"refused"}, "")
			if !reflect.DeepEqual(settled, h.fixture.Snapshot()) {
				t.Error("refused second call changed settled mailbox state")
			}
			triageMoveWire(t, h, []uint32{101}, "Folders/Existing")
		})
	}

	t.Run("invalid-destination-encoding", func(t *testing.T) {
		// Seed an exact existing wire name, so refusing this malformed modified
		// UTF-7 value cannot pass merely because its destination is missing.
		options := triageSeeds()
		options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: "Folders/&A-", UIDValidity: 8100})
		h := start(t, options)
		triageMoveRefused(t, h, triageMoveArgs([]uint32{101}, "Folders/&A-"), []uint32{101})
	})

	for _, input := range []struct{ name, destination string }{
		{"destination-empty", ""},
		{"destination-control-nul", "Folders/Existing\x00"},
		{"destination-control-crlf", "Folders/Existing\r\nA9 EXPUNGE"},
		{"destination-wildcard-star", "Folders/*"},
		{"destination-wildcard-percent", "Folders/%"},
	} {
		t.Run(input.name, func(t *testing.T) {
			h := start(t, triageSeeds())
			triageMoveRefused(t, h, triageMoveArgs([]uint32{101}, input.destination), []uint32{101})
		})
	}

	t.Run("root-system-destinations-and-unknown-root-refusal", func(t *testing.T) {
		for _, destination := range []string{"INBOX", "Sent", "Drafts", "Spam", "OpaqueSent", "OpaqueDrafts", "OpaqueJunk", "Archivage", "Corbeille", "UnknownRoot", "Archive", "Trash"} {
			t.Run(destination, func(t *testing.T) {
				options := triageSeeds()
				if destination != "INBOX" && destination != "Archivage" && destination != "Corbeille" {
					attrs := []string{}
					switch destination {
					case "Sent", "OpaqueSent":
						attrs = []string{`\Sent`}
					case "Drafts", "OpaqueDrafts":
						attrs = []string{`\Drafts`}
					case "Spam", "OpaqueJunk":
						attrs = []string{`\Junk`}
					}
					options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: destination, Attributes: attrs})
				}
				h := start(t, options)
				before := h.fixture.Snapshot()
				if destination == "UnknownRoot" || destination == "Archive" || destination == "Trash" {
					triageMoveRefused(t, h, triageMoveArgs([]uint32{101}, destination), []uint32{101})
					return
				}
				source := "INBOX"
				uid := uint32(101)
				args := triageMoveArgs([]uint32{uid}, destination)
				if destination == "INBOX" {
					source = "Folders/Existing"
					uid = 501
					args = map[string]any{"mailbox": source, "uidvalidity": uint32(7002), "uids": []uint32{uid}, "destination": destination}
				}
				triageResults(t, h.call(t, "move_mail", args), []uint32{uid}, []string{"applied"}, "")
				triageMoved(t, before, h.fixture.Snapshot(), source, destination, []uint32{uid})
				moves := triageDispatches(t, h, "UID MOVE")
				if len(moves) != 1 || moves[0].Set != fmt.Sprint(uid) || moves[0].Destination != destination {
					t.Fatal("root destination dispatch not exact")
				}
			})
		}
	})
	t.Run("root-system-protocol-attributes-required", func(t *testing.T) {
		for _, candidate := range []struct {
			name       string
			attributes []string
		}{
			{"Sent", nil}, {"Drafts", nil}, {"Spam", nil},
			{"AllRoot", []string{`\All`}}, {"FlaggedRoot", []string{`\Flagged`}},
		} {
			t.Run(candidate.name, func(t *testing.T) {
				options := triageSeeds()
				options.Mailboxes = append(options.Mailboxes, testkit.MailboxSeed{Name: candidate.name, Attributes: candidate.attributes})
				h := start(t, options)
				triageMoveRefused(t, h, triageMoveArgs([]uint32{101}, candidate.name), []uint32{101})
			})
		}
	})
	t.Run("bad-inputs", func(t *testing.T) {
		triageBadInputs(t, "move_mail", "Folders/Existing")
	})

	for _, fault := range []struct {
		name     string
		boundary testkit.FaultBoundary
		action   testkit.FaultAction
	}{
		{"lost-before-application", testkit.BeforeApplication, testkit.DropConnection},
		{"lost-after-application", testkit.AfterApplication, testkit.DropConnection},
		{"timeout-before-application", testkit.BeforeApplication, testkit.HoldResponse},
		{"timeout-after-application", testkit.AfterApplication, testkit.HoldResponse},
	} {
		t.Run(fault.name, func(t *testing.T) {
			h := start(t, triageSeeds())
			triageFault(t, h, "move_mail", "Folders/Existing", "UID MOVE", fault.boundary, fault.action)
			// The common witness proves [applied,refused,unknown,not_attempted],
			// the exact 102 fault boundary, settled state, timeout and no replay.
			triageMoveWire(t, h, []uint32{101, 102}, "Folders/Existing")
		})
	}
}

func triageMoveArgs(uids []uint32, destination string) map[string]any {
	args := triageArgs(uids)
	args["destination"] = destination
	return args
}

func triageMoveRefused(t *testing.T, h *triageHarness, args map[string]any, uids []uint32) {
	t.Helper()
	before := h.fixture.Snapshot()
	outcomes := make([]string, len(uids))
	for i := range outcomes {
		outcomes[i] = "refused"
	}
	destination, _ := args["destination"].(string)
	malformed := destination == "" || strings.ContainsAny(destination, "\x00\r\n*%") || destination == "Folders/&A-"
	stale := args["mailbox"] != "INBOX"
	r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: "move_mail", Arguments: args})
	if (err != nil || r == nil || r.IsError) && !(malformed || stale || h.fixture.Snapshot()[0].UIDValidity != 7001) {
		t.Fatal("semantic unsupported/same-source request must return per-ID refusal")
	}
	if err == nil && r != nil && !r.IsError {
		code := ""
		if !malformed && !stale && !strings.EqualFold(destination, "INBOX") && h.fixture.Snapshot()[0].UIDValidity == 7001 {
			code = "unsupported"
		}
		triageResults(t, r, uids, outcomes, code)
	}
	triageNoWrites(t, h, before)
}

// triageMoveWire independently tightens the common allowlist to native MOVE
// only, exact single UIDs in caller order, exact target, TLS, and a new ordinary
// LIST on the dispatch connection before EVERY MOVE (including faulted ones).
func triageMoveWire(t *testing.T, h *triageHarness, uids []uint32, destination string) {
	t.Helper()
	commands := triageCommands(t, h)
	moves := triageDispatches(t, h, "UID MOVE")
	if len(moves) != len(uids) {
		t.Fatalf("UID MOVE dispatch count=%d, want %d", len(moves), len(uids))
	}
	for _, command := range commands {
		switch command.Verb {
		case "UID STORE", "STORE", "MOVE", "COPY", "UID COPY", "EXPUNGE", "UID EXPUNGE", "DELETE", "APPEND", "CREATE":
			t.Errorf("forbidden movement fallback verb %s", command.Verb)
		case "LIST":
			if strings.Contains(strings.ToUpper(command.Arguments), "RETURN") {
				t.Error("ordinary LIST must not request extended RETURN options")
			}
		}
	}
	previousMove := 0
	for i, move := range moves {
		if !move.TLS {
			t.Error("MOVE was dispatched outside TLS")
		}
		if move.Set != fmt.Sprint(uids[i]) || len(move.Ranges) != 1 || move.Ranges[0].Start != uids[i] || move.Ranges[0].Stop != uids[i] {
			t.Errorf("dispatch %d is not the exact single requested UID %d", i, uids[i])
		}
		if move.Destination != destination {
			t.Errorf("dispatch %d target=%q, want %q", i, move.Destination, destination)
		}
		freshList := false
		selectedSource := false
		for _, command := range commands {
			if command.ConnectionID != move.ConnectionID || command.Sequence >= move.Sequence {
				continue
			}
			if command.Verb == "LIST" && command.Sequence > previousMove && command.TLS {
				freshList = true
			}
			if command.Verb == "SELECT" {
				selectedSource = strings.EqualFold(command.Arguments, "INBOX") || strings.EqualFold(command.Arguments, `"INBOX"`)
			}
		}
		if !freshList {
			t.Errorf("dispatch %d lacks a fresh TLS LIST on its own connection", i)
		}
		if !selectedSource {
			t.Errorf("dispatch %d lacks source INBOX SELECT on its own connection", i)
		}
		previousMove = move.Sequence
	}
}

type triageRaceProxy struct {
	addr, pin string
	trust     *tls.Config
	removed   chan error
}

// Remove the destination only after the CLIENT has dispatched its UID MOVE,
// and before the fixture applies it. Earlier same-connection LIST validation
// is forwarded unchanged. Definitive NO must mean refused, never applied/unknown.
func triageRaceStart(t *testing.T, fixture *testkit.Server, destination string) *triageRaceProxy {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, BasicConstraintsValid: true, IsCA: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	pin := sha256.Sum256(leaf.RawSubjectPublicKeyInfo)
	listener, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12})
	if err != nil {
		t.Fatal(err)
	}
	p := &triageRaceProxy{addr: listener.Addr().String(), pin: fmt.Sprintf("%x", pin), trust: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}, removed: make(chan error, 1)}
	var once sync.Once
	var mu sync.Mutex
	var wg sync.WaitGroup
	connections := map[net.Conn]bool{}
	track := func(c net.Conn) { mu.Lock(); connections[c] = true; mu.Unlock() }
	untrack := func(c net.Conn) { mu.Lock(); delete(connections, c); mu.Unlock() }
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			downstream, err := listener.Accept()
			if err != nil {
				return
			}
			track(downstream)
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer downstream.Close()
				defer untrack(downstream)
				upstream, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", fixture.Addr(), fixture.ClientTLSConfig())
				if err != nil {
					return
				}
				track(upstream)
				defer upstream.Close()
				defer untrack(upstream)
				_ = downstream.SetDeadline(time.Now().Add(8 * time.Second))
				_ = upstream.SetDeadline(time.Now().Add(8 * time.Second))
				copied := make(chan struct{})
				go func() { defer close(copied); _, _ = io.Copy(downstream, upstream); _ = downstream.Close() }()
				reader := bufio.NewReader(downstream)
				for {
					line, err := reader.ReadString('\n')
					if err != nil {
						break
					}
					parsed, parseErr := testkit.ParseTranscriptCommand(testkit.Command{Sequence: 1, ConnectionID: 1, TLS: true, Raw: strings.TrimSpace(line)})
					if parseErr == nil && parsed.Verb == "UID MOVE" {
						once.Do(func() { p.removed <- fixture.RemoveMailbox(destination) })
					}
					if _, err = io.WriteString(upstream, line); err != nil {
						break
					}
				}
				_ = upstream.Close()
				_ = downstream.Close()
				<-copied
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		mu.Lock()
		for c := range connections {
			_ = c.Close()
		}
		mu.Unlock()
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("race proxy did not terminate")
		}
	})
	return p
}

// Baseline-GREEN infrastructure proof, NOT an implementation of the MCP tool.
func TestStoryTriageValidationRaceHarness(t *testing.T) {
	fixture, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Stateful: triageSeeds()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close() })
	before := fixture.Snapshot()
	proxy := triageRaceStart(t, fixture, "Folders/Existing")
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", proxy.addr, proxy.trust)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(3 * time.Second))
	reader := bufio.NewReader(conn)
	if _, err = reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	exchange := func(tag, command string) string {
		t.Helper()
		if _, err = fmt.Fprintf(conn, "%s %s\r\n", tag, command); err != nil {
			t.Fatal(err)
		}
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			if strings.HasPrefix(line, tag+" ") {
				return line
			}
		}
	}
	for _, cmd := range []struct{ tag, command string }{{"A1", `LOGIN "` + triageUser + `" "` + triagePassword + `"`}, {"A2", `LIST "" "*"`}, {"A3", `SELECT INBOX`}} {
		if !strings.HasPrefix(exchange(cmd.tag, cmd.command), cmd.tag+" OK ") {
			t.Fatal("proxy prevalidation failed")
		}
	}
	if !strings.HasPrefix(exchange("A4", `UID MOVE 101 "Folders/Existing"`), "A4 NO ") {
		t.Fatal("disappeared destination must produce definitive NO")
	}
	select {
	case err := <-proxy.removed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("race removal did not happen")
	}
	expected := before[:0]
	for _, b := range before {
		if b.Name != "Folders/Existing" {
			expected = append(expected, b)
		}
	}
	h := &triageHarness{fixture: fixture}
	triageNoRaceMutation(t, h, expected)
}

func triageNoRaceMutation(t *testing.T, h *triageHarness, expected []testkit.MailboxState) {
	t.Helper()
	// A MOVE is dispatched once but rejected before application. The removed
	// destination is the CONTROL event; every remaining message/state is intact.
	got := h.fixture.Snapshot()
	if !reflect.DeepEqual(expected, got) {
		t.Error("rejected race MOVE changed remaining state")
	}
	moves := triageDispatches(t, h, "UID MOVE")
	if len(moves) != 1 || moves[0].Set != "101" {
		t.Fatal("race dispatch omitted or replayed")
	}
}

// Draft executable frozen-contract witness; synthetic isolated public CLI only.
package mcpserver_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/wevial/croton-mcp/internal/testkit"
)

func TestStoryTriageSeen(t *testing.T) {
	for _, setting := range []struct {
		name  string
		value any
	}{{"default-off", nil}, {"explicit-disabled", map[string]any{"enabled": false}}} {
		t.Run(setting.name, func(t *testing.T) {
			h := triageStart(t, triageSeeds(), setting.value)
			before := h.fixture.Snapshot()
			catalog, err := h.session.ListTools(h.ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			triageCatalog(t, catalog)
			for _, tool := range catalog.Tools {
				for _, name := range []string{"mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"} {
					if tool.Name == name {
						t.Errorf("disabled config exposed %s", name)
					}
				}
			}
			for _, name := range []string{"mark_read", "mark_unread"} {
				r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: name, Arguments: triageArgs([]uint32{101})})
				if err == nil && (r == nil || !r.IsError) {
					t.Error("disabled tool callable")
				}
			}
			triageNoWrites(t, h, before)
		})
	}
	t.Run("invalid-config-fails-closed", func(t *testing.T) {
		for _, setting := range []struct {
			name  string
			value any
		}{{"not-object", "true"}, {"not-boolean", map[string]any{"enabled": "true"}}, {"null-boolean", map[string]any{"enabled": nil}}, {"numeric-boolean", map[string]any{"enabled": 1}}, {"unknown-field", map[string]any{"enabled": true, "unexpected": true}}} {
			t.Run(setting.name, func(t *testing.T) {
				root := triageRoot(t)
				fixture, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Stateful: triageSeeds()})
				if err != nil {
					t.Fatal(err)
				}
				defer fixture.Close()
				triageInvalidCLI(t, root, fixture, setting.value)
			})
		}
	})
	for _, tool := range []string{"mark_read", "mark_unread"} {
		t.Run(tool, func(t *testing.T) {
			t.Run("displayed-snapshot-first-three-reordered-search-preserves-seven", func(t *testing.T) {
				h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
				h.requireTools(t, tool, "search_mail")
				original := triageDisplayed(t, h, false)
				if len(original) != 10 {
					t.Fatalf("displayed snapshot=%d want ten", len(original))
				}
				selected := append([]uint32(nil), original[:3]...)
				before := h.fixture.Snapshot()
				later := triageDisplayed(t, h, true)
				if reflect.DeepEqual(original, later) || len(later) == 0 || len(later) >= 10 {
					t.Fatal("later unread search did not change subset")
				}
				triageResults(t, h.call(t, tool, triageArgs(selected)), selected, []string{"applied", "applied", "applied"}, "")
				triageSeenState(t, before, h.fixture.Snapshot(), selected, tool == "mark_read")
				triageSeenWire(t, h, selected, tool)
				later = triageDisplayed(t, h, true)
				if reflect.DeepEqual(original, later) {
					t.Error("display ordering/subset unexpectedly unchanged")
				}
			})
			t.Run("missing-uid-mixed", func(t *testing.T) {
				h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
				h.requireTools(t, tool)
				before := h.fixture.Snapshot()
				uids := []uint32{101, 999, 102, 103}
				triageResults(t, h.call(t, tool, triageArgs(uids)), uids, []string{"applied", "refused", "applied", "applied"}, "")
				triageSeenState(t, before, h.fixture.Snapshot(), []uint32{101, 102, 103}, tool == "mark_read")
				triageSeenWire(t, h, []uint32{101, 102, 103}, tool)
			})
			t.Run("preflight-malformed-stale-source-batch-cap", func(t *testing.T) { triageBadInputs(t, tool, "") })
			t.Run("wrong-source-generation-with-colliding-uid", func(t *testing.T) {
				seeds := triageSeeds()
				for i := range seeds.Mailboxes {
					if seeds.Mailboxes[i].Name == "Folders/Existing" {
						seeds.Mailboxes[i].Messages = append([]testkit.MessageSeed{{UID: 101, Body: "Subject: synthetic collision\r\n\r\nwrong-source\r\n"}}, seeds.Mailboxes[i].Messages...)
					}
				}
				h := triageStart(t, seeds, map[string]any{"enabled": true})
				h.requireTools(t, tool)
				before := h.fixture.Snapshot()
				args := triageArgs([]uint32{101})
				args["mailbox"] = "Folders/Existing"
				r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
				if err == nil && r != nil && !r.IsError {
					triageResults(t, r, []uint32{101}, []string{"refused"}, "")
				}
				triageNoWrites(t, h, before)
			})
			t.Run("changed-source-generation-before-dispatch", func(t *testing.T) {
				h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
				h.requireTools(t, tool)
				_ = triageDisplayed(t, h, false)
				if err := h.fixture.ReplaceUIDValidity("INBOX", 7999); err != nil {
					t.Fatal(err)
				}
				before := h.fixture.Snapshot()
				r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: tool, Arguments: triageArgs([]uint32{101, 102})})
				if err == nil && !r.IsError {
					triageResults(t, r, []uint32{101, 102}, []string{"refused", "refused"}, "")
				}
				triageNoWrites(t, h, before)
			})
			for _, boundary := range []testkit.FaultBoundary{testkit.BeforeApplication, testkit.AfterApplication} {
				for _, action := range []struct {
					name  string
					value testkit.FaultAction
				}{{"lost-response", testkit.DropConnection}, {"timeout", testkit.HoldResponse}} {
					t.Run(boundary.String()+"-"+action.name, func(t *testing.T) {
						h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
						h.requireTools(t, tool)
						triageFault(t, h, tool, "", "UID STORE", boundary, action.value)
						triageSeenWire(t, h, []uint32{101, 102}, tool)
					})
				}
			}
		})
	}
	t.Run("tls-untrusted-pin-no-auth-no-write", func(t *testing.T) {
		h := triageStartPin(t, triageSeeds(), nil, strings.Repeat("0", 64))
		before := h.fixture.Snapshot()
		r := h.call(t, "list_folders", map[string]any{})
		if !r.IsError {
			t.Error("untrusted TLS pin accepted")
		}
		triageNoWrites(t, h, before)
		for _, c := range triageCommands(t, h) {
			if c.Verb == "LOGIN" || c.Verb == "AUTHENTICATE" {
				t.Error("credentials dispatched with untrusted TLS")
			}
		}
	})
}
func triageSeenWire(t *testing.T, h *triageHarness, uids []uint32, tool string) {
	t.Helper()
	if len(triageDispatches(t, h, "UID MOVE")) != 0 {
		t.Error("Seen tool moved mail")
	}
	stores := triageDispatches(t, h, "UID STORE")
	if len(stores) != len(uids) {
		t.Fatalf("STORE count=%d want %d", len(stores), len(uids))
	}
	for i, c := range stores {
		want := "+FLAGS"
		if tool == "mark_unread" {
			want = "-FLAGS"
		}
		if c.Ranges[0].Start != uids[i] || c.Store != want {
			t.Fatalf("STORE order/direction differs at %d", i)
		}
	}
}

// Respect the runner's disposable TMPDIR; never bake an operator host path into tests.
var triageScratch = filepath.Clean(os.TempDir())

const triageUser = "triage-username-sentinel@fixture.test"
const triagePassword = "triage-password-sentinel-synthetic-only"
const triageBody = "triage-body-sentinel-synthetic-only"

var triageCategory = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var triageBuild struct {
	sync.Once
	binary string
	helper string
	root   string
	err    error
}

type triageBuffer struct {
	sync.Mutex
	buffer bytes.Buffer
}

func (b *triageBuffer) Write(p []byte) (int, error) {
	b.Lock()
	defer b.Unlock()
	return b.buffer.Write(p)
}
func (b *triageBuffer) text() string { b.Lock(); defer b.Unlock(); return b.buffer.String() }

type triageHarness struct {
	fixture       *testkit.Server
	session       *mcp.ClientSession
	ctx           context.Context
	stderr        *triageBuffer
	mutationCalls int
}

func TestStoryTriageSyntheticHarness(t *testing.T) {
	h := triageStart(t, triageSeeds(), nil)
	before := h.fixture.Snapshot()
	ids := triageDisplayed(t, h, false)
	if len(ids) != 10 {
		t.Fatal("synthetic public search must display ten stable identities")
	}
	seen := map[uint32]bool{}
	for _, uid := range ids {
		if uid < 101 || uid > 110 || seen[uid] {
			t.Fatal("wrong or repeated displayed UID")
		}
		seen[uid] = true
	}
	if len(triageDisplayed(t, h, true)) != 5 {
		t.Fatal("UNSEEN subset fixture mismatch")
	}
	triageNoWrites(t, h, before)
}
func TestMain(m *testing.M) {
	code := m.Run()
	if triageBuild.root != "" {
		if err := os.RemoveAll(triageBuild.root); err != nil {
			fmt.Fprintln(os.Stderr, "synthetic CLI build cleanup failed")
			code = 1
		}
	}
	os.Exit(code)
}

func triageRoot(t *testing.T) string {
	t.Helper()
	if err := os.MkdirAll(triageScratch, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(triageScratch, "triage-witness-")
	if err != nil {
		t.Fatal(err)
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Errorf("scratch cleanup: %v", err)
		}
	})
	return root
}
func triageEnv(root string, build bool) []string {
	env := []string{"HOME=" + root, "TMPDIR=" + root, "TMP=" + root, "TEMP=" + root, "XDG_CONFIG_HOME=" + root, "XDG_CACHE_HOME=" + root, "XDG_DATA_HOME=" + root, "LANG=C", "LC_ALL=C", "PATH=/usr/bin:/bin"}
	if build {
		// The runner may supply disposable Go caches, never inherited credentials.
		// Only explicitly supplied isolated-runner caches are reused. They may
		// live outside TMPDIR; absent values belong to this unique build root.
		for _, key := range []string{"GOCACHE", "GOMODCACHE"} {
			value := os.Getenv(key)
			if value == "" || !filepath.IsAbs(value) {
				value = filepath.Join(root, key)
			}
			env = append(env, key+"="+value)
		}
		env = append(env, "GOPATH="+filepath.Join(root, "GOPATH"))
		env = append(env, "GOFLAGS=-mod=readonly -modcacherw", "GOTELEMETRY=off", "GOTOOLCHAIN=go1.26.6", "GOSUMDB=sum.golang.org", "GOPROXY=https://proxy.golang.org", "GONOSUMDB=", "GONOPROXY=", "GOPRIVATE=")
	}
	return env
}
func triageBinary(t *testing.T) string {
	t.Helper()
	triageBuild.Do(func() {
		root, err := os.MkdirTemp(triageScratch, "triage-cli-build-")
		if err != nil {
			triageBuild.err = err
			return
		}
		triageBuild.root = root
		cwd, err := os.Getwd()
		if err != nil {
			triageBuild.err = err
			return
		}
		module := filepath.Clean(filepath.Join(cwd, "../.."))
		if _, err = os.Stat(filepath.Join(module, "go.mod")); err != nil {
			triageBuild.err = fmt.Errorf("witness must run at intended internal/mcpserver module path: %w", err)
			return
		}
		goexe, err := exec.LookPath("go")
		if err != nil {
			triageBuild.err = fmt.Errorf("Go1.26.6 build launcher unavailable: %w", err)
			return
		}
		goexe, err = filepath.Abs(goexe)
		if err != nil {
			triageBuild.err = err
			return
		}
		cmd := exec.Command(goexe, "telemetry", "off")
		cmd.Env = triageEnv(root, true)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			triageBuild.err = fmt.Errorf("isolated telemetry setup: %v %s", err, out)
			return
		}
		cmd = exec.Command(goexe, "version")
		cmd.Env = triageEnv(root, true)
		cmd.Dir = module
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "go1.26.6") {
			triageBuild.err = fmt.Errorf("require Go1.26.6: %v %s", err, out)
			return
		}
		triageBuild.binary = filepath.Join(root, "croton-mcp")
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd = exec.CommandContext(ctx, goexe, "build", "-o", triageBuild.binary, "./cmd/croton-mcp")
		cmd.Env = triageEnv(root, true)
		cmd.Dir = module
		if out, err := cmd.CombinedOutput(); err != nil {
			triageBuild.err = fmt.Errorf("build actual CLI: %v %s", err, out)
			return
		}
		// Never execute a -race test binary as the credential helper: the race
		// runtime's exit delay exceeds the bounded 800ms credential deadline.
		// A separate non-race synthetic argv helper preserves the timeout gate.
		credentials, _ := json.Marshal(map[string]string{"username": triageUser, "password": triagePassword})
		source := filepath.Join(root, "credential-helper.go")
		program := "package main\nimport \"fmt\"\nfunc main(){fmt.Print(" + strconv.Quote(string(credentials)) + ")}\n"
		if err = os.WriteFile(source, []byte(program), 0600); err != nil {
			triageBuild.err = err
			return
		}
		triageBuild.helper = filepath.Join(root, "credential-helper")
		cmd = exec.CommandContext(ctx, goexe, "build", "-o", triageBuild.helper, source)
		cmd.Env = triageEnv(root, true)
		cmd.Dir = module
		if out, err := cmd.CombinedOutput(); err != nil {
			triageBuild.err = fmt.Errorf("build synthetic credential helper: %v %s", err, out)
		}
	})
	if triageBuild.err != nil {
		t.Fatal(triageBuild.err)
	}
	return triageBuild.binary
}
func triageSeeds() *testkit.StatefulOptions {
	seeds := &testkit.StatefulOptions{Move: true}
	inbox := testkit.MailboxSeed{Name: "INBOX", UIDValidity: 7001}
	for uid := uint32(101); uid <= 110; uid++ {
		flags := []string{`\Answered`, `\Flagged`}
		if uid%2 == 0 {
			flags = append(flags, `\Seen`)
		}
		inbox.Messages = append(inbox.Messages, testkit.MessageSeed{UID: uid, Flags: flags, Body: fmt.Sprintf("From: Fixture <sender@fixture.test>\r\nTo: Reader <reader@fixture.test>\r\nSubject: Synthetic %d\r\nMessage-ID: <%d@fixture.test>\r\n\r\n%s-%d\r\n", uid, uid, triageBody, uid)})
	}
	seeds.Mailboxes = []testkit.MailboxSeed{inbox, {Name: "Folders/Existing", UIDValidity: 7002, Messages: []testkit.MessageSeed{{UID: 501, Flags: []string{`\Flagged`}, Body: "Subject: destination sentinel\r\n\r\ndestination-synthetic-only\r\n"}}}, {Name: "Archivage", Attributes: []string{`\Archive`, `\Noinferiors`}}, {Name: "Corbeille", Attributes: []string{`\Trash`, `\Noinferiors`}}, {Name: "Labels/Tag"}, {Name: "All Mail", Attributes: []string{`\All`, `\Noinferiors`}}, {Name: "Starred", Attributes: []string{`\Flagged`, `\Noinferiors`}}, {Name: "Folders/NoSelect", Attributes: []string{`\Noselect`}}, {Name: "Folders", Attributes: []string{`\Noselect`}}, {Name: "Labels", Attributes: []string{`\Noselect`}}}
	return seeds
}
func triageConfig(t *testing.T, root string, fixture *testkit.Server, enabled any, pin, address string) string {
	t.Helper()
	host, p, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatal(err)
	}
	port, err := strconv.Atoi(p)
	if err != nil {
		t.Fatal(err)
	}
	_ = triageBinary(t) // build before any per-request deadline
	conf := map[string]any{"imap": map[string]any{"host": host, "port": port, "tlsMode": "implicit", "credentialCommand": []string{triageBuild.helper}, "tls": map[string]any{"spkiSha256": pin}, "connectTimeoutMs": 800, "commandTimeoutMs": 800}, "audit": map[string]any{"enabled": true}}
	if enabled != nil {
		conf["mutations"] = enabled
	}
	data, err := json.Marshal(conf)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "config.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func triageInvalidCLI(t *testing.T, root string, fixture *testkit.Server, enabled any) {
	t.Helper()
	path := triageConfig(t, root, fixture, enabled, fixture.SPKISHA256(), fixture.Addr())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, triageBinary(t), "--config", path)
	cmd.Env = triageEnv(root, false)
	cmd.Dir = root
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Error("invalid mutations config accepted by actual CLI")
	}
	if ctx.Err() != nil {
		t.Error("invalid config did not fail promptly")
	}
	triagePrivate(t, string(out))
	if len(out) > 1024 || strings.Contains(string(out), "unexpected") {
		t.Error("config failure output leaked details or exceeded bound")
	}
	if len(fixture.Commands()) != 0 {
		t.Error("invalid config contacted IMAP")
	}
}
func triageStart(t *testing.T, seeds *testkit.StatefulOptions, enabled any) *triageHarness {
	return triageStartPin(t, seeds, enabled, "")
}
func triageStartPin(t *testing.T, seeds *testkit.StatefulOptions, enabled any, pin string) *triageHarness {
	return triageStartRoute(t, seeds, enabled, pin, nil)
}

func triageStartRoute(t *testing.T, seeds *testkit.StatefulOptions, enabled any, pin string, route func(*testing.T, *testkit.Server) (string, string)) *triageHarness {
	t.Helper()
	root := triageRoot(t)
	fixture, err := testkit.Start(testkit.Options{Mode: testkit.ImplicitTLS, Stateful: seeds})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fixture.Close() })
	if pin == "" {
		pin = fixture.SPKISHA256()
	}
	address := fixture.Addr()
	if route != nil {
		address, pin = route(t, fixture)
	}
	path := triageConfig(t, root, fixture, enabled, pin, address)
	binary := triageBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	t.Cleanup(cancel)
	stderr := &triageBuffer{}
	cmd := exec.Command(binary, "--config", path)
	cmd.Dir = root
	cmd.Env = triageEnv(root, false)
	cmd.Stderr = stderr
	client := mcp.NewClient(&mcp.Implementation{Name: "triage-synthetic.test", Version: "1"}, nil)
	session, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		triagePrivate(t, stderr.text())
		t.Fatalf("real config-to-CLI MCP initialize failed (mutations=%v): %v; safe stderr=%s", enabled, err, stderr.text())
	}
	h := &triageHarness{fixture: fixture, session: session, ctx: ctx, stderr: stderr}
	t.Cleanup(func() {
		_ = session.Close()
		triagePrivate(t, stderr.text())
		triageAudit(t, stderr.text())
		if h.mutationCalls > 0 && strings.TrimSpace(stderr.text()) == "" {
			t.Error("enabled audited mutation call emitted no audit event")
		}
	})
	return h
}
func (h *triageHarness) requireTools(t *testing.T, names ...string) {
	t.Helper()
	catalog, err := h.session.ListTools(h.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	triageCatalog(t, catalog)
	found := map[string]bool{}
	for _, tool := range catalog.Tools {
		found[tool.Name] = true
	}
	for _, name := range names {
		if !found[name] {
			t.Fatalf("real CLI catalog missing required tool %s", name)
		}
	}
}
func triageCatalog(t *testing.T, catalog *mcp.ListToolsResult) {
	t.Helper()
	reads := map[string]bool{"list_folders": true, "search_mail": true, "get_message": true, "get_thread": true, "list_attachments": true, "select_digest_candidates": true}
	mutations := map[string]bool{"mark_read": true, "mark_unread": true, "move_mail": true, "archive_mail": true, "trash_mail": true}
	found := map[string]bool{}
	for _, tool := range catalog.Tools {
		if found[tool.Name] || (!reads[tool.Name] && !mutations[tool.Name]) {
			t.Fatalf("unexpected or duplicate catalog tool %s", tool.Name)
		}
		found[tool.Name] = true
		if tool.Annotations == nil || tool.Annotations.ReadOnlyHint != reads[tool.Name] {
			t.Errorf("incorrect read-only annotation for %s", tool.Name)
		}
		if mutations[tool.Name] {
			data, err := json.Marshal(tool.InputSchema)
			if err != nil {
				t.Fatal(err)
			}
			var schema struct {
				Properties map[string]json.RawMessage `json:"properties"`
			}
			if err = json.Unmarshal(data, &schema); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"approved", "ordinal", "ordinals"} {
				if schema.Properties[key] != nil {
					t.Errorf("mutation schema exposes untrusted %s", key)
				}
			}
		}
	}
	for name := range reads {
		if !found[name] {
			t.Errorf("read tool disappeared: %s", name)
		}
	}
}

func (h *triageHarness) call(t *testing.T, name string, args map[string]any) *mcp.CallToolResult {
	t.Helper()
	r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		triagePrivate(t, err.Error())
		t.Fatalf("public %s RPC failed: %v", name, err)
	}
	if strings.HasPrefix(name, "mark_") || name == "move_mail" || name == "archive_mail" || name == "trash_mail" {
		h.mutationCalls++
		data, _ := json.Marshal(r)
		triagePrivate(t, string(data))
	}
	return r
}
func triageArgs(uids []uint32) map[string]any {
	return map[string]any{"mailbox": "INBOX", "uidvalidity": uint32(7001), "uids": uids}
}
func triageJSON(t *testing.T, r *mcp.CallToolResult) []byte {
	t.Helper()
	if r == nil {
		t.Fatal("nil result")
	}
	if r.StructuredContent != nil {
		data, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if len(r.Content) != 1 {
		t.Fatal("expected one bounded text JSON result")
	}
	text, ok := r.Content[0].(*mcp.TextContent)
	if !ok {
		t.Fatal("expected JSON text")
	}
	return []byte(text.Text)
}
func triageResults(t *testing.T, r *mcp.CallToolResult, uids []uint32, outcomes []string, code string) {
	t.Helper()
	if r == nil || r.IsError {
		t.Fatalf("expected structured per-ID outcome, got tool error: %+v", r)
	}
	data := triageJSON(t, r)
	triagePrivate(t, string(data))
	if len(data) > 16384 {
		t.Fatal("unbounded result")
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope) != 1 || envelope["results"] == nil {
		t.Fatal("result envelope keys must be exactly results")
	}
	var rows []map[string]json.RawMessage
	if err := json.Unmarshal(envelope["results"], &rows); err != nil {
		t.Fatal(err)
	}
	if len(rows) != len(uids) || len(outcomes) != len(uids) {
		t.Fatalf("result cardinality %d want %d", len(rows), len(uids))
	}
	for i, row := range rows {
		for k := range row {
			if k != "uid" && k != "outcome" && k != "code" {
				t.Fatalf("unsafe result key %q", k)
			}
		}
		var uid uint32
		var outcome, c string
		if err := json.Unmarshal(row["uid"], &uid); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(row["outcome"], &outcome); err != nil {
			t.Fatal(err)
		}
		if raw := row["code"]; raw != nil {
			if err := json.Unmarshal(raw, &c); err != nil {
				t.Fatal(err)
			}
			if !triageCategory.MatchString(c) {
				t.Fatal("code not bounded category")
			}
		}
		if uid != uids[i] || outcome != outcomes[i] {
			t.Fatalf("result %d identity/outcome=%d/%s want %d/%s", i, uid, outcome, uids[i], outcomes[i])
		}
		if code != "" && outcome == "refused" && c != code {
			t.Fatalf("refusal code=%q want %q", c, code)
		}
	}
}
func triagePrivate(t *testing.T, text string) {
	t.Helper()
	for _, s := range []string{triageUser, triagePassword, triageBody, "TRYCREATE", "CANNOT", "EOF", "connection reset", "broken pipe", "i/o timeout"} {
		if strings.Contains(text, s) {
			t.Errorf("diagnostics/results leaked prohibited synthetic content or raw transport/protocol detail")
		}
	}
}
func triageAudit(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		if line == "" {
			continue
		}
		if len(line) > 1024 {
			t.Error("audit metadata exceeded bound")
		}
		var row map[string]json.RawMessage
		if json.Unmarshal([]byte(line), &row) != nil {
			t.Errorf("audit line is not bounded JSON metadata")
			continue
		}
		for key := range row {
			switch key {
			case "event", "tool", "outcome", "code", "category":
				var value string
				if json.Unmarshal(row[key], &value) != nil || !triageCategory.MatchString(value) {
					t.Errorf("unbounded/noncategorical audit value for %s", key)
				}
			case "truncated":
				var value bool
				if json.Unmarshal(row[key], &value) != nil {
					t.Error("audit truncated is not boolean")
				}
			default:
				t.Errorf("unsafe audit key %s", key)
			}
		}
	}
}
func triageCommands(t *testing.T, h *triageHarness) []testkit.TranscriptCommand {
	t.Helper()
	commands, err := testkit.ParseTranscript(h.fixture.Commands())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range commands {
		switch c.Verb {
		case "CAPABILITY", "LOGIN", "AUTHENTICATE", "SELECT", "EXAMINE", "STATUS", "LIST", "UID SEARCH", "UID FETCH", "NOOP", "LOGOUT":
		case "UID STORE":
			if (c.Store != "+FLAGS" && c.Store != "-FLAGS") || len(c.Flags) != 1 || c.Flags[0] != `\Seen` {
				t.Fatal("mutation not Seen-only incremental STORE")
			}
			fallthrough
		case "UID MOVE":
			if len(c.Ranges) != 1 || c.Ranges[0].Start == 0 || c.Ranges[0].Start != c.Ranges[0].Stop || c.Set != fmt.Sprint(c.Ranges[0].Start) {
				t.Fatal("mutation must dispatch one exact numeric UID")
			}
		default:
			t.Fatalf("forbidden command verb %s", c.Verb)
		}
		if (c.Verb == "LOGIN" || c.Verb == "AUTHENTICATE" || c.Verb == "UID STORE" || c.Verb == "UID MOVE") && !c.TLS {
			t.Fatal("authentication or mutation outside TLS")
		}
		if c.Verb == "UID FETCH" && strings.Contains(c.Arguments, "BODY[") {
			t.Fatal("non-PEEK read can mutate Seen")
		}
	}
	return commands
}
func triageDispatches(t *testing.T, h *triageHarness, verb string) []testkit.TranscriptCommand {
	t.Helper()
	var out []testkit.TranscriptCommand
	for _, c := range triageCommands(t, h) {
		if c.Verb == verb {
			out = append(out, c)
		}
	}
	return out
}
func triageNoWrites(t *testing.T, h *triageHarness, before []testkit.MailboxState) {
	t.Helper()
	if !reflect.DeepEqual(before, h.fixture.Snapshot()) {
		t.Error("refusal/read changed synthetic mailbox state")
	}
	if len(triageDispatches(t, h, "UID STORE"))+len(triageDispatches(t, h, "UID MOVE")) != 0 {
		t.Error("refused/read request dispatched mutation")
	}
}
func triageClone(states []testkit.MailboxState) []testkit.MailboxState {
	data, _ := json.Marshal(states)
	var out []testkit.MailboxState
	_ = json.Unmarshal(data, &out)
	return out
}
func triageMoved(t *testing.T, before, after []testkit.MailboxState, source, destination string, uids []uint32) {
	t.Helper()
	want := triageClone(before)
	si, di := -1, -1
	for i, b := range want {
		if b.Name == source {
			si = i
		}
		if b.Name == destination {
			di = i
		}
	}
	if si < 0 || di < 0 {
		t.Fatal("missing expected source/destination")
	}
	for _, uid := range uids {
		idx := -1
		for i, m := range want[si].Messages {
			if m.UID == uid {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("expected source UID %d missing", uid)
		}
		m := want[si].Messages[idx]
		want[si].Messages = append(want[si].Messages[:idx], want[si].Messages[idx+1:]...)
		m.UID = want[di].UIDNext
		want[di].UIDNext++
		want[di].Messages = append(want[di].Messages, m)
	}
	if !reflect.DeepEqual(want, after) {
		t.Error("settled move state differs: selected body/flags, destination UIDNext, or unselected mail changed")
	}
}
func triageSeenState(t *testing.T, before, after []testkit.MailboxState, uids []uint32, seen bool) {
	t.Helper()
	want := triageClone(before)
	selected := map[uint32]bool{}
	for _, uid := range uids {
		selected[uid] = true
	}
	for i := range want {
		if want[i].Name != "INBOX" {
			continue
		}
		for j := range want[i].Messages {
			m := &want[i].Messages[j]
			if !selected[m.UID] {
				continue
			}
			var flags []string
			for _, f := range m.Flags {
				if f != `\Seen` {
					flags = append(flags, f)
				}
			}
			if seen {
				flags = append(flags, `\Seen`)
			}
			m.Flags = flags
		}
	}
	normalize := func(states []testkit.MailboxState) {
		for i := range states {
			for j := range states[i].Messages {
				sort.Strings(states[i].Messages[j].Flags)
			}
		}
	}
	normalize(want)
	got := triageClone(after)
	normalize(got)
	if !reflect.DeepEqual(want, got) {
		t.Error("Seen mutation changed body, non-Seen flags, identity, or unselected mailbox/message")
	}
}
func triageDisplayed(t *testing.T, h *triageHarness, unread bool) []uint32 {
	t.Helper()
	r := h.call(t, "search_mail", map[string]any{"mailbox": "INBOX", "limit": 10, "unreadOnly": unread})
	if r.IsError {
		t.Fatal("search_mail failed")
	}
	var value struct {
		Results []struct {
			ID string `json:"id"`
		} `json:"results"`
	}
	if err := json.Unmarshal(triageJSON(t, r), &value); err != nil {
		t.Fatal(err)
	}
	var uids []uint32
	for _, item := range value.Results {
		raw, err := base64.RawURLEncoding.DecodeString(item.ID)
		if err != nil || len(raw) <= sha256.Size {
			t.Fatal("displayed opaque identity malformed")
		}
		var p struct {
			Mailbox    string `json:"m"`
			Generation uint32 `json:"v"`
			UID        uint32 `json:"u"`
		}
		if err := json.Unmarshal(raw[:len(raw)-sha256.Size], &p); err != nil {
			t.Fatal(err)
		}
		if p.Mailbox != "INBOX" || p.Generation != 7001 || p.UID == 0 {
			t.Fatal("display identity not bound to source snapshot")
		}
		uids = append(uids, p.UID)
	}
	return uids
}
func triageWait(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("bounded wait failed: %s", label)
	}
}
func triageFault(t *testing.T, h *triageHarness, tool, destination, verb string, boundary testkit.FaultBoundary, action testkit.FaultAction) {
	t.Helper()
	before := h.fixture.Snapshot()
	fault, err := h.fixture.InjectFault(testkit.MutationFault{Command: verb, UIDs: "102", Boundary: boundary, Action: action})
	if err != nil {
		t.Fatal(err)
	}
	uids := []uint32{101, 999, 102, 103}
	args := triageArgs(uids)
	if tool == "move_mail" {
		args["destination"] = destination
	}
	start := time.Now()
	r := h.call(t, tool, args)
	elapsed := time.Since(start)
	triageWait(t, fault.Triggered(), "fault trigger")
	triageWait(t, fault.Finished(), "fault connection closed")
	if elapsed > 3*time.Second {
		t.Errorf("fault outcome not bounded: %v", elapsed)
	}
	triageResults(t, r, uids, []string{"applied", "refused", "unknown", "not_attempted"}, "")
	applied := []uint32{101}
	if boundary == testkit.AfterApplication {
		applied = append(applied, 102)
		if !strings.Contains(fault.Completion(), " OK") {
			t.Error("after-application fixture did not apply successfully")
		}
	} else if fault.Completion() != "" {
		t.Error("before-application fault exposed completion")
	}
	if verb == "UID MOVE" {
		triageMoved(t, before, h.fixture.Snapshot(), "INBOX", destination, applied)
	} else {
		triageSeenState(t, before, h.fixture.Snapshot(), applied, tool == "mark_read")
	}
	dispatches := triageDispatches(t, h, verb)
	if len(dispatches) != 2 || dispatches[0].Set != "101" || dispatches[1].Set != "102" {
		t.Fatal("fault dispatch replay, missing UID dispatch, or continued writes")
	}
	command, ok := fault.Command()
	if !ok || command.ConnectionID != dispatches[1].ConnectionID || command.Sequence != dispatches[1].Sequence {
		t.Fatal("fault not tied to exact dispatch connection")
	}
	counts := map[int]int{}
	for _, c := range dispatches {
		if c.Set == "102" {
			counts[c.ConnectionID]++
		}
	}
	if len(counts) != 1 || counts[command.ConnectionID] != 1 {
		t.Fatal("ambiguous UID replayed")
	}
	triagePrivate(t, h.stderr.text())
}
func triageBadInputs(t *testing.T, tool, destination string) {
	t.Helper()
	t.Run("exact-cap-50-accepted", func(t *testing.T) {
		h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
		h.requireTools(t, tool)
		before := h.fixture.Snapshot()
		uids := make([]uint32, 50)
		outcomes := make([]string, 50)
		for i := range uids {
			uids[i] = uint32(101 + i)
			outcomes[i] = "refused"
			if i < 10 {
				outcomes[i] = "applied"
			}
		}
		args := triageArgs(uids)
		if tool == "move_mail" {
			args["destination"] = destination
		}
		triageResults(t, h.call(t, tool, args), uids, outcomes, "")
		selected := uids[:10]
		if tool == "mark_read" || tool == "mark_unread" {
			triageSeenState(t, before, h.fixture.Snapshot(), selected, tool == "mark_read")
			triageSeenWire(t, h, selected, tool)
		} else {
			dest := destination
			if tool == "archive_mail" {
				dest = "Archivage"
			}
			if tool == "trash_mail" {
				dest = "Corbeille"
			}
			triageMoved(t, before, h.fixture.Snapshot(), "INBOX", dest, selected)
			if len(triageDispatches(t, h, "UID MOVE")) != 10 {
				t.Error("exact-cap missing IDs dispatched or successful IDs omitted")
			}
		}
	})
	for _, missing := range []string{"mailbox", "uidvalidity", "uids"} {
		t.Run("missing-"+missing, func(t *testing.T) {
			h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
			h.requireTools(t, tool)
			before := h.fixture.Snapshot()
			args := triageArgs([]uint32{101})
			delete(args, missing)
			if tool == "move_mail" {
				args["destination"] = destination
			}
			r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err == nil && (r == nil || !r.IsError) {
				t.Error("missing identity field accepted")
			}
			triageNoWrites(t, h, before)
		})
	}
	cases := []struct {
		name  string
		patch map[string]any
	}{{"empty", map[string]any{"uids": []uint32{}}}, {"duplicates", map[string]any{"uids": []uint32{101, 102, 101}}}, {"zero", map[string]any{"uids": []any{101, 0}}}, {"negative", map[string]any{"uids": []any{101, -1}}}, {"fraction", map[string]any{"uids": []any{101, 1.5}}}, {"overflow", map[string]any{"uids": []any{101, uint64(4294967296)}}}, {"wrong-type", map[string]any{"uids": "101"}}, {"null", map[string]any{"uids": nil}}, {"unknown", map[string]any{"unexpected": true}}, {"empty-mailbox", map[string]any{"mailbox": ""}}, {"nonexistent-mailbox", map[string]any{"mailbox": "Folders/Missing"}}, {"stale", map[string]any{"uidvalidity": 7000}}, {"zero-generation", map[string]any{"uidvalidity": 0}}, {"negative-generation", map[string]any{"uidvalidity": -1}}}
	over := make([]uint32, 51)
	for i := range over {
		over[i] = uint32(101 + i)
	}
	cases = append(cases, struct {
		name  string
		patch map[string]any
	}{"over-cap", map[string]any{"uids": over}})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := triageStart(t, triageSeeds(), map[string]any{"enabled": true})
			h.requireTools(t, tool)
			before := h.fixture.Snapshot()
			args := triageArgs([]uint32{101, 102})
			if tool == "move_mail" {
				args["destination"] = destination
			}
			for k, v := range c.patch {
				args[k] = v
			}
			r, err := h.session.CallTool(h.ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
			if err != nil {
				triagePrivate(t, err.Error())
			}
			if r != nil {
				data, _ := json.Marshal(r)
				triagePrivate(t, string(data))
			}
			if err == nil && (r == nil || !r.IsError) {
				if c.name != "stale" && c.name != "nonexistent-mailbox" {
					t.Error("malformed request accepted without tool/MCP error")
				} else {
					triageResults(t, r, []uint32{101, 102}, []string{"refused", "refused"}, "")
				}
			}
			triageNoWrites(t, h, before)
		})
	}
}

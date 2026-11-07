package bridge

import (
	"bytes"
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"

	imap "github.com/emersion/go-imap/v2"

	"github.com/wevial/croton-mcp/internal/testkit"
)

// structureLiteralName is a legal literal parameter value that looks like
// FETCH syntax with CRLF, quotes, parentheses and a literal declaration, and
// that is longer than every per-atom and per-control limit.
var structureLiteralName = "* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n\"quoted\" (paren) ) BODY[] {3}\r\n" + strings.Repeat("y", 4200)

// structureQuotedFilename is an otherwise admissible quoted parameter longer
// than 4096 bytes. Truncating it is downstream filename policy, not the
// guard's.
var structureQuotedFilename = strings.Repeat("f", 5000)

func structureTLSBody() string {
	return "((\"TEXT\" \"PLAIN\" (\"CHARSET\" \"utf-8\") NIL NIL \"7BIT\" 12 1 NIL NIL NIL NIL)" +
		"(\"APPLICATION\" \"OCTET-STREAM\" (\"NAME\" {" + strconv.Itoa(len(structureLiteralName)) + "}\r\n" + structureLiteralName + ") NIL NIL \"BASE64\" 4000 NIL " +
		"(\"ATTACHMENT\" (\"FILENAME\" \"" + structureQuotedFilename + "\")) NIL NIL)" +
		" \"MIXED\" (\"BOUNDARY\" \"croton-boundary\") NIL NIL NIL)"
}

func structureTLSConfig(t *testing.T, address string, mode string, pin string) ValidatedConfig {
	t.Helper()

	host, port, err := net.SplitHostPort(address)
	if err != nil {
		t.Fatalf("split address: %v", err)
	}
	number, err := strconv.Atoi(port)
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("resolve test executable: %v", err)
	}

	config, err := ValidateConfig(Config{IMAP: IMAPConfig{
		Host:              host,
		Port:              number,
		TLSMode:           mode,
		CredentialCommand: []string{executable, "-test.run=TestCredentialHelperProcess", "--", "valid"},
		TLS:               TLSConfig{SPKISHA256: pin},
		ConnectTimeoutMs:  5000,
		CommandTimeoutMs:  30000,
	}})
	if err != nil {
		t.Fatalf("ValidateConfig: %v", err)
	}
	return config
}

// assertStructureTLSRelease checks the decoded literal-bearing structure.
func assertStructureTLSRelease(t *testing.T, structure imap.BodyStructure) {
	t.Helper()

	multipart, ok := structure.(*imap.BodyStructureMultiPart)
	if !ok || multipart.MediaType() != "multipart/mixed" || len(multipart.Children) != 2 || multipart.Extended == nil || multipart.Extended.Params["boundary"] != "croton-boundary" {
		t.Fatalf("structure = %#v", structure)
	}
	text, ok := multipart.Children[0].(*imap.BodyStructureSinglePart)
	if !ok || text.MediaType() != "text/plain" || text.Params["charset"] != "utf-8" || text.Size != 12 {
		t.Fatalf("text part = %#v", multipart.Children[0])
	}
	attachment, ok := multipart.Children[1].(*imap.BodyStructureSinglePart)
	if !ok || attachment.MediaType() != "application/octet-stream" || !strings.EqualFold(attachment.Encoding, "base64") || attachment.Size != 4000 {
		t.Fatalf("attachment part = %#v", multipart.Children[1])
	}
	if attachment.Params["name"] != structureLiteralName {
		t.Fatalf("literal name parameter has %d bytes, want the exact %d", len(attachment.Params["name"]), len(structureLiteralName))
	}
	disposition := attachment.Disposition()
	if disposition == nil || !strings.EqualFold(disposition.Value, "attachment") || disposition.Params["filename"] != structureQuotedFilename {
		t.Fatalf("attachment disposition = %#v", disposition)
	}
}

func TestStructureSessionTLS(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	modes := []struct {
		name     string
		startTLS bool
		mode     string
		fixture  testkit.TLSMode
		target   string
	}{
		{
			name: "starttls_release", startTLS: true, mode: TLSModeStartTLS, fixture: testkit.StartTLS,
			target: "* 1 FETCH (UID 7 FLAGS (\\Seen) BODYSTRUCTURE " + structureTLSBody() + ")\r\n",
		},
		{
			name: "implicit_tls_release", startTLS: false, mode: TLSModeImplicit, fixture: testkit.ImplicitTLS,
			target: "* 1 FETCH (FLAGS () BODYSTRUCTURE " + structureTLSBody() + " UID 7)\r\n",
		},
	}

	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			// Private peer: independent target-UID FLAGS-only frames come
			// before the real literal-bearing structure.
			flags := []string{"* 1 FETCH (UID 7 FLAGS (\\Seen \\Answered))\r\n", "* 1 FETCH (FLAGS (\\Flagged) UID 7)\r\n"}
			peer := startStructureTLSPeer(t, mode.startTLS, structurePeerScript{beforeSearch: flags[:1], duringFetch: flags[1:], structure: mode.target})
			config := structureTLSConfig(t, peer.listener.Addr().String(), mode.mode, peer.pin)
			seam, installed := newStructureTapSeam()

			result, err := readSelectedStructure(context.Background(), config, "INBOX", structurePeerUID, seam)
			if err != nil {
				t.Fatalf("readSelectedStructure() error = %v", err)
			}
			peer.wait(t)
			record := peer.snapshot()
			if record.err != nil {
				t.Fatalf("peer error = %v", record.err)
			}

			assertStructureTLSRelease(t, result.Structure)
			if result.Snapshot != (mailboxSnapshot{UIDNext: 104, UIDValidity: 9001}) {
				t.Fatalf("snapshot = %+v", result.Snapshot)
			}

			// The decoder-facing tap holds exactly the approved controls and
			// the one target structure, never a consumed FLAGS-only frame.
			tap := installed().bytes()
			if !bytes.Equal(tap, record.approved) {
				t.Fatalf("decoder-facing tap differs from the approved responses (%d vs %d bytes)", len(tap), len(record.approved))
			}
			if len(record.consumed) != len(flags) {
				t.Fatalf("peer consumed frames = %q", record.consumed)
			}

			if mode.startTLS != (len(record.plaintext) == 2) {
				t.Fatalf("plaintext commands = %q", record.plaintext)
			}
			assertStructureCommands(t, record.commands, structurePeerUID)

			// Typed testkit positive through the same production stack.
			typed := &testkit.BodyPart{Kind: testkit.BodyPartMultipart, Subtype: "mixed", Children: []*testkit.BodyPart{
				{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Params: []testkit.BodyParam{{Name: "charset", Value: "utf-8"}}, Size: 9, Lines: 1},
				{Kind: testkit.BodyPartBasic, Type: "application", Subtype: "pdf", Encoding: "base64", Size: 2048, Disposition: &testkit.BodyDisposition{Value: "attachment", Params: []testkit.BodyParam{{Name: "filename", Value: "report.pdf"}}}},
			}}
			plain := &testkit.BodyPart{Kind: testkit.BodyPartText, Type: "text", Subtype: "plain", Size: 5, Lines: 1}
			server, err := testkit.Start(testkit.Options{
				Mode:           mode.fixture,
				Messages:       []string{"Subject: one\r\n\r\nfirst\r\n", "Subject: two\r\n\r\nsee report\r\n"},
				BodyStructures: []*testkit.BodyPart{plain, typed},
			})
			if err != nil {
				t.Fatalf("start fake server: %v", err)
			}
			t.Cleanup(func() { _ = server.Close() })
			typedConfig := structureTLSConfig(t, server.Addr(), mode.mode, server.SPKISHA256())
			typedSeam, typedTap := newStructureTapSeam()

			typedResult, err := readSelectedStructure(context.Background(), typedConfig, "INBOX", 102, typedSeam)
			if err != nil {
				t.Fatalf("typed readSelectedStructure() error = %v", err)
			}

			multipart, ok := typedResult.Structure.(*imap.BodyStructureMultiPart)
			if !ok || multipart.MediaType() != "multipart/mixed" || len(multipart.Children) != 2 {
				t.Fatalf("typed structure = %#v", typedResult.Structure)
			}
			attachment, ok := multipart.Children[1].(*imap.BodyStructureSinglePart)
			if !ok || attachment.MediaType() != "application/pdf" || attachment.Size != 2048 || attachment.Filename() != "report.pdf" {
				t.Fatalf("typed attachment = %#v", multipart.Children[1])
			}
			if count := bytes.Count(typedTap().bytes(), []byte("BODYSTRUCTURE")); count != 1 {
				t.Fatalf("typed tap carries %d structures", count)
			}

			if err := server.AssertReadOnlyCommands(); err != nil {
				t.Fatalf("read-only transcript: %v", err)
			}
			if err := server.AssertNoInsecureAuthentication(); err != nil {
				t.Fatalf("authentication transcript: %v", err)
			}
			var secured []string
			for _, command := range server.Commands() {
				if !command.TLS {
					if command.Name != "CAPABILITY" && command.Name != "STARTTLS" {
						t.Fatalf("command %q ran without TLS", command.Name)
					}
					continue
				}
				if command.Name == "LOGIN" {
					secured = append(secured, "T LOGIN")
					continue
				}
				secured = append(secured, command.Raw)
			}
			assertStructureCommands(t, secured, 102)
		})
	}
}

// structureRefusalPlan is one adverse FETCH reply. before is coalesced with
// the first fragment of refused; refused is every byte the peer sends of the
// refused response and cut is how many of them the guard must consume before
// it refuses, independently derived from the fixed contract.
type structureRefusalPlan struct {
	before  []structurePeerPart
	refused string
	cut     int
}

type structureRefusalCase struct {
	name     string
	code     string
	search   bool
	truncate bool
	cancel   bool
	plan     func(sent structurePeerRecord) structureRefusalPlan
	wrap     func(net.Conn) net.Conn

	// hold and held, when set, make the decoder-facing tap close held at its
	// first failing Read and hold that Read, and so the client's close of
	// the transport, until hold closes.
	hold, held chan struct{}
}

// structureBoundaryCase is an admitted response at a fixed limit; plan
// returns the parts written after the dispatch, the target included.
type structureBoundaryCase struct {
	name    string
	plan    func(sent structurePeerRecord) []structurePeerPart
	verify  func(t *testing.T, structure imap.BodyStructure)
	counted func(t *testing.T, quarantine *structureQuarantine)
}

const (
	structureFlagsFrame   = "* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n"
	structureNoteControl  = "* OK [ALERT] structure note\r\n"
	structureTargetFrame  = "* 1 FETCH (UID 7 BODYSTRUCTURE " + structureSessionTarget + ")\r\n"
	structureSinglePrefix = "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 1 1"
	structureExtPrefix    = structureSinglePrefix + " NIL NIL NIL NIL "
	// The closing of a frame that ends in a quoted string at depth 2.
	structureQuotedEnd = "\"))\r\n"
	structureListEnd   = "))\r\n"
	// An incomplete target FETCH the peer never finishes.
	structureIncompleteFetch = "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\""
)

// structureDefaultBefore is a consumed target FLAGS-only frame and an
// approved control that precede every refused response.
var structureDefaultBefore = []structurePeerPart{{kind: peerConsumed, text: structureFlagsFrame}, {kind: peerApproved, text: structureNoteControl}}

// staticRefusal refuses frame after the default preceding parts; cut < 0
// means the whole frame is consumed before the refusal.
func staticRefusal(frame string, cut int) func(structurePeerRecord) structureRefusalPlan {
	if cut < 0 {
		cut = len(frame)
	}

	return func(structurePeerRecord) structureRefusalPlan {
		return structureRefusalPlan{before: structureDefaultBefore, refused: frame, cut: cut}
	}
}

// nestedStructure is a BODYSTRUCTURE of levels nested one-child multiparts
// around one text part, so its innermost list sits at depth levels+2 of the
// FETCH.
func nestedStructure(levels int) string {
	return "* 1 FETCH (UID 7 BODYSTRUCTURE " + strings.Repeat("(", levels) + "(\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 1 1)" + strings.Repeat(" \"MIXED\")", levels) + ")\r\n"
}

// tokenStructure has exactly tokens lexical tokens: five of the prefix, the
// eight body fields, NIL extensions and the two closing parentheses.
func tokenStructure(tokens int) string {
	return structureSinglePrefix + strings.Repeat(" NIL", tokens-15) + structureListEnd
}

func atomStructure(atom int) string {
	return structureExtPrefix + strings.Repeat("x", atom) + structureListEnd
}

// quotedStructure is a frame of exactly size bytes that ends in one quoted
// extension value.
func quotedStructure(size int) string {
	return structureExtPrefix + "\"" + strings.Repeat("q", size-len(structureExtPrefix)-len(structureQuotedEnd)-1) + structureQuotedEnd
}

// literalStructure is a frame of exactly size bytes that ends in one literal
// extension value, and the length of its prefix through the last declared
// digit.
func literalStructure(size int) (string, int) {
	for digits := 1; digits <= 5; digits++ {
		payload := size - len(structureExtPrefix) - 1 - digits - 3 - len(structureListEnd)
		declared := strconv.Itoa(payload)
		if len(declared) == digits {
			return structureExtPrefix + "{" + declared + "}\r\n" + strings.Repeat("l", payload) + structureListEnd, len(structureExtPrefix) + 1 + digits
		}
	}
	panic("no literal size")
}

// declarationCut is the length of prefix + "{" + digits through the first
// digit at which the declared payload, its "}" CRLF and the closing of depth
// lists and CRLF no longer fit one frame.
func declarationCut(prefix, digits string, depth int) int {
	for count := 1; count <= len(digits); count++ {
		declared, _ := strconv.Atoi(digits[:count])
		consumed := len(prefix) + 1 + count
		if consumed+3+declared+depth+2 > frameBytes {
			return consumed
		}
	}
	panic("declaration fits")
}

// flagsFill returns consumed FLAGS-only frames of about size bytes in total,
// each within one frame and every flag within one atom.
func flagsFill(size int) []structurePeerPart {
	var parts []structurePeerPart
	for size > 0 {
		var flags []string
		frame := 0
		for frame < min(size, 50000) {
			flags = append(flags, "k"+strings.Repeat("a", 3999))
			frame += 4001
		}
		text := "* 1 FETCH (FLAGS (" + strings.Join(flags, " ") + "))\r\n"
		parts = append(parts, structurePeerPart{kind: peerConsumed, text: text})
		size -= len(text)
	}

	return parts
}

// tokenFill returns consumed FLAGS-only frames of exactly tokens lexical
// tokens in total, each at least 5.
func tokenFill(tokens int) []structurePeerPart {
	var parts []structurePeerPart
	for tokens > 0 {
		flags := tokens - 5
		if tokens > 4005 {
			flags = min(4000, tokens-10)
		}
		text := "* 1 FETCH (FLAGS (" + strings.TrimSpace(strings.Repeat(" k", flags)) + "))\r\n"
		parts = append(parts, structurePeerPart{kind: peerConsumed, text: text})
		tokens -= flatFetchTokens(text)
	}

	return parts
}

// partsBytes and partsWork are the peer's own account of
// what scheduled parts cost the quarantine.
func partsBytes(parts []structurePeerPart) int {
	total := 0
	for _, part := range parts {
		total += len(part.text)
	}

	return total
}

// sentWork is the work of everything the peer sent before a schedule: one
// unit per approved control, the flat tokens of each consumed frame.
func sentWork(sent structurePeerRecord) int {
	work := sent.responses - len(sent.consumed)
	for _, frame := range sent.consumed {
		work += flatFetchTokens(frame)
	}

	return work
}

func partsWork(parts []structurePeerPart) int {
	work := 0
	for _, part := range parts {
		if strings.Contains(part.text, " FETCH ") {
			work += flatFetchTokens(part.text)
			continue
		}
		work++
	}

	return work
}

func structureRefusalCases() []structureRefusalCase {
	// The declaration of a literal that would make its frame one byte over,
	// sent without payload.
	over, overCut := literalStructure(frameBytes + 1)
	excessive := structureExtPrefix + "{4294967295}\r\n"

	return []structureRefusalCase{
		// Protocol refusals of the FETCH grammar and its classification.
		{name: "wrong_uid", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 8 BODYSTRUCTURE "+structureSessionTarget+")\r\n", -1)},
		{name: "missing_uid", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (BODYSTRUCTURE "+structureSessionTarget+")\r\n", -1)},
		{name: "duplicate_uid", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 UID 7 BODYSTRUCTURE "+structureSessionTarget+")\r\n", len("* 1 FETCH (UID 7 UID "))},
		{name: "missing_structure", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7)\r\n", -1)},
		{name: "duplicate_structure", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BODYSTRUCTURE "+structureSessionTarget+" BODYSTRUCTURE "+structureSessionTarget+")\r\n", len("* 1 FETCH (UID 7 BODYSTRUCTURE "+structureSessionTarget+" BODYSTRUCTURE "))},
		{name: "duplicate_flags", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 FLAGS (\\Seen) FLAGS (\\Seen) BODYSTRUCTURE "+structureSessionTarget+")\r\n", len("* 1 FETCH (UID 7 FLAGS (\\Seen) FLAGS "))},
		{name: "early_structure", code: CodeIMAPProtocol, search: true, plan: staticRefusal(structureTargetFrame, -1)},
		{name: "second_structure", code: CodeIMAPProtocol, plan: func(structurePeerRecord) structureRefusalPlan {
			before := append(append([]structurePeerPart(nil), structureDefaultBefore...), structurePeerPart{kind: peerApproved, text: structureTargetFrame})
			return structureRefusalPlan{before: before, refused: structureTargetFrame, cut: len(structureTargetFrame)}
		}},
		{name: "bare_body", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BODY "+structureSessionTarget+")\r\n", len("* 1 FETCH (UID 7 BODY "))},
		{name: "body_section", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BODY[] {6}\r\nsecret)\r\n", len("* 1 FETCH (UID 7 BODY[]"))},
		{name: "body_header_fields", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BODY[HEADER.FIELDS (SUBJECT)] {11}\r\nSubject: x\n)\r\n", len("* 1 FETCH (UID 7 BODY[HEADER.FIELDS "))},
		{name: "binary", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BINARY[1] {6}\r\nsecret)\r\n", len("* 1 FETCH (UID 7 BINARY[1]"))},
		{name: "binary_size", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 BINARY.SIZE[1] 6)\r\n", len("* 1 FETCH (UID 7 BINARY.SIZE[1]"))},
		{name: "unknown_attribute", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (UID 7 X-SECRET 1 BODYSTRUCTURE "+structureSessionTarget+")\r\n", len("* 1 FETCH (UID 7 X-SECRET "))},
		{name: "late_body_after_flags", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH (FLAGS (\\Seen) BODY[] {6}\r\nsecret)\r\n", len("* 1 FETCH (FLAGS (\\Seen) BODY[]"))},
		{name: "unclosed_response_code", code: CodeIMAPProtocol, plan: staticRefusal("* OK [ALERT unclosed {5}\r\n", -1)},
		{name: "fetch_without_space", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH(UID 7 BODYSTRUCTURE "+structureSessionTarget+")\r\n", -1)},
		{name: "fetch_leading_zero", code: CodeIMAPProtocol, plan: staticRefusal("* 01 FETCH (UID 7 BODYSTRUCTURE "+structureSessionTarget+")\r\n", len("* 01 FETCH "))},
		{name: "fetch_without_list", code: CodeIMAPProtocol, plan: staticRefusal("* 1 FETCH UID 7 BODYSTRUCTURE "+structureSessionTarget+"\r\n", len("* 1 FETCH U"))},
		{name: "truncation", code: CodeIMAPProtocol, truncate: true, plan: staticRefusal("* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PL", -1)},
		{name: "cancellation_incomplete", code: CodeOperationCanceled, cancel: true, plan: staticRefusal(structureIncompleteFetch, -1)},

		// Resource refusals one over each fixed limit.
		{name: "over_depth", code: CodeBoundsExceeded, plan: staticRefusal(nestedStructure(depthLimit-1), len("* 1 FETCH (UID 7 BODYSTRUCTURE ")+depthLimit)},
		{name: "over_tokens", code: CodeBoundsExceeded, plan: func(structurePeerRecord) structureRefusalPlan {
			frame := tokenStructure(fetchTokens + 1)
			return structureRefusalPlan{before: structureDefaultBefore, refused: frame, cut: len(frame) - len("NIL"+structureListEnd) + 1}
		}},
		{name: "over_atom", code: CodeBoundsExceeded, plan: staticRefusal(atomStructure(atomBytes+1), len(structureExtPrefix)+atomBytes+1)},
		{name: "over_frame_quoted", code: CodeBoundsExceeded, plan: staticRefusal(quotedStructure(frameBytes+1), frameBytes+1-len(structureQuotedEnd))},
		{name: "over_frame_literal", code: CodeBoundsExceeded, plan: staticRefusal(over[:overCut]+"}\r\n", overCut)},
		{name: "excessive_literal_declaration", code: CodeBoundsExceeded, plan: staticRefusal(excessive, declarationCut(structureExtPrefix, "4294967295", 2))},
		{name: "over_control", code: CodeBoundsExceeded, plan: staticRefusal("* OK "+strings.Repeat("c", controlBytes+1-len("* OK \r\n"))+"\r\n", controlBytes-1)},
		{name: "over_session_bytes", code: CodeBoundsExceeded, plan: func(sent structurePeerRecord) structureRefusalPlan {
			// Consumed frames are budgeted input: they leave about 20000
			// bytes, which a quoted value at depth 2 then exceeds.
			fill := flagsFill(sessionBytes - 20000 - sent.written - partsBytes(structureDefaultBefore))
			before := append(append([]structurePeerPart(nil), structureDefaultBefore...), fill...)
			base := sent.written + partsBytes(before)
			frame := structureExtPrefix + "\"" + strings.Repeat("q", 40000) + structureQuotedEnd
			// Refused at the first byte whose count plus the closing quote,
			// two lists and CRLF exceeds the session.
			return structureRefusalPlan{before: before, refused: frame, cut: sessionBytes - 2 - 3 + 1 - base}
		}},
		{name: "over_responses", code: CodeBoundsExceeded, plan: func(sent structurePeerRecord) structureRefusalPlan {
			before := append([]structurePeerPart(nil), structureDefaultBefore...)
			for sent.responses+len(before) < responseLimit {
				before = append(before, structurePeerPart{kind: peerConsumed, text: "* 2 FETCH (FLAGS ())\r\n"})
			}
			return structureRefusalPlan{before: before, refused: structureTargetFrame, cut: 0}
		}},
		{name: "over_work", code: CodeBoundsExceeded, plan: func(sent structurePeerRecord) structureRefusalPlan {
			total := flatFetchTokens(structureTargetFrame)
			fill := tokenFill(workUnits + 1 - total - sentWork(sent) - partsWork(structureDefaultBefore))
			before := append(append([]structurePeerPart(nil), structureDefaultBefore...), fill...)
			// The least completion reaches the frame's own token total at
			// the first byte of its last NIL.
			return structureRefusalPlan{before: before, refused: structureTargetFrame, cut: strings.LastIndex(structureTargetFrame, "NIL") + 1}
		}},
	}
}

func structureBoundaryCases() []structureBoundaryCase {
	single := func(t *testing.T, structure imap.BodyStructure) {
		t.Helper()
		part, ok := structure.(*imap.BodyStructureSinglePart)
		if !ok || part.MediaType() != "text/plain" || part.Size != 1 {
			t.Fatalf("structure = %#v", structure)
		}
	}
	target := func(frame string) func(structurePeerRecord) []structurePeerPart {
		return func(structurePeerRecord) []structurePeerPart {
			return append(append([]structurePeerPart(nil), structureDefaultBefore...), structurePeerPart{kind: peerApproved, text: frame})
		}
	}
	quoted := quotedStructure(frameBytes)
	literal, _ := literalStructure(frameBytes)

	return []structureBoundaryCase{
		{name: "at_depth", plan: target(nestedStructure(depthLimit - 2)), verify: func(t *testing.T, structure imap.BodyStructure) {
			t.Helper()
			for level := 0; level < depthLimit-2; level++ {
				multipart, ok := structure.(*imap.BodyStructureMultiPart)
				if !ok || len(multipart.Children) != 1 {
					t.Fatalf("level %d = %#v", level, structure)
				}
				structure = multipart.Children[0]
			}
			single(t, structure)
		}},
		{name: "at_tokens", plan: target(tokenStructure(fetchTokens)), verify: single},
		{name: "at_atom", plan: target(atomStructure(atomBytes)), verify: single},
		{name: "at_frame_quoted", plan: target(quoted), verify: single},
		{name: "at_frame_literal", plan: target(literal), verify: single},
		{name: "at_control", plan: func(structurePeerRecord) []structurePeerPart {
			control := "* OK " + strings.Repeat("c", controlBytes-len("* OK \r\n")) + "\r\n"
			return []structurePeerPart{{kind: peerApproved, text: control}, {kind: peerApproved, text: structureTargetFrame}}
		}},
		// The fetch completion and both LOGOUT responses complete the
		// session at exactly 64 responses and 8192 work units.
		{name: "at_responses", plan: func(sent structurePeerRecord) []structurePeerPart {
			parts := append([]structurePeerPart(nil), structureDefaultBefore...)
			for sent.responses+len(parts)+1+3 < responseLimit {
				parts = append(parts, structurePeerPart{kind: peerConsumed, text: "* 2 FETCH (FLAGS ())\r\n"})
			}
			return append(parts, structurePeerPart{kind: peerApproved, text: structureTargetFrame})
		}, counted: func(t *testing.T, quarantine *structureQuarantine) {
			t.Helper()
			if quarantine.responses != responseLimit {
				t.Fatalf("responses = %d, want %d", quarantine.responses, responseLimit)
			}
		}},
		{name: "at_work", plan: func(sent structurePeerRecord) []structurePeerPart {
			total := flatFetchTokens(structureTargetFrame)
			fill := tokenFill(workUnits - 3 - total - sentWork(sent) - partsWork(structureDefaultBefore))
			parts := append(append([]structurePeerPart(nil), structureDefaultBefore...), fill...)
			return append(parts, structurePeerPart{kind: peerApproved, text: structureTargetFrame})
		}, counted: func(t *testing.T, quarantine *structureQuarantine) {
			t.Helper()
			if quarantine.work != workUnits {
				t.Fatalf("work = %d, want %d", quarantine.work, workUnits)
			}
		}},
	}
}

// fragmentRefusal coalesces the preceding parts with the first fragment of
// the refused bytes and writes the rest in two more fragments.
func fragmentRefusal(plan structureRefusalPlan, whole bool) []structurePeerWrite {
	refused := plan.refused
	if whole || len(refused) < 3 {
		return []structurePeerWrite{{parts: append(append([]structurePeerPart(nil), plan.before...), structurePeerPart{kind: peerRefused, text: refused})}}
	}

	first, second := len(refused)/3, 2*len(refused)/3
	return []structurePeerWrite{
		{parts: append(append([]structurePeerPart(nil), plan.before...), structurePeerPart{kind: peerRefused, text: refused[:first]})},
		{parts: []structurePeerPart{{kind: peerRefused, text: refused[first:second]}}},
		{parts: []structurePeerPart{{kind: peerRefused, text: refused[second:]}}},
	}
}

func TestStructureSessionTLSRefusal(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"HOME", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME", "TMPDIR"} {
		t.Setenv(name, root)
	}

	for _, mode := range []struct {
		name     string
		startTLS bool
		mode     string
	}{
		{name: "starttls_refusal", startTLS: true, mode: TLSModeStartTLS},
		{name: "implicit_tls_refusal", startTLS: false, mode: TLSModeImplicit},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, test := range structureRefusalCases() {
				t.Run(test.name, func(t *testing.T) {
					runStructureTLSRefusal(t, mode.startTLS, mode.mode, test)
				})
			}
			for _, test := range structureBoundaryCases() {
				t.Run(test.name, func(t *testing.T) {
					runStructureTLSBoundary(t, mode.startTLS, mode.mode, test)
				})
			}
		})
	}
}

// runStructureTLSRefusal runs the actual dedicated stack and pinned client
// over verified TLS against one adverse reply and proves that the decoder saw
// exactly the approved preceding responses and none of the refused bytes.
// Every expectation is derived from the plan and the baseline the peer had
// sent before the schedule, never from how much of the schedule it then
// managed to write: the guard may close the transport as soon as it holds
// the offending prefix. It returns the peer's record.
func runStructureTLSRefusal(t *testing.T, startTLS bool, mode string, test structureRefusalCase) structurePeerRecord {
	t.Helper()

	var plan structureRefusalPlan
	var baseline structurePeerRecord
	sent := make(chan struct{})
	adverse := &structurePeerAdverse{search: test.search, truncate: test.truncate, sent: sent, schedule: func(record structurePeerRecord) []structurePeerWrite {
		baseline = record
		plan = test.plan(record)
		return fragmentRefusal(plan, test.cancel)
	}}
	peer := startStructureTLSPeer(t, startTLS, structurePeerScript{adverse: adverse, wrap: test.wrap})
	config := structureTLSConfig(t, peer.listener.Addr().String(), mode, peer.pin)
	seam, installed := newHeldStructureTapSeam(test.hold, test.held)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	type outcome struct {
		result selectedStructure
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := readSelectedStructure(ctx, config, "INBOX", structurePeerUID, seam)
		done <- outcome{result, err}
	}()
	if test.cancel {
		// The incomplete FETCH was coalesced with its approved predecessor,
		// so once the decoder holds that predecessor the guard consumed the
		// incomplete bytes too and the decoder waits on the next Read.
		<-sent
		installed().awaitBytes(t, plannedApproved(baseline, plan))
		cancel()
	}
	got := <-done
	peer.wait(t)
	record := peer.snapshot()

	if CodeOf(got.err) != test.code || got.err.Error() != test.code {
		t.Fatalf("readSelectedStructure() error = %v, want %s", got.err, test.code)
	}
	if got.result != (selectedStructure{}) {
		t.Fatalf("refused session returned a partial result %#v", got.result)
	}
	if record.err != nil {
		t.Fatalf("peer error = %v", record.err)
	}

	// Zero delivery: the decoder-facing tap holds exactly the independently
	// planned approved responses, any accepted target included, and no byte
	// of the refused response.
	approved := plannedApproved(baseline, plan)
	tap := installed().bytes()
	if !bytes.Equal(tap, approved) {
		t.Fatalf("decoder-facing tap (%d bytes) differs from the planned approved responses (%d bytes)", len(tap), len(approved))
	}

	// Sender evidence: the peer delivered only planned bytes, every one
	// before the refused response and at least the offending prefix of it.
	before := baseline.written + partsBytes(plan.before)
	if !strings.HasPrefix(plan.refused, string(record.refused)) || record.written != before+len(record.refused) {
		t.Fatalf("peer delivered %d bytes with %d refused, planned %d before the refused response", record.written, len(record.refused), before)
	}

	// Guard input: the quarantine consumed every byte before the refused
	// response, consumed FLAGS-only frames included, and exactly the
	// offending prefix through its threshold, and its first category stuck.
	quarantine := installed().quarantine(t)
	quarantine.mu.Lock()
	consumed, terminal := quarantine.sessionBytes, quarantine.terminal
	quarantine.mu.Unlock()
	if want := before + plan.cut; consumed != want {
		t.Fatalf("guard consumed %d bytes, want %d (%d of %d refused bytes)", consumed, want, plan.cut, len(plan.refused))
	}
	if CodeOf(terminal) != test.code {
		t.Fatalf("quarantine terminal = %v, want %s", terminal, test.code)
	}

	// The transport was aborted, not drained or logged out: the peer saw the
	// client close it after the refused command and nothing more.
	if !test.truncate && !record.closed {
		t.Fatal("peer did not observe the client close the transport")
	}
	if len(record.trailing) != 0 {
		t.Fatalf("client wrote %q after the refused command", record.trailing)
	}
	last := strings.ToUpper(record.commands[len(record.commands)-1])
	if test.search != strings.Contains(last, " UID SEARCH ") || !test.search && !strings.Contains(last, " UID FETCH ") {
		t.Fatalf("last command = %q", last)
	}

	return record
}

// plannedApproved is the approved bytes the peer had sent before the
// schedule followed by the approved parts the plan places before the refused
// response.
func plannedApproved(baseline structurePeerRecord, plan structureRefusalPlan) []byte {
	approved := append([]byte(nil), baseline.approved...)
	for _, part := range plan.before {
		if part.kind == peerApproved {
			approved = append(approved, part.text...)
		}
	}

	return approved
}

// runStructureTLSBoundary admits a response at a fixed limit through the
// same stack and requires the exact approved bytes and the decoded result.
func runStructureTLSBoundary(t *testing.T, startTLS bool, mode string, test structureBoundaryCase) {
	t.Helper()

	adverse := &structurePeerAdverse{complete: true, schedule: func(record structurePeerRecord) []structurePeerWrite {
		return []structurePeerWrite{{parts: test.plan(record)}}
	}}
	peer := startStructureTLSPeer(t, startTLS, structurePeerScript{adverse: adverse})
	config := structureTLSConfig(t, peer.listener.Addr().String(), mode, peer.pin)
	seam, installed := newStructureTapSeam()

	result, err := readSelectedStructure(context.Background(), config, "INBOX", structurePeerUID, seam)
	if err != nil {
		t.Fatalf("readSelectedStructure() error = %v", err)
	}
	peer.wait(t)
	record := peer.snapshot()
	if record.err != nil {
		t.Fatalf("peer error = %v", record.err)
	}

	if test.verify != nil {
		test.verify(t, result.Structure)
	} else if result.Structure == nil {
		t.Fatal("no structure")
	}
	if tap := installed().bytes(); !bytes.Equal(tap, record.approved) {
		t.Fatalf("decoder-facing tap (%d bytes) differs from the approved responses (%d bytes)", len(tap), len(record.approved))
	}

	quarantine := installed().quarantine(t)
	quarantine.mu.Lock()
	defer quarantine.mu.Unlock()
	if quarantine.sessionBytes != record.written || quarantine.responses != record.responses {
		t.Fatalf("quarantine counted %d bytes/%d responses, peer sent %d/%d", quarantine.sessionBytes, quarantine.responses, record.written, record.responses)
	}
	if test.counted != nil {
		test.counted(t, quarantine)
	}
	assertStructureCommands(t, record.commands, structurePeerUID)
}

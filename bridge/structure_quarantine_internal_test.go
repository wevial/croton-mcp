package bridge

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

const quarantineTestUID = 7

// The fixed contract of docs/design/0010-structure-quarantine.md, restated
// so a changed limit in the reader fails these tests.
const (
	frameBytes    = 65536
	depthLimit    = 64
	fetchTokens   = 4096
	atomBytes     = 4096
	controlBytes  = 4096
	sessionBytes  = 131072
	responseLimit = 64
	workUnits     = 8192
)

const quarantineTestStructure = "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"CHARSET\" \"utf-8\") NIL NIL \"7BIT\" 12 1 NIL NIL NIL NIL))\r\n"

// quarantineHarness drives a structureQuarantine over a net.Pipe. One writer
// goroutine sends chunks in order; finish closes the peer after them.
type quarantineHarness struct {
	t          *testing.T
	quarantine *structureQuarantine
	writes     chan string
	finished   bool
}

func newQuarantineHarness(t *testing.T) *quarantineHarness {
	t.Helper()

	client, server := net.Pipe()
	if err := client.SetReadDeadline(time.Now().Add(60 * time.Second)); err != nil {
		t.Fatalf("SetReadDeadline() error = %v", err)
	}
	quarantine, err := newStructureQuarantine(client, quarantineTestUID)
	if err != nil {
		t.Fatalf("newStructureQuarantine() error = %v", err)
	}
	harness := &quarantineHarness{t: t, quarantine: quarantine, writes: make(chan string, 4096)}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer server.Close()
		for chunk := range harness.writes {
			if _, err := server.Write([]byte(chunk)); err != nil {
				for range harness.writes {
				}
				return
			}
		}
	}()
	t.Cleanup(func() {
		client.Close()
		server.Close()
		harness.finish()
		<-done
	})
	return harness
}

func (harness *quarantineHarness) send(chunks ...string) {
	for _, chunk := range chunks {
		harness.writes <- chunk
	}
}

func (harness *quarantineHarness) sendBytewise(input string) {
	for index := range len(input) {
		harness.writes <- input[index : index+1]
	}
}

func (harness *quarantineHarness) finish() {
	if !harness.finished {
		harness.finished = true
		close(harness.writes)
	}
}

func (harness *quarantineHarness) dispatch() {
	harness.t.Helper()

	if err := harness.quarantine.beginDispatch(); err != nil {
		harness.t.Fatalf("beginDispatch() error = %v", err)
	}
}

// expect reads exactly want, failing on any error or other bytes.
func (harness *quarantineHarness) expect(want string) {
	harness.t.Helper()

	got := make([]byte, 0, len(want))
	buffer := make([]byte, 1500)
	for len(got) < len(want) {
		count, err := harness.quarantine.Read(buffer[:min(len(buffer), len(want)-len(got))])
		got = append(got, buffer[:count]...)
		if err != nil {
			harness.t.Fatalf("Read() error = %v after %d of %d bytes", err, len(got), len(want))
		}
	}
	if string(got) != want {
		harness.t.Fatalf("released bytes differ from approved input:\n got %q\nwant %q", clip(string(got)), clip(want))
	}
}

// refused requires the next read to release no byte and fail with code.
func (harness *quarantineHarness) refused(code string) {
	harness.t.Helper()

	count, err := harness.quarantine.Read(make([]byte, 4096))
	if count != 0 || CodeOf(err) != code || err.Error() != code {
		harness.t.Fatalf("Read() = %d, %v; want 0 bytes and %q", count, err, code)
	}
}

// releaseExactly reads everything up to the terminal error and requires
// exactly want followed by code.
func (harness *quarantineHarness) releaseExactly(want, code string) {
	harness.t.Helper()

	var got []byte
	var err error
	buffer := make([]byte, 4096)
	for err == nil {
		var count int
		count, err = harness.quarantine.Read(buffer)
		got = append(got, buffer[:count]...)
		if count > 0 && err != nil {
			harness.t.Fatalf("Read() returned %d bytes with error %v", count, err)
		}
	}
	if string(got) != want {
		harness.t.Fatalf("released bytes:\n got %q\nwant %q", clip(string(got)), clip(want))
	}
	if CodeOf(err) != code || err.Error() != code {
		harness.t.Fatalf("terminal error = %v, want %q", err, code)
	}
}

func clip(value string) string {
	if len(value) > 200 {
		return value[:100] + "..." + value[len(value)-100:]
	}

	return value
}

func flagsFrameWithTokens(flags int) string {
	return "* 2 FETCH (FLAGS (" + strings.TrimSuffix(strings.Repeat("a ", flags), " ") + "))\r\n"
}

// flagsFrameOfSize is a FLAGS-only FETCH of exactly size bytes whose flags
// are at most 4000 bytes each.
func flagsFrameOfSize(size int) string {
	prefix, suffix := "* 2 FETCH (FLAGS (", "))\r\n"
	remaining := size - len(prefix) - len(suffix)
	var body strings.Builder
	for remaining > 0 {
		if remaining <= 4000 {
			body.WriteString(strings.Repeat("f", remaining))
			break
		}
		part := min(4000, remaining-2)
		body.WriteString(strings.Repeat("f", part) + " ")
		remaining -= part + 1
	}

	return prefix + body.String() + suffix
}

func controlOfSize(size int) string {
	return "* OK " + strings.Repeat("c", size-7) + "\r\n"
}

// structureWithQuoted and structureWithLiteral are structure FETCH frames of
// exactly size bytes whose last value fills everything but the closing of
// its lists, the largest value a frame of that size can carry. The literal's
// head is everything through its declaration.
func structureWithQuoted(size int) string {
	head, tail := "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL \"", "\"))\r\n"

	return head + strings.Repeat("d", size-len(head)-len(tail)) + tail
}

func structureWithLiteral(size int) (frame, head string, payload int) {
	prefix, tail := "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL ", "))\r\n"
	for payload = size - len(prefix) - len(tail) - 4; ; payload-- {
		head = prefix + "{" + strconv.Itoa(payload) + "}\r\n"
		if len(head)+payload+len(tail) == size {
			return head + strings.Repeat("l", payload) + tail, head, payload
		}
	}
}

func TestStructureQuarantinePrimitive(t *testing.T) {
	t.Run("quarantine_and_envelope", testQuarantineAndEnvelope)
	t.Run("flags_routing", testQuarantineFlagsRouting)
	t.Run("resource_bounds", testQuarantineResourceBounds)
	t.Run("framing_subset", testQuarantineFramingSubset)
	t.Run("sticky_release", testQuarantineStickyRelease)
}

func testQuarantineAndEnvelope(t *testing.T) {
	if _, err := newStructureQuarantine(strings.NewReader(""), 0); CodeOf(err) != CodeInvalidRequest {
		t.Fatalf("zero selected UID error = %v", err)
	}

	const control = "* OK [READ-ONLY] selected\r\n"
	for _, frame := range []string{
		quarantineTestStructure,
		"* 4294967295 FETCH (BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 0 0) UID 7)\r\n",
		"* 3 FETCH (FLAGS (\\Seen) UID 7 BODYSTRUCTURE ((\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 1 1)(\"TEXT\" \"HTML\" NIL NIL NIL \"7BIT\" 1 1) \"ALTERNATIVE\"))\r\n",
		"* 3 fetch (bodystructure (\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 0 0) flags () uid 7)\r\n",
	} {
		t.Run("approved", func(t *testing.T) {
			harness := newQuarantineHarness(t)
			harness.dispatch()

			harness.send(control, frame)
			harness.finish()

			harness.releaseExactly(control+frame, CodeBridgeUnreachable)
		})
	}

	body := "(\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 0 0)"
	for _, test := range []struct {
		name     string
		frame    string
		dispatch bool
	}{
		{name: "early structure", frame: quarantineTestStructure},
		{name: "wrong UID", frame: "* 1 FETCH (UID 8 BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "missing UID", frame: "* 1 FETCH (BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "duplicate UID", frame: "* 1 FETCH (UID 7 UID 7 BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "missing structure", frame: "* 1 FETCH (UID 7)\r\n", dispatch: true},
		{name: "empty list", frame: "* 1 FETCH ()\r\n", dispatch: true},
		{name: "duplicate structure", frame: "* 1 FETCH (UID 7 BODYSTRUCTURE " + body + " BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "duplicate FLAGS", frame: "* 1 FETCH (UID 7 FLAGS () FLAGS () BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "unknown attribute", frame: "* 1 FETCH (UID 7 INTERNALDATE \"01-Jan-2026 00:00:00 +0000\" BODYSTRUCTURE " + body + ")\r\n", dispatch: true},
		{name: "bare BODY", frame: "* 1 FETCH (UID 7 BODY " + body + ")\r\n", dispatch: true},
		{name: "BODY section", frame: "* 1 FETCH (UID 7 BODYSTRUCTURE " + body + " BODY[HEADER] {5}\r\nHello)\r\n", dispatch: true},
		{name: "BINARY", frame: "* 1 FETCH (UID 7 BODYSTRUCTURE " + body + " BINARY[1] {3}\r\nabc)\r\n", dispatch: true},
		{name: "BINARY.SIZE", frame: "* 1 FETCH (UID 7 BODYSTRUCTURE " + body + " BINARY.SIZE[1] 3)\r\n", dispatch: true},
		{name: "late BODY after FLAGS-only prefix", frame: "* 1 FETCH (FLAGS (\\Seen) BODY[] {5}\r\nhello)\r\n", dispatch: true},
		{name: "structure value NIL", frame: "* 1 FETCH (UID 7 BODYSTRUCTURE NIL)\r\n", dispatch: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			if test.dispatch {
				harness.dispatch()
			}

			harness.send(control, test.frame)
			harness.finish()

			harness.releaseExactly(control, CodeIMAPProtocol)
		})
	}

	t.Run("second structure", func(t *testing.T) {
		harness := newQuarantineHarness(t)
		harness.dispatch()

		harness.send(control, quarantineTestStructure, quarantineTestStructure)
		harness.finish()

		harness.releaseExactly(control+quarantineTestStructure, CodeIMAPProtocol)
	})

	t.Run("unfinished prefix withheld", func(t *testing.T) {
		harness := newQuarantineHarness(t)
		harness.dispatch()

		harness.send(control, "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" ")
		harness.expect(control)
		harness.finish()

		harness.refused(CodeIMAPProtocol)
	})
}

func testQuarantineFlagsRouting(t *testing.T) {
	t.Run("consumed before during and after dispatch", func(t *testing.T) {
		harness := newQuarantineHarness(t)
		frames := []string{
			"* 2 FETCH (UID 9 FLAGS (\\Seen))\r\n",
			"* 1 FETCH (FLAGS ())\r\n",
			"* 1 FETCH (UID 7 FLAGS (\\Seen $Label))\r\n",
			"* 1 FETCH (FLAGS (\\Flagged) UID 7)\r\n",
			"* 1 FETCH (UID 7 FLAGS (\\Deleted))\r\n",
		}
		wantBytes := 0
		for _, frame := range append(frames, "* OK m1\r\n", "* OK m2\r\n", "* OK m3\r\n", quarantineTestStructure) {
			wantBytes += len(frame)
		}

		harness.send(frames[0], "* OK m1\r\n")
		harness.expect("* OK m1\r\n")
		if harness.quarantine.dispatch != quarantineIdle {
			t.Fatal("FLAGS-only frame changed the dispatch record")
		}
		harness.dispatch()
		harness.send(frames[1], frames[2], frames[3], "* OK m2\r\n")
		harness.expect("* OK m2\r\n")
		if harness.quarantine.dispatch != quarantineActive {
			t.Fatal("FLAGS-only frame for the target consumed the structure slot")
		}
		harness.send(quarantineTestStructure)
		harness.expect(quarantineTestStructure)
		harness.sendBytewise(frames[4] + "* OK m3\r\n")
		harness.expect("* OK m3\r\n")

		quarantine := harness.quarantine
		if quarantine.dispatch != quarantineDelivered {
			t.Fatalf("dispatch = %v, want delivered", quarantine.dispatch)
		}
		if quarantine.tokens != 8 {
			t.Fatalf("consumed FLAGS-only frame counted %d tokens, want 8", quarantine.tokens)
		}
		if want := 8 + 5 + 9 + 8 + 22 + 8 + 3; quarantine.work != want {
			t.Fatalf("work = %d, want %d including consumed FLAGS", quarantine.work, want)
		}
		if quarantine.responses != 9 || quarantine.sessionBytes != wantBytes {
			t.Fatalf("responses = %d, session bytes = %d; want 9, %d", quarantine.responses, quarantine.sessionBytes, wantBytes)
		}
		harness.finish()
		harness.refused(CodeBridgeUnreachable)
	})

	for _, test := range []struct {
		name  string
		frame string
	}{
		{name: "bare backslash flag", frame: "* 1 FETCH (FLAGS (\\))\r\n"},
		{name: "wildcard flag", frame: "* 1 FETCH (FLAGS (\\*))\r\n"},
		{name: "quoted flag", frame: "* 1 FETCH (FLAGS (\"Seen\"))\r\n"},
		{name: "literal flag", frame: "* 1 FETCH (FLAGS ({4}\r\nSeen))\r\n"},
		{name: "nested flag list", frame: "* 1 FETCH (FLAGS ((\\Seen)))\r\n"},
		{name: "FLAGS NIL", frame: "* 1 FETCH (FLAGS NIL)\r\n"},
		{name: "zero UID", frame: "* 1 FETCH (UID 0 FLAGS ())\r\n"},
		{name: "other attribute", frame: "* 1 FETCH (FLAGS () MODSEQ (5))\r\n"},
		{name: "leading space in list", frame: "* 1 FETCH (FLAGS ( \\Seen))\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			harness.dispatch()

			harness.send("* OK m\r\n", test.frame, quarantineTestStructure)
			harness.finish()

			harness.releaseExactly("* OK m\r\n", CodeIMAPProtocol)
		})
	}
}

func testQuarantineResourceBounds(t *testing.T) {
	type boundCase struct {
		name     string
		dispatch bool
		input    []string
		want     string
		check    func(*testing.T, *structureQuarantine)
	}
	literalAt, _, _ := structureWithLiteral(frameBytes)
	// After this "{" a frame at depth 2 still needs a digit, "}", CRLF, two
	// closing parentheses and CRLF: eight bytes, one more than remain.
	literalOpening := strings.TrimSuffix(structureWithQuoted(frameBytes-5), "\"))\r\n") + "\" {"
	literalOver, literalOverHead, _ := structureWithLiteral(frameBytes + 1)
	nested := func(depth int) string {
		return "* 1 FETCH (UID 7 BODYSTRUCTURE " + strings.Repeat("(", depth-1) + strings.Repeat(")", depth-1) + ")\r\n"
	}
	responses := func(count int) ([]string, string) {
		var input []string
		var want strings.Builder
		for index := range count {
			if index%2 == 0 {
				input = append(input, "* 2 FETCH (FLAGS (\\Seen))\r\n")
				continue
			}
			control := "* OK r" + strconv.Itoa(index) + "\r\n"
			input = append(input, control)
			want.WriteString(control)
		}
		return input, want.String()
	}
	responsesAt, responsesAtWant := responses(responseLimit)
	responsesOver, _ := responses(responseLimit + 1)
	sessionTail := sessionBytes - frameBytes - controlBytes
	maximumAllocation := func(t *testing.T, quarantine *structureQuarantine) {
		if cap(quarantine.frame) > frameBytes {
			t.Fatalf("raw frame capacity %d exceeds %d", cap(quarantine.frame), frameBytes)
		}
	}

	at := []boundCase{
		{name: "frame bytes with quoted value", dispatch: true, input: []string{structureWithQuoted(frameBytes)}, want: structureWithQuoted(frameBytes), check: maximumAllocation},
		{name: "frame bytes with literal value", dispatch: true, input: []string{literalAt}, want: literalAt, check: maximumAllocation},
		{name: "depth", dispatch: true, input: []string{nested(depthLimit)}, want: nested(depthLimit)},
		{name: "tokens per FETCH", input: []string{flagsFrameWithTokens(fetchTokens - 5)}, check: func(t *testing.T, quarantine *structureQuarantine) {
			if quarantine.tokens != fetchTokens {
				t.Fatalf("tokens = %d", quarantine.tokens)
			}
		}},
		{name: "atom", input: []string{"* 2 FETCH (FLAGS (" + strings.Repeat("a", atomBytes) + "))\r\n"}},
		{name: "control line", input: []string{controlOfSize(controlBytes)}, want: controlOfSize(controlBytes)},
		{name: "session bytes", input: []string{flagsFrameOfSize(frameBytes), flagsFrameOfSize(sessionTail), controlOfSize(controlBytes)}, want: controlOfSize(controlBytes), check: func(t *testing.T, quarantine *structureQuarantine) {
			if quarantine.sessionBytes != sessionBytes {
				t.Fatalf("session bytes = %d", quarantine.sessionBytes)
			}
		}},
		{name: "responses", input: responsesAt, want: responsesAtWant, check: func(t *testing.T, quarantine *structureQuarantine) {
			if quarantine.responses != responseLimit {
				t.Fatalf("responses = %d", quarantine.responses)
			}
		}},
		{name: "work units", input: []string{"* OK w\r\n", flagsFrameWithTokens(fetchTokens - 5), flagsFrameWithTokens(fetchTokens - 6)}, want: "* OK w\r\n", check: func(t *testing.T, quarantine *structureQuarantine) {
			if quarantine.work != workUnits {
				t.Fatalf("work = %d", quarantine.work)
			}
		}},
	}
	over := []boundCase{
		{name: "frame bytes with quoted value", dispatch: true, input: []string{structureWithQuoted(frameBytes + 1)}, check: maximumAllocation},
		{name: "frame bytes with literal value", dispatch: true, input: []string{literalOver}},
		{name: "literal declaration without payload", dispatch: true, input: []string{literalOverHead}},
		{name: "literal declaration digits without payload", dispatch: true, input: []string{strings.TrimSuffix(literalOverHead, "}\r\n")}},
		{name: "literal opening without digit", dispatch: true, input: []string{literalOpening}},
		{name: "depth", dispatch: true, input: []string{nested(depthLimit + 1)}},
		{name: "tokens per FETCH", input: []string{flagsFrameWithTokens(fetchTokens - 4)}},
		{name: "atom", input: []string{"* 2 FETCH (FLAGS (" + strings.Repeat("a", atomBytes+1) + "))\r\n"}},
		{name: "control line", input: []string{controlOfSize(controlBytes + 1)}},
		{name: "session bytes", input: []string{flagsFrameOfSize(frameBytes), flagsFrameOfSize(sessionTail + 1), controlOfSize(controlBytes)}, check: func(t *testing.T, quarantine *structureQuarantine) {
			if quarantine.sessionBytes > sessionBytes {
				t.Fatalf("session bytes = %d", quarantine.sessionBytes)
			}
		}},
		{name: "responses", input: responsesOver, want: responsesAtWant},
		{name: "work units", input: []string{"* OK w\r\n", flagsFrameWithTokens(fetchTokens - 5), flagsFrameWithTokens(fetchTokens - 5)}, want: "* OK w\r\n"},
		{name: "work units by control", input: []string{flagsFrameWithTokens(fetchTokens - 5), flagsFrameWithTokens(fetchTokens - 5), "* OK w\r\n"}},
	}

	for _, test := range at {
		t.Run("at/"+test.name, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			if test.dispatch {
				harness.dispatch()
			}

			harness.send(test.input...)
			harness.finish()

			harness.releaseExactly(test.want, CodeBridgeUnreachable)
			if test.check != nil {
				test.check(t, harness.quarantine)
			}
		})
	}
	// Over cases never close the peer: the refusal must arrive without
	// waiting for input that could not be admitted.
	for _, test := range over {
		t.Run("over/"+test.name, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			if test.dispatch {
				harness.dispatch()
			}

			harness.send(test.input...)

			if test.want != "" {
				harness.expect(test.want)
			}
			harness.refused(CodeBoundsExceeded)
			harness.refused(CodeBoundsExceeded)
			if harness.quarantine.frame != nil {
				t.Fatal("refusal retained raw frame bytes")
			}
			if test.check != nil {
				test.check(t, harness.quarantine)
			}
		})
	}
}

func testQuarantineFramingSubset(t *testing.T) {
	controls := strings.Join([]string{
		"* OK [PERMANENTFLAGS (\\Seen \\*)] ready",
		"* NO notice {5}",
		"* BAD [x] text \"open",
		"* BYE closing",
		"* PREAUTH ok",
		"* ok",
		"* OK FETCH completed",
		"* NO FETCH (failed {5}",
		"* BYE fetch ( later",
		"* CAPABILITY IMAP4rev1 AUTH=PLAIN MOVE",
		"* FLAGS (\\Seen \\Answered $Label)",
		"* FLAGS ()",
		"* 0 EXISTS",
		"* 0 RECENT",
		"* 12 exists",
		"* 3 EXPUNGE",
		"* SEARCH",
		"* SEARCH 1 2 (MODSEQ 9)",
		"* ESEARCH (TAG \"A\\\"1\") UID ALL 1:3",
		"+",
		"+ go ahead {3}",
		"+ [ALERT] ( unbalanced",
		"A1 OK done",
		"a.2 no failed [x",
		"A3 BAD bad",
	}, "\r\n") + "\r\n"
	payload := ")\r\n* 2 FETCH (BODY[] {5}\r\nfake\") \"x\\\"\r\n"
	literal := "{" + strconv.Itoa(len(payload)) + "}\r\n" + payload
	structure := "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" (\"NAME\" \"a\\\"b\\\\c\") NIL " + literal + " \"7BIT\" 0 0))\r\n"
	flags := "* 1 FETCH (UID 7 FLAGS (\\Seen))\r\n"

	for _, delivery := range []string{"coalesced", "fragmented"} {
		t.Run(delivery, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			harness.dispatch()
			input := controls + flags + structure + "A4 OK fetched\r\n"

			if delivery == "coalesced" {
				harness.send(input)
			} else {
				harness.sendBytewise(input)
			}
			harness.finish()

			harness.releaseExactly(controls+structure+"A4 OK fetched\r\n", CodeBridgeUnreachable)
		})
	}

	head := "* 1 FETCH (UID 7 BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL "
	for _, test := range []struct {
		name  string
		input string
		code  string
	}{
		{name: "bare LF control", input: "* OK x\n"},
		{name: "bare LF FETCH", input: "* 1 FETCH (FLAGS ())\n"},
		{name: "interior CR control", input: "* OK a\rb\r\n"},
		{name: "interior CR FETCH", input: "* 1 FETCH (FLAGS ()\r)\r\n"},
		{name: "quoted CR", input: head + "\"a\rb\" \"7BIT\" 0 0))\r\n"},
		{name: "quoted LF", input: head + "\"a\nb\" \"7BIT\" 0 0))\r\n"},
		{name: "quoted escape", input: head + "\"a\\x\" \"7BIT\" 0 0))\r\n"},
		{name: "literal8", input: head + "~{3}\r\nabc \"7BIT\" 0 0))\r\n"},
		{name: "nonsynchronizing literal", input: head + "{3+}\r\nabc \"7BIT\" 0 0))\r\n"},
		{name: "negative literal", input: head + "{-1}\r\n \"7BIT\" 0 0))\r\n"},
		{name: "empty literal declaration", input: head + "{}\r\n \"7BIT\" 0 0))\r\n"},
		{name: "overflow literal", input: head + "{4294967296}\r\n", code: CodeBoundsExceeded},
		{name: "long literal declaration", input: head + "{00000000001}\r\na"},
		{name: "literal without CRLF", input: head + "{1}a \"7BIT\" 0 0))\r\n"},
		{name: "unterminated quote", input: head + "\"abc"},
		{name: "unterminated escape", input: head + "\"abc\\"},
		{name: "truncated literal", input: head + "{10}\r\nabc"},
		{name: "unknown control", input: "* LIST () \"/\" INBOX\r\n"},
		{name: "unknown word", input: "* XYZZY\r\n"},
		{name: "numbered status", input: "* 1 NO x\r\n"},
		{name: "zero EXPUNGE", input: "* 0 EXPUNGE\r\n"},
		{name: "EXISTS leading zero", input: "* 01 EXISTS\r\n"},
		{name: "EXISTS trailing space", input: "* 1 EXISTS \r\n"},
		{name: "missing sequence FETCH", input: "* FETCH (FLAGS ())\r\n"},
		{name: "zero sequence FETCH", input: "* 0 FETCH (FLAGS ())\r\n"},
		{name: "overflow sequence FETCH", input: "* 4294967296 FETCH (FLAGS ())\r\n"},
		{name: "leading zero sequence FETCH", input: "* 01 FETCH (FLAGS ())\r\n"},
		{name: "word sequence FETCH", input: "* x FETCH (FLAGS ())\r\n"},
		{name: "FETCH without list", input: "* 1 FETCH\r\n"},
		{name: "FETCH unparenthesized", input: "* 1 FETCH FLAGS ()\r\n"},
		{name: "FETCH unspaced list", input: "* 1 FETCH(FLAGS ())\r\n"},
		{name: "FETCH double space", input: "* 1 FETCH (FLAGS  ())\r\n"},
		{name: "FETCH trailing data", input: "* 1 FETCH (FLAGS ()) x\r\n"},
		{name: "CAPABILITY literal", input: "* CAPABILITY IMAP4rev1 {5}\r\nhello\r\n"},
		{name: "SEARCH literal", input: "* SEARCH {3}\r\n"},
		{name: "SEARCH unfinished quote", input: "* SEARCH \"abc\r\n"},
		{name: "SEARCH quoted brace", input: "* SEARCH \"{3}\"\r\n"},
		{name: "SEARCH closing brace", input: "* SEARCH }\r\n"},
		{name: "ESEARCH quoted closing brace", input: "* ESEARCH (TAG \"}\") UID ALL 1\r\n"},
		{name: "FLAGS control quoted", input: "* FLAGS (\"x\")\r\n"},
		{name: "CAPABILITY closing brace", input: "* CAPABILITY }\r\n"},
		{name: "CAPABILITY opening brace", input: "* CAPABILITY IMAP4rev1 X{\r\n"},
		{name: "FLAGS control closing brace", input: "* FLAGS (})\r\n"},
		{name: "FLAGS control brace in keyword", input: "* FLAGS (\\Seen a}b)\r\n"},
		{name: "unclosed status code", input: "* OK [ALERT notice\r\n"},
		{name: "unclosed continuation code", input: "+ [ALERT notice\r\n"},
		{name: "continuation without space", input: "+x\r\n"},
		{name: "tag with plus", input: "A+ OK done\r\n"},
		{name: "tagged unknown status", input: "A1 PREAUTH done\r\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			harness.dispatch()

			harness.send("* OK m\r\n", test.input)
			harness.finish()

			code := test.code
			if code == "" {
				code = CodeIMAPProtocol
			}
			harness.releaseExactly("* OK m\r\n", code)
		})
	}
}

// sourceFunc is a test source for a final read that returns bytes and an
// error together.
type sourceFunc func([]byte) (int, error)

func (source sourceFunc) Read(buffer []byte) (int, error) {
	return source(buffer)
}

func testQuarantineStickyRelease(t *testing.T) {
	const control = "* OK m\r\n"
	const refused = "* 1 FETCH (UID 7 BODY[] {6}\r\nsecret BODYSTRUCTURE (\"TEXT\" \"PLAIN\" NIL NIL NIL \"7BIT\" 0 0))\r\n"

	t.Run("refusal is fixed through later input abort and dispatch", func(t *testing.T) {
		harness := newQuarantineHarness(t)
		harness.dispatch()

		harness.send(control + "* 1 FETCH (FLAGS (\\Seen))\r\n" + refused + quarantineTestStructure + control)

		harness.expect(control)
		harness.refused(CodeIMAPProtocol)
		if err := harness.quarantine.abort(context.Canceled); CodeOf(err) != CodeIMAPProtocol {
			t.Fatalf("abort() = %v", err)
		}
		if err := harness.quarantine.beginDispatch(); CodeOf(err) != CodeIMAPProtocol {
			t.Fatalf("beginDispatch() = %v", err)
		}
		for range 3 {
			harness.refused(CodeIMAPProtocol)
		}
	})

	for _, test := range []struct {
		cause error
		code  string
	}{
		{cause: context.Canceled, code: CodeOperationCanceled},
		{cause: context.DeadlineExceeded, code: CodeCommandTimedOut},
	} {
		t.Run("cancellation first "+test.code, func(t *testing.T) {
			harness := newQuarantineHarness(t)
			harness.dispatch()

			if err := harness.quarantine.abort(test.cause); CodeOf(err) != test.code {
				t.Fatalf("abort() = %v", err)
			}
			harness.send(control, refused)

			harness.refused(test.code)
			harness.refused(test.code)
		})
	}

	t.Run("abort during blocked read releases nothing", func(t *testing.T) {
		harness := newQuarantineHarness(t)
		type result struct {
			count int
			err   error
		}
		results := make(chan result, 1)

		go func() {
			count, err := harness.quarantine.Read(make([]byte, 64))
			results <- result{count, err}
		}()
		time.Sleep(10 * time.Millisecond)
		harness.quarantine.abort(context.Canceled)
		harness.send(control)

		got := <-results
		if got.count != 0 || CodeOf(got.err) != CodeOperationCanceled {
			t.Fatalf("Read() = %d, %v", got.count, got.err)
		}
	})

	for _, test := range []struct {
		name  string
		input string
		want  string
		code  string
	}{
		{name: "bytes with error between responses", input: control, want: control, code: CodeBridgeUnreachable},
		{name: "bytes with error inside a frame", input: control + "* 1 FETCH (FLAGS (\\Seen", want: control, code: CodeIMAPProtocol},
		{name: "bytes with error after consumed flags", input: control + "* 1 FETCH (FLAGS (\\Seen))\r\n", want: control, code: CodeBridgeUnreachable},
	} {
		t.Run(test.name, func(t *testing.T) {
			sent := false
			source := sourceFunc(func(buffer []byte) (int, error) {
				if sent {
					t.Fatal("reader read again after its source failed")
				}
				sent = true
				return copy(buffer, test.input), errors.New("peer secret text")
			})
			quarantine, err := newStructureQuarantine(source, quarantineTestUID)
			if err != nil {
				t.Fatal(err)
			}

			got, err := io.ReadAll(quarantine)

			if string(got) != test.want || CodeOf(err) != test.code || err.Error() != test.code {
				t.Fatalf("ReadAll() = %q, %v; want %q, %q", got, err, test.want, test.code)
			}
			for range 2 {
				if count, err := quarantine.Read(make([]byte, 8)); count != 0 || err.Error() != test.code {
					t.Fatalf("repeated Read() = %d, %v", count, err)
				}
			}
		})
	}
}

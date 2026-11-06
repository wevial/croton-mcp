package bridge

import (
	"context"
	"errors"
	"io"
	"math"
	"strconv"
	"strings"
	"sync"
)

// Limits of the structure quarantine. docs/design/0010-structure-quarantine.md
// fixes each one; none is reset during a reader's lifetime.
const (
	quarantineFrameBytes   = 65536
	quarantineDepth        = 64
	quarantineFetchTokens  = 4096
	quarantineAtomBytes    = 4096
	quarantineControlBytes = 4096
	quarantineSessionBytes = 131072
	quarantineResponses    = 64
	quarantineWorkUnits    = 8192
	quarantineReadBytes    = 4096
)

type quarantineDispatch int

const (
	quarantineIdle quarantineDispatch = iota
	quarantineActive
	quarantineDelivered
)

type quarantineMode int

const (
	// quarantineBetween is the boundary between two responses.
	quarantineBetween quarantineMode = iota
	// quarantineLine is a response read as one flat CRLF line: a control,
	// or the prefix of a FETCH until its name is known.
	quarantineLine
	quarantineFetch
)

type fetchState int

const (
	fetchOpen fetchState = iota
	fetchItems
	fetchAtom
	fetchQuoted
	fetchEscape
	fetchLiteralSize
	fetchLiteralCR
	fetchLiteralLF
	fetchLiteral
	fetchEndCR
	fetchEndLF
)

// fetchPrevious is the last lexical element of a FETCH list, which decides
// what may follow it.
type fetchPrevious int

const (
	previousOpen fetchPrevious = iota
	previousSpace
	previousToken
	previousClose
)

// fetchItemPhase is the position within the top-level FETCH attribute list.
type fetchItemPhase int

const (
	itemName fetchItemPhase = iota
	itemNamed
	itemValue
	itemDone
)

type fetchItem int

const (
	itemUID fetchItem = iota
	itemFlags
	itemStructure
)

// structureQuarantine is a private, blocking pre-decoder reader for one
// dedicated session that fetches the BODYSTRUCTURE of one selected UID. It
// frames every server response completely before releasing it, releases
// approved controls and exactly one approved structure FETCH byte for byte,
// and consumes whole validated FLAGS-only FETCH frames so they cannot take
// the pending client's single delivery for that UID. Every other FETCH, and
// any input it cannot frame within its budgets, is a sticky refusal that
// releases none of that frame's bytes.
//
// It has no production caller; docs/design/0010-structure-quarantine.md
// records the integration that must precede one.
type structureQuarantine struct {
	source      io.Reader
	selectedUID uint32

	// readMu serializes Read so only one source read is outstanding.
	readMu  sync.Mutex
	scratch []byte

	mu       sync.Mutex
	dispatch quarantineDispatch
	pending  []byte
	terminal error

	sessionBytes int
	responses    int
	work         int

	mode       quarantineMode
	frame      []byte
	lineSpaces int
	lineCR     bool

	state     fetchState
	depth     int
	tokens    int
	previous  fetchPrevious
	phase     fetchItemPhase
	item      fetchItem
	flagsList bool
	atomStart int
	literal   uint64
	digits    int
	uidCount  int
	uid       uint32
	flags     int
	structure int
}

func newStructureQuarantine(source io.Reader, selectedUID uint32) (*structureQuarantine, error) {
	if source == nil || selectedUID == 0 {
		return nil, errorCode(CodeInvalidRequest)
	}

	return &structureQuarantine{
		source:      source,
		selectedUID: selectedUID,
		scratch:     make([]byte, quarantineReadBytes),
	}, nil
}

// beginDispatch records the single structure dispatch. Only a structure
// FETCH that completes after it, and before another has been released, can be
// released. A second dispatch is a sticky protocol refusal.
func (quarantine *structureQuarantine) beginDispatch() error {
	quarantine.mu.Lock()
	defer quarantine.mu.Unlock()

	if quarantine.terminal != nil {
		return quarantine.terminal
	}
	if quarantine.dispatch != quarantineIdle {
		quarantine.refuseLocked(errorCode(CodeIMAPProtocol))
		return quarantine.terminal
	}

	quarantine.dispatch = quarantineActive
	return nil
}

// abort ends the reader with the fixed context category of cause unless a
// refusal or earlier abort already ended it. It neither drains nor resets.
func (quarantine *structureQuarantine) abort(cause error) error {
	quarantine.mu.Lock()
	defer quarantine.mu.Unlock()

	if quarantine.terminal != nil {
		return quarantine.terminal
	}

	if errors.Is(cause, context.Canceled) {
		quarantine.refuseLocked(errorCode(CodeOperationCanceled))
	} else {
		quarantine.refuseLocked(errorCode(CodeCommandTimedOut))
	}
	return quarantine.terminal
}

// Read returns approved bytes before any terminal error, and never returns
// bytes together with an error. Bytes of a refused or unfinished frame are
// never returned.
func (quarantine *structureQuarantine) Read(buffer []byte) (int, error) {
	quarantine.readMu.Lock()
	defer quarantine.readMu.Unlock()

	quarantine.mu.Lock()
	for {
		if len(quarantine.pending) > 0 {
			count := copy(buffer, quarantine.pending)
			quarantine.pending = quarantine.pending[count:]
			if len(quarantine.pending) == 0 {
				quarantine.pending = nil
			}
			quarantine.mu.Unlock()
			return count, nil
		}
		if quarantine.terminal != nil {
			err := quarantine.terminal
			quarantine.mu.Unlock()
			return 0, err
		}
		if len(buffer) == 0 {
			quarantine.mu.Unlock()
			return 0, nil
		}

		// One byte past the session budget is read only to learn whether
		// the peer sends an overage; it is refused, never released.
		limit := min(len(quarantine.scratch), quarantineSessionBytes-quarantine.sessionBytes+1)
		quarantine.mu.Unlock()

		count, err := quarantine.source.Read(quarantine.scratch[:limit])

		quarantine.mu.Lock()
		if quarantine.terminal != nil {
			continue
		}
		quarantine.acceptLocked(quarantine.scratch[:count])
		if quarantine.terminal == nil && err != nil {
			quarantine.sourceFailedLocked()
		}
	}
}

func (quarantine *structureQuarantine) acceptLocked(input []byte) {
	for _, value := range input {
		if err := quarantine.feedLocked(value); err != nil {
			quarantine.refuseLocked(err)
			return
		}
	}
}

// sourceFailedLocked ends the reader on a source error without its text: a
// partial response is truncated input, and a failure between responses is
// the existing unreachable category.
func (quarantine *structureQuarantine) sourceFailedLocked() {
	if quarantine.mode != quarantineBetween {
		quarantine.refuseLocked(errorCode(CodeIMAPProtocol))
		return
	}

	quarantine.refuseLocked(errorCode(CodeBridgeUnreachable))
}

// refuseLocked makes err sticky and drops the raw frame. Earlier approved
// bytes stay pending; nothing of the current frame is ever released.
func (quarantine *structureQuarantine) refuseLocked(err error) {
	quarantine.terminal = err
	quarantine.frame = nil
}

func (quarantine *structureQuarantine) feedLocked(value byte) error {
	if quarantine.sessionBytes == quarantineSessionBytes {
		return errorCode(CodeBoundsExceeded)
	}
	if quarantine.mode == quarantineBetween {
		if quarantine.responses == quarantineResponses || quarantine.work == quarantineWorkUnits {
			return errorCode(CodeBoundsExceeded)
		}
		quarantine.startResponseLocked()
	}

	quarantine.sessionBytes++
	quarantine.growFrameLocked()
	quarantine.frame = append(quarantine.frame, value)

	if quarantine.mode == quarantineLine {
		return quarantine.lineByteLocked(value)
	}
	if err := quarantine.fetchByteLocked(value); err != nil {
		return err
	}
	if quarantine.mode == quarantineFetch {
		return quarantine.fetchFeasibleLocked()
	}
	return nil
}

// growFrameLocked keeps raw storage within one complete frame: a byte is
// appended only after the budgets admitted it, so capacity never needs to
// exceed quarantineFrameBytes.
func (quarantine *structureQuarantine) growFrameLocked() {
	if len(quarantine.frame) < cap(quarantine.frame) {
		return
	}

	grown := make([]byte, len(quarantine.frame), min(max(2*cap(quarantine.frame), 256), quarantineFrameBytes))
	copy(grown, quarantine.frame)
	quarantine.frame = grown
}

func (quarantine *structureQuarantine) startResponseLocked() {
	quarantine.responses++
	quarantine.mode = quarantineLine
	quarantine.frame = quarantine.frame[:0]
	quarantine.lineSpaces = 0
	quarantine.lineCR = false
}

// lineByteLocked frames one flat line. The third space of an untagged line
// decides whether it is the prefix of a FETCH, which is then framed as a
// FETCH and never as a control.
func (quarantine *structureQuarantine) lineByteLocked(value byte) error {
	switch {
	case quarantine.lineCR:
		if value != '\n' {
			return errorCode(CodeIMAPProtocol)
		}
		return quarantine.completeLineLocked()
	case value == '\r':
		quarantine.lineCR = true
	case value == '\n':
		return errorCode(CodeIMAPProtocol)
	case value == ' ' && quarantine.frame[0] == '*':
		quarantine.lineSpaces++
		if quarantine.lineSpaces == 3 {
			fetch, err := fetchPrefix(quarantine.frame)
			if err != nil {
				return err
			}
			if fetch {
				quarantine.startFetchLocked()
				return quarantine.fetchFeasibleLocked()
			}
		}
	}

	need := 2
	if quarantine.lineCR {
		need = 1
	}
	if len(quarantine.frame)+need > quarantineControlBytes || quarantine.sessionBytes+need > quarantineSessionBytes {
		return errorCode(CodeBoundsExceeded)
	}
	return nil
}

// fetchPrefix reports whether prefix, "* " and two words each followed by a
// space, starts a FETCH. A FETCH needs a positive uint32 sequence number.
func fetchPrefix(prefix []byte) (bool, error) {
	number, name, _ := strings.Cut(string(prefix[2:len(prefix)-1]), " ")
	if !strings.EqualFold(name, "FETCH") {
		return false, nil
	}
	if _, ok := quarantineNumber(number, false); !ok {
		return false, errorCode(CodeIMAPProtocol)
	}

	return true, nil
}

func (quarantine *structureQuarantine) completeLineLocked() error {
	if !quarantineControl(string(quarantine.frame[:len(quarantine.frame)-2])) {
		return errorCode(CodeIMAPProtocol)
	}

	quarantine.work++
	quarantine.releaseLocked()
	return nil
}

func (quarantine *structureQuarantine) releaseLocked() {
	quarantine.pending = append(quarantine.pending, quarantine.frame...)
	quarantine.mode = quarantineBetween
	quarantine.frame = quarantine.frame[:0]
}

func (quarantine *structureQuarantine) startFetchLocked() {
	quarantine.mode = quarantineFetch
	quarantine.state = fetchOpen
	quarantine.depth = 0
	quarantine.tokens = 0
	quarantine.phase = itemName
	quarantine.flagsList = false
	quarantine.uidCount = 0
	quarantine.uid = 0
	quarantine.flags = 0
	quarantine.structure = 0
}

func (quarantine *structureQuarantine) fetchByteLocked(value byte) error {
	switch quarantine.state {
	case fetchOpen:
		if value != '(' {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.state = fetchItems
		return quarantine.openLocked()
	case fetchItems:
		return quarantine.itemsByteLocked(value)
	case fetchAtom:
		if quarantineAtomChar(value) {
			if len(quarantine.frame)-quarantine.atomStart > quarantineAtomBytes {
				return errorCode(CodeBoundsExceeded)
			}
			return nil
		}
		if err := quarantine.endAtomLocked(len(quarantine.frame) - 1); err != nil {
			return err
		}
		quarantine.state = fetchItems
		return quarantine.itemsByteLocked(value)
	case fetchQuoted:
		switch value {
		case '"':
			quarantine.state = fetchItems
			quarantine.previous = previousToken
		case '\\':
			quarantine.state = fetchEscape
		case '\r', '\n', 0:
			return errorCode(CodeIMAPProtocol)
		}
		return nil
	case fetchEscape:
		if value != '"' && value != '\\' {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.state = fetchQuoted
		return nil
	case fetchLiteralSize:
		return quarantine.literalSizeByteLocked(value)
	case fetchLiteralCR:
		if value != '\r' {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.state = fetchLiteralLF
		return nil
	case fetchLiteralLF:
		if value != '\n' {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.state = fetchLiteral
		if quarantine.literal == 0 {
			quarantine.state = fetchItems
			quarantine.previous = previousToken
		}
		return nil
	case fetchLiteral:
		quarantine.literal--
		if quarantine.literal == 0 {
			quarantine.state = fetchItems
			quarantine.previous = previousToken
		}
		return nil
	case fetchEndCR:
		if value != '\r' {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.state = fetchEndLF
		return nil
	default:
		if value != '\n' {
			return errorCode(CodeIMAPProtocol)
		}
		return quarantine.completeFetchLocked()
	}
}

// itemsByteLocked reads one byte between lexical tokens of the FETCH list.
// Elements are separated by one space; within a structure a list may also
// directly follow a closed list, as nested multipart bodies do.
func (quarantine *structureQuarantine) itemsByteLocked(value byte) error {
	previous := quarantine.previous
	switch {
	case value == ' ':
		if previous != previousToken && previous != previousClose {
			return errorCode(CodeIMAPProtocol)
		}
		if quarantine.depth == 1 {
			switch quarantine.phase {
			case itemNamed:
				quarantine.phase = itemValue
			case itemDone:
				quarantine.phase = itemName
			default:
				return errorCode(CodeIMAPProtocol)
			}
		}
		quarantine.previous = previousSpace
		return nil
	case value == '(':
		nested := previous == previousClose && quarantine.depth >= 2 && !quarantine.flagsList
		if previous != previousOpen && previous != previousSpace && !nested {
			return errorCode(CodeIMAPProtocol)
		}
		if quarantine.depth == 1 {
			if quarantine.phase != itemValue || quarantine.item == itemUID {
				return errorCode(CodeIMAPProtocol)
			}
			quarantine.flagsList = quarantine.item == itemFlags
		} else if quarantine.flagsList {
			return errorCode(CodeIMAPProtocol)
		}
		return quarantine.openLocked()
	case value == ')':
		if previous == previousSpace || (quarantine.depth == 1 && quarantine.phase != itemDone) {
			return errorCode(CodeIMAPProtocol)
		}
		return quarantine.closeLocked()
	}

	if previous != previousOpen && previous != previousSpace {
		return errorCode(CodeIMAPProtocol)
	}
	atom := quarantineAtomChar(value) || value == '\\'
	if quarantine.depth == 1 && (!atom || (quarantine.phase == itemValue && quarantine.item != itemUID)) {
		return errorCode(CodeIMAPProtocol)
	}
	if quarantine.flagsList && !atom {
		return errorCode(CodeIMAPProtocol)
	}

	switch {
	case value == '"':
		quarantine.state = fetchQuoted
	case value == '{':
		quarantine.state = fetchLiteralSize
		quarantine.literal = 0
		quarantine.digits = 0
	case atom:
		quarantine.state = fetchAtom
		quarantine.atomStart = len(quarantine.frame) - 1
	default:
		return errorCode(CodeIMAPProtocol)
	}
	return quarantine.tokenLocked()
}

func (quarantine *structureQuarantine) literalSizeByteLocked(value byte) error {
	if value == '}' && quarantine.digits > 0 {
		quarantine.state = fetchLiteralCR
		return nil
	}
	if value < '0' || value > '9' || quarantine.digits == 10 {
		return errorCode(CodeIMAPProtocol)
	}

	quarantine.digits++
	quarantine.literal = quarantine.literal*10 + uint64(value-'0')
	if quarantine.literal > math.MaxUint32 {
		return errorCode(CodeIMAPProtocol)
	}
	return nil
}

// endAtomLocked judges the atom that ended before end where the grammar
// gives it meaning: an item name or UID value of the top-level list, or a
// flag of a FLAGS list.
func (quarantine *structureQuarantine) endAtomLocked(end int) error {
	atom := string(quarantine.frame[quarantine.atomStart:end])
	quarantine.previous = previousToken

	switch {
	case quarantine.depth == 1 && quarantine.phase == itemName:
		return quarantine.nameLocked(atom)
	case quarantine.depth == 1:
		uid, ok := quarantineNumber(atom, false)
		if !ok {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.uid = uid
		quarantine.phase = itemDone
	case quarantine.flagsList && !quarantineFlag(atom):
		return errorCode(CodeIMAPProtocol)
	}
	return nil
}

// nameLocked admits only UID, FLAGS and BODYSTRUCTURE, each at most once.
// BODY, BODY sections, BINARY, BINARY.SIZE and every other item are refused.
func (quarantine *structureQuarantine) nameLocked(name string) error {
	var count *int
	switch strings.ToUpper(name) {
	case "UID":
		quarantine.item, count = itemUID, &quarantine.uidCount
	case "FLAGS":
		quarantine.item, count = itemFlags, &quarantine.flags
	case "BODYSTRUCTURE":
		quarantine.item, count = itemStructure, &quarantine.structure
	default:
		return errorCode(CodeIMAPProtocol)
	}
	if *count > 0 {
		return errorCode(CodeIMAPProtocol)
	}

	*count = 1
	quarantine.phase = itemNamed
	return nil
}

func (quarantine *structureQuarantine) openLocked() error {
	if quarantine.depth == quarantineDepth {
		return errorCode(CodeBoundsExceeded)
	}
	if err := quarantine.tokenLocked(); err != nil {
		return err
	}

	quarantine.depth++
	quarantine.previous = previousOpen
	return nil
}

func (quarantine *structureQuarantine) closeLocked() error {
	if err := quarantine.tokenLocked(); err != nil {
		return err
	}

	quarantine.depth--
	quarantine.previous = previousClose
	switch quarantine.depth {
	case 1:
		quarantine.phase = itemDone
		quarantine.flagsList = false
	case 0:
		quarantine.state = fetchEndCR
	}
	return nil
}

// tokenLocked counts one lexical token against the FETCH and session work
// budgets.
func (quarantine *structureQuarantine) tokenLocked() error {
	if quarantine.tokens == quarantineFetchTokens || quarantine.work == quarantineWorkUnits {
		return errorCode(CodeBoundsExceeded)
	}

	quarantine.tokens++
	quarantine.work++
	return nil
}

// fetchFeasibleLocked refuses a FETCH as soon as its least possible
// completion, closing every open list and its CRLF, cannot fit the frame,
// session, token or work budgets, so no read waits on a frame that must fail.
func (quarantine *structureQuarantine) fetchFeasibleLocked() error {
	closing := quarantine.depth
	bytesNeeded := uint64(closing) + 2
	switch quarantine.state {
	case fetchOpen:
		closing = 2
		bytesNeeded = 4
	case fetchQuoted:
		bytesNeeded++
	case fetchEscape:
		bytesNeeded += 2
	case fetchLiteralSize:
		bytesNeeded += 3 + quarantine.literal
	case fetchLiteralCR:
		bytesNeeded += 2 + quarantine.literal
	case fetchLiteralLF:
		bytesNeeded += 1 + quarantine.literal
	case fetchLiteral:
		bytesNeeded += quarantine.literal
	case fetchEndLF:
		bytesNeeded = 1
	}

	if uint64(len(quarantine.frame))+bytesNeeded > quarantineFrameBytes ||
		uint64(quarantine.sessionBytes)+bytesNeeded > quarantineSessionBytes ||
		quarantine.tokens+closing > quarantineFetchTokens ||
		quarantine.work+closing > quarantineWorkUnits {
		return errorCode(CodeBoundsExceeded)
	}
	return nil
}

// completeFetchLocked classifies one completely framed FETCH. A structure
// frame needs the selected UID and the active dispatch and is released whole;
// a frame of one FLAGS list and at most one UID is consumed.
func (quarantine *structureQuarantine) completeFetchLocked() error {
	switch {
	case quarantine.structure == 1:
		if quarantine.uidCount == 0 || quarantine.uid != quarantine.selectedUID || quarantine.dispatch != quarantineActive {
			return errorCode(CodeIMAPProtocol)
		}
		quarantine.dispatch = quarantineDelivered
		quarantine.releaseLocked()
	case quarantine.flags == 1:
		quarantine.mode = quarantineBetween
		quarantine.frame = quarantine.frame[:0]
	default:
		return errorCode(CodeIMAPProtocol)
	}

	return nil
}

// quarantineAtomChar is an IMAP ATOM-CHAR.
func quarantineAtomChar(value byte) bool {
	if value <= ' ' || value >= 0x7f {
		return false
	}

	return strings.IndexByte(`(){"%*\]`, value) < 0
}

func quarantineAtom(value string) bool {
	if value == "" {
		return false
	}
	for index := range len(value) {
		if !quarantineAtomChar(value[index]) {
			return false
		}
	}

	return true
}

// quarantineFlag is a keyword or a system flag of a FETCH or FLAGS list.
func quarantineFlag(value string) bool {
	return quarantineAtom(strings.TrimPrefix(value, `\`))
}

// quarantineNumber parses a canonical uint32. Zero is accepted only when
// zero is allowed.
func quarantineNumber(value string, zero bool) (uint32, bool) {
	if value == "" || value[0] < '0' || value[0] > '9' || value[0] == '0' && (len(value) > 1 || !zero) {
		return 0, false
	}
	number, err := strconv.ParseUint(value, 10, 32)
	if err != nil {
		return 0, false
	}

	return uint32(number), true
}

// quarantineControl reports whether line, without CRLF, is one admitted flat
// control response. Controls carry no literals.
func quarantineControl(line string) bool {
	if rest, ok := strings.CutPrefix(line, "+"); ok {
		if rest == "" {
			return true
		}
		text, spaced := strings.CutPrefix(rest, " ")
		return spaced && quarantineText(text)
	}

	rest, untagged := strings.CutPrefix(line, "* ")
	if !untagged {
		tag, status, _ := strings.Cut(line, " ")
		if !quarantineAtom(tag) || strings.Contains(tag, "+") {
			return false
		}
		name, text, hasText := strings.Cut(status, " ")
		switch strings.ToUpper(name) {
		case "OK", "NO", "BAD":
			return !hasText || quarantineText(text)
		}
		return false
	}

	name, arguments, hasArguments := strings.Cut(rest, " ")
	if name != "" && name[0] >= '0' && name[0] <= '9' {
		number, valid := quarantineNumber(name, true)
		kind, _, extra := strings.Cut(arguments, " ")
		if !valid || !hasArguments || extra {
			return false
		}
		switch strings.ToUpper(kind) {
		case "EXISTS", "RECENT":
			return true
		case "EXPUNGE":
			return number > 0
		}
		return false
	}

	switch strings.ToUpper(name) {
	case "OK", "NO", "BAD", "BYE", "PREAUTH":
		return !hasArguments || quarantineText(arguments)
	case "CAPABILITY":
		if !hasArguments {
			return false
		}
		for _, capability := range strings.Split(arguments, " ") {
			if !quarantineAtom(capability) {
				return false
			}
		}
		return true
	case "FLAGS":
		list, closed := strings.CutSuffix(arguments, ")")
		list, opened := strings.CutPrefix(list, "(")
		if !hasArguments || !closed || !opened {
			return false
		}
		if list == "" {
			return true
		}
		for _, flag := range strings.Split(list, " ") {
			if !quarantineFlag(flag) {
				return false
			}
		}
		return true
	case "SEARCH", "ESEARCH":
		return !hasArguments || quarantineFlat(arguments)
	}
	return false
}

// quarantineText is opaque status or continuation text. A leading response
// code must close on the same line.
func quarantineText(text string) bool {
	if strings.HasPrefix(text, "[") {
		return strings.IndexByte(text, ']') >= 0
	}

	return true
}

// quarantineFlat accepts printable ASCII with complete quoted strings and no
// literal declaration.
func quarantineFlat(value string) bool {
	quoted, escaped := false, false
	for index := range len(value) {
		character := value[index]
		switch {
		case character < ' ' || character >= 0x7f:
			return false
		case escaped:
			if character != '"' && character != '\\' {
				return false
			}
			escaped = false
		case quoted && character == '\\':
			escaped = true
		case character == '"':
			quoted = !quoted
		case !quoted && character == '{':
			return false
		}
	}

	return !quoted && !escaped
}

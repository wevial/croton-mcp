package testkit

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// TranscriptCommand is one recorded command parsed by an exact grammar. Only
// STORE, COPY and MOVE forms, with or without UID, get structured operands.
type TranscriptCommand struct {
	Sequence     int
	ConnectionID int
	TLS          bool
	Tag          string
	// Verb is the uppercase command name, followed by the subcommand for UID
	// forms, for example "UID STORE" or "SELECT".
	Verb string
	// Arguments is the raw text after Verb.
	Arguments string
	// Set is the exact message set operand: UIDs for UID forms and sequence
	// numbers otherwise. Ranges decodes it in wire order.
	Set    string
	Ranges []SetRange
	// Store is "FLAGS", "+FLAGS" or "-FLAGS"; Silent reports a .SILENT suffix.
	Store  string
	Silent bool
	// Flags are the STORE flags as sent, without parentheses.
	Flags []string
	// Destination is the unquoted COPY or MOVE mailbox name.
	Destination string
}

// SetRange is one message set element as sent. Zero stands for "*"; a single
// number has Start equal to Stop.
type SetRange struct {
	Start, Stop uint32
}

var errMalformedTranscript = errors.New("testkit: malformed transcript command")

// ParseTranscript parses every command in order. It fails on the first
// malformed command and never echoes raw command text, which may hold
// synthetic credentials.
func ParseTranscript(commands []Command) ([]TranscriptCommand, error) {
	parsed := make([]TranscriptCommand, 0, len(commands))
	for _, command := range commands {
		entry, err := ParseTranscriptCommand(command)
		if err != nil {
			return nil, fmt.Errorf("sequence %d: %w", command.Sequence, err)
		}

		parsed = append(parsed, entry)
	}

	return parsed, nil
}

// ParseTranscriptCommand parses one recorded command. The verb is taken only
// from the position after the tag, and structured operands must match the
// grammar exactly: single spaces, no trailing operands and no literals.
func ParseTranscriptCommand(command Command) (TranscriptCommand, error) {
	parsed := TranscriptCommand{Sequence: command.Sequence, ConnectionID: command.ConnectionID, TLS: command.TLS}

	for index := 0; index < len(command.Raw); index++ {
		if character := command.Raw[index]; character < 0x20 || character == 0x7f {
			return TranscriptCommand{}, fmt.Errorf("%w: control character", errMalformedTranscript)
		}
	}

	cursor := &commandCursor{input: command.Raw}
	parsed.Tag = cursor.run(isTagChar)
	if parsed.Tag == "" {
		return TranscriptCommand{}, fmt.Errorf("%w: tag", errMalformedTranscript)
	}

	if !cursor.space() {
		return TranscriptCommand{}, fmt.Errorf("%w: missing command", errMalformedTranscript)
	}

	parsed.Verb = strings.ToUpper(cursor.run(isAtomChar))
	if parsed.Verb == "" {
		return TranscriptCommand{}, fmt.Errorf("%w: command name", errMalformedTranscript)
	}

	if parsed.Verb == "UID" {
		subcommand := ""
		if cursor.space() {
			subcommand = strings.ToUpper(cursor.run(isAtomChar))
		}
		if subcommand == "" {
			return TranscriptCommand{}, fmt.Errorf("%w: UID subcommand", errMalformedTranscript)
		}

		parsed.Verb += " " + subcommand
	}

	if !cursor.done() {
		if !cursor.space() || cursor.done() {
			return TranscriptCommand{}, fmt.Errorf("%w: command separator", errMalformedTranscript)
		}

		parsed.Arguments = cursor.input[cursor.offset:]
	}

	var err error
	switch strings.TrimPrefix(parsed.Verb, "UID ") {
	case "STORE":
		err = parsed.parseStore(cursor)
	case "COPY", "MOVE":
		err = parsed.parseTransfer(cursor)
	default:
		return parsed, nil
	}

	if err == nil && !cursor.done() {
		err = errors.New("trailing operands")
	}
	if err != nil {
		return TranscriptCommand{}, fmt.Errorf("%w: %s %v", errMalformedTranscript, parsed.Verb, err)
	}

	return parsed, nil
}

func (parsed *TranscriptCommand) parseSet(cursor *commandCursor) error {
	parsed.Set = cursor.run(isSequenceSetChar)

	ranges, err := parseSequenceSet(parsed.Set)
	if err != nil {
		return err
	}
	parsed.Ranges = ranges

	if !cursor.space() {
		return errors.New("missing operand after set")
	}

	return nil
}

// parseStore parses set SP ["+" / "-"] "FLAGS" [".SILENT"] SP flags, where
// flags is a parenthesized list or one or more space-separated flags.
func (parsed *TranscriptCommand) parseStore(cursor *commandCursor) error {
	if err := parsed.parseSet(cursor); err != nil {
		return err
	}

	operation := strings.ToUpper(cursor.run(isAtomChar))
	parsed.Store, parsed.Silent = strings.CutSuffix(operation, ".SILENT")
	if parsed.Store != "FLAGS" && parsed.Store != "+FLAGS" && parsed.Store != "-FLAGS" {
		return errors.New("store operation")
	}

	if !cursor.space() || cursor.done() {
		return errors.New("missing flags")
	}

	parenthesized := cursor.consume('(')
	parsed.Flags = []string{}
	for !parenthesized || !cursor.consume(')') {
		if len(parsed.Flags) > 0 && !cursor.space() {
			return errors.New("flag separator")
		}

		flag := cursor.flag()
		if flag == "" {
			return errors.New("flag")
		}
		parsed.Flags = append(parsed.Flags, flag)

		if !parenthesized && cursor.done() {
			break
		}
	}

	return nil
}

// parseTransfer parses set SP mailbox, decoding a quoted mailbox name.
func (parsed *TranscriptCommand) parseTransfer(cursor *commandCursor) error {
	if err := parsed.parseSet(cursor); err != nil {
		return err
	}

	if !cursor.consume('"') {
		parsed.Destination = cursor.run(isAstringChar)
		if parsed.Destination == "" {
			return errors.New("mailbox")
		}

		return nil
	}

	var destination strings.Builder
	for !cursor.done() {
		character := cursor.input[cursor.offset]
		cursor.offset++

		switch character {
		case '"':
			parsed.Destination = destination.String()
			return nil
		case '\\':
			if cursor.done() || cursor.input[cursor.offset] != '"' && cursor.input[cursor.offset] != '\\' {
				return errors.New("quoted escape")
			}
			character = cursor.input[cursor.offset]
			cursor.offset++
		}

		destination.WriteByte(character)
	}

	return errors.New("unterminated quoted mailbox")
}

// parseSequenceSet parses an IMAP sequence set of nz-numbers and "*".
func parseSequenceSet(text string) ([]SetRange, error) {
	if text == "" {
		return nil, errors.New("empty set")
	}

	var ranges []SetRange
	for _, element := range strings.Split(text, ",") {
		startText, stopText, isRange := strings.Cut(element, ":")
		if !isRange {
			stopText = startText
		}

		start, startErr := parseSequenceNumber(startText)
		stop, stopErr := parseSequenceNumber(stopText)
		if startErr != nil || stopErr != nil {
			return nil, errors.New("malformed set")
		}

		ranges = append(ranges, SetRange{Start: start, Stop: stop})
	}

	return ranges, nil
}

func parseSequenceNumber(text string) (uint32, error) {
	if text == "*" {
		return 0, nil
	}

	if text == "" || text[0] < '1' || text[0] > '9' {
		return 0, errors.New("not an nz-number")
	}

	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, err
	}

	return uint32(value), nil
}

type commandCursor struct {
	input  string
	offset int
}

func (cursor *commandCursor) done() bool {
	return cursor.offset == len(cursor.input)
}

func (cursor *commandCursor) consume(character byte) bool {
	if cursor.done() || cursor.input[cursor.offset] != character {
		return false
	}

	cursor.offset++
	return true
}

func (cursor *commandCursor) space() bool {
	return cursor.consume(' ')
}

func (cursor *commandCursor) run(allowed func(byte) bool) string {
	start := cursor.offset
	for !cursor.done() && allowed(cursor.input[cursor.offset]) {
		cursor.offset++
	}

	return cursor.input[start:cursor.offset]
}

// flag reads "\" atom or a keyword atom.
func (cursor *commandCursor) flag() string {
	start := cursor.offset
	cursor.consume('\\')
	if cursor.run(isAtomChar) == "" {
		cursor.offset = start
		return ""
	}

	return cursor.input[start:cursor.offset]
}

// isAtomChar reports RFC 3501 ATOM-CHAR.
func isAtomChar(character byte) bool {
	return character > 0x20 && character < 0x7f && strings.IndexByte(`(){%*"\]`, character) < 0
}

// isAstringChar reports RFC 3501 ASTRING-CHAR.
func isAstringChar(character byte) bool {
	return isAtomChar(character) || character == ']'
}

// isTagChar reports ASTRING-CHAR except "+".
func isTagChar(character byte) bool {
	return isAstringChar(character) && character != '+'
}

func isSequenceSetChar(character byte) bool {
	return character >= '0' && character <= '9' || character == ':' || character == ',' || character == '*'
}

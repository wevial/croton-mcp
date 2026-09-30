package testkit

import (
	"bufio"
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// StatefulOptions opts a Server into a mutable synthetic mailbox model. It is
// test infrastructure only: Seen-only UID STORE and UID MOVE change fixture
// state, while every other mutation is recorded and refused.
type StatefulOptions struct {
	// Mailboxes seeds the namespace in LIST order. INBOX is not implicit.
	Mailboxes []MailboxSeed
	// Move advertises the MOVE capability and enables UID MOVE.
	Move bool
}

// MailboxSeed describes one synthetic mailbox.
type MailboxSeed struct {
	// Name is the exact wire name. INBOX matches case-insensitively.
	Name string
	// Attributes are returned verbatim by LIST, for example \Archive or
	// \Noselect. \Noselect and \NonExistent make a mailbox nonselectable.
	Attributes []string
	// UIDValidity is the mailbox generation. Zero assigns 1001 plus the seed index.
	UIDValidity uint32
	// Messages are stored in UID order.
	Messages []MessageSeed
}

// MessageSeed describes one synthetic message.
type MessageSeed struct {
	// UID zero assigns the previous UID plus one, starting at 1.
	UID uint32
	// Flags are system flags or keywords, for example \Seen or \Flagged.
	Flags []string
	// Body is the complete synthetic RFC 5322 message.
	Body string
}

// MailboxState is a detached snapshot of one stateful mailbox.
type MailboxState struct {
	Name        string
	Attributes  []string
	UIDValidity uint32
	UIDNext     uint32
	Messages    []MessageState
}

// MessageState is a detached snapshot of one stateful message.
type MessageState struct {
	UID   uint32
	Flags []string
	Body  string
}

const statefulDelimiter = "/"

var (
	errNotStateful      = errors.New("testkit: server is not in stateful mode")
	errUnknownMailbox   = errors.New("testkit: unknown mailbox")
	trailingLiteral     = regexp.MustCompile(`\{[0-9]+(\+?)\}$`)
	permanentSeenFlags  = "* OK [PERMANENTFLAGS (\\Seen)] only \\Seen is mutable"
	systemFlagsResponse = "* FLAGS (\\Answered \\Flagged \\Deleted \\Seen \\Draft)"
)

type mailboxStore struct {
	mu        sync.Mutex
	move      bool
	mailboxes []*statefulMailbox
}

type statefulMailbox struct {
	name        string
	attributes  []string
	uidValidity uint32
	uidNext     uint32
	messages    []*statefulMessage
}

type statefulMessage struct {
	uid   uint32
	flags []string
	body  string
}

// statefulSession is connection-local; only mailbox contents are shared.
type statefulSession struct {
	selected string
	readOnly bool
}

func newMailboxStore(options StatefulOptions) (*mailboxStore, error) {
	store := &mailboxStore{move: options.Move}

	for index, seed := range options.Mailboxes {
		if err := validateMailboxName(seed.Name); err != nil {
			return nil, err
		}

		if store.find(seed.Name) != nil {
			return nil, fmt.Errorf("testkit: duplicate mailbox %q", seed.Name)
		}

		attributes, err := validateAtoms(seed.Attributes, true)
		if err != nil {
			return nil, fmt.Errorf("testkit: mailbox %q attributes: %w", seed.Name, err)
		}

		mailbox := &statefulMailbox{name: seed.Name, attributes: attributes, uidValidity: seed.UIDValidity}
		if mailbox.uidValidity == 0 {
			mailbox.uidValidity = uint32(1001 + index)
		}

		previous := uint32(0)
		for _, messageSeed := range seed.Messages {
			uid := messageSeed.UID
			if uid == 0 {
				uid = previous + 1
			}
			if uid <= previous {
				return nil, fmt.Errorf("testkit: mailbox %q UIDs must strictly increase", seed.Name)
			}
			if uid == math.MaxUint32 {
				return nil, fmt.Errorf("testkit: mailbox %q UID %d leaves no valid UIDNEXT", seed.Name, uid)
			}

			flags, err := validateAtoms(messageSeed.Flags, false)
			if err != nil {
				return nil, fmt.Errorf("testkit: mailbox %q UID %d flags: %w", seed.Name, uid, err)
			}

			mailbox.messages = append(mailbox.messages, &statefulMessage{uid: uid, flags: flags, body: messageSeed.Body})
			previous = uid
		}
		mailbox.uidNext = previous + 1

		store.mailboxes = append(store.mailboxes, mailbox)
	}

	return store, nil
}

func validateMailboxName(name string) error {
	if name == "" {
		return errors.New("testkit: mailbox name must not be empty")
	}

	for index := 0; index < len(name); index++ {
		character := name[index]
		if character < 0x20 || character > 0x7e || strings.IndexByte("\"\\*%", character) >= 0 {
			return fmt.Errorf("testkit: mailbox name %q must be printable ASCII without quotes, backslashes or wildcards", name)
		}
	}

	return nil
}

// validateAtoms accepts flag or attribute atoms and rejects case-insensitive duplicates.
func validateAtoms(values []string, requireBackslash bool) ([]string, error) {
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		atom := strings.TrimPrefix(value, "\\")
		if atom == "" || requireBackslash && !strings.HasPrefix(value, "\\") || !isAtom(atom) {
			return nil, fmt.Errorf("invalid atom %q", value)
		}

		if seen[strings.ToLower(value)] {
			return nil, fmt.Errorf("duplicate atom %q", value)
		}
		seen[strings.ToLower(value)] = true
	}

	return append([]string(nil), values...), nil
}

func isAtom(value string) bool {
	for index := 0; index < len(value); index++ {
		character := value[index]
		if character <= 0x20 || character >= 0x7f || strings.IndexByte("(){%*\"\\]", character) >= 0 {
			return false
		}
	}

	return value != ""
}

func (store *mailboxStore) find(name string) *statefulMailbox {
	for _, mailbox := range store.mailboxes {
		if mailboxNameMatches(mailbox.name, name) {
			return mailbox
		}
	}

	return nil
}

func mailboxNameMatches(stored, requested string) bool {
	if strings.EqualFold(stored, "INBOX") {
		return strings.EqualFold(requested, "INBOX")
	}

	return stored == requested
}

func (mailbox *statefulMailbox) selectable() bool {
	for _, attribute := range mailbox.attributes {
		if strings.EqualFold(attribute, "\\Noselect") || strings.EqualFold(attribute, "\\NonExistent") {
			return false
		}
	}

	return true
}

func (mailbox *statefulMailbox) unseen() int {
	count := 0
	for _, message := range mailbox.messages {
		if !message.seen() {
			count++
		}
	}

	return count
}

func (mailbox *statefulMailbox) maxUID() uint32 {
	if len(mailbox.messages) == 0 {
		return 0
	}

	return mailbox.messages[len(mailbox.messages)-1].uid
}

func (mailbox *statefulMailbox) snapshot() MailboxState {
	state := MailboxState{
		Name:        mailbox.name,
		Attributes:  append([]string(nil), mailbox.attributes...),
		UIDValidity: mailbox.uidValidity,
		UIDNext:     mailbox.uidNext,
	}
	for _, message := range mailbox.messages {
		state.Messages = append(state.Messages, MessageState{
			UID:   message.uid,
			Flags: append([]string(nil), message.flags...),
			Body:  message.body,
		})
	}

	return state
}

func (message *statefulMessage) seen() bool {
	for _, flag := range message.flags {
		if strings.EqualFold(flag, "\\Seen") {
			return true
		}
	}

	return false
}

func (message *statefulMessage) setSeen(seen bool) {
	if message.seen() == seen {
		return
	}

	if seen {
		message.flags = append(message.flags, "\\Seen")
		return
	}

	kept := message.flags[:0]
	for _, flag := range message.flags {
		if !strings.EqualFold(flag, "\\Seen") {
			kept = append(kept, flag)
		}
	}
	message.flags = kept
}

// Snapshot returns a detached copy of every stateful mailbox in LIST order.
// It returns nil for a legacy server.
func (server *Server) Snapshot() []MailboxState {
	if server.stateful == nil {
		return nil
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	states := make([]MailboxState, 0, len(server.stateful.mailboxes))
	for _, mailbox := range server.stateful.mailboxes {
		states = append(states, mailbox.snapshot())
	}

	return states
}

// MailboxSnapshot returns a detached copy of one stateful mailbox.
func (server *Server) MailboxSnapshot(name string) (MailboxState, bool) {
	if server.stateful == nil {
		return MailboxState{}, false
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	mailbox := server.stateful.find(name)
	if mailbox == nil {
		return MailboxState{}, false
	}

	return mailbox.snapshot(), true
}

// SetMailboxAttributes replaces the LIST attributes of an existing mailbox.
func (server *Server) SetMailboxAttributes(name string, attributes ...string) error {
	validated, err := validateAtoms(attributes, true)
	if err != nil {
		return fmt.Errorf("testkit: mailbox %q attributes: %w", name, err)
	}

	return server.withMailbox(name, func(mailbox *statefulMailbox) error {
		mailbox.attributes = validated
		return nil
	})
}

// ReplaceUIDValidity gives an existing mailbox a new nonzero generation. UIDs
// and messages are retained so tests can witness stale-generation handling.
func (server *Server) ReplaceUIDValidity(name string, uidValidity uint32) error {
	return server.withMailbox(name, func(mailbox *statefulMailbox) error {
		if uidValidity == 0 || uidValidity == mailbox.uidValidity {
			return fmt.Errorf("testkit: mailbox %q needs a new nonzero UIDVALIDITY", name)
		}

		mailbox.uidValidity = uidValidity
		return nil
	})
}

// RemoveMailbox deletes a mailbox and its messages from fixture state.
// Connections that selected it receive NO for later mailbox operations.
func (server *Server) RemoveMailbox(name string) error {
	if server.stateful == nil {
		return errNotStateful
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	for index, mailbox := range server.stateful.mailboxes {
		if mailboxNameMatches(mailbox.name, name) {
			server.stateful.mailboxes = append(server.stateful.mailboxes[:index], server.stateful.mailboxes[index+1:]...)
			return nil
		}
	}

	return fmt.Errorf("%w %q", errUnknownMailbox, name)
}

func (server *Server) withMailbox(name string, change func(*statefulMailbox) error) error {
	if server.stateful == nil {
		return errNotStateful
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	mailbox := server.stateful.find(name)
	if mailbox == nil {
		return fmt.Errorf("%w %q", errUnknownMailbox, name)
	}

	return change(mailbox)
}

// serveStateful handles one command in stateful mode. It reports whether the
// command was handled and whether the connection should remain open. Commands
// it leaves unhandled use the shared TLS, authentication, NOOP and LOGOUT paths.
func (server *Server) serveStateful(session *statefulSession, writer *bufio.Writer, tag, name, raw string, tlsEstablished, authenticated bool) (bool, bool) {
	if match := trailingLiteral.FindStringSubmatch(raw); match != nil {
		// Refuse before any continuation so a synchronizing literal is never
		// transmitted. A non-synchronizing literal cannot be resynchronized.
		if match[1] == "+" {
			return true, false
		}

		return true, server.writeLine(writer, tagged(tag, "NO [CANNOT] literals are refused by the stateful fixture")) == nil
	}

	switch name {
	case "STARTTLS", "LOGIN", "AUTHENTICATE", "NOOP", "LOGOUT":
		return false, true
	case "CAPABILITY":
		if !tlsEstablished {
			return false, true
		}

		capabilities := "* CAPABILITY IMAP4rev1 AUTH=PLAIN"
		if server.stateful.move {
			capabilities += " MOVE"
		}

		return true, server.writeLines(writer, capabilities, tagged(tag, "OK CAPABILITY completed"))
	}

	if !authenticated {
		return true, server.writeLine(writer, tagged(tag, "NO authenticate first")) == nil
	}

	arguments, err := tokenizeArguments(raw)
	if err != nil {
		return true, server.writeLine(writer, tagged(tag, "BAD "+err.Error())) == nil
	}

	var lines []string
	switch name {
	case "LIST":
		lines = server.statefulList(tag, arguments)
	case "SELECT", "EXAMINE":
		lines = server.statefulSelect(session, tag, name, arguments)
	case "STATUS":
		lines = server.statefulStatus(tag, arguments)
	case "UID":
		lines = server.statefulUID(session, tag, arguments)
	case "COPY", "MOVE", "STORE", "EXPUNGE", "DELETE", "APPEND", "CREATE", "RENAME", "SUBSCRIBE", "UNSUBSCRIBE", "CLOSE":
		lines = []string{refused(tag, name)}
	default:
		lines = []string{tagged(tag, "BAD unsupported command")}
	}

	return true, server.writeLines(writer, lines...)
}

func refused(tag, command string) string {
	return tagged(tag, "NO [CANNOT] "+command+" is refused by the stateful fixture")
}

func (server *Server) statefulList(tag string, arguments []imapToken) []string {
	if len(arguments) != 2 || arguments[0].isList || arguments[1].isList {
		return []string{tagged(tag, "BAD LIST requires a reference and a pattern")}
	}

	if arguments[0].value != "" {
		return []string{tagged(tag, "BAD only an empty LIST reference is supported")}
	}

	pattern := arguments[1].value
	if pattern == "" {
		return []string{"* LIST (\\Noselect) \"/\" \"\"", tagged(tag, "OK LIST completed")}
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	lines := make([]string, 0, len(server.stateful.mailboxes)+1)
	for _, mailbox := range server.stateful.mailboxes {
		if listPatternMatches(pattern, mailbox.name) || mailboxNameMatches(mailbox.name, pattern) {
			lines = append(lines, fmt.Sprintf("* LIST (%s) %q %s", strings.Join(mailbox.attributes, " "), statefulDelimiter, quoteMailbox(mailbox.name)))
		}
	}

	return append(lines, tagged(tag, "OK LIST completed"))
}

// listPatternMatches implements LIST wildcards: * matches anything and %
// matches anything except the hierarchy delimiter.
func listPatternMatches(pattern, name string) bool {
	if pattern == "" {
		return name == ""
	}

	switch pattern[0] {
	case '*', '%':
		for index := 0; index <= len(name); index++ {
			if listPatternMatches(pattern[1:], name[index:]) {
				return true
			}
			if index < len(name) && pattern[0] == '%' && name[index] == statefulDelimiter[0] {
				return false
			}
		}
		return false
	default:
		return name != "" && name[0] == pattern[0] && listPatternMatches(pattern[1:], name[1:])
	}
}

func quoteMailbox(name string) string {
	return `"` + name + `"`
}

func (server *Server) statefulSelect(session *statefulSession, tag, name string, arguments []imapToken) []string {
	session.selected, session.readOnly = "", false

	if len(arguments) != 1 || arguments[0].isList {
		return []string{tagged(tag, "BAD "+name+" requires exactly one mailbox")}
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	mailbox := server.stateful.find(arguments[0].value)
	if mailbox == nil || !mailbox.selectable() {
		return []string{tagged(tag, "NO mailbox does not exist or is not selectable")}
	}

	session.selected = mailbox.name
	session.readOnly = name == "EXAMINE"

	permanentFlags, completion := permanentSeenFlags, "OK [READ-WRITE] SELECT completed"
	if session.readOnly {
		permanentFlags, completion = "* OK [PERMANENTFLAGS ()] read-only", "OK [READ-ONLY] EXAMINE completed"
	}

	return []string{
		systemFlagsResponse,
		fmt.Sprintf("* %d EXISTS", len(mailbox.messages)),
		"* 0 RECENT",
		permanentFlags,
		fmt.Sprintf("* OK [UIDVALIDITY %d] mailbox generation", mailbox.uidValidity),
		fmt.Sprintf("* OK [UIDNEXT %d] next UID", mailbox.uidNext),
		tagged(tag, completion),
	}
}

func (server *Server) statefulStatus(tag string, arguments []imapToken) []string {
	if len(arguments) != 2 || arguments[0].isList || !arguments[1].isList || len(arguments[1].list) == 0 {
		return []string{tagged(tag, "BAD STATUS requires a mailbox and an item list")}
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	mailbox := server.stateful.find(arguments[0].value)
	if mailbox == nil || !mailbox.selectable() {
		return []string{tagged(tag, "NO mailbox does not exist or is not selectable")}
	}

	items := make([]string, 0, len(arguments[1].list))
	for _, item := range arguments[1].list {
		switch upper := strings.ToUpper(item.value); {
		case item.isList || item.quoted:
			return []string{tagged(tag, "BAD malformed STATUS item")}
		case upper == "MESSAGES":
			items = append(items, fmt.Sprintf("MESSAGES %d", len(mailbox.messages)))
		case upper == "UIDNEXT":
			items = append(items, fmt.Sprintf("UIDNEXT %d", mailbox.uidNext))
		case upper == "UIDVALIDITY":
			items = append(items, fmt.Sprintf("UIDVALIDITY %d", mailbox.uidValidity))
		case upper == "UNSEEN":
			items = append(items, fmt.Sprintf("UNSEEN %d", mailbox.unseen()))
		default:
			return []string{tagged(tag, "BAD unsupported STATUS item")}
		}
	}

	return []string{
		fmt.Sprintf("* STATUS %s (%s)", quoteMailbox(mailbox.name), strings.Join(items, " ")),
		tagged(tag, "OK STATUS completed"),
	}
}

func (server *Server) statefulUID(session *statefulSession, tag string, arguments []imapToken) []string {
	if len(arguments) == 0 || arguments[0].isList || arguments[0].quoted {
		return []string{tagged(tag, "BAD incomplete UID command")}
	}

	subcommand := strings.ToUpper(arguments[0].value)
	switch subcommand {
	case "FETCH", "SEARCH", "STORE":
	case "MOVE":
		if !server.stateful.move {
			return []string{tagged(tag, "BAD MOVE is not enabled")}
		}
	case "COPY", "EXPUNGE":
		return []string{refused(tag, "UID "+subcommand)}
	default:
		return []string{tagged(tag, "BAD unsupported UID command")}
	}

	if session.selected == "" {
		return []string{tagged(tag, "BAD no mailbox selected")}
	}

	server.stateful.mu.Lock()
	defer server.stateful.mu.Unlock()

	mailbox := server.stateful.find(session.selected)
	if mailbox == nil {
		return []string{tagged(tag, "NO selected mailbox no longer exists")}
	}

	switch subcommand {
	case "FETCH":
		return statefulFetch(mailbox, tag, arguments[1:])
	case "SEARCH":
		return statefulSearch(mailbox, tag, arguments[1:])
	case "STORE":
		return statefulStore(session, mailbox, tag, arguments[1:])
	default:
		return server.statefulMove(session, mailbox, tag, arguments[1:])
	}
}

type uidRange struct{ start, end uint32 }

// parseUIDSet parses a UID sequence set; * resolves to the largest UID present.
func parseUIDSet(token imapToken, largest uint32) ([]uidRange, error) {
	if token.isList || token.quoted || token.value == "" {
		return nil, errors.New("malformed UID set")
	}

	var ranges []uidRange
	for _, span := range strings.Split(token.value, ",") {
		startText, endText, isRange := strings.Cut(span, ":")
		if !isRange {
			endText = startText
		}

		start, startErr := parseUIDSetNumber(startText, largest)
		end, endErr := parseUIDSetNumber(endText, largest)
		if startErr != nil || endErr != nil {
			return nil, errors.New("malformed UID set")
		}
		if start > end {
			start, end = end, start
		}

		ranges = append(ranges, uidRange{start: start, end: end})
	}

	return ranges, nil
}

func parseUIDSetNumber(text string, largest uint32) (uint32, error) {
	if text == "*" {
		return largest, nil
	}

	value, err := strconv.ParseUint(text, 10, 32)
	if err != nil || value == 0 {
		return 0, errors.New("malformed UID")
	}

	return uint32(value), nil
}

func uidInRanges(uid uint32, ranges []uidRange) bool {
	for _, span := range ranges {
		if uid >= span.start && uid <= span.end {
			return true
		}
	}

	return false
}

// statefulSearch supports ALL, SEEN, UNSEEN and UID sets combined by AND.
func statefulSearch(mailbox *statefulMailbox, tag string, arguments []imapToken) []string {
	if len(arguments) == 0 {
		return []string{tagged(tag, "BAD missing search criteria")}
	}

	type criterion func(*statefulMessage) bool
	var criteria []criterion
	for index := 0; index < len(arguments); index++ {
		argument := arguments[index]
		if argument.isList || argument.quoted {
			return []string{tagged(tag, "BAD unsupported search grammar")}
		}

		switch strings.ToUpper(argument.value) {
		case "ALL":
		case "SEEN":
			criteria = append(criteria, func(message *statefulMessage) bool { return message.seen() })
		case "UNSEEN":
			criteria = append(criteria, func(message *statefulMessage) bool { return !message.seen() })
		case "UID":
			if index+1 >= len(arguments) {
				return []string{tagged(tag, "BAD UID search key requires a set")}
			}

			index++
			ranges, err := parseUIDSet(arguments[index], mailbox.maxUID())
			if err != nil {
				return []string{tagged(tag, "BAD "+err.Error())}
			}
			criteria = append(criteria, func(message *statefulMessage) bool { return uidInRanges(message.uid, ranges) })
		default:
			return []string{tagged(tag, "BAD unsupported search key")}
		}
	}

	response := "* SEARCH"
	for _, message := range mailbox.messages {
		matched := true
		for _, matches := range criteria {
			matched = matched && matches(message)
		}

		if matched {
			response += fmt.Sprintf(" %d", message.uid)
		}
	}

	return []string{response, tagged(tag, "OK SEARCH completed")}
}

var bodyPeekItem = regexp.MustCompile(`^BODY\.PEEK\[(|HEADER|TEXT)\](?:<([0-9]+)\.([0-9]+)>)?$`)

// statefulFetch supports UID, FLAGS, RFC822.SIZE, INTERNALDATE and
// BODY.PEEK of the whole message, HEADER or TEXT with an optional partial.
// Non-peek body items are refused because they would implicitly set \Seen.
func statefulFetch(mailbox *statefulMailbox, tag string, arguments []imapToken) []string {
	if len(arguments) != 2 {
		return []string{tagged(tag, "BAD UID FETCH requires a UID set and items")}
	}

	ranges, err := parseUIDSet(arguments[0], mailbox.maxUID())
	if err != nil {
		return []string{tagged(tag, "BAD "+err.Error())}
	}

	items := []imapToken{arguments[1]}
	if arguments[1].isList {
		items = arguments[1].list
	}

	for _, item := range items {
		upper := strings.ToUpper(item.value)
		if item.isList || item.quoted || upper != "UID" && upper != "FLAGS" && upper != "RFC822.SIZE" && upper != "INTERNALDATE" && !bodyPeekItem.MatchString(upper) {
			return []string{tagged(tag, "BAD unsupported FETCH item")}
		}
	}

	var lines []string
	for index, message := range mailbox.messages {
		if !uidInRanges(message.uid, ranges) {
			continue
		}

		parts := []string{fmt.Sprintf("UID %d", message.uid)}
		for _, item := range items {
			switch upper := strings.ToUpper(item.value); upper {
			case "UID":
			case "FLAGS":
				parts = append(parts, fmt.Sprintf("FLAGS (%s)", strings.Join(message.flags, " ")))
			case "RFC822.SIZE":
				parts = append(parts, fmt.Sprintf("RFC822.SIZE %d", len(message.body)))
			case "INTERNALDATE":
				parts = append(parts, `INTERNALDATE "01-Jan-2026 00:00:00 +0000"`)
			default:
				parts = append(parts, bodyPeekResponse(message.body, bodyPeekItem.FindStringSubmatch(upper)))
			}
		}

		lines = append(lines, fmt.Sprintf("* %d FETCH (%s)", index+1, strings.Join(parts, " ")))
	}

	return append(lines, tagged(tag, "OK FETCH completed"))
}

func bodyPeekResponse(body string, match []string) string {
	header, text, found := strings.Cut(body, "\r\n\r\n")
	literal := body
	switch match[1] {
	case "HEADER":
		literal = header + "\r\n\r\n"
	case "TEXT":
		literal = ""
		if found {
			literal = text
		}
	}

	label := "BODY[" + match[1] + "]"
	if match[2] != "" {
		offset, _ := strconv.Atoi(match[2])
		size, _ := strconv.Atoi(match[3])
		offset = min(offset, len(literal))
		literal = literal[offset:min(len(literal), offset+size)]
		label += fmt.Sprintf("<%d>", offset)
	}

	return fmt.Sprintf("%s {%d}\r\n%s", label, len(literal), literal)
}

// statefulStore permits only adding or removing \Seen on a read-write selection.
func statefulStore(session *statefulSession, mailbox *statefulMailbox, tag string, arguments []imapToken) []string {
	if session.readOnly {
		return []string{tagged(tag, "NO [READ-ONLY] mailbox was opened with EXAMINE")}
	}

	if len(arguments) != 3 || arguments[1].isList || arguments[1].quoted {
		return []string{tagged(tag, "BAD UID STORE requires a UID set, an operation and flags")}
	}

	ranges, err := parseUIDSet(arguments[0], mailbox.maxUID())
	if err != nil {
		return []string{tagged(tag, "BAD "+err.Error())}
	}

	operation := strings.ToUpper(arguments[1].value)
	silent := strings.HasSuffix(operation, ".SILENT")
	var seen bool
	switch strings.TrimSuffix(operation, ".SILENT") {
	case "+FLAGS":
		seen = true
	case "-FLAGS":
		seen = false
	case "FLAGS":
		return []string{refused(tag, "FLAGS replacement")}
	default:
		return []string{tagged(tag, "BAD unsupported STORE operation")}
	}

	flags := []imapToken{arguments[2]}
	if arguments[2].isList {
		flags = arguments[2].list
	}
	if len(flags) != 1 || flags[0].isList || flags[0].quoted || !strings.EqualFold(flags[0].value, "\\Seen") {
		return []string{refused(tag, "changing flags other than \\Seen")}
	}

	var lines []string
	for index, message := range mailbox.messages {
		if !uidInRanges(message.uid, ranges) {
			continue
		}

		message.setSeen(seen)

		if !silent {
			lines = append(lines, fmt.Sprintf("* %d FETCH (UID %d FLAGS (%s))", index+1, message.uid, strings.Join(message.flags, " ")))
		}
	}

	return append(lines, tagged(tag, "OK STORE completed"))
}

// statefulMove moves existing source UIDs, allocating destination UIDs from
// UIDNEXT in ascending source order. Missing source UIDs are ignored. A move
// that would exhaust destination UIDs is refused before either mailbox changes.
func (server *Server) statefulMove(session *statefulSession, source *statefulMailbox, tag string, arguments []imapToken) []string {
	if session.readOnly {
		return []string{tagged(tag, "NO [READ-ONLY] mailbox was opened with EXAMINE")}
	}

	if len(arguments) != 2 || arguments[1].isList {
		return []string{tagged(tag, "BAD UID MOVE requires a UID set and a mailbox")}
	}

	ranges, err := parseUIDSet(arguments[0], source.maxUID())
	if err != nil {
		return []string{tagged(tag, "BAD "+err.Error())}
	}

	destination := server.stateful.find(arguments[1].value)
	switch {
	case destination == nil:
		return []string{tagged(tag, "NO [TRYCREATE] destination mailbox does not exist")}
	case !destination.selectable():
		return []string{tagged(tag, "NO [CANNOT] destination mailbox is not selectable")}
	case destination == source:
		return []string{tagged(tag, "NO [CANNOT] destination is the selected mailbox")}
	}

	moving := 0
	for _, message := range source.messages {
		if uidInRanges(message.uid, ranges) {
			moving++
		}
	}
	// UIDNEXT must stay a valid nonzero 32-bit UID after allocation.
	if uint64(destination.uidNext)+uint64(moving) > math.MaxUint32 {
		return []string{tagged(tag, "NO [LIMIT] destination mailbox has too few UIDs left")}
	}

	var sourceUIDs, destinationUIDs, expunged []string
	kept := make([]*statefulMessage, 0, len(source.messages))
	for index, message := range source.messages {
		if !uidInRanges(message.uid, ranges) {
			kept = append(kept, message)
			continue
		}

		sourceUIDs = append(sourceUIDs, strconv.FormatUint(uint64(message.uid), 10))
		destinationUIDs = append(destinationUIDs, strconv.FormatUint(uint64(destination.uidNext), 10))
		// Descending sequence numbers stay valid as each EXPUNGE is applied.
		expunged = append([]string{fmt.Sprintf("* %d EXPUNGE", index+1)}, expunged...)

		destination.messages = append(destination.messages, &statefulMessage{uid: destination.uidNext, flags: message.flags, body: message.body})
		destination.uidNext++
	}
	source.messages = kept

	if len(sourceUIDs) == 0 {
		return []string{tagged(tag, "OK MOVE completed")}
	}

	lines := []string{fmt.Sprintf("* OK [COPYUID %d %s %s] moved", destination.uidValidity, strings.Join(sourceUIDs, ","), strings.Join(destinationUIDs, ","))}
	lines = append(lines, expunged...)

	return append(lines, tagged(tag, "OK MOVE completed"))
}

// imapToken is an atom, quoted string or parenthesized list. Atoms keep
// bracketed sections such as BODY.PEEK[HEADER] intact.
type imapToken struct {
	value  string
	quoted bool
	isList bool
	list   []imapToken
}

// tokenizeArguments parses the arguments after the tag and command name.
func tokenizeArguments(raw string) ([]imapToken, error) {
	fields := strings.SplitN(raw, " ", 3)
	if len(fields) < 3 {
		return nil, nil
	}

	tokens, rest, err := tokenizeList(fields[2], false)
	if err != nil {
		return nil, err
	}
	if rest != "" {
		return nil, errors.New("unbalanced parentheses")
	}

	return tokens, nil
}

func tokenizeList(input string, nested bool) ([]imapToken, string, error) {
	var tokens []imapToken
	for {
		input = strings.TrimLeft(input, " ")
		switch {
		case input == "":
			if nested {
				return nil, "", errors.New("unbalanced parentheses")
			}
			return tokens, "", nil
		case input[0] == ')':
			if !nested {
				return nil, input, nil
			}
			return tokens, input[1:], nil
		case input[0] == '(':
			list, rest, err := tokenizeList(input[1:], true)
			if err != nil {
				return nil, "", err
			}
			tokens = append(tokens, imapToken{isList: true, list: list})
			input = rest
		case input[0] == '"':
			value, rest, err := readQuoted(input[1:])
			if err != nil {
				return nil, "", err
			}
			tokens = append(tokens, imapToken{value: value, quoted: true})
			input = rest
		default:
			end, depth := 0, 0
			for end < len(input) && (depth > 0 || input[end] != ' ' && input[end] != '(' && input[end] != ')') {
				switch input[end] {
				case '[':
					depth++
				case ']':
					depth--
				}
				end++
			}
			if depth != 0 {
				return nil, "", errors.New("unbalanced brackets")
			}
			tokens = append(tokens, imapToken{value: input[:end]})
			input = input[end:]
		}
	}
}

func readQuoted(input string) (string, string, error) {
	var value strings.Builder
	for index := 0; index < len(input); index++ {
		switch input[index] {
		case '"':
			return value.String(), input[index+1:], nil
		case '\\':
			index++
			if index == len(input) || input[index] != '"' && input[index] != '\\' {
				return "", "", errors.New("malformed quoted string")
			}
		}
		value.WriteByte(input[index])
	}

	return "", "", errors.New("unterminated quoted string")
}

package testkit

import (
	"bufio"
	"cmp"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// LabelOptions opts a Server into the linked folder and label-view model; see
// STATEFUL.md. It is test infrastructure only: single-UID UID COPY into a
// label view, single-UID \Deleted UID STORE and single-UID UID EXPUNGE in a
// label view change fixture state, while every other mutation is refused.
type LabelOptions struct {
	// Messages seeds stable synthetic identities and their shared state.
	Messages []LabelMessage
	// Views seeds folders and label views in LIST order. INBOX is not implicit.
	Views []LabelViewSeed
	// UIDPlus advertises UIDPLUS after authentication, adds COPYUID to UID
	// COPY completions and enables UID EXPUNGE.
	UIDPlus bool
}

// ViewRole declares how a label-mode mailbox behaves. It is never inferred
// from the mailbox name.
type ViewRole uint8

const (
	// FolderRole is an ordinary folder. Each message has exactly one folder
	// membership, and only a folder can be a UID COPY source.
	FolderRole ViewRole = iota + 1
	// LabelRole is a label view. Only a label view can be a UID COPY target or
	// accept \Deleted UID STORE and UID EXPUNGE.
	LabelRole
)

// String returns the stable name of a view role.
func (role ViewRole) String() string {
	switch role {
	case FolderRole:
		return "folder"
	case LabelRole:
		return "label"
	default:
		return fmt.Sprintf("unknown-%d", role)
	}
}

// LabelMessage is one synthetic message identity, seeded or snapshotted.
type LabelMessage struct {
	// ID is the stable synthetic identity. It is never sent on the wire.
	ID string
	// Flags are shared by every view of the message, for example \Seen.
	// \Deleted is view-local and is rejected here.
	Flags []string
	// Body is the complete synthetic RFC 5322 message.
	Body string
}

// LabelViewSeed describes one folder or label view.
type LabelViewSeed struct {
	// Name is the exact wire name. INBOX matches case-insensitively.
	Name string
	// Role is required: FolderRole or LabelRole.
	Role ViewRole
	// Attributes are returned verbatim by LIST. \Noselect and \NonExistent
	// make a view nonselectable.
	Attributes []string
	// UIDValidity is the view generation. Zero assigns 1001 plus the seed index.
	UIDValidity uint32
	// Members are the messages present in this view, in any order.
	Members []Membership
}

// Membership places one message identity in one view, seeded or snapshotted.
type Membership struct {
	// Message is a LabelMessage ID.
	Message string
	// UID is this view's nonzero UID for the message.
	UID uint32
	// Deleted is this view's \Deleted marker.
	Deleted bool
}

// LabelState is a detached snapshot of the label model.
type LabelState struct {
	Messages []LabelMessage
	Views    []LabelViewState
}

// LabelViewState is a detached snapshot of one folder or label view. Members
// are in UID order.
type LabelViewState struct {
	Name        string
	Role        ViewRole
	Attributes  []string
	UIDValidity uint32
	UIDNext     uint32
	Members     []Membership
}

type labelStore struct {
	mu       sync.Mutex
	uidPlus  bool
	messages []*labelMessage
	views    []*labelView
}

type labelMessage struct {
	id    string
	flags []string
	body  string
}

type labelView struct {
	name        string
	role        ViewRole
	attributes  []string
	uidValidity uint32
	uidNext     uint32
	members     []*membership
}

type membership struct {
	message *labelMessage
	uid     uint32
	deleted bool
}

func newLabelStore(options LabelOptions) (*labelStore, error) {
	store := &labelStore{uidPlus: options.UIDPlus}

	byID := make(map[string]*labelMessage, len(options.Messages))
	for _, seed := range options.Messages {
		if seed.ID == "" {
			return nil, errors.New("testkit: label message ID must not be empty")
		}

		if byID[seed.ID] != nil {
			return nil, fmt.Errorf("testkit: duplicate label message ID %q", seed.ID)
		}

		flags, err := validateAtoms(seed.Flags, false)
		if err != nil {
			return nil, fmt.Errorf("testkit: label message %q flags: %w", seed.ID, err)
		}

		if slices.ContainsFunc(flags, isDeletedFlag) {
			return nil, fmt.Errorf("testkit: label message %q: \\Deleted is view-local", seed.ID)
		}

		message := &labelMessage{id: seed.ID, flags: flags, body: seed.Body}
		byID[seed.ID] = message
		store.messages = append(store.messages, message)
	}

	folders := make(map[*labelMessage]string, len(store.messages))
	for index, seed := range options.Views {
		if err := validateMailboxName(seed.Name); err != nil {
			return nil, err
		}

		if store.find(seed.Name) != nil {
			return nil, fmt.Errorf("testkit: duplicate mailbox %q", seed.Name)
		}

		if seed.Role != FolderRole && seed.Role != LabelRole {
			return nil, fmt.Errorf("testkit: mailbox %q needs an explicit view role", seed.Name)
		}

		attributes, err := validateAtoms(seed.Attributes, true)
		if err != nil {
			return nil, fmt.Errorf("testkit: mailbox %q attributes: %w", seed.Name, err)
		}

		view := &labelView{name: seed.Name, role: seed.Role, attributes: attributes, uidValidity: seed.UIDValidity}
		if view.uidValidity == 0 {
			view.uidValidity = uint32(1001 + index)
		}

		for _, member := range seed.Members {
			message := byID[member.Message]
			switch {
			case message == nil:
				return nil, fmt.Errorf("testkit: mailbox %q references unknown message %q", seed.Name, member.Message)
			case member.UID == 0 || member.UID == math.MaxUint32:
				return nil, fmt.Errorf("testkit: mailbox %q UID %d must be nonzero and leave a valid UIDNEXT", seed.Name, member.UID)
			case view.byUID(member.UID) != nil:
				return nil, fmt.Errorf("testkit: mailbox %q has duplicate UID %d", seed.Name, member.UID)
			case view.byMessage(message) != nil:
				return nil, fmt.Errorf("testkit: mailbox %q has duplicate membership of message %q", seed.Name, member.Message)
			}

			if seed.Role == FolderRole {
				if folder, owned := folders[message]; owned {
					return nil, fmt.Errorf("testkit: message %q is in folders %q and %q", member.Message, folder, seed.Name)
				}
				folders[message] = seed.Name
			}

			view.members = append(view.members, &membership{message: message, uid: member.UID, deleted: member.Deleted})
		}

		slices.SortFunc(view.members, func(left, right *membership) int { return cmp.Compare(left.uid, right.uid) })

		view.uidNext = 1
		if count := len(view.members); count > 0 {
			view.uidNext = view.members[count-1].uid + 1
		}

		store.views = append(store.views, view)
	}

	for _, message := range store.messages {
		if _, owned := folders[message]; !owned {
			return nil, fmt.Errorf("testkit: message %q needs exactly one folder membership", message.id)
		}
	}

	return store, nil
}

func isDeletedFlag(flag string) bool {
	return strings.EqualFold(flag, "\\Deleted")
}

func (store *labelStore) find(name string) *labelView {
	for _, view := range store.views {
		if mailboxNameMatches(view.name, name) {
			return view
		}
	}

	return nil
}

func (view *labelView) byUID(uid uint32) *membership {
	for _, member := range view.members {
		if member.uid == uid {
			return member
		}
	}

	return nil
}

func (view *labelView) byMessage(message *labelMessage) *membership {
	for _, member := range view.members {
		if member.message == message {
			return member
		}
	}

	return nil
}

// mailbox renders a detached ordinary mailbox so the stateful read grammar can
// serve the view. A member's flags are the shared flags plus its own \Deleted.
func (view *labelView) mailbox() *statefulMailbox {
	mailbox := &statefulMailbox{
		name:        view.name,
		attributes:  slices.Clone(view.attributes),
		uidValidity: view.uidValidity,
		uidNext:     view.uidNext,
	}
	for _, member := range view.members {
		flags := slices.Clone(member.message.flags)
		if member.deleted {
			flags = append(flags, "\\Deleted")
		}

		mailbox.messages = append(mailbox.messages, &statefulMessage{uid: member.uid, flags: flags, body: member.message.body})
	}

	return mailbox
}

// LabelSnapshot returns a detached copy of the label model. It reports false
// outside label mode.
func (server *Server) LabelSnapshot() (LabelState, bool) {
	if server.labels == nil {
		return LabelState{}, false
	}

	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	state := LabelState{
		Messages: make([]LabelMessage, 0, len(server.labels.messages)),
		Views:    make([]LabelViewState, 0, len(server.labels.views)),
	}
	for _, message := range server.labels.messages {
		state.Messages = append(state.Messages, LabelMessage{ID: message.id, Flags: slices.Clone(message.flags), Body: message.body})
	}
	for _, view := range server.labels.views {
		viewState := LabelViewState{
			Name:        view.name,
			Role:        view.role,
			Attributes:  slices.Clone(view.attributes),
			UIDValidity: view.uidValidity,
			UIDNext:     view.uidNext,
			Members:     make([]Membership, 0, len(view.members)),
		}
		for _, member := range view.members {
			viewState.Members = append(viewState.Members, Membership{Message: member.message.id, UID: member.uid, Deleted: member.deleted})
		}
		state.Views = append(state.Views, viewState)
	}

	return state, true
}

// serveLabels handles one command in label mode, like serveStateful. Commands
// it leaves unhandled use the shared TLS, authentication, NOOP and LOGOUT paths.
func (server *Server) serveLabels(session *statefulSession, writer *bufio.Writer, tag, name, raw string, tlsEstablished, authenticated bool) (bool, bool) {
	if handled, keep := server.refuseLiteral(writer, tag, raw); handled {
		return true, keep
	}

	switch name {
	case "STARTTLS", "LOGIN", "AUTHENTICATE", "NOOP", "LOGOUT":
		return false, true
	case "CAPABILITY":
		if !tlsEstablished {
			return false, true
		}

		capabilities := "* CAPABILITY IMAP4rev1 AUTH=PLAIN"
		if authenticated && server.labels.uidPlus {
			capabilities += " UIDPLUS"
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
		lines = server.labelList(tag, arguments)
	case "SELECT", "EXAMINE":
		lines = server.labelSelect(session, tag, name, arguments)
	case "STATUS":
		lines = server.labelStatus(tag, arguments)
	case "UID":
		lines = server.labelUID(session, tag, arguments)
	case "COPY", "MOVE", "STORE", "EXPUNGE", "DELETE", "APPEND", "CREATE", "RENAME", "SUBSCRIBE", "UNSUBSCRIBE", "CLOSE":
		lines = []string{refused(tag, name)}
	default:
		lines = []string{tagged(tag, "BAD unsupported command")}
	}

	return true, server.writeLines(writer, lines...)
}

func (server *Server) labelList(tag string, arguments []imapToken) []string {
	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	mailboxes := make([]*statefulMailbox, 0, len(server.labels.views))
	for _, view := range server.labels.views {
		mailboxes = append(mailboxes, &statefulMailbox{name: view.name, attributes: view.attributes})
	}

	return listLines(tag, arguments, mailboxes)
}

func (server *Server) labelStatus(tag string, arguments []imapToken) []string {
	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	return statusLines(tag, arguments, func(name string) *statefulMailbox {
		view := server.labels.find(name)
		if view == nil {
			return nil
		}

		return view.mailbox()
	})
}

func (server *Server) labelSelect(session *statefulSession, tag, name string, arguments []imapToken) []string {
	session.selected, session.readOnly = "", false

	if len(arguments) != 1 || arguments[0].isList {
		return []string{tagged(tag, "BAD "+name+" requires exactly one mailbox")}
	}

	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	view := server.labels.find(arguments[0].value)
	if view == nil || !selectableAttributes(view.attributes) {
		return []string{tagged(tag, "NO mailbox does not exist or is not selectable")}
	}

	session.selected = view.name
	session.readOnly = name == "EXAMINE"

	permanentFlags, completion := "* OK [PERMANENTFLAGS ()] folder flags are read-only", "OK [READ-WRITE] SELECT completed"
	if view.role == LabelRole {
		permanentFlags = "* OK [PERMANENTFLAGS (\\Deleted)] only \\Deleted is mutable"
	}
	if session.readOnly {
		permanentFlags, completion = "* OK [PERMANENTFLAGS ()] read-only", "OK [READ-ONLY] EXAMINE completed"
	}

	return []string{
		systemFlagsResponse,
		fmt.Sprintf("* %d EXISTS", len(view.members)),
		"* 0 RECENT",
		permanentFlags,
		fmt.Sprintf("* OK [UIDVALIDITY %d] mailbox generation", view.uidValidity),
		fmt.Sprintf("* OK [UIDNEXT %d] next UID", view.uidNext),
		tagged(tag, completion),
	}
}

func (server *Server) labelUID(session *statefulSession, tag string, arguments []imapToken) []string {
	if len(arguments) == 0 || arguments[0].isList || arguments[0].quoted {
		return []string{tagged(tag, "BAD incomplete UID command")}
	}

	subcommand := strings.ToUpper(arguments[0].value)
	switch subcommand {
	case "FETCH", "SEARCH", "COPY", "STORE":
	case "EXPUNGE":
		if !server.labels.uidPlus {
			return []string{tagged(tag, "BAD UIDPLUS is not enabled")}
		}
	case "MOVE":
		return []string{refused(tag, "UID MOVE")}
	default:
		return []string{tagged(tag, "BAD unsupported UID command")}
	}

	if session.selected == "" {
		return []string{tagged(tag, "BAD no mailbox selected")}
	}

	server.labels.mu.Lock()
	defer server.labels.mu.Unlock()

	view := server.labels.find(session.selected)
	if view == nil {
		return []string{tagged(tag, "NO selected mailbox no longer exists")}
	}

	switch subcommand {
	case "FETCH":
		return statefulFetch(view.mailbox(), tag, arguments[1:])
	case "SEARCH":
		return statefulSearch(view.mailbox(), tag, arguments[1:])
	}

	if session.readOnly {
		return []string{tagged(tag, "NO [READ-ONLY] mailbox was opened with EXAMINE")}
	}

	switch subcommand {
	case "COPY":
		return server.labelCopy(view, tag, arguments[1:])
	case "STORE":
		return labelStoreDeleted(view, tag, arguments[1:])
	default:
		return labelExpunge(view, tag, arguments[1:])
	}
}

// parseSingleUID accepts exactly one UID number. A well-formed set with a
// range, list or * is refused rather than applied to several messages.
func parseSingleUID(tag string, token imapToken) (uint32, []string) {
	if _, err := parseUIDSet(token, 0); err != nil {
		return 0, []string{tagged(tag, "BAD "+err.Error())}
	}

	uid, err := strconv.ParseUint(token.value, 10, 32)
	if err != nil {
		return 0, []string{refused(tag, "a UID set other than one UID")}
	}

	return uint32(uid), nil
}

// labelCopy adds the selected folder's message to one label view. A missing
// source UID or an existing membership completes OK without a change.
func (server *Server) labelCopy(source *labelView, tag string, arguments []imapToken) []string {
	if len(arguments) != 2 || arguments[1].isList {
		return []string{tagged(tag, "BAD UID COPY requires a UID set and a mailbox")}
	}

	uid, failure := parseSingleUID(tag, arguments[0])
	if failure != nil {
		return failure
	}

	if source.role != FolderRole {
		return []string{tagged(tag, "NO [CANNOT] UID COPY source must be a folder")}
	}

	destination := server.labels.find(arguments[1].value)
	switch {
	case destination == nil:
		return []string{tagged(tag, "NO [TRYCREATE] destination mailbox does not exist")}
	case destination.role != LabelRole:
		return []string{tagged(tag, "NO [CANNOT] destination is not a label view")}
	case !selectableAttributes(destination.attributes):
		return []string{tagged(tag, "NO [CANNOT] destination mailbox is not selectable")}
	}

	member := source.byUID(uid)
	if member == nil || destination.byMessage(member.message) != nil {
		return []string{tagged(tag, "OK COPY completed")}
	}

	// UIDNEXT must stay a valid nonzero 32-bit UID after allocation.
	if destination.uidNext == math.MaxUint32 {
		return []string{tagged(tag, "NO [LIMIT] destination mailbox has no UIDs left")}
	}

	added := &membership{message: member.message, uid: destination.uidNext}
	destination.members = append(destination.members, added)
	destination.uidNext++

	if !server.labels.uidPlus {
		return []string{tagged(tag, "OK COPY completed")}
	}

	return []string{tagged(tag, fmt.Sprintf("OK [COPYUID %d %d %d] COPY completed", destination.uidValidity, uid, added.uid))}
}

// labelStoreDeleted permits only a silent single-UID +FLAGS (\Deleted) on a
// read-write label view. The marker belongs to that view's membership.
func labelStoreDeleted(view *labelView, tag string, arguments []imapToken) []string {
	if len(arguments) != 3 || arguments[1].isList || arguments[1].quoted {
		return []string{tagged(tag, "BAD UID STORE requires a UID set, an operation and flags")}
	}

	uid, failure := parseSingleUID(tag, arguments[0])
	if failure != nil {
		return failure
	}

	switch strings.ToUpper(arguments[1].value) {
	case "+FLAGS.SILENT":
	case "+FLAGS", "-FLAGS", "-FLAGS.SILENT":
		return []string{refused(tag, "STORE other than +FLAGS.SILENT")}
	case "FLAGS", "FLAGS.SILENT":
		return []string{refused(tag, "FLAGS replacement")}
	default:
		return []string{tagged(tag, "BAD unsupported STORE operation")}
	}

	flags := []imapToken{arguments[2]}
	if arguments[2].isList {
		flags = arguments[2].list
	}
	if len(flags) != 1 || flags[0].isList || flags[0].quoted || !isDeletedFlag(flags[0].value) {
		return []string{refused(tag, "changing flags other than \\Deleted")}
	}

	if view.role != LabelRole {
		return []string{tagged(tag, "NO [CANNOT] \\Deleted STORE requires a selected label view")}
	}

	if member := view.byUID(uid); member != nil {
		member.deleted = true
	}

	return []string{tagged(tag, "OK STORE completed")}
}

// labelExpunge removes one marked membership from the selected label view. An
// absent or unmarked UID completes OK without a change.
func labelExpunge(view *labelView, tag string, arguments []imapToken) []string {
	if len(arguments) != 1 {
		return []string{tagged(tag, "BAD UID EXPUNGE requires a UID set")}
	}

	uid, failure := parseSingleUID(tag, arguments[0])
	if failure != nil {
		return failure
	}

	if view.role != LabelRole {
		return []string{tagged(tag, "NO [CANNOT] UID EXPUNGE requires a selected label view")}
	}

	index := slices.IndexFunc(view.members, func(member *membership) bool { return member.uid == uid && member.deleted })
	if index < 0 {
		return []string{tagged(tag, "OK EXPUNGE completed")}
	}

	view.members = slices.Delete(view.members, index, index+1)

	return []string{fmt.Sprintf("* %d EXPUNGE", index+1), tagged(tag, "OK EXPUNGE completed")}
}

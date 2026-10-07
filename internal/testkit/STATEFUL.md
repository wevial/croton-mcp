# Stateful synthetic IMAP fixture

`testkit.Start` has an opt-in stateful mode for tests that must witness
persisted `\Seen` changes and cross-mailbox moves through a real IMAP client.
It is test infrastructure only. It does not enable mutation in Croton, and it
does not relax `AssertReadOnlyCommands` or the read-tool transcript guards.

Leaving `Options.Stateful` nil keeps the legacy fixture: `Messages`, `Seen` and
`Scenario` behave as before. Stateful mode cannot be combined with those
fields. It reuses the generated trust, TLS modes and ephemeral `127.0.0.1`
listener, and it records every command in `Commands()`, including refused ones.

All seeds must be synthetic. Use reserved `.test` addresses and invented
content. Mailbox names such as `Labels/...` are fixture examples and do not
describe actual Proton Mail Bridge layout.

## Seeding

```go
server, err := testkit.Start(testkit.Options{
	Mode: testkit.ImplicitTLS,
	Stateful: &testkit.StatefulOptions{
		Move: true, // advertise MOVE and accept UID MOVE
		Mailboxes: []testkit.MailboxSeed{
			{Name: "INBOX", Messages: []testkit.MessageSeed{
				{Body: "From: Fixture <fixture@croton.test>\r\nSubject: One\r\n\r\nBody\r\n"},
				{Body: "...", Flags: []string{`\Seen`, `\Flagged`}},
			}},
			{Name: "Folders/Existing"},
			{Name: "Archive", Attributes: []string{`\Archive`}},
			{Name: "Trash", Attributes: []string{`\Trash`}, UIDValidity: 7001},
		},
	},
})
```

- Mailboxes are listed in seed order. INBOX is not created implicitly and
  matches case-insensitively; other names match exactly. The delimiter is `/`.
- `Attributes` are returned verbatim by LIST. `\Noselect` and `\NonExistent`
  make a mailbox nonselectable for SELECT, EXAMINE, STATUS and MOVE targets.
  No `SPECIAL-USE` capability is advertised.
- `UIDValidity` zero assigns `1001 + seed index`. Message `UID` zero assigns the
  previous UID plus one, starting at 1. UIDs must increase. `UIDNEXT` starts at
  the highest seeded UID plus one, so a seeded UID of 4294967295 is rejected.
- Clients authenticate with any synthetic LOGIN or AUTHENTICATE PLAIN
  credentials after TLS.

## Snapshots and controls

- `Snapshot() []MailboxState` returns every mailbox in LIST order.
  `MailboxSnapshot(name)` returns one. Both are detached deep copies, so
  editing them cannot change fixture state. Legacy servers return nil or false.
- `SetMailboxAttributes(name, attributes...)` replaces LIST attributes.
- `RemoveMailbox(name)` deletes a mailbox and its messages. Connections that
  selected it get `NO` for later message commands.
- `ReplaceUIDValidity(name, value)` sets a new nonzero generation. Messages and
  UIDs are kept, so tests can check stale-generation handling.

Every change is visible to the next command on any connection. Other
connections are not sent unsolicited EXISTS or EXPUNGE updates.

## Supported grammar

Mailbox state is shared. Selection belongs to one connection and is lost on
reconnect. A failed SELECT or EXAMINE clears it.

| Command | Behavior |
| --- | --- |
| `CAPABILITY` | `IMAP4rev1 AUTH=PLAIN`, plus `MOVE` when enabled |
| `LIST "" <pattern>` | `*` and `%` wildcards; `""` returns the delimiter |
| `SELECT` / `EXAMINE` | FLAGS, EXISTS, RECENT, PERMANENTFLAGS, UIDVALIDITY, UIDNEXT; `[READ-WRITE]` or `[READ-ONLY]` |
| `STATUS <mailbox> (...)` | `MESSAGES`, `UIDNEXT`, `UIDVALIDITY`, `UNSEEN` |
| `UID SEARCH` | `ALL`, `SEEN`, `UNSEEN`, `UID <set>`, combined with AND |
| `UID FETCH <set> (...)` | `UID`, `FLAGS`, `RFC822.SIZE`, `INTERNALDATE`, `BODY.PEEK[]`, `BODY.PEEK[HEADER]`, `BODY.PEEK[TEXT]`, each with an optional `<offset.size>` |
| `UID STORE <set> ±FLAGS[.SILENT] (\Seen)` | read-write selection only; non-silent forms return `FETCH (UID FLAGS)` |
| `UID MOVE <set> <mailbox>` | read-write selection and `Move: true` only |

UID sets accept numbers, ranges, comma lists and `*`, which means the highest
UID present. A partial `<offset.size>` needs a 32-bit offset and a nonzero
32-bit size. The response echoes the requested origin; an origin past the end
of the section returns an empty literal. Unsupported search keys, `CHARSET`,
`RETURN`, parenthesized search groups and unsupported FETCH items return tagged
`BAD`. They never fall back to broader results. Non-peek body items are refused
because they would set `\Seen` implicitly.

UID MOVE ignores missing source UIDs. It gives the existing source messages
new destination UIDs from the destination `UIDNEXT`, in ascending source
order, and keeps their bodies and flags. It then sends
`* OK [COPYUID ...]`, followed by one `EXPUNGE` per moved message in
descending sequence order, and a tagged OK. The go-imap client parses this
into `MoveData` and updates its message count. Refusals leave both mailboxes
unchanged:

- MOVE disabled: `BAD`
- no selection: `BAD`
- EXAMINE selection: `NO [READ-ONLY]`
- missing destination: `NO [TRYCREATE]`
- too few destination UIDs left for every moved message while keeping a valid
  `UIDNEXT`: `NO [LIMIT]`
- nonselectable destination or same mailbox: `NO [CANNOT]`

With MOVE disabled, go-imap's `Client.Move` falls back to COPY, STORE and
EXPUNGE. The fixture refuses those, so send a raw `UID MOVE` to test the
disabled case.

## Refused and unsupported commands

`COPY`, `UID COPY`, `EXPUNGE`, `UID EXPUNGE`, `DELETE`, `APPEND`, `CREATE`,
`RENAME`, `SUBSCRIBE`, `UNSUBSCRIBE`, `CLOSE` and non-UID `MOVE` or `STORE`
return `NO [CANNOT]`. UID STORE also returns `NO [CANNOT]` for `FLAGS`
replacement and for any flag list other than exactly `\Seen`. A synchronizing
literal (`{n}`) is refused before a continuation is granted. A
non-synchronizing literal (`{n+}`) closes the connection. Other commands
return `BAD`.

Not implemented: ENVELOPE, BODYSTRUCTURE, sequence-number FETCH or SEARCH,
IDLE, UNSELECT, NAMESPACE, CONDSTORE, UIDPLUS, extended LIST, COPY, EXPUNGE,
arbitrary flag changes, and Scenario fault injection. Use the mutation faults
below instead. Scoped COPY, `\Deleted` STORE and UID EXPUNGE exist only in the
separate label mode below.

## Mutation faults

`InjectFault` arms a one-shot fault for a lost completion or a timeout around
one `UID STORE` or `UID MOVE`. It does not change mutation semantics, and it
never retries or replays a command.

```go
fault, err := server.InjectFault(testkit.MutationFault{
	Command:  "UID MOVE",        // or "UID STORE"
	UIDs:     "2,5",             // exact UID set operand as sent
	Boundary: testkit.AfterApplication,
	Action:   testkit.DropConnection, // or testkit.HoldResponse
})
```

- Selector: all four fields are required. The fault matches the first
  authenticated command whose parsed verb and exact `Set` equal the selector
  (see the transcript parser below). The match does not depend on selection,
  flags, destination or the dispatch outcome. A command that does not parse
  exactly does not match and runs normally. Faults are consumed in the order
  they were injected. Legacy servers return an error.
- `BeforeApplication` fires after the command is recorded, before the fixture
  checks the selection or touches state. The target is never applied, even
  after the client or the fixture closes the connection.
- `AfterApplication` dispatches the target once, exactly as it would run
  without a fault, and releases the state lock. Then it fires. The target's
  tagged line, which may be `OK`, `NO` or `BAD`, is available from
  `Completion()`. No response line, untagged or tagged, is written.
- `DropConnection` closes the connection. The client sees EOF and no tagged
  completion.
- `HoldResponse` writes nothing and discards unrecorded input until the client
  closes the connection or `Server.Close` runs. There is no release. Other
  connections and `Snapshot` keep working while the response is held, and they
  already see an after-application change.

`FaultHandle` provides deterministic synchronization, so no sleeps are needed:

- `Triggered()` closes once the target reaches its boundary, after an
  after-application change is visible.
- `Finished()` closes after the fixture has closed the faulted connection.
- `Command()` returns the recorded target `Command`, including its
  `Sequence` and `ConnectionID`.
- `Completion()` returns the withheld tagged completion. It is empty for
  `BeforeApplication`.

`Server.Close` closes every connection, including held ones, and waits for
their handlers to return, so `Finished()` is closed by the time `Close`
returns. An armed fault that never matched is discarded. To model a
deadline-bounded client, wait on `Triggered()`, let the client's own deadline
expire, close the client, and then wait on `Finished()`. Bound every wait with
a test deadline.

A replay of a spent fault's command runs normally. A replayed `UID MOVE` of
UIDs that already moved completes `OK` with no `COPYUID`. A replayed
`+FLAGS \Seen` leaves state unchanged, so a snapshot alone cannot count
replays. Count them with the transcript parser.

## Transcript parser

`ParseTranscript(server.Commands())` and `ParseTranscriptCommand(command)`
provide an independent, structured view of recorded commands. They do not
change or replace `AssertReadOnlyCommands`, which remains the read-only
allowlist.

- Framing is exact: `tag SP verb [SP arguments]` with single spaces. A valid
  tag has no `+`. Control characters are rejected. `Verb` is uppercase and
  comes only from the position after the tag. UID forms become `UID STORE`,
  `UID MOVE` and so on, so `UID COPY 1 "UID MOVE 1 Trash"` is a `UID COPY`.
  An unknown verb such as `XMOVE` is not a MOVE.
- `STORE`, `COPY` and `MOVE`, with or without `UID`, and `UID EXPUNGE` get
  structured operands. `Set` is the exact set text, and `Ranges` decodes it in
  wire order, with 0 meaning `*`. `UID EXPUNGE` takes exactly one set and
  nothing after it. Bare `EXPUNGE` keeps no operands and no `Set`. For STORE, `Store` is `FLAGS`, `+FLAGS` or `-FLAGS`, `Silent`
  reports `.SILENT`, and `Flags` lists the flags as sent from a parenthesized
  or bare list. For COPY and MOVE, `Destination` is the atom or the unquoted
  string with `\"` and `\\` escapes decoded. Mailbox names are not
  modified-UTF-7 decoded.
- Every other verb keeps only its raw `Arguments`.
- Malformed input returns an error, never a partial result. This covers
  missing, extra or doubled spaces, missing or trailing operands, zero,
  leading-zero or out-of-range set numbers, quoted sets, unknown STORE operations, nested or
  unbalanced flag lists, bad escapes, unterminated strings and literals.
  `ParseTranscript` stops at the first error and names its sequence number.
  Errors never include raw command text.
- Output keeps receive order, and each entry carries `Sequence`,
  `ConnectionID` and `TLS`.

## Use from other packages

MCP tests in other packages can start the fixture as shown above. Point a
`bridge.Adapter` at `server.Addr()` with `server.SPKISHA256()`, then compare
`server.Snapshot()` before and after a tool call. Seed state and change it only
through `Options.Stateful`, the controls above and `InjectFault`. For a
lost-completion or no-replay witness, arm the fault before the tool call, wait
on `Finished()`, then compare the snapshot and count parsed `UID STORE` or
`UID MOVE` entries per `ConnectionID`. Never use live mailbox data.

## Label mode

`Options.Labels` opts into a separate linked folder and label-view model. It
is the first prerequisite for future label-safety witnesses, not a label
adapter, a registered tool or proof of installed Bridge behavior. It cannot be
combined with `Stateful`, `Messages`, `Seen` or `Scenario`, and it leaves the
legacy and ordinary stateful modes, `AssertReadOnlyCommands` and the shipped
Seen and MOVE guards unchanged. `InjectFault` and the mailbox controls above
return an error in label mode; use `InjectLabelFault` below.

```go
server, err := testkit.Start(testkit.Options{
	Mode: testkit.ImplicitTLS,
	Labels: &testkit.LabelOptions{
		UIDPlus: true, // advertise UIDPLUS after login, emit COPYUID, accept UID EXPUNGE
		Messages: []testkit.LabelMessage{
			{ID: "A", Body: "...", Flags: []string{`\Seen`}},
			{ID: "B", Body: "..."},
		},
		Views: []testkit.LabelViewSeed{
			{Name: "INBOX", Role: testkit.FolderRole, UIDValidity: 7001, Members: []testkit.Membership{
				{Message: "A", UID: 101}, {Message: "B", UID: 501},
			}},
			{Name: "Labels/One", Role: testkit.LabelRole, UIDValidity: 8001, Members: []testkit.Membership{
				{Message: "B", UID: 101}, {Message: "A", UID: 501, Deleted: true},
			}},
		},
	},
})
```

- A message `ID` is a stable synthetic identity. It is never sent on the wire
  and is not a UID mapping API. Identity is never inferred from bodies,
  Message-ID or equal UID numbers; a client addresses each view by that view's
  own UID and UIDVALIDITY.
- Body and message `Flags`, such as `\Seen`, are shared by every view of a
  message. `\Deleted` is view-local: it belongs to one `Membership` and is
  rejected in message flags. FETCH returns the shared flags followed by the
  view's `\Deleted`.
- Every view needs an explicit `Role`. Behavior follows the role, never the
  name: a `FolderRole` view named `Labels/...` is still a folder.
- Seeds are rejected for a missing role, an empty or duplicate message ID, a
  reference to an unknown message, a duplicate membership of one message in
  one view, a duplicate or zero view UID, or a message without exactly one
  folder membership. Members may be listed in any order and are stored in UID
  order. UIDVALIDITY and UIDNEXT follow the ordinary stateful rules.
- `LabelSnapshot()` returns a detached `LabelState` with messages in seed order
  and views in LIST order. It reports false outside label mode, and
  `Snapshot()` returns nil in label mode.

Selection belongs to one connection and is lost on reconnect, as above. Other
connections are not sent unsolicited updates.

| Command | Behavior |
| --- | --- |
| `CAPABILITY` | `IMAP4rev1 AUTH=PLAIN`, plus `UIDPLUS` after authentication when enabled; never `MOVE` |
| `LIST`, `STATUS` | as in ordinary stateful mode |
| `SELECT` / `EXAMINE` | as in ordinary stateful mode; a read-write label view has `PERMANENTFLAGS (\Deleted)`, a folder `()` |
| `UID SEARCH`, `UID FETCH` | ordinary stateful grammar over the selected view |
| `UID COPY <uid> <mailbox>` | read-write folder selection, one existing selectable `LabelRole` target |
| `UID STORE <uid> +FLAGS.SILENT (\Deleted)` | read-write label view selection only |
| `UID EXPUNGE <uid>` | read-write label view selection and `UIDPlus: true` only |

The new forms take exactly one UID number. A well-formed range, list or `*`
returns `NO [CANNOT]`; a malformed set returns `BAD`.

- UID COPY adds the folder message to the label view with the next UID from
  the view's UIDNEXT. The folder, its UID, the body, shared flags and other
  label memberships are unchanged. With UIDPLUS the tagged OK carries
  `[COPYUID <uidvalidity> <source> <destination>]`. A missing source UID, or a
  message already in the target, completes OK with no change and no COPYUID.
- `\Deleted` STORE marks only that view's membership and returns no FETCH
  update. A missing UID completes OK with no change.
- UID EXPUNGE removes the membership only if it is marked `\Deleted` in the
  selected view, sending one `* <n> EXPUNGE` before the tagged OK. The message
  stays in its folder and other labels, and other marked entries in the view
  are kept. An absent or unmarked UID completes OK with no change. These empty
  completions are protocol results, not product refusals; a later tool must
  check identities itself.

Refusals are recorded and leave state unchanged:

- UID EXPUNGE without UIDPLUS, or any new form without a selection: `BAD`
- EXAMINE selection: `NO [READ-ONLY]`
- missing COPY target: `NO [TRYCREATE]`
- COPY from a label view, to a folder or to a nonselectable label view;
  `\Deleted` STORE or UID EXPUNGE in a folder: `NO [CANNOT]`
- `FLAGS` replacement, `-FLAGS`, non-silent `+FLAGS`, and any flag list other
  than exactly `\Deleted`, including `\Seen`: `NO [CANNOT]`
- `UID MOVE`, `COPY`, `STORE`, `EXPUNGE`, `CLOSE`, `MOVE`, `DELETE`, `APPEND`,
  `CREATE`, `RENAME`, `SUBSCRIBE` and `UNSUBSCRIBE`: `NO [CANNOT]`

Literals are refused as in ordinary stateful mode. Not modeled: label
creation, deletion or rename, rename or recreate races, and any implicit
expunge.

## Label faults

`InjectLabelFault` arms a one-shot fault around one label-mode `UID COPY`,
`\Deleted` `UID STORE` or `UID EXPUNGE`. It is fixture proof infrastructure
for later witnesses, not Croton result handling: a dropped, held or refused
fixture response does not show how a future tool classifies, aborts or avoids
replay. Those claims need the real public entrypoint, with its transcript and
state compared against this fixture.

```go
fault, err := server.InjectLabelFault(testkit.LabelFault{
	Command:     "UID COPY",   // or "UID STORE", "UID EXPUNGE"
	View:        "INBOX",      // the selected view
	UID:         "101",        // exactly one UID as sent
	Destination: "Labels/Two", // UID COPY only
	Boundary:    testkit.AfterApplication,
	Action:      testkit.DropConnection, // or HoldResponse, RejectCommand
})
```

- Selector: `Command`, an existing `View`, a single nonzero `UID` without
  leading zeros, `Boundary` and `Action` are required. UID COPY needs an
  existing `Destination`; the other forms reject one. Ranges, lists, `*`,
  other verbs and use outside label mode return an error and arm nothing.
- The fault matches the first authenticated command that the label dispatcher
  would apply, issued in the selector's view selected read-write, whose parsed
  verb, exact `Set` and COPY destination equal the selector. UID STORE must be
  exactly `+FLAGS.SILENT (\Deleted)`, and UID EXPUNGE needs `UIDPlus`. COPY
  needs a folder selection and a selectable label destination; STORE and
  EXPUNGE need a label view. Any other command, including EXAMINE selections,
  refusals, reads and commands that do not parse exactly, runs normally and
  leaves the fault armed. Faults are consumed in injection order.
- `BeforeApplication` never applies the target. `AfterApplication`
  dispatches it once through the label dispatcher, releases the store lock,
  and then fires; `Completion()` returns the withheld tagged line. That line
  is a fixture oracle only; the client never sees it.
- `DropConnection` closes the connection without a response line.
- `HoldResponse` writes nothing. Unlike the ordinary hold, every further
  input line is recorded in `Commands()` with the connection's ID and TLS
  state, but never dispatched, until the client or `Server.Close` ends the
  connection. A replay sent on a held connection is therefore visible in the
  transcript and absent from state.
- `RejectCommand` is accepted only for UID EXPUNGE at `BeforeApplication`.
  It writes exactly `<tag> NO [UNAVAILABLE] UID EXPUNGE refused by fixture
  fault`, with no EXPUNGE response, and keeps the connection open. A
  `\Deleted` marker stored earlier stays visible; nothing repairs it. A later,
  explicitly issued UID EXPUNGE runs normally.

`Triggered()`, `Finished()`, `Command()` and `Completion()` behave as for
ordinary faults. After `RejectCommand`, `Finished()` closes when that
connection later ends. The ordinary `InjectFault`, its selectors and its hold
semantics are unchanged and still return an error in label mode.

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
below instead.

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
- `STORE`, `COPY` and `MOVE`, with or without `UID`, get structured operands.
  `Set` is the exact set text, and `Ranges` decodes it in wire order, with 0
  meaning `*`. For STORE, `Store` is `FLAGS`, `+FLAGS` or `-FLAGS`, `Silent`
  reports `.SILENT`, and `Flags` lists the flags as sent from a parenthesized
  or bare list. For COPY and MOVE, `Destination` is the atom or the unquoted
  string with `\"` and `\\` escapes decoded. Mailbox names are not
  modified-UTF-7 decoded.
- Every other verb keeps only its raw `Arguments`.
- Malformed input returns an error, never a partial result. This covers
  missing, extra or doubled spaces, trailing operands, zero,
  leading-zero or out-of-range set numbers, unknown STORE operations, nested or
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

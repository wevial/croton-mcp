# Croton MCP server

`croton-mcp` exposes the bridge layer as an MCP server over stdio. It is
read-only unless its configuration file opts in to the Seen
[triage tools](#triage-tools).
Stdout carries protocol frames only; all diagnostics go to stderr. There is no
network listener, and no MCP resources, prompts, Roots, Sampling, or Logging —
a fail-closed method allowlist rejects everything except initialization,
discovery, ping, and the tool methods.

This page covers the Mail server only. The separate Drive executable,
`croton-drive-mcp`, has its own contract in [docs/DRIVE-MCP.md](DRIVE-MCP.md);
its bounds, error vocabulary, and always-on audit stream differ from those
described here, and every claim there names the test that pins it.

The historical [proposed Mail mutation boundary](design/0002-mail-mutation.md)
is superseded by the [Mail triage design](design/0005-mail-triage.md). The
successor plans five default-off triage tools behind a local
`mutations.enabled` file opt-in, limited to Seen `UID STORE` and native
`UID MOVE`, with approval owned by the trusted client. All five are
implemented: the Seen tools `mark_read` and `mark_unread`, and the move tools
`move_mail`, `archive_mail` and `trash_mail`. With the opt-in absent or false,
the default, the catalog is the six read tools below and Mail remains
read-only.

## Protocol

The server targets MCP `2026-07-28` and retains legacy compatibility through
the official Go SDK (`modelcontextprotocol/go-sdk`).

`TestIndependentStdioCatalog` provides independent legacy protocol coverage
against the built Mail executable using standard-library JSON and pipes, without
an MCP SDK client or Hermes. It initializes with `2025-11-25` and checks the exact
six read-only tools, with one frame per write and a frame split across two writes.
Each case uses a fresh child and synthetic secure config, verifies protocol-only
stdout and clean shutdown after stdin EOF, and asserts the credential helper was
never invoked. It performs no tool calls or Bridge/account access. This is generic
protocol evidence, not validated Claude Code or Codex compatibility or branded-client
certification. `TestIndependentStdioCatalogDocumentation` pins this coverage note.

## Running

```sh
croton-mcp --config /absolute/path/to/croton.json
```

The configuration file is bounded JSON (64 KiB maximum, top-level object required,
unknown, null, duplicate, and case-folded-alias fields rejected) mirroring
`bridge.Config`. Secure configuration loading is supported on Linux and macOS,
which share one descriptor-relative no-follow traversal over every path
component. Windows and all other platforms compile but fail closed: they refuse
to load configuration at all until they have an equivalent secure opener.
The file must be an absolute, symlink-free regular path owned by the current
user and must not grant group or world permissions (normally mode `0600`):

```json
{
  "imap": {
    "host": "127.0.0.1",
    "port": 1143,
    "tlsMode": "starttls",
    "credentialCommand": ["/usr/local/bin/croton-credentials"],
    "tls": {"spkiSha256": "<lowercase hex SPKI pin>"}
  },
  "bounds": {"maxSearchResults": 50},
  "audit": {"enabled": true}
}
```

The optional `mutations` object accepts only the boolean `enabled`. Absent or
false, the default, registers only the six read tools; true also registers
`mark_read`, `mark_unread`, `move_mail`, `archive_mail` and `trash_mail`. Any other type, including null, a string or a
number, and any unknown key inside `mutations` fail startup with the static
invalid-configuration error before any Bridge connection. Setting it is a
local operator opt-in, not approval of any call. The example above and the
installation guides leave it absent.

Validation and clamping remain the bridge's responsibility; the loader only
reads and decodes. `SIGTERM` and `SIGINT` shut the server down cleanly and log
out the retained IMAP session.

## Hermes registration

Hermes runs Croton as a stdio MCP server. Two prerequisites: an absolute path
to a built `croton-mcp` binary, and the absolute path of a secure
configuration file as described above.

```sh
hermes mcp add croton --connect-timeout 60 \
  --command /absolute/path/to/croton-mcp \
  --args --config /absolute/path/to/croton.json
```

`--args` must come last: everything after it is passed through to Croton.
`hermes mcp add` starts Croton once to discover its catalog and stores the
resulting registration in the active Hermes profile.

Verify and inspect the registration:

```sh
hermes mcp test croton
hermes mcp list
```

`hermes mcp test croton` reports the six raw tool names Croton advertises:

- `list_folders`
- `search_mail`
- `get_message`
- `get_thread`
- `list_attachments`
- `select_digest_candidates`

When Hermes registers those tools for model use, it prefixes each name with
the server name, producing exactly six runtime names:

- `mcp__croton__list_folders`
- `mcp__croton__search_mail`
- `mcp__croton__get_message`
- `mcp__croton__get_thread`
- `mcp__croton__list_attachments`
- `mcp__croton__select_digest_candidates`

Croton exposes no resources or prompts, so Hermes registers no prefixed
`list_resources`, `read_resource`, `list_prompts`, or `get_prompt` utilities.

Registration needs no `--env` secrets. Croton reads its own configuration file
from the path given after `--config`; credential material belongs behind that
file's `credentialCommand` and never on a command line or in profile
documentation.

To try the registration without touching an existing profile, point Hermes at a
throwaway profile directory first:

```sh
export HERMES_HOME="$(mktemp -d)"
```

The profile and configuration changes made by the commands above are then
confined to that temporary directory, leaving the existing profile untouched.

Removal is the rollback:

```sh
hermes mcp remove croton
```

`hermes mcp remove` asks for interactive confirmation before dropping the
registration; afterwards `hermes mcp list` no longer shows the `croton`
registration (in the throwaway profile above, it lists nothing at all).

## Tools

By default exactly six read-only tools are registered, each with `readOnlyHint: true`,
a closed (`additionalProperties: false`) bounded input schema, and
server-authoritative validation and clamping that does not trust schema
enforcement by the caller. Raw argument objects are capped at 24 KiB and reject
non-objects, nulls, duplicate/case-folded-alias or unknown fields, excessive
nesting, and trailing JSON values. String schemas publish both standard
character `maxLength` and authoritative `x-maxBytes` annotations. The stdio
transport admits only one unambiguous newline-delimited JSON object of at most
64 KiB before handing each frame to the SDK:

| Tool | Purpose |
| ---- | ------- |
| `list_folders` | Bounded folder list from the local Bridge. |
| `search_mail` | Structured, bounded search in one mailbox; returns metadata. |
| `get_message` | One message by opaque id: headers, normalized text, attachment metadata. |
| `get_thread` | Local thread resolution with bounded sibling fetches. |
| `list_attachments` | Attachment metadata only; never attachment bytes. |
| `select_digest_candidates` | Metadata-first digest selection composing STATUS and one bounded search; no body fetches. |

Message identifiers are the bridge's opaque HMAC-bound ids; they are validated
against a fresh UIDVALIDITY generation on every use.

### Flat read identities

Four flat read shapes carry numeric `uidvalidity` and `uid` beside the opaque
`id` and the exact `mailbox`: each `search_mail` result, each
`select_digest_candidates` candidate, and the parent message of `get_message`
and `list_attachments`. Both values are positive 32-bit integers, 1 to
4294967295. They come from the same successful read-only EXAMINE and UID FETCH
generation that the bridge bound into that `id`, so clients never decode the
opaque id. A read that cannot supply a positive tuple fails rather than
returning a partial, zero or guessed identity, and a stale id fails with
`stale_id` and no tuple. Attachment entries, digest counters and the
`get_thread` aggregate carry no UID.

These fields are read metadata only. They are not account authentication or
approval, grant no write authority, and different accounts may share the same
tuple. Account continuity is unavailable. The write pilot remains blocked. The
[Mail principal selection record](design/0007-mail-principal-selection.md)
describes the blocked continuity work.

Each `get_thread` node carries its own `mailbox`, `uidvalidity` and `uid`,
taken from the metadata already fetched for that member and associated by its
opaque `key`. A node never borrows the target's tuple, an ordinal or its RFC
`messageId`, and no extra read reconstructs a missing tuple. The thread's
top-level `id` and `mailbox` are context and carry no UID. A target without a
positive tuple fails with `unavailable` and a stale target fails with
`stale_id`. A stale sibling is omitted as before, and a sibling without a
positive tuple is omitted and the result is marked truncated.

Bounded output keeps every retained identity exact. Oversized search results,
digest candidates and thread nodes are removed whole, so each retained item
keeps its exact tuple, mailbox and opaque id. The `get_message` and
`list_attachments` fallbacks drop content but keep the parent's exact
identity; when even that cannot fit, the call fails with `bounds_exceeded`
instead of returning a shortened identity. An empty fallback list contains no
selectable item, and a thread fallback's top-level `id` and `mailbox` are
bounded context, not a selectable identity. A truncated or fallback result
lists fewer items, but every item it retains is a complete selectable
identity.

### Attachment size and MIME completeness

Each attachment entry of `get_message` and `list_attachments` carries
`hasDeclaredSize`. When it is `true`, `declaredSize` is present and is the
nonnegative `size` parameter the sender declared in `Content-Disposition`,
including a declared `0`. When it is `false` the size is unknown and
`declaredSize` is omitted; a missing or invalid declaration is never reported
as zero. The value is a sender-controlled declaration, not a count of decoded
bytes or of the transferred part.

Both results also carry a `truncation` object that names every limit that
shaped them. It is always present and holds only `true` keys, so a complete
result has an empty `truncation` object and no `truncated` marker.
`get_message` reasons are `headers`, `headerCount`, `text`, `mimeParts`,
`mimeDepth`, `attachments`, `references` and `output`. `list_attachments`
reasons are `headers`, `headerCount`, `mimeParts`, `mimeDepth`, `attachments`
and `output`; text and reference limits do not mark an attachment list
incomplete. `mimeParts`, `mimeDepth` and `headerCount` mean MIME parts, nested
entities or header fields beyond the parser's bounds were not examined, so
attachments may be missing; `attachments` means entries beyond the 20-entry
cap were omitted or their metadata was shortened. `output` means the
100 000-byte result cap removed text or whole attachment entries, or replaced
the result with its identity-only fallback; earlier reasons are kept.
`truncated` is `true` whenever any listed reason is.

An empty `attachments` list with an empty `truncation` object means the
parsed message declared no attachments. An empty list with any reason is
incomplete, not proof that the message has no attachments.

Listing attachments still fetches and parses the bounded raw MIME message with
`BODY.PEEK[]`; it is not a metadata-only transfer and never returns attachment
bytes. A message whose raw MIME or header block exceeds the parser's bounds
may be refused with an error code instead of a result.

### Search order and limits

`search_mail` and the search behind `select_digest_candidates` return results
in strictly descending UID order, whatever order the server sends SEARCH and
FETCH replies in. UID order is not header `Date` or delivery-date order, so a
page is not a chronological, latest-mail or complete-inventory guarantee.

Each search examines at most 8 UID windows of 100 UIDs each, newest first. A
dense window with more matches than the remaining result budget is not a
failure: the bridge reads the whole bounded SEARCH response, keeps the highest
UIDs that fit the budget, fetches metadata for only those, and reports
`truncated: true`. `truncated` is also set when an older UID range is left
unexamined. It stays false only when every window down to UID 1 was examined
and no match was left out. The result budget defaults to 50 and is capped at
250. The SEARCH response is still capped at 4096 bytes. An oversized response,
or a UID that is zero, outside its window or repeated in the raw SEARCH reply
or ESEARCH `ALL` set, fails the whole call with a code and returns no partial
page.

This release has no continuation cursor: a truncated search cannot be resumed
from where it stopped. Ordering and truncation describe read results only and
do not mean any write path is ready.

### Search metadata

Each `search_mail` result and each `select_digest_candidates` candidate also
carries `from`, `date`, `dateSource`, `readState` and, when it applies,
`metadataTruncated`. All of them come from the same read-only metadata FETCH
that produced the entry's `id`, `mailbox`, `uidvalidity` and `uid`: the
`BODY.PEEK[HEADER]` literal and the `FLAGS` item. No body, `INTERNALDATE` or
write command is used, and no tool, input or configuration is added.

- `from` is the decoded `From` header text, capped at 512 UTF-8 bytes. It is
  omitted when the header is missing, repeated or invalid. It is untrusted,
  sender-controlled text and proves no account, address or sender identity.
- `metadataTruncated` is `true` only when `from` was cut to its 512-byte cap,
  and is omitted otherwise.
- `date` is the message's own header `Date`, normalized to RFC 3339 UTC. It is
  sender-controlled and is not arrival, delivery or `INTERNALDATE` time.
- `dateSource` is `header` when `date` is present and `unknown` when the header
  `Date` is missing, repeated or invalid; `date` is then omitted. No other
  clock is substituted.
- `readState` is `read` when `FLAGS` includes `\Seen`, `unread` when `FLAGS`
  lacks it, and `unknown` when no `FLAGS` item was observed. Unread is never
  guessed.

An unknown date or read state does not fail an otherwise valid call.

Returning sender text and header dates is a deliberate disclosure of private
mail metadata to the MCP client, beyond subjects and sizes. It is not
permission to copy that metadata into shared notes or logs, and the audit
stream never records it.

Entries keep descending UID order. They are never sorted by `date`, so a
higher UID can carry an older or equal header date. A result is a bounded
sample, not a complete inventory: `truncated: true` means matches were left
out or a UID range was not examined, while an empty list with `truncated`
absent means no message matched in the examined range down to UID 1. The
digest `totalMessages` and `unseenCount` are mailbox STATUS counters, not the
number of matches examined or returned. When output is too large, whole
entries are removed from the end and `truncated` is set; a retained entry keeps
every field. There is no continuation cursor.

### Triage tools

When `mutations.enabled` is true, five more tools are registered with
`readOnlyHint: false` and `openWorldHint: false`. The Seen tools also publish
`destructiveHint: false` and `idempotentHint: true`; the move tools publish
`destructiveHint: true` and `idempotentHint: false`, because they remove
messages from their source. Annotations describe the tools; they neither
prompt for nor prove approval, which belongs to the trusted client under the
[Mail triage design](design/0005-mail-triage.md).

| Tool | Purpose |
| ---- | ------- |
| `mark_read` | Add `\Seen` to the given UIDs of one mailbox generation. |
| `mark_unread` | Remove `\Seen` from the given UIDs of one mailbox generation. |
| `move_mail` | Natively move the given UIDs of one mailbox generation to one exact existing destination. |
| `archive_mail` | Natively move the given UIDs of one mailbox generation to the one selectable `\Archive` mailbox. |
| `trash_mail` | Natively move the given UIDs of one mailbox generation to the one selectable `\Trash` mailbox. Nothing is deleted. |

All five take exactly `mailbox`, `uidvalidity` and `uids`: a positive 32-bit
generation and 1 to 50 distinct positive 32-bit UIDs. `move_mail` also takes
an exact `destination` mailbox name; `archive_mail` and `trash_mail` take no
destination. There is no approval, ordinal or message-id argument. A missing, null, zero, negative, fractional,
out-of-range, duplicate or unknown input fails the whole request with
`invalid_argument` before any IMAP command. So does a `destination` that is
empty, longer than 512 bytes, not UTF-8, or contains a control character, `*`
or `%`. Any other exact UTF-8 name is accepted as written, including one with
an ampersand such as `Folders/R&D-2026`.

The server selects the source mailbox read-write on its one authenticated
session and compares the fresh UIDVALIDITY with `uidvalidity`. A mismatch
fails with `stale_id`, and a mailbox that cannot be selected fails the
request; neither sends a write. The server then handles one UID at a time in
input order: `UID SEARCH` confirms the UID is present, and only then one
`UID STORE <uid> +FLAGS.SILENT (\Seen)` or `-FLAGS.SILENT (\Seen)` is sent.
No other flag, STORE form, MOVE, COPY or EXPUNGE is sent, and the read tools
keep their `BODY.PEEK` fetches.

`move_mail` uses the same selection and presence check, then resolves its
destination from a fresh ordinary `LIST "" "*"` on that same session before
every UID, without selection or `RETURN` options, and sends one native
`UID MOVE <uid> <destination>`. Every listed wire name is decoded from
modified UTF-7, so a malformed wire name anywhere in the account fails the
listing; if nothing has been moved yet the request fails with no write. The
destination must match exactly one listed name, INBOX case-insensitively and
every other name exactly, and be selectable: not `\Noselect` or
`\NonExistent`, and not a `\All` or `\Flagged` virtual view. It must then be
INBOX, an entry beneath `Folders/`, or a hierarchy-root mailbox carrying
`\Sent`, `\Drafts`, `\Junk`, `\Archive` or `\Trash`. A name such as
`Archive` without the attribute, anything under `Labels/` and every other
mailbox are refused. A destination equal to the source, or a server that does
not advertise `MOVE`, refuses every UID before any LIST or write. There is no
COPY, STORE or EXPUNGE fallback.

`archive_mail` and `trash_mail` follow `move_mail` exactly, except that each
fresh listing maps the destination from protocol attributes: the one listed
mailbox carrying `\Archive` or `\Trash` respectively. Neither the
`SPECIAL-USE` capability nor `LIST RETURN (SPECIAL-USE)` is needed, and a
name such as `Archive` or `Trash` without the attribute never maps. No holder,
several holders, or a holder that fails any `move_mail` destination rule above,
including `Labels/`, virtual views and nonselectable mailboxes, is
`unsupported`. So is a holder named `All Mail` or `Starred`, Bridge's
aggregate views, even without `\All` or `\Flagged`. A holder equal to the
source mailbox is refused. Because the mapping is redone before every UID, a
mailbox that gained, lost or now shares the attribute is resolved afresh.
`trash_mail` only moves; it never deletes, expunges or empties Trash.

The result is `{"results": [...]}` with one entry per input UID in input
order, each with `uid`, `outcome` and an optional `code` from the error
vocabulary below:

- `applied`: the STORE or MOVE completed with OK.
- `refused`: the UID is not present (`not_found`), or the server answered NO
  or BAD (`unavailable`). Later UIDs continue. For the move tools, an
  unsupported destination or a missing `MOVE` capability is `unsupported`
  and refuses that UID and every later one; a NO after validation, for
  example because the destination disappeared, refuses only that UID.
- `unknown`: the connection dropped or the command deadline expired after
  dispatch (`unavailable`, `timed_out` or `canceled`). The session is
  discarded, nothing is retried or replayed, and every later UID is
  `not_attempted`.
- `not_attempted`: never dispatched. If the presence check itself fails, that
  UID carries a code and every later UID is also not attempted.

Each IMAP step has its own `imap.commandTimeoutMs` deadline, so a batch is
bounded. To reconcile an `unknown`, run a fresh read such as `search_mail`,
then make a new, separately approved call. `TestStoryTriageSeen`,
`TestStoryTriageMove` and `TestStoryTriageSpecial` witness these boundaries against the built executable
and the synthetic stateful fixture; no live account write has been exercised.

### Mail thread membership

Thread results may include unlinked messages with the same base subject.
`get_thread` gathers bounded same-subject candidates from the target mailbox;
the base subject strips repeated `Re:`, `Fw:`, and `Fwd:` prefixes. Reply
relationships are resolved from `References` and `In-Reply-To` headers.
An unlinked subject sibling has depth 0 and no parent; subject similarity alone does not establish a reply relationship.

`maxMessages` bounds the returned nodes and body fetches. When matching siblings
are omitted because of this bound, the result reports `truncated: true`.
`TestGetThreadSyntheticWire` verifies linked and independent-root membership,
the one-message bound, and read-only PEEK fetches over loopback TLS using
synthetic mail. `TestThreadSubjectSiblingDocumentation` pins the contract
sentences above.

## Output

Serialized tool results are capped (100 000 bytes). Oversize results are
truncated structurally — whole list items or text halved on rune boundaries —
and re-marshaled, so output is always syntactically valid JSON with a
`truncated` marker. Search-backed tools also set `truncated` when the bridge's
bounded UID-window traversal leaves any mailbox range unexamined, even when the
returned item count is below the tool's requested limit. Serialized JSON is
never byte-sliced.

## Errors

Adapter failures map to a stable, secret-free vocabulary:
`invalid_argument`, `not_found`, `stale_id`, `bounds_exceeded`, `timed_out`,
`canceled`, `unavailable`, `unsupported`, `internal`. Unknown or wrapped
errors collapse to `internal`; no credentials, endpoints, mailbox data, or
stack details can reach the protocol stream.

## Audit

With `audit.enabled`, one JSON line per tool call is written to stderr using
only the allowlisted vocabulary `event`, `tool`, `outcome`, `code`,
`truncated`. Tool names, outcomes, and codes are re-validated against fixed
sets before logging; caller inputs, folder names, identifiers, subjects,
addresses, and error text never appear. A triage call that returns per-UID
results logs `outcome` `ok`; its UIDs and per-UID outcomes are not logged.

With the same opt-in setting, Mail `Serve` completion writes exactly one
independent lifecycle JSON line to the diagnostic stream (stderr), never to
the JSON-RPC stream. Its only fields are `event` (always `transport_end`) and
`category`, from this closed vocabulary:

- `normal_close`: nil, EOF, or MCP connection closed.
- `canceled`: context cancellation.
- `client_disconnected`: a broken pipe (`EPIPE`); this is not proof of user intent.
- `transport_failure`: an unclassified transport failure.

Classification uses typed error identity, including wrapped errors, and never
error text. No underlying error detail is emitted. Normal closure and
cancellation still return success; other failures still return the static
`server unavailable` error. With audit disabled, no lifecycle event is
written. Direct `Connect` behavior is unchanged.

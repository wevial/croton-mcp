# Croton MCP tool reference

`croton-mcp` is a stdio MCP server. It targets MCP `2026-07-28` and keeps the
legacy `2025-11-25` `initialize` flow through the official Go SDK. Install and
registration steps are in [INSTALL.md](INSTALL.md).

## Protocol surface

- Stdout carries only JSON-RPC frames, tool errors included; audit lines and
  startup errors go to stderr.
- A fail-closed allowlist serves only `initialize`, `ping`, `server/discover`,
  `tools/list`, `tools/call` and the `initialized`, `cancelled` and
  `progress` notifications. Everything else is `method not supported`.
- No resources, prompts, Roots, Sampling or MCP Logging.
- Each inbound frame must be one strictly decoded JSON object of at most
  64 KiB before the SDK sees it.
- Tool arguments are capped at 24 KiB and decoded strictly: one object, no
  unknown, null, duplicate or case-folded-alias fields, no trailing values.
  Schemas are closed (`additionalProperties: false`) and publish `maxLength`
  and `x-maxBytes`, but the server validates and clamps every value itself.
- Every successful result is one JSON text item of at most 100 000 bytes.
  Oversize results shrink structurally (whole list items removed, text halved
  on rune boundaries) and set `truncated`; JSON is never byte-sliced. A result
  that cannot shrink below the cap fails with `bounds_exceeded`.

## Read tools

Six tools are always registered, with `readOnlyHint: true` and
`openWorldHint: false`. Reads use `EXAMINE` and `BODY.PEEK`, so they never
set `\Seen`.

| Tool | Arguments |
| --- | --- |
| `list_folders` | none |
| `search_mail` | `mailbox` (required), `since`, `before` (RFC 3339), `sender`, `subject` (≤ 1024 bytes), `unreadOnly` (default false), `limit` (1–250, default 50), `cursor` |
| `get_message` | `messageId` (required) |
| `get_thread` | `messageId` (required), `maxMessages` (1–20, default 10) |
| `list_attachments` | `messageId` (required) |
| `select_digest_candidates` | `mailbox` (required), `sinceHours` (1–168, default 24), `limit` (1–50, default 20), `unreadOnly` (default true; false includes read mail) |

Mailbox arguments are 1 to 512 bytes. `limit` is further clamped by the
configured `bounds.maxSearchResults`.

### Message identity

The `messageId` argument, every returned `id` and each thread node `key` are
opaque, HMAC-bound to the mailbox, UIDVALIDITY and UID, not to the account.
Every use re-checks the current UIDVALIDITY; a stale or forged ID fails with
`stale_id`. If the helper switches accounts, an old ID can reach a different
message with the same tuple, so run a fresh read after any account change.

Search results, digest candidates, `get_thread` nodes and the parent of
`get_message` and `list_attachments` also carry the exact `mailbox` and
positive 32-bit `uidvalidity` and `uid` from the same read, or the read fails.
These fields are the input to the triage writes; they grant no write
authority. Output shrinking removes whole items, so retained identity is exact;
`search_mail` keeps at least one result and halves its `subject` instead,
marking it `metadataTruncated`.

### `list_folders`

`{"folders": [{"name", "delimiter"}], "truncated"}`, at most
`bounds.maxFolderResults` (default 200, ceiling 1000) entries.

### `search_mail`

`{"results": [...], "truncated", "nextCursor"}`. Each result carries `id`, `mailbox`,
`uidvalidity`, `uid`, `subject`, `size`, `from`, `date`, `dateSource`,
`readState` and, when `from` or `subject` was cut, `metadataTruncated`. One
metadata FETCH of `BODY.PEEK[HEADER]` and `FLAGS` supplies them; no body is
read.

- `from` is the decoded `From` header, at most 512 bytes; omitted when
  missing, repeated or invalid. It is sender-controlled and proves nothing.
- `date` is the header `Date` in RFC 3339 UTC, with `dateSource: "header"`;
  otherwise it is omitted and `dateSource` is `unknown`. It is not arrival time.
- `readState` is `read`, `unread`, or `unknown` when no `FLAGS` was seen.

Results are in strictly descending UID order, not date order. A search walks
at most 8 windows of 100 UIDs, newest first. A raw SEARCH response is capped
at 4096 bytes; an oversize response or a zero, out-of-window or repeated UID
fails the whole call. `truncated` is set when matches were dropped or an older
UID range was not examined.

`nextCursor` is present exactly when `truncated` is. Pass it back as `cursor`
with the same `mailbox`, `since`, `before`, `sender`, `subject` and
`unreadOnly` to get the next older page; `limit` may change between pages.
Each match appears on exactly one page. The cursor is opaque and HMAC-bound
to the mailbox, UIDVALIDITY, the filters and the page's exclusive upper UID.
A cursor reused with another mailbox or other filters, or a forged one, fails
with `invalid_argument` before any IMAP command; a UIDVALIDITY change fails
with `stale_id`. When output shrinking drops results, `nextCursor` resumes
just below the last result kept.

### `get_message`

Reads `UID BODYSTRUCTURE` within a 128 KiB response budget, then
`BODY.PEEK[HEADER]` within `bounds.maxHeaderBytes` and one body section: the
first `text/plain` leaf, else the first `text/html` leaf, that is not an
attachment. The section is fetched as `BODY.PEEK[<section>]<0.N>` with `N` set
to `bounds.maxBodyBytes` (default 256 KiB, ceiling 1 MiB); the whole message
and attachment bytes are never fetched. It returns `id`, `mailbox`,
`uidvalidity`, `uid`, `size`, `headers`, `text`, `textSource` (`plain`, `html`
or `none`), attachment metadata from the structure as in `list_attachments`,
and a `truncation` object. A section longer than `N`, or text longer than `N`
once decoded, is cut and sets `text`. Header and structure reads keep 100
header fields, `bounds.maxMimeParts` parts at depth 10,
`bounds.maxAttachmentCount` attachment entries and 50 references.

`truncation` is always present and holds only `true` keys among `headers`,
`headerCount`, `text`, `mimeParts`, `mimeDepth`, `attachments`, `references`
and `output`. `truncated` is true whenever any key is.

### `list_attachments`

Reads only `UID BODYSTRUCTURE`, never the body, within a 128 KiB response
budget, and returns `id`, `mailbox`, `uidvalidity`, `uid`, `attachments` and
`truncation` (keys `mimeParts`, `mimeDepth`, `attachments`, `output`). Each
entry has `filename`, `contentType`, `disposition` and `hasDeclaredSize`;
`declaredSize` is present only when the sender declared a valid size, and is
that declaration, not a byte count. No bytes, part identifiers, headers or
read state are returned. An empty list with an empty `truncation` means the
structure declared no attachments; with any reason it is incomplete.

### `get_thread`

Returns `id`, `mailbox`, `nodes`, `truncated` and optional
`unavailableMembers`. The target and its same-subject candidates (base subject
with repeated `Re:`, `Fw:` and `Fwd:` stripped) in the target mailbox are read
with header-only `BODY.PEEK[HEADER]`; bodies are never fetched. Reply links
come from `References` and `In-Reply-To`; an unlinked same-subject sibling is
a root at depth 0.

Each node carries `key`, `mailbox`, `uidvalidity`, `uid`, `messageId` (the RFC
`Message-ID`; pass `key` to read tools), `parentKey`, `depth`, `subject`,
`from`, `date` and a `truncation` object (`headers`, `headerCount`,
`references`). `maxMessages` bounds the nodes and the reads: one target read
plus at most `maxMessages - 1` sibling attempts.

A sibling that is stale, lacks a positive tuple or has a malformed RFC 2047
header is omitted and reported only as `{"reason": "stale_id" |
"invalid_identity" | "malformed_header"}` in `unavailableMembers`. `truncated`
is set when a node hit a header limit, a sibling was omitted, or eligible
siblings were not attempted. A target that is stale fails with `stale_id`;
one with a malformed encoded word or no positive tuple fails with
`unavailable`.

### `select_digest_candidates`

Composes one `STATUS` and one bounded search since `sinceHours` ago, with no
body fetch. Returns `mailbox`, `candidates` (same shape and order as
`search_mail` results), `totalMessages`, `unseenCount` and `truncated`. The
two counters are mailbox STATUS values, not the number of matches examined.

## Triage writes (opt-in)

With `"mutations": {"enabled": true}` in the configuration, five more tools
are registered, with `readOnlyHint: false` and `openWorldHint: false`. Absent
or false, the default, they do not exist and Croton sends no write. Croton
never sends, deletes, expunges, copies or changes labels.

| Tool | Write | Hints |
| --- | --- | --- |
| `mark_read` | `UID STORE <uid> +FLAGS.SILENT (\Seen)` | idempotent, not destructive |
| `mark_unread` | `UID STORE <uid> -FLAGS.SILENT (\Seen)` | idempotent, not destructive |
| `move_mail` | `UID MOVE <uid> <destination>` | destructive |
| `archive_mail` | `UID MOVE` to the `\Archive` mailbox | destructive |
| `trash_mail` | `UID MOVE` to the `\Trash` mailbox; nothing is deleted | destructive |

Annotations describe the tools; they neither prompt for nor prove approval.
Croton cannot verify that a human approved a call. The client owns approval,
and each tool description asks the model to call only for an exact payload the
user approved.

### Arguments

All five take exactly `mailbox`, `uidvalidity` (positive 32-bit) and `uids`
(1 to 50 distinct positive 32-bit UIDs); `move_mail` also takes
`destination`. There is no approval, ordinal or opaque-ID argument. A missing,
null, zero, out-of-range, duplicate or unknown value fails the request with
`invalid_argument` before any IMAP command. So does a destination that is
empty, over 512 bytes, not UTF-8, or contains a control character, `*` or `%`.

### Behaviour

1. The source mailbox is selected read-write on the one session and its fresh
   UIDVALIDITY compared with `uidvalidity`. A mismatch fails with `stale_id`
   and nothing is written.
2. UIDs are handled one at a time in input order. `UID SEARCH` confirms the
   UID is present before its one write.
3. `move_mail` refuses every UID as `unsupported`, before any IMAP command,
   when `destination` equals the source; every move tool refuses every UID,
   before any LIST or write, when the server lacks `MOVE`. No COPY, STORE or
   EXPUNGE fallback exists.
4. Before each UID, move tools resolve the destination from a fresh
   `LIST "" "*"` on that session. `move_mail` needs exactly one listed match
   (INBOX case-insensitively, others exactly) that is selectable (not
   `\Noselect`, `\NonExistent`, `\All` or `\Flagged`) and is INBOX, under
   `Folders/`, or a hierarchy root carrying `\Sent`, `\Drafts`, `\Junk`,
   `\Archive` or `\Trash`. `Labels/` and everything else are refused.
   `archive_mail` and `trash_mail` map the one mailbox carrying `\Archive` or
   `\Trash`, which must pass the same rules and is never `All Mail` or
   `Starred`; none or several holders, or a holder equal to the source, is
   `unsupported` for that UID and every later one.

### Result

`{"results": [{"uid", "outcome", "code"}]}`, one row per UID in input order:

- `applied`: the STORE or MOVE returned OK.
- `refused`: the UID is gone (`not_found`) or the server answered NO or BAD
  (`unavailable`); later UIDs continue. An unsupported destination or missing
  `MOVE` is `unsupported` and refuses that UID and every later one.
- `unknown`: the connection dropped or the command deadline expired after
  dispatch. The session is discarded, nothing is retried, and every later UID
  is `not_attempted`. Run a fresh read before any new call.
- `not_attempted`: never dispatched. A failing presence check or move `LIST`
  marks that UID and every later one `not_attempted`; earlier writes stay
  applied. A move call with nothing applied yet fails whole with that code.

Each IMAP step runs under `imap.commandTimeoutMs`, so a batch is bounded.

## Errors

Every failure is `{"error": {"code": ...}}` with `isError: true` and one code
from `invalid_argument`, `not_found`, `stale_id`, `bounds_exceeded`,
`timed_out`, `canceled`, `unavailable`, `unsupported` or `internal`. Unknown
errors and handler panics collapse to `internal`; no error text, credential,
endpoint or mailbox data reaches the client.

## Audit

With `"audit": {"enabled": true}`, each tool call writes one stderr line with
only `event` (`tool_call`), `tool`, `outcome` (`ok` or `error`), `code` and
`truncated`. Arguments, mailbox names, IDs, subjects, addresses and error text
never appear. A triage call that returns per-UID rows logs `ok`; its UIDs and
row outcomes are not logged.

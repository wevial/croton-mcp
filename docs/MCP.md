# Croton MCP server

`croton-mcp` exposes the read-only bridge layer as an MCP server over stdio.
Stdout carries protocol frames only; all diagnostics go to stderr. There is no
network listener, and no MCP resources, prompts, Roots, Sampling, or Logging —
a fail-closed method allowlist rejects everything except initialization,
discovery, ping, and the tool methods.

This page covers the Mail server only. The separate Drive executable,
`croton-drive-mcp`, has its own contract in [docs/DRIVE-MCP.md](DRIVE-MCP.md);
its bounds, error vocabulary, and always-on audit stream differ from those
described here, and every claim there names the test that pins it.

The [proposed Mail mutation boundary](design/0002-mail-mutation.md) records
possible flag, mark-read and move semantics for maintainer review. Its approval
model remains Reserved; it authorizes no writes. Mail remains read-only.

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

Exactly six read-only tools are registered, each with `readOnlyHint: true`,
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
`canceled`, `unavailable`, `internal`. Unknown or wrapped errors collapse to
`internal`; no credentials, endpoints, mailbox data, or stack details can
reach the protocol stream.

## Audit

With `audit.enabled`, one JSON line per tool call is written to stderr using
only the allowlisted vocabulary `event`, `tool`, `outcome`, `code`,
`truncated`. Tool names, outcomes, and codes are re-validated against fixed
sets before logging; caller inputs, folder names, identifiers, subjects,
addresses, and error text never appear.

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

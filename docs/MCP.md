# Croton MCP server

`croton-mcp` exposes the read-only bridge layer as an MCP server over stdio.
Stdout carries protocol frames only; all diagnostics go to stderr. There is no
network listener, and no MCP resources, prompts, Roots, Sampling, or Logging —
a fail-closed method allowlist rejects everything except initialization,
discovery, ping, and the tool methods.

## Protocol

The server targets MCP `2026-07-28` and retains legacy compatibility through
the official Go SDK (`modelcontextprotocol/go-sdk`).

## Running

```sh
croton-mcp --config /absolute/path/to/croton.json
```

The configuration file is bounded JSON (64 KiB maximum, top-level object required,
unknown, null, duplicate, and case-folded-alias fields rejected) mirroring
`bridge.Config`. Secure configuration loading is currently Linux-only, using
descriptor-relative no-follow traversal for every path component. Other
platforms compile but fail closed until they have an equivalent secure opener.
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

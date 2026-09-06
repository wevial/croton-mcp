# Croton Drive MCP server

`croton-drive-mcp` exposes an operator-installed Proton Drive CLI as a
read-only MCP server over stdio. It is a separate executable, process, and
configuration from the Mail server described in [MCP.md](MCP.md); nothing in
the Mail contract carries over unless this page says so. Stdout carries
protocol frames only; all diagnostics, including the audit stream, go to
stderr. There is no network listener and no MCP resources, prompts, Roots,
Sampling, or Logging.

Every claim below names the synthetic test that pins it. The tests run against
the fake Drive CLI in `internal/testkit/fakedrive` and the fixtures in
`internal/drivecli/testdata`; no live Proton account, credential, or Drive
content is involved. Claims the tree has no test for are listed in
[Not yet witnessed](#not-yet-witnessed) with the enforcing source line.
`scripts/verify_docs_drive_mcp.py` cross-checks the tool names, the error
codes, every cited test name, and every enforcing line against the tree.

## Running

```sh
croton-drive-mcp --config /absolute/path/to/croton-drive.json
```

The configuration file goes through the same secure loader as the Mail
executable (absolute, symlink-free, owner-only regular file; bounded JSON with
unknown, null, duplicate, and case-folded-alias fields rejected). Its strict
schema has three keys:

```json
{
  "cli": {"binaryPath": "/opt/proton-drive/proton-drive"},
  "allowedDownloadDirectories": [],
  "writes": {"enabled": false}
}
```

`cli.binaryPath` must be absolute. `allowedDownloadDirectories` and
`writes.enabled` are reserved: the server registers no download or write
tools, and `writes.enabled` is `false` by Go's zero value. The file carries no
credentials and the server never reads any; authentication is the CLI's own
concern, and a CLI that reports it needs authentication surfaces as
`unavailable`.

The server targets MCP `2026-07-28` and accepts the legacy `2025-11-25`
initialization flow. A fail-closed method allowlist admits only `initialize`,
`ping`, `server/discover`, `tools/list`, `tools/call`, and the
`notifications/initialized`, `notifications/cancelled`, and
`notifications/progress` notifications; every other method is answered with
JSON-RPC "method not found". Every data command is gated behind one exact
version handshake with the CLI; until it succeeds, tools fail closed.

Witnesses: `TestStdioInitializesAnIndependentDriveServerWithThreeReadOnlyTools`
(stdio startup, protocol version, catalog, clean stderr) and
`TestNewSupportsLegacyInitialize` (legacy negotiation).

## Tools

Exactly three read-only tools are registered, each with `readOnlyHint: true`,
`openWorldHint: false`, and a closed (`additionalProperties: false`) input
schema. The `path` schema publishes both the character `maxLength` and the
authoritative `x-maxBytes` annotation, each set to the 1024-byte path bound;
the `type` schema is a plain string `enum` and `limit` an integer range. The
server enforces every bound itself and never trusts schema enforcement by the
caller, and it is the enforcement that the tests witness:
`TestNewNegotiatesCurrentProtocolWithTheReadOnlyDriveCatalog` pins the count,
the names and `readOnlyHint`; `TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`
pins the closed object (an unknown `surprise` field is rejected), the `type`
enum (`device` is rejected) and the `limit` range (`0` and `-1` are rejected);
`TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths` pins the 1024-byte
`path` bound with the longest accepted and the shortest rejected path. The
advertised schema text itself (the `additionalProperties`, `maxLength`,
`x-maxBytes` and `openWorldHint` keys in the `tools/list` reply) is asserted
by no Go test; see [Not yet witnessed](#not-yet-witnessed) for the enforcing
lines.

| Tool | Purpose | Witness |
| ---- | ------- | ------- |
| `list_drive_entries` | List one absolute Drive path: root sections at `/`, synced devices at `/devices`, or bounded folder entries elsewhere. | `TestListDriveEntriesSupportsRootSectionsDevicesAndTypeFilter` |
| `get_drive_metadata` | The frozen file or folder metadata object for one absolute Drive path. | `TestGetDriveMetadataReturnsTheFrozenNodeObject` |
| `get_drive_sharing_status` | Whether one absolute Drive path is shared, with bounded invitation and member lists and its public link, never its password. | `TestGetDriveSharingStatusReportsSharedUnsharedAndCommandErrors` |

`TestNewNegotiatesCurrentProtocolWithTheReadOnlyDriveCatalog` pins the
catalog: three tools, these names, every one marked read-only.
`TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation` drives all
three over the real stdio executable after a successful handshake and pins the
exact CLI argument vectors they produce.

## Arguments

Raw argument objects are capped at 24 KiB and decoded strictly: non-objects,
nulls, unknown fields, duplicate or case-folded-alias fields, excessive
nesting, and trailing JSON values are all rejected with `invalid_argument`
before the CLI is consulted.
`TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI` is the Drive
witness: it sends a missing `path`, an empty object, an unknown field, a bad
`type`, out-of-range `limit` values and non-canonical paths to all three tools,
requires `invalid_argument` for each, and proves the CLI was never executed.
The 24 KiB byte cap and the null, duplicate-key, alias, nesting and
trailing-value rules are enforced by the shared decoder in
`internal/strictjson` (`DecodeObject`, called with `maxToolArgumentsBytes`
from `internal/drivemcp/tools.go`). The decoder's own synthetic tests in
`internal/strictjson` pin excessive nesting, exact duplicate keys,
case-folded aliases and unknown fields. No Drive-level Go test sends an
oversize argument object; the cap is listed in
[Not yet witnessed](#not-yet-witnessed).

- `path` (every tool, required): at most 1024 bytes, valid UTF-8, no control
  characters, and canonical absolute form only. `/` is accepted; otherwise the
  path is `/`-joined non-empty segments with no `.` or `..` segment and no
  trailing separator. An accepted path is never flag-shaped and reaches the
  CLI as exactly one argument. Witness:
  `TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths`, a table over the
  root, nested, spaced and Unicode paths that pass and the relative, `.`/`..`,
  double-slash, trailing-slash, flag-shaped, control-character, invalid-UTF-8
  and overlong paths that fail.
- `type` (`list_drive_entries`, optional): `file` or `folder`. An omitted
  `type` and an explicit empty string `""` both mean no filter and reach the
  CLI without a `--type` flag; the handler and the adapter accept `""` even
  though the published `enum` lists only the two names. Any other value is
  rejected (`TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`
  sends `device`). `TestListDriveEntriesSupportsRootSectionsDevicesAndTypeFilter`
  pins the omitted-`type` argv (no `--type`) and the `file` argv (`--type
  file`); no test sends the explicit empty string.
- `limit` (`list_drive_entries`, optional): an integer between 1 and 200,
  default 100. Zero and negative values are rejected
  (`TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`); an accepted
  limit bounds the entries and sets `truncated` when it cuts them
  (`TestListDriveEntriesEnforcesEntryLimitAndSignalsTruncation`). Values
  above 200 are clamped to 200 by `clampLimit` in `internal/drivemcp/tools.go`;
  no test sends an over-limit value.

Witnesses: `TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`
(relative paths, traversal, empty segments, trailing separators, control
characters, bad `type`, non-positive `limit`, unknown fields, and missing
`path` all return `invalid_argument` and never spawn the CLI) and
`TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths` (the path grammar).

## Output

Each result is one JSON text content item. Serialized results are capped at
100 000 bytes. Oversize results are shrunk structurally, halving the largest
list and re-marshaling until the result fits, so output is always
syntactically valid JSON; serialized bytes are never sliced. A result that
cannot shrink further is replaced by `{"truncated":true}`.

`truncated` is set whenever anything was dropped, by either mechanism:

- byte shrinking under the 100 000 byte cap;
- the `list_drive_entries` count cap: `limit` entries (default 100, at most
  200) from the sections, devices, or entries list;
- the `get_drive_sharing_status` count cap: at most 100 members in each of
  `protonInvitations`, `nonProtonInvitations`, and `members`.

Result shapes are frozen. `list_drive_entries` returns `path` plus exactly one
of `sections`, `devices`, or `entries`, present even when empty.
`get_drive_metadata` returns the CLI's node object unchanged.
`get_drive_sharing_status` returns `shared`, the three member lists (always
present, empty when unshared), `urlAccess` when a public link exists,
`editorsCanShare`, and `truncated`. When a sharing result must shrink, the
lists are halved first and the public link is dropped last, so `shared` stays
truthful in every shrunken output.

Witnesses: `TestEncodeBoundedShrinksOversizeListResultsIntoValidJSON`
(oversize lists shrink into valid JSON under the byte cap; the unshrinkable
fallback), `TestListDriveEntriesEnforcesEntryLimitAndSignalsTruncation` (the
`limit` cap sets `truncated`),
`TestListDriveEntriesReturnsFrozenNodeShapesAfterNegotiation` (frozen list
shape), `TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree` (the
per-list sharing cap), and
`TestEncodeBoundedPreservesSharingStateWhenURLAccessOverflows` (public link
dropped last).

## Errors

An error result carries `isError: true` and one JSON text item of the form
`{"error":{"code":"..."}}`. The code vocabulary is exactly:

- `invalid_argument`: the argument object or a field failed validation.
- `bounds_exceeded`: the CLI's output overflowed the adapter's output budget.
- `timed_out`: the CLI or the request deadline expired.
- `canceled`: the request context was canceled.
- `unavailable`: the CLI is not configured, its version handshake failed, its
  output was malformed or truncated, the command exited nonzero, or it reported
  that authentication is required.
- `internal`: a panic in the handler or any adapter error the server does not
  recognize.

Every adapter failure maps to one of these codes. The mapping unwraps errors,
so a recognized adapter code or context error keeps its code through wrapping;
only errors the server does not recognize collapse to `internal`. Either way
no CLI stderr, path, node name, or stack detail can cross the protocol
boundary. This vocabulary is a subset of the Mail server's:
Drive has no not-found or stale-id code, because it resolves paths on every
call and issues no identifiers of its own.

Witnesses: `TestMapDriveErrorCoversEveryAdapterCode` (every adapter code,
context cancellation, deadline expiry, and an unknown wrapped error),
`TestDriveToolsMapAdapterFailuresToStableCodes` (over a live session),
`TestDriveToolsFailClosedWhenNegotiationFails` and
`TestDriveToolsFailClosedWithoutConfiguredCLI` (`unavailable`), and
`TestStdioDriveToolsFailClosedWhenNegotiationFails` (over stdio).

## Audit

The audit stream is always on. Unlike the Mail server, there is no
`audit.enabled` key in the Drive configuration: the executable attaches an
auditor to stderr unconditionally, and one JSON line is written per tool call.
The vocabulary is exactly `event` (always `tool_call`), `tool`, `outcome`
(`ok` or `error`), `code` (present only on `error`), and `truncated` (present
only when `true`):

```json
{"event":"tool_call","tool":"list_drive_entries","outcome":"ok"}
{"event":"tool_call","tool":"list_drive_entries","outcome":"error","code":"invalid_argument"}
{"event":"tool_call","tool":"get_drive_sharing_status","outcome":"ok","truncated":true}
```

The tool name, outcome, and code are each re-validated against fixed sets
before logging, so an unexpected upstream value becomes `unknown_tool`,
`error`, or `internal` rather than reaching the log. Caller arguments, Drive
paths, node names, addresses, CLI output, and error text never appear. A clean
protocol startup writes nothing to stderr.

Witnesses: `TestDriveAuditRecordsOnlyToolNameAndOutcome` (the exact `ok` and
`error` lines; no path, name, or address leaks),
`TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree` (the
`truncated` line; no member data leaks), and
`TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation` (the line
reaches the real process's stderr).

## Sharing

`get_drive_sharing_status` reports shares without changing them. The CLI's
sharing object carries the public link's `customPassword`; the tool never
returns it. The server-side `urlAccess` struct has no password field at all,
so `customPassword` is unreachable by construction rather than by filtering:
it never appears in a result, in an audit line, or on the server's stderr.
`urlAccess` carries only `uid`, `creationTime`, `role`, `url`,
`expirationTime`, and `numberOfInitializedDownloads`.

The CLI's own stderr is likewise never forwarded. A nonzero exit produces the
bare `unavailable` code, so an account address, a password, or a failure
message printed by the CLI cannot reach a result.

Witnesses: `TestGetDriveSharingStatusReportsSharedUnsharedAndCommandErrors`
(the `sharing-status.json` fixture's `fixture-password` and the
`customPassword` key are absent from the shared result; the unshared result
keeps every list present and empty; a failing CLI yields `unavailable` with
none of its stderr) and
`TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation` (the password is
absent from the stdio result and from the process's stderr).

## Not yet witnessed

These claims are enforced by the named line in `internal/drivemcp` but no
tracked Drive test asserts them. Each row is retired by the code ticket that
adds its synthetic test. `scripts/verify_docs_drive_mcp.py` requires every
row's `path:line` to resolve to a non-blank line in the tree.

| Claim | Enforcing line |
| ----- | -------------- |
| Every input schema is closed: `objectSchema()` emits `additionalProperties: false`. | `internal/drivemcp/tools.go:412` |
| String schemas publish `maxLength` and `x-maxBytes` from the same byte bound. | `internal/drivemcp/tools.go:431` |
| Every registered tool carries `readOnlyHint: true` and `openWorldHint: false` in the `tools/list` reply. | `internal/drivemcp/tools.go:105` |
| Raw argument objects are capped at 24 KiB (`maxToolArgumentsBytes = 24 * 1024`). | `internal/drivemcp/tools.go:34` |
| `decodeArguments` passes that cap to `strictjson.DecodeObject`, so an oversize object is `invalid_argument` before the CLI is consulted. | `internal/drivemcp/tools.go:198` |
| An explicit empty-string `type` means no filter and is not rejected. | `internal/drivemcp/tools.go:250` |
| A `limit` above 200 is clamped to 200 by `clampLimit`. | `internal/drivemcp/tools.go:395` |

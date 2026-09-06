# Croton Drive MCP server

`croton-drive-mcp` exposes an operator-installed Proton Drive CLI as a
read-only MCP server over stdio. It is a separate executable, process, and
configuration from the Mail server described in [MCP.md](MCP.md); nothing in
the Mail contract carries over unless this page says so. Stdout carries
protocol frames only; all diagnostics, including the audit stream, go to
stderr.

Every claim below names the synthetic test that pins it. The tests run against
the fake Drive CLI in `internal/testkit/fakedrive` and the fixtures in
`internal/drivecli/testdata`; no live Proton account, credential, or Drive
content is involved. The five claims the tree has no test for are listed in
[Not yet witnessed](#not-yet-witnessed) with their enforcing source lines; the
page states nothing beyond the cited tests and that table.
`scripts/verify_docs_drive_mcp.py` cross-checks the tool names, the error
codes, every cited test name, and every enforcing line against the tree.

## Running

```sh
croton-drive-mcp --config /absolute/path/to/croton-drive.json
```

The configuration file goes through the same secure loader as the Mail
executable (see [MCP.md](MCP.md)). Its strict schema has three keys:

```json
{
  "cli": {"binaryPath": "/opt/proton-drive/proton-drive"},
  "allowedDownloadDirectories": [],
  "writes": {"enabled": false}
}
```

`allowedDownloadDirectories` and `writes.enabled` are reserved: the server
registers no download or write tools. The file carries no credentials and the
server never reads any; authentication is the CLI's own concern, and a CLI
that reports it needs authentication surfaces as `unavailable`.

The server targets MCP `2026-07-28` and accepts the legacy `2025-11-25`
initialization flow. A fail-closed method allowlist admits only `initialize`,
`ping`, `server/discover`, `tools/list`, `tools/call`, and the
`notifications/initialized`, `notifications/cancelled`, and
`notifications/progress` notifications; every other method is answered with
JSON-RPC "method not found". Every data command is gated behind one exact
version handshake with the CLI; until it succeeds, tools fail closed with
`unavailable` and the CLI sees nothing but the `version` command. No Go test
sends a method outside the allowlist; the rejection is listed in
[Not yet witnessed](#not-yet-witnessed).

Witnesses: `TestStdioInitializesAnIndependentDriveServerWithThreeReadOnlyTools`
(stdio startup, protocol version, catalog, clean stderr),
`TestNewSupportsLegacyInitialize` (legacy negotiation), and
`TestStdioDriveToolsFailClosedWhenNegotiationFails` (fail-closed handshake
over stdio).

## Tools

Exactly three read-only tools are registered, each with `readOnlyHint: true`.
Each input schema is closed (`additionalProperties: false`); the `path` schema
publishes both the character `maxLength` and the `x-maxBytes` annotation, each
set to the 1024-byte path bound. The server enforces every bound itself and
never trusts schema enforcement by the caller, and it is the enforcement that
the tests witness: `TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`
pins the closed object (an unknown `surprise` field is rejected), the `type`
enum (`device` is rejected) and the `limit` floor (`0` and `-1` are rejected);
`TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths` pins the 1024-byte
`path` bound with the longest accepted and the shortest rejected path. The
schema text itself is asserted by `TestDriveToolSchemasAreClosedAndBounded`:
every input schema is closed, every free-text string carries equal
`maxLength` and `x-maxBytes`, and the `type` enum publishes exactly `file`
and `folder`.

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

Raw argument objects are capped at 24 KiB and decoded strictly: a missing
`path`, an empty object, an unknown field, a bad `type`, a zero or negative
`limit` or a non-canonical path is rejected with `invalid_argument` before the
CLI is consulted. `TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`
is the witness: it sends that table to `list_drive_entries`, covers the
metadata and sharing tools for malformed and missing paths, requires
`invalid_argument` for each, and proves the CLI was never executed.
`TestDriveToolsRejectOversizeRawArgumentsWithoutExecutingTheCLI` sends an
object one byte over the 24 KiB cap and proves the same.

- `path` (every tool, required): at most 1024 bytes, valid UTF-8, no control
  characters, and canonical absolute form only. `/` is accepted; otherwise the
  path is `/`-joined non-empty segments with no `.` or `..` segment and no
  trailing separator. An accepted path is never flag-shaped and reaches the
  CLI as exactly one argument. Witness:
  `TestValidDrivePathAcceptsOnlyCanonicalAbsolutePaths`, a table over the
  root, nested, spaced and Unicode paths that pass and the relative, `.`/`..`,
  double-slash, trailing-slash, flag-shaped, control-character, invalid-UTF-8
  and overlong paths that fail.
- `type` (`list_drive_entries`, optional): `file` or `folder`. The published
  schema enumerates only those two values, so a client that validates
  arguments against the advertised schema omits `type` to mean no filter. The
  server is more lenient than its schema: it also accepts an explicit `""`,
  which it treats exactly like an omitted `type` and forwards without a
  `--type` flag (`TestListDriveEntriesTreatsEmptyTypeAsNoFilter`;
  `TestListDriveEntriesSupportsRootSectionsDevicesAndTypeFilter` pins the
  no-filter and `--type file` argv). That leniency is not part of the
  advertised contract. Any other value such as `device` is rejected
  (`TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`).
- `limit` (`list_drive_entries`, optional): a positive integer; the schema
  publishes `minimum` 1 and `maximum` 200. Zero and negative values are
  rejected (`TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI`); an
  accepted limit bounds the entries and sets `truncated` when it cuts them
  (`TestListDriveEntriesEnforcesEntryLimitAndSignalsTruncation`, which sends
  explicit limits of 2 and 3). A value above 200 is not rejected: the server
  clamps it to 200. No test omits `limit` or sends one above 200, so the
  default of 100 and the clamp are listed together in
  [Not yet witnessed](#not-yet-witnessed).

## Output

Each result is one JSON text content item. Serialized results are capped at
100 000 bytes. Oversize results are shrunk structurally, halving the largest
list and re-marshaling until the result fits, so output is always
syntactically valid JSON; serialized bytes are never sliced. A result that
cannot shrink further is replaced by `{"truncated":true}`.

`truncated` is set whenever anything was dropped, by either mechanism:

- byte shrinking under the 100 000 byte cap;
- the `list_drive_entries` count cap: `limit` entries (default 100, clamped
  to at most 200) from the sections, devices, or entries list;
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
shape), `TestGetDriveMetadataReturnsTheFrozenNodeObject` (the unchanged node
object), `TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree` (the
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
  recognize. The unknown-error path is tested; the panic path is not, and is
  listed in [Not yet witnessed](#not-yet-witnessed).

Every adapter failure maps to one of these codes; errors the server does not
recognize collapse to `internal`, so no CLI stderr, path, node name, or stack
detail can cross the protocol boundary. The mapping test passes every
recognized error bare and wraps only the unknown one, so "a recognized code
survives wrapping" is listed in [Not yet witnessed](#not-yet-witnessed). This
vocabulary is a subset of the Mail server's: Drive has no not-found or
stale-id code.

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
protocol startup writes nothing to stderr. The cited tests feed the auditor
only expected values; the sanitizers' handling of unexpected ones is listed in
[Not yet witnessed](#not-yet-witnessed).

Witnesses: `TestDriveAuditRecordsOnlyToolNameAndOutcome` (the exact `ok` and
`error` lines; no path, name, or address leaks),
`TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree` (the
`truncated` line; no member data leaks),
`TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation` (the line
reaches the real process's stderr), and
`TestStdioInitializesAnIndependentDriveServerWithThreeReadOnlyTools` (clean
startup writes nothing).

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

These five claims are enforced by the named lines in `internal/drivemcp` but
no tracked Drive test asserts them. Each row is retired by the code ticket
that adds its synthetic test; the closed-schema and argument-cap rows were
retired by `TestDriveToolSchemasAreClosedAndBounded` and
`TestDriveToolsRejectOversizeRawArgumentsWithoutExecutingTheCLI`.
`scripts/verify_docs_drive_mcp.py` requires exactly these five rows and
requires every `path:line` to resolve to a non-blank line in the tree.

| Claim | Enforcing line |
| ----- | -------------- |
| An omitted `limit` defaults to 100 and a `limit` above 200 is clamped to 200, not rejected (`clampLimit` with `defaultListEntries` as fallback and `maxListEntries` as ceiling). | `internal/drivemcp/tools.go:253`, `internal/drivemcp/tools.go:395` |
| A wrapped recognized error keeps its code: `mapDriveError` tests context errors with `errors.Is` and adapter codes with `drivecli.CodeOf`, which unwraps with `errors.As`. | `internal/drivemcp/tools.go:205`, `internal/drivemcp/tools.go:212` |
| The audit sanitizers log an unexpected `tool` as `unknown_tool`, an `outcome` other than `ok` as `error`, and a `code` outside the six-code vocabulary as `internal`. | `internal/drivemcp/audit.go:81`, `internal/drivemcp/audit.go:90`, `internal/drivemcp/audit.go:98` |
| A method outside the allowlist is answered with JSON-RPC "method not found" by `allowlistMiddleware`. | `internal/drivemcp/tools.go:128` |
| A panicking handler is recovered by `runTool` and reported as `internal` with no stack detail on the protocol stream. | `internal/drivemcp/tools.go:170` |

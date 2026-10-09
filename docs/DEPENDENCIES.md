# Dependency rationale

Reviewed: 2026-08-05; Model Context Protocol SDK section reviewed 2026-10-08.

## Model Context Protocol SDK

Croton uses the official [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) at `v1.8.0` (2026-09-14). That release implements MCP `2026-07-28`, which is still the newest revision it negotiates, while preserving compatibility with `2025-11-25` and earlier clients. It hardens the transports against resource exhaustion and fixes session leaks, deadlocks, and teardown hangs without adding a protocol revision. Croton therefore uses one server implementation—not parallel hand-written protocol stacks—and pins tests for both the current stateless discovery path and the legacy `initialize` fallback.

For `2026-07-28`, Croton relies on the SDK's per-request protocol metadata and required `server/discover` implementation. New Croton features will not adopt roots, sampling, or protocol logging because those capabilities are deprecated in this revision. The stdio transport remains persistent at the process level, but protocol requests do not depend on hidden session state. Both executables keep Croton's own bounded stdio transports rather than the SDK's `StdioTransport.MaxLineLength`, and Croton sets no `MCPGODEBUG` options.

## `github.com/emersion/go-imap/v2`

Croton uses this package only in the `bridge` package, the narrow read-only
IMAP adapter, at the pinned `v2.0.0-beta.8` release. Due diligence on 2026-08-05 found:

- the upstream default branch is `v2`, is active (last push reported 2026-07-02), and the repository is not archived;
- the published v2 package version is `v2.0.0-beta.8` (2025-12-16), and upstream explicitly describes v2 as still in development;
- GitHub's repository security-advisories endpoint returned no published advisories at review time;
- its MIT license is compatible with Croton's Apache-2.0 license.

The pre-release status is material. The adapter keeps the dependency behind a
small read-only interface; it enforces per-command input/result limits and uses
only synthetic loopback fixtures. Future upgrades require re-running
`govulncheck`, reviewing upstream release notes/advisories, and retaining the
bounded integration coverage. No live mailbox data, credentials, or account
identifiers are permitted in tests or logs.

## Transitive vulnerability remediation

The initial `govulncheck -show verbose ./...` scan identified `GO-2026-5024` in transitive `golang.org/x/sys@v0.41.0` (a Windows-only integer overflow fixed in `v0.44.0`). The bootstrap has no reachable calls, but Croton explicitly upgrades the indirect requirement to `golang.org/x/sys@v0.48.0`, which includes that fix, so cross-platform consumers do not inherit the known vulnerable version.

## Reproducibility

Use `go mod tidy -diff`, `go mod verify`, and `govulncheck ./...` before accepting a dependency update. CI records the corresponding commands.

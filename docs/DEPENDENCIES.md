# Dependency rationale

Reviewed: 2026-08-05.

## Model Context Protocol SDK

Croton uses the official [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk) at `v1.7.0`. That release implements MCP `2026-07-28` while preserving compatibility with `2025-11-25` and earlier clients. Croton therefore uses one server implementation—not parallel hand-written protocol stacks—and pins tests for both the current stateless discovery path and the legacy `initialize` fallback.

For `2026-07-28`, Croton relies on the SDK's per-request protocol metadata and required `server/discover` implementation. New Croton features will not adopt roots, sampling, or protocol logging because those capabilities are deprecated in this revision. The stdio transport remains persistent at the process level, but protocol requests do not depend on hidden session state.

## `github.com/emersion/go-imap/v2`

This package is **not adopted in the bootstrap layer**. Due diligence on 2026-08-05 found:

- the upstream default branch is `v2`, is active (last push reported 2026-07-02), and the repository is not archived;
- the published v2 package version is `v2.0.0-beta.8` (2025-12-16), and upstream explicitly describes v2 as still in development;
- GitHub's repository security-advisories endpoint returned no published advisories at review time;
- its MIT license is compatible with Croton's Apache-2.0 license.

The pre-release status is material. A later mail-transport work package may adopt a pinned v2 release only after re-running `govulncheck`, reviewing upstream release notes/advisories, and adding integration tests with redacted fixture data. No live mailbox data, credentials, or account identifiers are permitted in tests or logs.

## Reproducibility

Use `go mod tidy -diff`, `go mod verify`, and `govulncheck ./...` before accepting a dependency update. CI records the corresponding commands.

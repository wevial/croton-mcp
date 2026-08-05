# Dependency rationale

Reviewed: 2026-08-05.

## Model Context Protocol SDK

Croton uses the official [`github.com/modelcontextprotocol/go-sdk`](https://github.com/modelcontextprotocol/go-sdk), pinned in `go.mod` when the first MCP implementation is added. The official repository describes this as the Go SDK for MCP servers and clients; release `v1.7.0` is the current stable release reviewed for this bootstrap. It retains backwards compatibility with the legacy `initialize` lifecycle while adding newer protocol support, which lets this project verify a conventional stdio initialize exchange without implementing a transport itself.

## `github.com/emersion/go-imap/v2`

This package is **not adopted in the bootstrap layer**. Due diligence on 2026-08-05 found:

- the upstream default branch is `v2`, is active (last push reported 2026-07-02), and the repository is not archived;
- the published v2 package version is `v2.0.0-beta.8` (2025-12-16), and upstream explicitly describes v2 as still in development;
- GitHub's repository security-advisories endpoint returned no published advisories at review time;
- its MIT license is compatible with Croton's Apache-2.0 license.

The pre-release status is material. A later mail-transport work package may adopt a pinned v2 release only after re-running `govulncheck`, reviewing upstream release notes/advisories, and adding integration tests with redacted fixture data. No live mailbox data, credentials, or account identifiers are permitted in tests or logs.

## Reproducibility

Use `go mod tidy -diff`, `go mod verify`, and `govulncheck ./...` before accepting a dependency update. CI records the corresponding commands.

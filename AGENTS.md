# Croton Agent Guide

Croton is a local, read-only MCP server for Proton Mail Bridge.

## Invariants

- Never expose credentials, account identifiers, live mailbox content, or unredacted diagnostics to agents. Use synthetic `.test` fixtures.
- Keep Bridge connections loopback-only with TLS, and never add mutating IMAP operations.
- Target MCP `2026-07-28`; retain legacy compatibility through the official Go SDK. Do not add Roots, Sampling, or MCP Logging.
- Keep stdout protocol-only; diagnostics go to stderr.

## Workflow

- Use Go 1.26.5.
- Run `go build ./...`, `go vet ./...`, and `go test -race ./...` before submitting.
- Follow Conventional Commits and `.github/PULL_REQUEST_TEMPLATE.md`.
- Do not merge without explicit maintainer approval.

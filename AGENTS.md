# Croton Agent Guide

Croton ships two local, read-only MCP servers: `croton-mcp` for Proton Mail Bridge and `croton-drive-mcp` for Proton Drive.

## Invariants

- Never expose credentials, account identifiers, live mailbox content, or unredacted diagnostics to agents. Use synthetic `.test` fixtures.
- Keep Bridge connections loopback-only with TLS, and never add mutating IMAP operations beyond the planned default-off Seen `UID STORE` and native `UID MOVE` in `docs/design/0005-mail-triage.md`; those require a separately approved implementation, and shipped Mail stays read-only until then.
- Target MCP `2026-07-28`; retain legacy compatibility through the official Go SDK. Do not add Roots, Sampling, or MCP Logging.
- Keep stdout protocol-only; diagnostics go to stderr.

## Workflow

- Use Go 1.26.9.
- Use blank lines to separate validation, setup, state transitions, I/O, and return paths inside functions.
- Run `go build ./...`, `go vet ./...`, and `go test -race ./...` before submitting.
- Follow Conventional Commits and `.github/PULL_REQUEST_TEMPLATE.md`.
- Do not merge without explicit maintainer approval.

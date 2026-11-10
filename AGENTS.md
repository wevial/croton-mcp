# Croton Agent Guide

Croton ships one local MCP server, `croton-mcp`, for Proton Mail Bridge. It is
read-only by default; the triage writes in `docs/MCP.md` exist only behind
`mutations.enabled`.

## Invariants

- Never expose credentials, account identifiers, live mailbox content, or unredacted diagnostics to agents. Use synthetic `.test` fixtures.
- Keep Bridge connections loopback-only with TLS pinned to an explicit anchor or SPKI hash.
- Writes stay off by default and limited to Seen-only `UID STORE` and native `UID MOVE`. Never add send, delete, EXPUNGE, COPY or label changes.
- Never return attachment bytes. Keep results metadata first, with hard caps on body and output.
- Keep message IDs opaque and bound to UIDVALIDITY.
- Keep the 64 KiB frame bound, the no-symlink `--config` open, and timeouts and byte budgets on Bridge reads.
- Bridge is trusted; senders are not. Keep every limit on sender-controlled content.
- Target MCP `2026-07-28`; retain legacy compatibility through the official Go SDK. Do not add Roots, Sampling, or MCP Logging.
- Keep stdout protocol-only; diagnostics go to stderr.

## Ticket rules

- Stay inside the ticket's scope. Its acceptance criteria and verify commands are the contract.
- Simplify: delete rather than deprecate. No shims, no compatibility layers, no new abstractions.
- Tests move with the code they test. Each contract is tested once, at the lowest layer that observes it.
- Update `docs/MCP.md` or `docs/INSTALL.md` in the same change as the behaviour they describe. Keep `docs/` to INSTALL, MCP and RELEASE.

## Workflow

- Use Go 1.26.10.
- Use blank lines to separate validation, setup, state transitions, I/O, and return paths inside functions.
- Run `go build ./...`, `go vet ./...`, and `go test -race ./...` before submitting.
- Follow Conventional Commits and `.github/PULL_REQUEST_TEMPLATE.md`.
- Do not merge without explicit maintainer approval.

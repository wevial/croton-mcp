# Contributing to Croton

## Checks

Use Go 1.26.10 (`toolchain go1.26.10` in `go.mod`). CI runs these on Linux and
macOS; run them locally before opening a pull request:

```sh
export GOBIN="$PWD/.bin"
go install honnef.co/go/tools/cmd/staticcheck@v0.7.0
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0

test -z "$(gofmt -l $(git ls-files '*.go'))"
go build ./... && go vet ./... && "$GOBIN/staticcheck" ./...
go test -race ./...
go mod tidy -diff && go mod verify && "$GOBIN/govulncheck" ./...
```

`.bin/` is ignored by Git. Race tests need a C compiler.

## Packages

| Package | Role |
| --- | --- |
| `cmd/croton-mcp` | The stdio executable: loads the config, builds the adapter and server. |
| `bridge` | MCP-neutral IMAP adapter over Bridge: loopback TLS, credentials, bounded reads, the opt-in Seen and move writes. |
| `internal/config` | No-follow, owner-only configuration opener (Linux and macOS). |
| `internal/mcpserver` | Tool catalog, argument validation, output cap, error codes, audit. |
| `internal/stdioframe` | Bounded stdio transport: one strict JSON object per frame, at most 64 KiB. |
| `internal/strictjson` | Strict, bounded JSON object decoding. |
| `internal/testkit` | Synthetic loopback IMAP server for tests only. |

`bridge` imports no MCP types. `internal/testkit` is never imported by
non-test code.

## Dependencies

- `github.com/modelcontextprotocol/go-sdk` v1.8.0: MCP `2026-07-28` with
  legacy `2025-11-25` support. Croton uses its own stdio frame limiter.
- `github.com/emersion/go-imap/v2` v2.0.0-beta.8: pre-release, used only in
  `bridge` behind per-command byte budgets and timeouts.

On any dependency update, review upstream release notes and advisories and
rerun `go mod tidy -diff`, `go mod verify` and `govulncheck`. Pin GitHub
Actions to a commit SHA.

## Rules

- Use synthetic `.test` data only; never live accounts, credentials, mailbox
  content or unredacted logs.
- Write a failing test first. Test each contract once, at the lowest layer
  that observes it.
- Keep commits small and use Conventional Commits.

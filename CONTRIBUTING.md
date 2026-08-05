# Contributing to Croton MCP

## Development prerequisites

Install Go 1.26.5. The module declares `toolchain go1.26.5`; use that toolchain for all checks.

## Required checks

Run the documented checks before proposing a change:

```sh
gofmt -w $(git ls-files '*.go')
/usr/local/go/bin/go build ./...
/usr/local/go/bin/go vet ./...
staticcheck ./...
/usr/local/go/bin/go test -race ./...
/usr/local/go/bin/go mod tidy -diff
/usr/local/go/bin/go mod verify
govulncheck ./...
```

## Privacy

Use synthetic data only. Do not use live Proton accounts, account identifiers, mailbox content, credentials, or unredacted logs in development, tests, issues, commits, or CI.

## Changes

Keep commits small and coherent. Add a focused test before production behavior, observe it fail, and then implement the smallest passing change. Format Go files with `gofmt`.

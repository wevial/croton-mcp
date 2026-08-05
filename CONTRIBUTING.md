# Contributing to Croton MCP

## Development prerequisites

Install Go 1.26.5. The module declares `toolchain go1.26.5`; use that toolchain for all checks.

## Required checks

Run the documented checks before proposing a change. `go` must resolve to Go 1.26.5 on your `PATH`; do not depend on a fixed installation directory. Install the external analyzers into a project-local directory so they do not alter the module dependencies:

```sh
export GOBIN="$PWD/.bin"
mkdir -p "$GOBIN"
go install honnef.co/go/tools/cmd/staticcheck@v0.7.0
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0

gofmt -w $(git ls-files '*.go')
go build ./...
go vet ./...
"$GOBIN/staticcheck" ./...
go test -race ./...
go mod tidy -diff
go mod verify
"$GOBIN/govulncheck" ./...
```

## Privacy

Use synthetic data only. Do not use live Proton accounts, account identifiers, mailbox content, credentials, or unredacted logs in development, tests, issues, commits, or CI.

## Changes

Keep commits small and coherent. Add a focused test before production behavior, observe it fail, and then implement the smallest passing change. Format Go files with `gofmt`.

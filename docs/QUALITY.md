# Quality and security tooling

Croton's GitHub Actions workflow is the canonical verification path. It installs Go 1.26.6 and pins the two externally installed analyzers:

- `honnef.co/go/tools/cmd/staticcheck@v0.7.0` (Staticcheck 2026.1, which supports Go 1.26)
- `golang.org/x/vuln/cmd/govulncheck@v1.6.0`

To reproduce locally without adding tools to `go.mod`:

```sh
export PATH=/usr/local/go/bin:$PATH
export GOBIN="$PWD/.bin"
export GOCACHE="$PWD/.gocache"
mkdir -p "$GOBIN" "$GOCACHE"

go install honnef.co/go/tools/cmd/staticcheck@v0.7.0
go install golang.org/x/vuln/cmd/govulncheck@v1.6.0

test -z "$(gofmt -l $(git ls-files '*.go'))"
go build ./...
go vet ./...
staticcheck ./...
go test -race ./...
go mod tidy -diff
go mod verify
govulncheck ./...
```

Tool binaries and caches remain local-only and are ignored by Git. `govulncheck` consults the public Go vulnerability database; do not run it against a module graph containing private module paths or with credentials in its environment.

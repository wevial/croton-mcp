# Croton MCP for Proton Mail

Croton is a privacy-first, local stdio [Model Context Protocol](https://modelcontextprotocol.io/) server that provides controlled access to Proton Mail through Proton Mail Bridge. The repository includes a production read-only Bridge adapter and synthetic protocol fixtures; it does not bundle Proton credentials, account identifiers, mailbox content, or live fixture data.

## Status

Early read-only implementation. The executable supports MCP `2026-07-28` by default and the legacy `2025-11-25` initialization flow for older clients. Its local Bridge adapter supports bounded folder, status, search, metadata, and body reads over verified loopback TLS; it does not expose mail mutation operations.

## Requirements

- Go 1.26.5 (the module's `toolchain` directive enforces this release)

## Local development

```sh
/usr/local/go/bin/go test ./...
/usr/local/go/bin/go vet ./...
/usr/local/go/bin/go run ./cmd/croton-mcp
```

The server speaks JSON-RPC over standard input/output. Diagnostics must go to standard error; standard output is protocol-only.

## Layout

- `cmd/croton-mcp`: stdio executable
- `internal/config`: future configuration boundary
- `bridge`: narrow, bounded read-only IMAP adapter boundary
- `internal/mcpserver`: MCP server construction
- `docs/DEPENDENCIES.md`: reviewed dependency choices and adoption constraints

## Security and privacy

See [SECURITY.md](SECURITY.md). Never commit credentials, account identifiers, mailbox contents, or unredacted protocol logs.

A single accepted transport replay opens a fresh authenticated session and may invoke the configured credential helper one additional time. Credential helpers should therefore be idempotent and free of unrelated side effects.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Croton is licensed under the [Apache License 2.0](LICENSE).

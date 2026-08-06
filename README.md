# Croton MCP for Proton Mail

Croton is a privacy-first, local stdio [Model Context Protocol](https://modelcontextprotocol.io/) server that provides controlled access to Proton Mail through Proton Mail Bridge. This repository intentionally begins with protocol and boundary scaffolding only; it does not contain Proton credentials, account identifiers, mailbox content, or any live-data integration.

## Status

Early bootstrap. The executable supports MCP `2026-07-28` by default and the legacy `2025-11-25` initialization flow for older clients. Mail access, authentication, and message operations are not implemented yet.

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
- `internal/imap`: future narrow IMAP adapter boundary
- `internal/mcpserver`: MCP server construction
- `docs/DEPENDENCIES.md`: reviewed dependency choices and adoption constraints

## Security and privacy

See [SECURITY.md](SECURITY.md). Never commit credentials, account identifiers, mailbox contents, or unredacted protocol logs.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Croton is licensed under the [Apache License 2.0](LICENSE).

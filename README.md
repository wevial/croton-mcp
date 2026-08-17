# Croton MCP for Proton Mail

Croton is a privacy-first, local stdio [Model Context Protocol](https://modelcontextprotocol.io/) server that provides controlled access to Proton Mail through Proton Mail Bridge. The repository includes a production read-only Bridge adapter and synthetic protocol fixtures; it does not bundle Proton credentials, account identifiers, mailbox content, or live fixture data.

## Status

Early read-only implementation. The executable supports MCP `2026-07-28` by default and the legacy `2025-11-25` initialization flow for older clients. Its local Bridge adapter supports bounded folder, status, search, metadata, and body reads over verified loopback TLS; it does not expose mail mutation operations.

## Croton Drive MCP (unofficial)

`croton-drive-mcp` is a separate stdio executable wrapping an
operator-installed Proton Drive CLI with two read-only tools:
`list_drive_entries` and `get_drive_metadata`. Every data command is gated
behind a successful exact-version CLI handshake and fails closed otherwise.
Croton is an unofficial community project: it is not affiliated with or
endorsed by Proton AG. It does not use Proton logos or imitate Proton
branding.

Drive uses its own `--config` file, process, and MCP server. Its strict JSON
schema requires an absolute CLI `binaryPath` and reserves an
`allowedDownloadDirectories` allowlist and a `writes.enabled` policy that is
disabled by default. The server never accesses credentials and registers no
write-capable tools.

## Requirements

- Go 1.26.6 (the module's `toolchain` directive enforces this release)

## Platform support

Linux and macOS are supported and receive identical configuration-loading
guarantees. Both resolve the `--config` path with descriptor-relative,
no-follow traversal over every component, so no symlinked parent or final
component is ever followed and there is no check-then-open race. On both, the
configuration file must be an absolute regular file owned by the current user
with no group or world permission bits (normally mode `0600`).

Windows and every other platform compile but fail closed: they refuse to load a
configuration file at all rather than fall back to a path-based open that
cannot offer the same guarantees.

## Local development

```sh
/usr/local/go/bin/go test ./...
/usr/local/go/bin/go vet ./...
/usr/local/go/bin/go run ./cmd/croton-mcp
```

The server speaks JSON-RPC over standard input/output. Diagnostics must go to standard error; standard output is protocol-only.

## MCP client compatibility

Croton implements the Model Context Protocol rather than depending on a
particular client, so standards-compatible MCP clients can connect to its local
stdio server. Hermes, Claude Code, and Codex are examples of such clients.
Croton's required CI exercises its client-neutral MCP contract against
synthetic fixtures; it does not install a client. A separate non-gating Hermes
consumer-compatibility smoke exercises registration and catalog discovery with
a throwaway `HERMES_HOME`. Hermes is the only client-specific compatibility
path currently exercised in CI; Claude Code and Codex compatibility harnesses
remain intentionally separate from the core contract checks.

## Use with Hermes

Croton registers with Hermes as a local stdio server exposing six read-only
tools and no resources or prompts:

```sh
hermes mcp add croton --connect-timeout 60 \
  --command /absolute/path/to/croton-mcp \
  --args --config /absolute/path/to/croton.json
```

To try this out without touching an existing profile, export
`HERMES_HOME="$(mktemp -d)"` first so the registration lands in a throwaway
profile directory. See [Hermes registration](docs/MCP.md#hermes-registration)
for prerequisites, verification, the exact tool names, and removal.

## Layout

- `cmd/croton-mcp`: stdio executable
- `cmd/croton-drive-mcp`: independent Drive stdio executable scaffold
- `internal/config`: secure configuration boundary for separate Mail and Drive schemas
- `internal/drivemcp`: independent, currently empty Drive MCP server
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

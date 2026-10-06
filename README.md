# Croton MCP for Proton Mail

Croton is a privacy-first, local stdio [Model Context Protocol](https://modelcontextprotocol.io/) server that provides controlled access to Proton Mail through Proton Mail Bridge. The repository includes a production Bridge adapter, read-only by default, and synthetic protocol fixtures; it does not bundle Proton credentials, account identifiers, mailbox content, or live fixture data.

## Why Croton

Croton makes Proton usable by AI assistants without handing them unrestricted access to your account. Privacy is the purpose; prompt-injection resistance is a supporting safeguard.

An assistant connected to Croton reaches Proton only through a small set of
named tools served by a local process on your machine. Mail goes through Proton
Mail Bridge over loopback, and Drive is a separate server with its own
configuration. The same approach can extend to other Proton services where a
supported local interface exists; this README describes only what ships today.
Tools return metadata first and read message bodies only on request, within
fixed bounds, and credentials are kept out of every tool response. Croton's own
audit records name the tool and its outcome, never the content. An assistant
steered by a prompt injection remains limited to what those tools and your
configuration allow.

## Status

Early implementation, read-only by default. The executable supports MCP `2026-07-28` by default and the legacy `2025-11-25` initialization flow for older clients. Its local Bridge adapter supports bounded folder, status, search, metadata, and body reads over verified loopback TLS. Unless the configuration file sets `mutations.enabled` to true, it exposes no mail mutation. With that opt-in, the `mark_read` and `mark_unread` tools from the [Mail triage design](docs/design/0005-mail-triage.md) change only the Seen flag, one UID per `UID STORE`, `move_mail` moves messages to one exact, validated existing folder, one UID per native `UID MOVE`, and `archive_mail` and `trash_mail` do the same into the one selectable mailbox the server marks `\Archive` or `\Trash`, never deleting anything; no live account write has been exercised.

## Croton Drive MCP (unofficial)

`croton-drive-mcp` is a separate stdio executable wrapping an
operator-installed Proton Drive CLI with three read-only tools:
`list_drive_entries`, `get_drive_metadata`, and `get_drive_sharing_status`.
The sharing-status tool reports shares without changing them and never
returns a public link's password. Every data command is gated behind a
successful exact-version CLI handshake and fails closed otherwise.
The tool contract, bounds, error codes, and audit line are documented in
[docs/DRIVE-MCP.md](docs/DRIVE-MCP.md).
Croton is an unofficial community project: it is not affiliated with or
endorsed by Proton AG. It does not use Proton logos or imitate Proton
branding.

Drive uses its own `--config` file, process, and MCP server. Its strict JSON
schema requires an absolute CLI `binaryPath` and reserves an
`allowedDownloadDirectories` allowlist and a `writes.enabled` policy that is
disabled by default. Until a write capability exists, a configuration that
sets `writes.enabled` to true is refused at startup rather than ignored: the
server exits with a static diagnostic before executing the CLI. The server
never accesses credentials and registers no write-capable tools.

## Requirements

- Go 1.26.6 (the module's `toolchain` directive enforces this release)

## User-owned Mail installation

See [the user-owned installation guide](docs/USER-INSTALL.md) for source builds,
configuration and credential-helper prerequisites, catalog verification, and
staged updates and rollback.

See [the Mail release guide](docs/RELEASE.md) for the manual tagged-release
checklist, staged per-platform checksums and acceptance of published bytes.

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

- `cmd/croton-mcp`: Mail stdio executable
- `cmd/croton-drive-mcp`: Drive stdio executable
- `bridge`: read-only IMAP adapter over Proton Mail Bridge
- `internal/config`: secure configuration opener for the separate Mail and Drive schemas
- `internal/mcpserver`: Mail MCP server
- `internal/drivemcp`: Drive MCP server
- `internal/drivecli`: bounded Drive CLI subprocess adapter
- `internal/drivefs`: internal confined-output primitives (confined opening, transactional publication, bounded copying) that no registered Drive tool uses; Drive downloads are not shipped
- `internal/stdioframe`: the bounded newline-framed stdio reader both servers share
- `internal/strictjson`: strict JSON decoding
- `internal/testkit`: synthetic loopback IMAP server and fake-Drive builder for tests
- `internal/testkit/fakedrive`: the fake Proton Drive CLI executable for tests
- [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md): the two servers, the packages behind them, and where the product vision lives in the tree
- `docs/DEPENDENCIES.md`: reviewed dependency choices and adoption constraints

## Security and privacy

See [SECURITY.md](SECURITY.md) and the threat model in
[docs/THREAT_MODEL.md](docs/THREAT_MODEL.md), which names the assets, trust
boundaries, residual risks and what Croton does not defend against. Never commit credentials, account identifiers, mailbox contents, or unredacted protocol logs.

Croton keeps access local and narrow, but the assistant you connect decides
where returned content goes. Content returned to a cloud-backed assistant can reach that assistant's model provider. Croton limits disclosure; it does not guarantee confidentiality after that handoff or that returned content stays on your machine.
A local stdio transport means Croton runs locally, not that the model does.
Croton's audit records exclude content, but Croton does not control what the
assistant, its CLI or its model provider logs.

Croton does not protect a compromised operating system, user account, Proton account or Bridge.
It runs as you and trusts them, so an attacker holding any of them already
holds what Croton guards. Its controls bound what a connected client, including
a prompt-injected one, can reach through the tools.

Croton cannot verify that a human approved a tool call. Any approval prompt
comes from your MCP client. If you set `mutations.enabled` to true, an
auto-approving or prompt-injected client could change read state, move,
archive or trash mail within the configured limits, so enable actions only with
a client whose approval settings you trust.

A single accepted transport replay opens a fresh authenticated session and may invoke the configured credential helper one additional time. Credential helpers should therefore be idempotent and free of unrelated side effects.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Croton is licensed under the [Apache License 2.0](LICENSE).

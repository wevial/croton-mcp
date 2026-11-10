# Croton MCP for Proton Mail

Croton is a local, read-only-by-default stdio
[Model Context Protocol](https://modelcontextprotocol.io/) server for Proton
Mail. It reaches your mailbox only through Proton Mail Bridge over loopback TLS
and offers an assistant a small set of named, bounded tools instead of your
account.

Croton is an unofficial community project. It is not affiliated with or
endorsed by Proton AG and does not use Proton branding. The repository bundles
no credentials, account identifiers or mailbox content; its tests use synthetic
fixtures.

## Status

Early implementation with no tagged release yet. The `croton-mcp` executable
speaks MCP `2026-07-28` and the legacy `2025-11-25` initialization flow. Linux
and macOS are supported; other platforms compile but refuse to load a
configuration and exit. No live account write has been exercised.

## Tools

Six read-only tools are always registered:

- `list_folders`: the mailbox folders Bridge exposes.
- `search_mail`: bounded metadata search in one mailbox.
- `get_message`: one message by opaque ID, with bounded headers and text.
- `get_thread`: the local conversation around one message.
- `list_attachments`: attachment metadata, never attachment bytes.
- `select_digest_candidates`: bounded recent-mail candidates for a digest.

Setting `"mutations": {"enabled": true}` in the configuration adds five triage
writes. `mark_read` and `mark_unread` change only the Seen flag, one UID per
`UID STORE`. `move_mail` moves to one validated existing folder, and
`archive_mail` and `trash_mail` move to the server's `\Archive` or `\Trash`
mailbox, one UID per native `UID MOVE`. Nothing is deleted, and Croton never
sends mail. Labels are not supported.

## Quick start

Requires Go 1.26.10 and a running, logged-in Proton Mail Bridge.

```sh
mkdir -p ~/bin ~/.config
go build -o ~/bin/croton-mcp ./cmd/croton-mcp
test -e ~/.config/croton.json || install -m 0600 /dev/null ~/.config/croton.json
# edit ~/.config/croton.json as described below
~/bin/croton-mcp --config "$HOME/.config/croton.json"
```

The configuration names a loopback IP endpoint, `starttls` or `implicit`, a
`tls.trustAnchorFile` (the exact Bridge leaf PEM) or `tls.spkiSha256` pin, and
an absolute `credentialCommand` that prints the Bridge username and password as
JSON. Register the binary with your MCP client as a stdio server. See
[docs/USER-INSTALL.md](docs/USER-INSTALL.md) for the full procedure.

## How it limits access

- Tools return metadata first; bodies are read only on request and capped,
  and tool results are capped at 100 000 bytes of valid JSON.
- Attachment bytes are never returned.
- Message IDs are opaque and bound to the mailbox's UIDVALIDITY.
- Credentials never appear in tool output or audit lines; audit lines name
  only the tool, outcome and a fixed error code.
- Inbound stdio frames are capped at 64 KiB and tool arguments at 24 KiB, both
  strictly decoded.
- `--config` must be an absolute, owner-only regular file, opened without
  following any symlink in its path.
- Bridge connections are loopback-only, use TLS pinned to an explicit anchor
  or SPKI hash, and run under timeouts and byte budgets.
- Standard output carries only the protocol; diagnostics go to standard error.

## Security summary

Croton keeps access local and narrow, but the assistant you connect decides
where returned content goes. Content returned to a cloud-backed assistant can
reach that assistant's model provider. Croton limits disclosure; it does not
guarantee confidentiality after that handoff or that returned content stays on
your machine. A local stdio transport means Croton runs locally, not that the
model does.

Croton cannot verify that a human approved a tool call. Any approval prompt
comes from your MCP client. If you set `mutations.enabled` to true, an
auto-approving or prompt-injected client could change read state, move,
archive or trash mail, so enable writes only with a client whose approval
settings you trust.

Croton trusts the operating system, your user account, your Proton account and
Bridge. See [SECURITY.md](SECURITY.md) for reporting, the attackers in scope
and residual risks.

## Layout

- `cmd/croton-mcp`: the stdio executable
- `bridge`: the IMAP adapter over Proton Mail Bridge
- `internal/config`: the secure configuration opener
- `internal/mcpserver`: the MCP tools, output caps and audit
- `internal/stdioframe`: the bounded stdio frame reader
- `internal/strictjson`: strict JSON decoding
- `internal/testkit`: a synthetic loopback IMAP server for tests

## Development

```sh
go build ./... && go vet ./... && go test -race ./...
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Croton is licensed under the [Apache License 2.0](LICENSE).

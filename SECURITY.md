# Security policy

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability or include secrets in
a report. Contact the maintainers through the repository's private
security-reporting channel when it is enabled; until then, ask a maintainer for
an encrypted reporting route.

Before the first release, security fixes land on the default branch.
Supported release versions will be listed here once releases exist.

## Sensitive data

Croton handles mail. Never commit, paste into issues, or keep in test fixtures:

- credentials, tokens, recovery material or private keys;
- Proton account identifiers, email addresses or mailbox metadata;
- message bodies, attachments, headers or unredacted protocol logs.

Use synthetic `.test` fixtures. Keep standard output for MCP JSON-RPC and send
diagnostics to standard error.

## What Croton protects

- **Bridge credentials.** Obtained from an operator-configured helper, held
  in memory for one session and zeroed afterwards. They never appear in tool
  output, errors or audit lines.
- **Mailbox content.** Bodies, headers, subjects, folder names and attachment
  names, disclosed only through the registered tools and within their caps.
- **The configuration file.** It names the endpoint, the TLS trust and the
  credential helper, so whoever can rewrite it controls what Croton runs.
- **The stderr stream.** Audit lines carry only a fixed vocabulary of tool
  names, outcomes and error codes; no content has a field to land in.

## Attackers in scope

### Untrusted senders and prompt injection

Anyone can send you mail, so every subject, address, header, body and
attachment name is attacker-controlled. Text in a message may try to steer the
assistant reading it, and a steered or malicious MCP client may send any frame,
argument or volume. Croton bounds what such a client can reach and receive:

- Six read-only tools by default; writes need `mutations.enabled` in the
  configuration file and never include send or delete.
- Only an allowlist of MCP methods is served. Inbound frames are capped at
  64 KiB and must be one strictly decoded JSON object; tool arguments are
  capped at 24 KiB and strictly decoded.
- Results are metadata first. Bodies are read only on request, with hard caps
  on raw, header and decoded text bytes, and every tool result is capped at
  100 000 bytes and truncated to valid JSON.
- Attachment bytes are never fetched or returned.
- Message IDs are opaque and bound to the mailbox UIDVALIDITY, so a stale or
  forged ID fails instead of reaching another message.
- Each write touches one UID per command: Seen-only `UID STORE` or native
  `UID MOVE` to a destination revalidated on the same session. With no `MOVE`
  capability nothing is sent; there is no COPY or EXPUNGE fallback.
- Errors map to a fixed vocabulary; underlying error text never reaches the
  client.

Sender-controlled content stays bounded at every step: header, search, fetch
and structure responses each have a byte budget, and IMAP reads run under
connect and command timeouts.

### Local file tampering

Another local user may plant a symlink or substitute a file on the path passed
with `--config`. On Linux and macOS the opener walks every path component
relative to the previous directory descriptor with no-follow semantics, so no
symlinked parent or final component is followed and there is no
check-then-open race. The file must then be a regular file owned by the
current user, with no group or world permission bits, at most 64 KiB, and
strict JSON with no unknown or duplicate fields. Any failure stops startup.
Other platforms refuse to load a configuration at all.

The configured endpoint must be a loopback IP literal, not a hostname, and TLS
is mandatory with an explicit trust anchor or SPKI pin compared in constant
time. A mismatch aborts before any credential is sent. The credential helper
must be an absolute argv, runs without a shell in a scrubbed environment with
a 64 KiB output cap and a timeout, and is killed with its process group on
cancellation.

## Out of scope

Croton runs as you and trusts these; an attacker holding any of them already
holds what Croton guards:

- **Proton Mail Bridge.** Bridge is trusted. The TLS pin confirms Croton
  reached the Bridge you configured, not that Bridge is uncompromised.
- **The operating system and your user account,** including the kernel, the
  filesystem permission model and physical access to the machine.
- **Your Proton account.**
- **The assistant and its model provider.** Content Croton returns to a
  cloud-backed assistant can reach that assistant's model provider and is
  governed by its terms. Croton does not control what the client, its CLI or
  its provider logs.
- **A configuration file you own and edit yourself.** The opener proves the
  file is private to you, not that its contents are benign.

## Residual risks

- **No human-approval check.** Croton cannot verify that a human approved a
  tool call; any prompt comes from your MCP client. With `mutations.enabled`,
  an auto-approving or prompt-injected client could change read state, move,
  archive or trash mail. Enable writes only with a client whose approval
  settings you trust.
- **Unknown write outcomes are not replayed.** A write reported `unknown` may
  or may not have taken effect until a fresh read shows it.
- **Replay can rerun the credential helper.** One accepted transport replay
  opens a fresh session and may run the helper once more, so helpers must be
  idempotent and free of unrelated side effects.
- **Helper descendants can escape teardown.** A child that starts its own
  session or process group outlives the group kill.
- **No rate limit.** The frame cap bounds one frame; nothing limits how many
  valid frames or tool calls a client sends.
- **Pre-release IMAP library.** `go-imap` v2 is pre-release; see
  [docs/DEPENDENCIES.md](docs/DEPENDENCIES.md).
- **Stderr after redirection.** Audit lines are content-free, but Croton does
  not protect stderr once an operator sends it somewhere shared.

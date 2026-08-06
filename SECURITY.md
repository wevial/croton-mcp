# Security policy

## Reporting a vulnerability

Do not open a public issue for a suspected vulnerability or include secrets in a report. Contact the maintainers through the repository's private security-reporting channel when it is enabled; until then, ask a maintainer for an encrypted reporting route.

## Sensitive data rules

Croton handles mail-adjacent workflows. Contributors must never commit, paste into issues, or retain in test fixtures:

- credentials, tokens, recovery material, or private keys;
- Proton account identifiers, email addresses, or mailbox metadata;
- message bodies, attachments, headers, or unredacted protocol logs.

Use synthetic, non-identifying fixtures. Route diagnostics to standard error and keep standard output reserved for MCP JSON-RPC.

## Supported versions

Before the first release, security fixes are made on the default development branch. Released-version support windows will be documented here.

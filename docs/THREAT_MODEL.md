# Croton threat model

This document states what Croton protects, whom it protects it from, where
each protection is enforced in the tree, how each one fails closed, and what
Croton deliberately does not defend against. It is written from the invariants
in `AGENTS.md`, the data rules in `SECURITY.md`, the `README.md` platform and
security sections and the code itself. Every boundary below names the code that
enforces it so that a reviewer can check a change against this model rather
than against memory. `scripts/verify_docs_threat_model.py` pins the structure
and the statements that are easiest to get wrong.

Croton ships two separate local executables, `cmd/croton-mcp` for Proton Mail
Bridge and `cmd/croton-drive-mcp` for the Proton Drive CLI. They share no
process, configuration file or credential, which is the first boundary of the
model. The separation is one of code and configuration, not of privilege: both
executables, and every child they spawn, run as the person invoking them, so a
compromised Drive CLI holds the same operating-system access as that person,
including access to the Mail configuration file. Croton does not enforce
compromise isolation between the two surfaces.

## Assets

What Croton is trusted with, in decreasing order of sensitivity:

- **Bridge IMAP credentials.** The username and password that Proton Mail
  Bridge accepts on loopback. Croton obtains them from a credential helper,
  holds them only in memory for the life of a session and zeroes the bytes
  afterwards.
- **Mailbox content and metadata.** Message bodies, headers, subjects, folder
  names, attachment names and the opaque identifiers Croton mints for them.
- **Drive content, sharing members and public links.** Node names, paths,
  sizes, the members a node is shared with and the existence and settings of a
  public link. Croton never returns a public link's password.
- **The configuration file.** The Mail file names the Bridge endpoint, the
  TLS trust anchor or pin and the credential helper command; the Drive file
  names the CLI binary and its reserved download and write policy. Whoever can
  rewrite either file controls what Croton connects to and executes.
- **The audit and diagnostic stream.** Stderr output that an operator may
  retain. It must never carry any of the assets above.

## Attacker model

Croton assumes the following adversaries, each holding a specific position:

- **A prompt-injected or malicious MCP client.** The consuming assistant, or
  anything on the other end of stdin and stdout, may send any JSON-RPC frame,
  any method, any argument shape and any size, and may replay frames. Its goal
  is to reach a capability Croton does not offer, exhaust the process, or pull
  secrets or unbounded data through the protocol stream.
- **A local user who can place files or symlinks on the configuration path.**
  Someone sharing the machine who can create a symlink or substitute a file at
  or above the path passed with `--config`, hoping Croton follows it or accepts
  a world-readable copy.
- **A hostile or malformed Bridge or CLI process.** The loopback IMAP peer or
  the Drive CLI child may be a different program than expected, may return
  malformed or unbounded output, or may hang. Croton treats both as untrusted
  input sources.
- **A swapped CLI binary.** The executable at the configured Drive
  `binaryPath` may have been replaced with something that reproduces the
  expected version banner.

Croton does not assume a trustworthy consuming assistant; it limits what any
client can obtain. It does assume the operating system, the user account
running Croton, the Proton account and Bridge are not themselves compromised
(see Out of scope).

## Trust boundaries

Each subsection states what crosses the boundary, what enforces it and where,
how it fails closed, and what it does not guarantee.

### Mail Bridge loopback TLS

**What crosses it.** IMAP commands and responses between Croton and Proton
Mail Bridge on the local machine, including the Bridge credentials at login.

**What enforces it.** `bridge/endpoint.go` parses the configured endpoint as
an IP literal and accepts only loopback addresses; hostnames are refused so no
resolver can redirect the connection. `bridge/tlspolicy.go` builds the TLS
configuration: the verifier is installed atomically, requires either a trust
anchor file or an SPKI pin, and compares the presented leaf certificate in
constant time before checking its validity window and server-authentication
usage. `bridge/dial.go` performs the connection and STARTTLS upgrade under a
byte and time budget, and `bridge/config.go` documents the anchor-versus-pin
choice. The adapter in `bridge` exposes no mutating IMAP operation, so nothing
sent through it can alter the mailbox.

**How it fails closed.** A non-loopback endpoint, a hostname, a configuration
with neither anchor nor pin, an unparseable anchor, a leaf that does not match,
or a STARTTLS exchange that exceeds its budget each abort before any credential
is sent.

**What it does not guarantee.** Loopback TLS with explicit trust proves that
the peer holds the expected certificate. It does not prove that the peer is the
genuine Bridge or that the Bridge itself is uncompromised, and it offers no
protection if the trust anchor or pin in the configuration file has been
replaced (see the configuration opener).

### Credential helper

**What crosses it.** The Bridge username and password, produced by an
operator-configured command and consumed once per authenticated session.

**What enforces it.** `bridge/credentials.go` runs the helper only as an
absolute argv with no shell, in a scrubbed environment, with a 64 KiB output
cap and a timeout, parses a strict JSON object and zeroes the credential bytes
when the session ends. `bridge/credentials_process_linux.go` places the helper
in its own process group and kills the whole group on cancellation, so a
helper's direct children in that group are torn down with it.

**How it fails closed.** A relative command, output over the cap, output that
is not exactly the expected object, or a helper that exceeds its timeout all
fail the session; the partial output buffer is discarded and no login is
attempted.

**What it does not guarantee.** Process-group teardown kills only the group
the helper was started in. A descendant that creates its own session or process
group escapes that kill and can outlive the budget; Croton does not track or
confine such descendants. Croton does not authenticate the helper: whoever
controls the configuration file controls which program runs. Process-group
teardown exists only where the tree implements it: Linux in
`bridge/credentials_process_linux.go` and macOS and FreeBSD in
`bridge/credentials_process_unix.go`. On any other platform
`bridge/credentials_process_other.go` reports the group kill as unsupported and
`isSafeCredentialCommand` rejects every credential command, so no helper runs
without teardown. As `README.md` states, a single accepted transport replay opens a
fresh session and may invoke the helper one more time, so helpers must be
idempotent.

### Configuration opener

**What crosses it.** The bytes of the Mail or Drive configuration file, read
from the path given with `--config`.

**What enforces it.** `internal/config/open_unix.go` opens the path one
component at a time relative to the previous directory descriptor with
no-follow semantics, so no symlinked parent or final component is ever
traversed and there is no check-then-open race. `internal/config/config.go`
then requires, on the descriptor already held, a regular file owned by the
current user with no group or world permission bits and at most 64 KiB, and
decodes it with `internal/strictjson` so unknown fields and duplicate keys are
rejected. `internal/config/drive.go` applies the same loader to the separate
Drive schema. `internal/config/open_other.go` is the implementation for every
platform other than Linux and macOS: it refuses to open anything.

**How it fails closed.** A symlink anywhere on the path, a FIFO or device, a
file that is group- or world-accessible, a file owned by someone else, an
oversize file, or a schema violation all stop the executable at startup. On
Windows and every other unsupported platform the executable starts, reports
that secure loading is unavailable, and exits.

**What it does not guarantee.** The opener proves the file was reachable
without following links and is private to the invoking user. It does not
protect against a user who legitimately owns the file being tricked into
editing it, and it does not detect a malicious file with correct ownership and
mode.

### Drive CLI subprocess

**What crosses it.** Command lines from Croton to the Proton Drive CLI, and the
CLI's standard output back.

**What enforces it.** `internal/drivecli/client.go` pins one exact CLI version:
`Handshake` runs the version command and accepts only a first stdout line that
parses to the pinned version. Every later invocation is checked by
`internal/drivecli/commands.go` against a frozen allowlist of command lines,
element by element, before it runs. `internal/drivecli/exec.go` builds a
scrubbed environment containing only a fixed PATH, a handful of locale and home
variables and the two Drive-specific cache and credential-store variables; the
child runs from a neutral working directory with a capped output buffer and a
deadline. `internal/drivemcp/gate.go` refuses every data tool until the
handshake has succeeded, and `internal/drivemcp/path.go` validates each Drive
path argument before it can become an operand. Croton never holds a Proton
password on the Drive side: the CLI keeps its own session, and the Drive server
never reads a credential.

**How it fails closed.** A missing binary, a banner that does not parse or
names another version, an argument vector outside the allowlist, output over
the cap, a child that exceeds its deadline or output that is not the expected
JSON each map to a fixed error code and return nothing else to the client.

**What it does not guarantee.** Version negotiation checks compatibility, not
provenance. The handshake proves that the program at the configured path
printed the expected banner; it does not prove which binary is running, that it
is unmodified, or that it is confined. Nothing in Croton sandboxes the child
beyond the scrubbed environment, the neutral working directory and the output
and time caps. The download command exists on the client but is not registered
as a tool, and local download confinement and write approval are reserved and
not shipped.

### MCP stdio surface

**What crosses it.** JSON-RPC frames between the consuming assistant and
either executable, over standard input and standard output only. Neither
executable listens on a socket.

**What enforces it.** Both servers register only read-only tools and install an
allowlist middleware, `internal/mcpserver/tools.go` for Mail and
`internal/drivemcp/tools.go` for Drive, that rejects every method outside a
fixed set. Tool arguments are capped at 24 KiB and decoded strictly
(`internal/mcpserver/handlers.go`, `internal/drivemcp/tools.go`); serialized
results are capped at 100 000 bytes and truncated structurally so the output is
always valid JSON (`internal/mcpserver/output.go`,
`internal/drivemcp/output.go`). Errors map to a fixed vocabulary and audit
lines use a fixed vocabulary; `docs/MCP.md` states the Mail output, error and
audit contracts and this document does not restate them.

Inbound frame size is bounded on the Mail side only. `internal/mcpserver/stdio.go`
wraps standard input in a bounded frame reader with a 64 KiB limit that is
installed before the SDK sees a byte, and additionally rejects frames that
alias protocol fields. `internal/drivemcp/server.go` returns the SDK's plain
`IOTransport` with no frame bound, so the Drive executable relies on the
argument cap alone once the SDK has read a frame.

**How it fails closed.** An unlisted method, oversize arguments, an argument
object with unknown or duplicated fields or a frame over the Mail bound
produce a fixed error or close the transport; no partial result and no
underlying error text reach standard output.

**What it does not guarantee.** The surface limits what a client can request
and how much it receives. It does not make the client trustworthy: anything a
tool legitimately returns is disclosed to that client. Drive stdio frames are
unbounded until the frame-bound ticket lands, so a client can make the Drive
process buffer an arbitrarily large single frame.

### Audit and diagnostic stream

**What crosses it.** One JSON line per tool call when auditing is enabled, and
startup diagnostics, all written to standard error.

**What enforces it.** Standard output is reserved for protocol frames; every
diagnostic goes to stderr. `internal/mcpserver/audit.go` and
`internal/drivemcp/audit.go` emit only the allowlisted fields and re-validate
the tool name, outcome and error code against fixed sets before writing, so a
value that did not originate in Croton's own vocabulary is replaced rather than
logged.

**How it fails closed.** A tool name, outcome or code outside the fixed set is
rewritten to a neutral placeholder. Caller inputs, folder names, identifiers,
subjects, addresses and error text have no field to land in.

**What it does not guarantee.** The stream proves that Croton's own records are
secret-free. It does not control what the Bridge, the Drive CLI or the
consuming assistant write to their own logs, and it does not protect stderr
once an operator redirects it somewhere shared.

## Residual risks

Risks the code already admits and this model records rather than hides:

- **Replay may invoke the credential helper once more.** A single accepted
  transport replay opens a fresh authenticated session and may run the helper
  again, as `README.md` states. Helpers must be idempotent and side-effect free.
- **go-imap is a pre-release dependency.** `docs/DEPENDENCIES.md` records why it
  was adopted and the constraints on its use. Its parser sits behind the Bridge
  boundary and processes untrusted peer output.
- **Drive stdio frames are unbounded.** Only the Mail executable installs the
  64 KiB frame guard; Drive's SDK transport reads whole frames without a limit
  until its own ticket lands.
- **The version banner does not authenticate a swapped executable.** A
  replaced CLI that prints the pinned banner passes the handshake.
- **Drive downloads and writes are reserved and not shipped.** The
  configuration schema reserves an allowed-directory list and a write policy,
  both disabled by default, and the server registers no download or write
  tool. Their confinement and approval semantics are not designed here.

## Out of scope

Croton does not defend against the following, by design:

- **Compromise of the Proton account, Proton Mail Bridge, the operating
  system, or the user account running Croton.** Croton runs as that user and
  trusts the kernel, the filesystem permission model and the Bridge it
  connects to. An attacker holding any of these already holds every asset
  above.
- **The consuming assistant and its model provider.** Croton limits and
  controls what a client can access; it does not make the consumer's
  processing local and does not guarantee confidentiality after disclosure.
  Content returned to a consuming assistant may reach that assistant's model
  provider, may be retained there, and is governed by that provider's terms,
  not by Croton. "Privacy-first" means Croton discloses the minimum a request
  needs, not that the disclosed content stays on the machine.
- **Physical access.** Anyone with the machine can read process memory, the
  configuration file and the credential helper's inputs.
- **Windows.** Both executables compile but refuse to load a configuration
  file, so nothing runs. Support is fail-closed by design until a
  descriptor-relative, no-follow opener exists for that platform.

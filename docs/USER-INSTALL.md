# User-owned Mail installation

This guide describes an operator-performed source installation of `croton-mcp`
on Linux or macOS. It performs no installation, Bridge login, service change,
client profile change, or mail access. All paths are synthetic role placeholders;
replace them locally, never with private values in a ticket or agent conversation.
The same non-root operator owns the files and runs the MCP client and Croton.
The separate [dry deployment design](DEPLOYMENT.md) and its approval gates remain
unchanged; this guide does not authorize that deployment.

## Source build

Obtain the source from the repository and explicitly select a full reviewed
commit SHA. Do not build an unreviewed moving branch or infer a release from a
tag. The commands below are examples for the operator, run in a new checkout;
stop on any failure. Git and Go must already be installed. Dependency download
may require network access; these are not offline build instructions.

```sh
git clone https://github.com/wevial/croton-mcp.git /absolute/operator/source/croton-mcp
cd /absolute/operator/source/croton-mcp
REVIEWED_REVISION='<full-reviewed-commit-sha>'
git checkout --detach "$REVIEWED_REVISION"
git rev-parse HEAD
export GOTOOLCHAIN=go1.26.6
go version
go build ./...
go vet ./...
go test -race ./...
MAIL_CANDIDATE_DIR=/absolute/operator/candidates/mail-reviewed
python3 scripts/stage_mail_candidate.py --revision "$REVIEWED_REVISION" --output "$MAIL_CANDIDATE_DIR"
```

Confirm HEAD equals the reviewed SHA and `go version` reports Go 1.26.6,
matching the repository's `go.mod` toolchain. Review and retain the candidate
manifest with its SHA, toolchain, platform, and binary SHA-256 locally. Race tests
require a supported C compiler.
Build only the Mail executable for installation; Drive has a separate setup.

The local staging helper requires Python 3.12 or newer, a full maintainer-reviewed
revision equal to HEAD, a clean tracked working tree and index, and an absent
absolute output directory outside the checkout (with an existing parent).
It does not fetch or check out revisions. It builds only `./cmd/croton-mcp` with
`-trimpath` from a temporary Git archive of the selected revision; ignored and
untracked checkout inputs are excluded. Git archive export attributes apply.
Only native Linux and macOS targets are supported; cross-compilation is rejected
before building. The helper selects Go 1.26.6, disables Go workspace and persisted
Go environment settings, and clears GOFLAGS. Dependency downloads may still occur;
this is not hermetic reproducibility.

The candidate directory contains `croton-mcp` and `manifest.json`, recording
`revision`, `toolchain`, `GOOS`, `GOARCH`, `binary` (filename), and `sha256` of the
actual binary bytes. Existing output is never replaced. Failed builds remove
temporary staging and do not create final output. After success, review the
manifest and independently compare the binary's SHA-256 before manual installation
below. Checksums provide integrity, not signatures or provenance attestation.
Nothing is published or installed by the helper; it makes no profile or service
changes and accesses no live accounts. Synthetic subprocess tests verify the
helper contract only; they are not evidence that a real release was built.
Actual candidate acceptance remains a separate operator step.

Candidate publication is not atomic. If linking the candidate files fails, the
helper attempts to remove its linked files and the output directory. Cleanup
errors can leave partial output; the helper reports incomplete rollback and
returns nonzero. Inspect the requested output locally, do not install it, and
remove only artifacts you identify as belonging to the failed attempt before
retrying with an absent output path. Concurrently created entries are not removed
by rollback.

## User-owned layout

Choose absolute, canonical, operator-writable paths before staging. The following
roles need not share a directory. Do not use `sudo` or a service account.

| Role | Synthetic example | Operator policy |
| --- | --- | --- |
| User bin directory | `/absolute/operator/bin` | directory, mode `0700` |
| Mail executable | `/absolute/operator/bin/croton-mcp` | regular file, mode `0700` |
| Credential helper | `/absolute/operator/bin/croton-credentials` | regular executable, mode `0700` |
| Config directory | `/absolute/operator/config/croton` | directory, mode `0700` |
| Config | `/absolute/operator/config/croton/mail.json` | regular file, mode `0600` |
| Trust | `/absolute/operator/config/croton/bridge-leaf.pem` | regular file, mode `0600` |
| Backups | `/absolute/operator/rollback/croton` | directory, mode `0700` |

Use `umask 077` before creating these directories and files. Inspect the selected
paths' metadata, including every existing parent, locally before writing. Use
symlink-free paths throughout; on macOS choose canonical paths rather than
symlink aliases. Keep directories and artifacts operator-owned and unwritable
by other users. This is operator policy for the binary, helper and trust file.

For a first install, ensure both final and candidate names are absent (including
dangling symlinks). Create the selected directories with mode `0700`, then stage
the build in the selected bin directory, for example:

```sh
umask 077
CROTON_BIN_DIR=/absolute/operator/bin
install -m 0700 "$MAIL_CANDIDATE_DIR/croton-mcp" "$CROTON_BIN_DIR/croton-mcp.candidate"
```

Compare the staged binary's SHA-256 with the build artifact using the platform's
local hash utility. Prepare and verify the config, trust and helper below before
renaming the candidate to `croton-mcp` in the same directory. Existing installs
must use the backup and replacement procedure under Update and rollback.

## TLS and configuration prerequisites

Separately, the operator must install Proton Mail Bridge, log in, and complete
its account setup using Bridge's own interface. Obtain its loopback IMAP port,
TLS mode, Bridge-generated credentials and exact leaf certificate through trusted
local setup. This guide supplies no Bridge login commands and executes none.
Bridge must already be available in the operator's session with loopback-only
IMAP. Do not expose Bridge on a network or disable TLS to fix connectivity.

Privately prepare the config from this **synthetic configuration fixture**.
Replace the path roles and endpoint settings locally; the example PEM path is
not an existing certificate. No inline credentials belong in the config.

<!-- user-install-config -->
```json
{
  "imap": {
    "host": "127.0.0.1",
    "port": 1143,
    "tlsMode": "starttls",
    "credentialCommand": ["/absolute/operator/bin/croton-credentials"],
    "tls": {"trustAnchorFile": "/absolute/operator/config/croton/bridge-leaf.pem"}
  },
  "bounds": {"maxSearchResults": 50},
  "audit": {"enabled": true}
}
```

Config must be an absolute regular owner-only file owned by the effective user
running Croton, with no group or world permission bits (normally `0600`), at
most 64 KiB. The loader performs descriptor-relative no-follow traversal of
every path component: symlinked parents and the final component are rejected.
JSON must be one object; unknown, duplicate, null and case-aliased fields fail.
Linux and macOS support this loader; other platforms fail closed at startup.

Use a loopback IP literal, not a hostname; set the port and `starttls` or
`implicit` to match Bridge. Explicit trust is mandatory. `trustAnchorFile` must
contain exactly one PEM CERTIFICATE block, the exact non-CA Bridge leaf, at most
16 KiB, with the certificate header at byte zero and no extra blocks or
trailing data. Croton requires a regular file and does not follow the final symlink;
trust-file parent paths, owner and modes are operator policy, unlike config
traversal. Obtain and verify the certificate out of band through trusted local
Bridge setup, not by blindly accepting a presented network certificate.
Alternatively, `spkiSha256` is the lowercase 64-hex SHA-256 of the certificate's
SubjectPublicKeyInfo, not the whole certificate. If both trust forms are set,
both must match. Do not substitute a public CA bundle.

## Credential helper contract

The authoritative implementation is [bridge/credentials.go](../bridge/credentials.go);
configuration rules are in [internal/config](../internal/config/config.go) and
[bridge/config.go](../bridge/config.go). Prepare a reviewed, operator-owned,
pass-backed helper at the absolute `credentialCommand[0]` path. Treat that
program and all argv elements as privileged configuration, never model input.
Croton invokes argv without a shell: no shell expansion, pipelines, `~`, or
variable substitution is performed by Croton.

The helper environment is exactly fixed `PATH=/usr/bin:/bin` plus `HOME`, `USER`,
`LOGNAME`, `LANG`, `LC_ALL` when those variables exist in the parent. There is no
arbitrary environment passthrough: `GNUPGHOME` and `PASSWORD_STORE_DIR` are not
inherited. A helper script may set its own explicit environment for its private
store and keyring and use absolute programs, including its interpreter and
`pass` executable. Resolve those local program paths during operator setup;
do not assume a package manager's bin directory is in PATH.

Prepare the pass entries privately with the **Bridge-generated IMAP username
and password**, not the Proton account password. Define an unambiguous local
entry format: for example, separate entries each containing exactly one value,
with only the store's terminating newline removed. Reject missing or empty
values; never silently truncate multiline data or interpolate it into JSON.
Use a real JSON serializer (such as Python's `json.dumps`) to encode the values,
including quotes, backslashes and control characters. The output must be one
UTF-8 JSON object with exactly two non-empty string fields, `username` and
`password`, without duplicate keys or additional JSON values. Only trailing
whitespace is allowed. Banners and status text must not appear on stdout.
A synthetic output shape is `{"username":"bridge-user@example.test","password":"synthetic-only"}`;
it is not a secret to enroll or a command to execute.

Croton supplies empty stdin: no interactive stdin is available. Secret tooling
must be pre-unlocked in the operator's session and able to run without pinentry,
TTY prompts or interactive login. Review how the helper locates its GPG agent
under the restricted environment; a successful interactive shell invocation
does not establish that it works here. Helper stderr is discarded, stdout is
bounded to 64 KiB, and execution is bounded by the configured command timeout
(default five seconds). Fail nonzero without emitting partial credentials.
Keep it idempotent: an accepted transport replay can invoke it again.

Never print real helper output, run the real helper by hand, capture its output
in a file, or paste credentials into logs, prompts or shell history. Review helper
logic with synthetic `.test` values and synthetic stubs for pass/secret tooling
only, including quotes, backslashes, newlines and failure cases. This guide does
not provide or execute a helper script, enroll secrets, or read private helpers.

## Verification and stdio registration

First run the offline documentation checks from the repository:

```sh
python3 scripts/verify_docs_user_install.py --self-test
python3 scripts/verify_docs_user_install.py
```

These checks inspect repository text and synthetic fixtures only; they do not
start Croton, run a helper, connect to Bridge or validate a live installation.
Check artifact types, ownership, modes, canonical paths and binary hashes locally
without publishing private paths or config contents.

For client-neutral stdio registration, configure the client's executable field
as `/absolute/operator/bin/croton-mcp` and its argument array as
`["--config", "/absolute/operator/config/croton/mail.json"]`. Use the same
operator account; supply no credentials in client fields or environment.
Keep stdout protocol-only and diagnostics on stderr. Croton has no listener or
service unit. Consult your client's stdio registration mechanism locally; this
guide changes no Hermes profile and claims no validated Claude Code or Codex
compatibility.

**Catalog-only verification:** use a client operation restricted to initialization
and `tools/list`, without automatic tool execution. Expect `list_folders`,
`search_mail`, `get_message`, `get_thread`, `list_attachments`, and
`select_digest_candidates`, with no resources or prompts. Catalog discovery
validates config but does not invoke the helper, dial Bridge, authenticate, or
read mail. It does not prove that trust material or credentials work for a live
connection. Do not use a client test that automatically calls mail tools.

**Explicitly authorized live read:** only after the operator separately authorizes
a named bounded mail read may a client issue `tools/call`. Even `list_folders`
is a live read and exposes mailbox information. This is the first helper
execution and Bridge connection/authentication, not part of catalog validation.
Keep results private; never send live mailbox content, account identifiers,
credentials, protocol captures or unredacted diagnostics to agents or tickets.

## Update and rollback

1. Select another explicit reviewed revision and repeat Source build in a fresh
   checkout. Record its SHA, Go version and binary hash. Review configuration
   changes before using an older config with a newer executable.
2. Before replacing anything, close the client's Croton session and prevent new
   launches through the client's normal controls. Confirm that its Croton child
   has exited. Do not overwrite a running binary or alter Bridge services.
3. In a new private backup directory, record for each target (binary, config,
   helper and trust) whether it existed, plus its type, mode, owner and hash.
   Back up each existing artifact with its permissions intact. Keep private
   backups local and protected; record the previous source SHA and client
   executable/argument settings. Do not log config or secret contents.
4. Stage the candidate under a new unused filename in the selected bin directory.
   Stage every required reviewed config, helper and trust change as a regular
   file under a new unused name in its target directory with restrictive
   permissions. Verify staged hashes, ownership and modes, and confirm that
   the config references the intended final helper and trust paths. Retain the
   matched prior set for rollback; do not make the config a symlink.
5. Keep launches blocked while installing the complete matched artifact set:
   rename the staged executable and every staged config, helper and trust file
   over their respective final targets within each target's directory. Each
   rename is atomic, but the set of renames is not a transaction. Verify all
   final artifacts against the intended set, including unchanged supporting
   files reviewed for compatibility. If any installation or verification step
   fails, keep launches blocked and restore the complete prior set using step 7.
   Do not reopen the client session until the complete matched set is installed
   and verified.
6. Reopen the client session and repeat catalog-only verification. Catalog-only
   verification does not exercise the helper or trust material. A live read
   still requires separate explicit authorization. Retain the backup until the
   operator has accepted the update.
7. On failure, close the Croton session and prevent new launches again. Restore
   each changed artifact from the recorded backup via a staged regular file and
   same-directory rename; verify original hashes, owner and modes. Restore prior
   client executable/argument settings if changed. Remove only exact newly
   created targets recorded as previously absent; never recursively delete the
   install tree. Keep launches blocked until the complete prior set is restored
   and verified, then reopen the session and repeat catalog-only verification.
   Retain backups on failure.

Rolling Croton back does not roll Bridge back. Bridge may run as a user-service
and may self-update independently, changing availability or its certificate.
Do not bind the install to a transient versioned Bridge binary path. If Bridge
is unavailable or trust changes, stop and resolve its setup separately with the
operator; this guide contains no commands to modify or restart running services.
Never bypass certificate verification to make an update or rollback pass.

## Troubleshooting

- Configuration unreadable: check the effective user, absolute regular file,
  owner-only permissions, size and every parent for symlinks. Use canonical
  paths. Unsupported platforms intentionally fail closed.
- Configuration invalid: check JSON shape, loopback IP, port, TLS mode, explicit
  trust and absolute helper argv against the source schema.
- Catalog succeeds but a live read fails: catalog does not test Bridge, its leaf
  certificate, helper execution or credentials. Privately check that Bridge setup
  is complete and trust matches. Diagnose helper behavior with synthetic stubs
  under the documented environment; check pre-unlocked tooling and timeouts.
- After Bridge self-update or session logout: user-service availability and trust
  may differ. Resolve Bridge prerequisites separately; do not edit services,
  chase transient executable paths, or weaken TLS here.
- Client startup failure: check absolute executable and argument fields and
  protocol-only stdout. Share only static error codes or synthetic reproductions,
  never unredacted diagnostics.

## Distribution limitations

This is a source-build path. The repository does not currently provide published
binary releases, supported install packages, a release updater, or a validated
binary download channel through this guide. No release automation or publishing
is performed here. Operators review revisions, build, stage, update and retain
rollback artifacts themselves. Compilation on another platform is not proof of
supported secure configuration loading or client compatibility.

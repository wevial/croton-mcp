# Croton private deployment runbook (dry design)

## 1. Purpose, scope, and gates

A planned deployment and rollback runbook for one private, single-operator
Croton install under `/srv/agents/croton`. It is documentation only: nothing
here has been executed, and every command is an example to be reviewed now and,
if approved, run later by a human operator in a named follow-up task (4B/4C/4D).

Croton owns **no listening socket, no network listener, and no service unit**.
It runs only as a Hermes stdio child process, on demand, and its only network
activity is an **outbound loopback TLS connection to Bridge** at
`127.0.0.1:1143`. Nothing about Croton is exposed to any network.

Permanently out of scope: public listeners, Cloudflare or Tailscale exposure,
firewall changes, systemd units, Bridge login or Bridge configuration changes,
service restarts, release packaging, and digest product work.

### Variables

All values are selected and approved by the operator before any action. This
document names no real account and no real source path.

| Variable | Meaning |
| -------- | ------- |
| `CROTON_ROOT` | Planned root, `/srv/agents/croton`. |
| `CROTON_USER` | Approved non-root account that owns the artifacts and runs Hermes — and therefore runs Croton. Must be one account for all three. |
| `CROTON_GROUP` | Approved primary group of `CROTON_USER`. |
| `STAGED_BINARY` | Approved path of the reviewed, pre-built `croton-mcp` candidate. |
| `STAGED_HELPER` | Approved path of the reviewed `croton-credentials` helper program. |
| `STAGED_LEAF_PEM` | Approved path of the Bridge leaf certificate PEM provided out of band. |
| `BACKUP_ID` | Approved non-empty bare directory name under `$CROTON_ROOT/rollback`; only ASCII letters, digits, `_`, or `-` — never a path. |

Croton's config loader requires the config file to be owned by the effective UID
of the running process, so confirm the Hermes account first; a mismatch fails
closed at startup.

### Approval matrix

Each gate is an explicit, separately recorded human approval. Approval for one
gate never implies the next.

| Gate | Covers | Task |
| ---- | ------ | ---- |
| G0 | Read-only preflight (section 3) | 4A/4B — no approval needed; prints no secrets |
| G1 | Host writes: staging the named regular artifacts under `$CROTON_ROOT` (directories, binary, helper program, trust PEM, config). Approval names the exact paths written. | 4B |
| G2 | Credential/secret enrollment or provisioning, by Ko via approved secret tooling | 4B — separate from G1 |
| G3 | Hermes profile change (`hermes mcp add`) and catalog discovery (`hermes mcp test`), which starts the Croton process. Catalog-only add/test/list does not invoke the helper, authenticate, or dial Bridge. | 4C |
| G4 | Any Bridge or service change (login, config, restart) | Out of scope — separate task |
| G5 | One live read-only mail tool call — the first helper execution and the first outbound Bridge TLS connection and authentication | 4D — never implied by G3 |
| G6 | Rollback: Hermes deregistration | rollback |
| G7 | Rollback: restore or delete of approved Croton artifacts | rollback — separate from G6 |

## 2. Planned layout, ownership, and modes

No symlink anywhere in the tree is operator policy for the whole install, and no
symlink in any parent component of `CROTON_ROOT` either.

| Path | Type | Owner | Mode | Notes |
| ---- | ---- | ----- | ---- | ----- |
| `$CROTON_ROOT` | dir | `CROTON_USER:CROTON_GROUP` | `0750` | Planned root |
| `$CROTON_ROOT/bin` | dir | same | `0750` | |
| `$CROTON_ROOT/config` | dir | same | `0750` | |
| `$CROTON_ROOT/trust` | dir | same | `0750` | |
| `$CROTON_ROOT/rollback` | dir | same | `0750` | Holds one `$BACKUP_ID` subdirectory per attempt |
| `bin/croton-mcp` | regular file | same | `0750` | Operator policy |
| `bin/croton-credentials` | regular file | same | `0700` | Operator policy; argv[0] must be absolute |
| `config/croton.json` | regular file | same | `0600` | `0600` is operator policy; see note below |
| `trust/bridge-leaf.pem` | regular file | same | `0600` | Operator policy; Croton checks neither owner nor mode |
| `rollback/$BACKUP_ID/` | dir | same | `0750` | Mirrors the prior artifacts it backs up |

What Croton enforces, versus what is operator policy:

Treat the implementation as authoritative and re-verify these claims against
`internal/config/config.go`, `bridge/tlspolicy.go`, `bridge/credentials.go`, and
`internal/mcpserver/audit.go` before relying on them after a code change.

- **Config — enforced by Croton:** absolute path, every path component
  traversed with `O_NOFOLLOW` (secure parent traversal), regular file, owned by
  the effective UID of the running process, size ≤ 64 KiB, and **no group or
  world permission bits set**. Mode `0600` satisfies this, but so would `0400`;
  the exact `0600` in the table is operator policy, not a Croton check. Content
  must be a single bounded JSON object; unknown, null, duplicate, and
  case-folded-alias fields are rejected.
- **Trust file — enforced by Croton:** opened no-follow, must be a regular
  file, ≤ 16 KiB, and must contain exactly one PEM `CERTIFICATE` block that is
  the exact non-CA Bridge **leaf**. CA bundles and multi-block files are
  rejected. Croton does not check the trust file's owner, mode, or its parent
  components for symlinks — those rows are operator policy.
- **Credential helper — enforced by Croton:** the configured `credentialCommand`
  argv[0] must be an absolute path, and is executed verbatim without a shell.
  Its mode, owner, and no-symlink placement are operator policy.
- **Binary and directory modes:** operator policy.

### Planned configuration shape

Placeholders for review; the file is written only under gate G1.

```json
{
  "imap": {
    "host": "127.0.0.1",
    "port": 1143,
    "tlsMode": "starttls",
    "credentialCommand": ["/srv/agents/croton/bin/croton-credentials"],
    "tls": {"trustAnchorFile": "/srv/agents/croton/trust/bridge-leaf.pem"}
  },
  "bounds": {"maxSearchResults": 50},
  "audit": {"enabled": true}
}
```

- `host` accepts only IP literals that are loopback after unmapping
  (`127.0.0.1`, `::1`, IPv4-mapped loopback). Hostnames are rejected.
- `tlsMode` is `starttls` or `implicit`. Explicit trust is mandatory: with
  neither `trustAnchorFile` nor `spkiSha256`, Croton fails closed.
- The leaf PEM is the recommended trust material, because the frozen scope names
  trust material as a deployed artifact. The alternative is an inline
  `spkiSha256` — lowercase 64-hex SHA-256 of the certificate's
  SubjectPublicKeyInfo, not of the whole certificate. If both are set, both must
  match.
- No credential material appears in this file, on any command line, or in the
  Hermes profile. It exists only behind `credentialCommand`.

## 3. Preflight (read-only, gate G0)

These checks read metadata only. None prints configuration contents, helper
output, credentials, or mail. Set the approved values first:

```sh
CROTON_ROOT=/srv/agents/croton
CROTON_USER=the-approved-account
CROTON_GROUP=the-approved-group
STAGED_BINARY=/the/approved/path/to/croton-mcp
STAGED_HELPER=/the/approved/path/to/croton-credentials
STAGED_LEAF_PEM=/the/approved/path/to/bridge-leaf.pem
BACKUP_ID=the-approved-backup-id

case "$BACKUP_ID" in
  ''|*[!ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-]*)
    printf '%s\n' 'invalid BACKUP_ID' >&2; false ;;
esac
```

The guard never echoes the value; non-zero status is a stop condition before
constructing any backup, restore, `rm`, or `rmdir` path.

1. **Confirm the account that runs Hermes** equals `CROTON_USER`:

   ```sh
   id -un
   ```

2. **Confirm every exact planned path is symlink-free.** Parents and the root
   first, then each planned component that already exists. Only these explicitly
   named paths are inspected; no directory is listed or enumerated:

   ```sh
   namei -om "$CROTON_ROOT"
   for path in \
     "$CROTON_ROOT" "$CROTON_ROOT/bin" "$CROTON_ROOT/config" \
     "$CROTON_ROOT/trust" "$CROTON_ROOT/rollback" \
     "$CROTON_ROOT/rollback/$BACKUP_ID" \
     "$CROTON_ROOT/bin/croton-mcp" "$CROTON_ROOT/bin/croton-credentials" \
     "$CROTON_ROOT/config/croton.json" "$CROTON_ROOT/trust/bridge-leaf.pem"
   do
     if [ -e "$path" ] || [ -L "$path" ]; then namei -om "$path"; fi
   done
   ```

   The `-L` arm is required: a broken symlink fails `-e` but is still a stop
   condition. Any `l` component in any `namei` output — including a planned path
   that is itself a symlink, whatever its target — is a stop condition. `namei`
   is required here; do not substitute an incomplete parent-path check. The
   post-staging, root-scoped symlink check is section 5 check 2.

3. **Confirm no Croton process is running.** Match the exact process name so the
   check cannot match its own command line:

   ```sh
   pgrep -a -x croton-mcp || echo "no croton-mcp process"
   ```

4. **Confirm Bridge's endpoint is loopback-only.** Address metadata only; no
   connection is made and nothing authenticates:

   ```sh
   ss -lnt 'sport = :1143'
   ```

   Expect `127.0.0.1:1143` and/or `[::1]:1143`. A wildcard bind (`0.0.0.0` or
   `[::]`) is a stop condition to report, **not** to fix here — Bridge changes
   are gate G4 in a separate task. This says nothing about Croton, which has no
   listening code at all.

5. **Record the Bridge leaf fingerprint** from the staged PEM. This reads a
   local file; it authenticates nothing and does not talk to Bridge:

   The first command quietly enforces Croton's byte-zero certificate header;
   any non-zero status is a stop condition:

   ```sh
   LC_ALL=C head -n 1 -- "$STAGED_LEAF_PEM" | LC_ALL=C grep -qx -- '-----BEGIN CERTIFICATE-----'
   openssl x509 -in "$STAGED_LEAF_PEM" -noout -fingerprint -sha256
   openssl x509 -in "$STAGED_LEAF_PEM" -noout -checkend 0
   grep -c -- "-----BEGIN CERTIFICATE-----" "$STAGED_LEAF_PEM"
   openssl x509 -in "$STAGED_LEAF_PEM" -noout -ext basicConstraints
   ```

   `-noout` keeps the certificate body unprinted. Expect a block count of `1`
   and `CA:FALSE`.

6. **Record the candidate hashes** (building is not part of this runbook):

   ```sh
   sha256sum "$STAGED_BINARY" "$STAGED_HELPER"
   git -C /srv/dev/croton-mcp rev-parse HEAD
   ```

Never run `croton-credentials` by hand, and never pipe it into a file, a log, a
pager, or an example. It is invoked only by Croton, which gives it empty stdin,
discards stderr, bounds its stdout, and passes a small allowlisted environment.

## 4. Staged tasks (documented, not performed)

### Task 4B — host writes and credential enrollment

> **STOP — gate G1.** No command below runs until a human explicitly approves
> host writes, naming the exact paths under `$CROTON_ROOT` to be written.
> Preflight must have passed with no stop condition.

4B stages **all** approved regular artifacts under the root. It does not execute
the helper, does not start Croton, and does not register anything with Hermes.

1. Create the tree owned by `CROTON_USER:CROTON_GROUP`, mode `0750`:
   `$CROTON_ROOT`, `bin`, `config`, `trust`, `rollback`.
2. Build the **per-target pre-deployment manifest** before writing anything. The
   install may be mixed — some targets present, some absent — so each of these
   four final artifact paths gets its own independent entry:

   - `bin/croton-mcp`
   - `bin/croton-credentials`
   - `config/croton.json`
   - `trust/bridge-leaf.pem`

   For each path, record `existed` or `absent`. If `existed`, record its
   pre-deployment SHA-256, file type, owner, and mode, and back up that exact
   artifact into `"$CROTON_ROOT/rollback/$BACKUP_ID"` under its own name,
   preserving mode and owner. If `absent`, record `absent` — there is nothing to
   back up for that path, and rollback will treat it as G1-created. Record
   `BACKUP_ID` and the full manifest in the evidence log; never record file
   contents, only metadata and hashes.
3. Install `bin/croton-mcp` from `STAGED_BINARY`; owner as above, mode `0750`.
4. Install `bin/croton-credentials` from `STAGED_HELPER`; owner as above, mode
   `0700`. This installs helper **code** only — no secret is created, read, or
   passed here, and the helper is not executed.
5. Install `trust/bridge-leaf.pem` from `STAGED_LEAF_PEM`; owner as above, mode
   `0600`.
6. Write `config/croton.json` from the reviewed shape in section 2; owner as
   above, mode `0600`, written under a restrictive umask so it is never briefly
   group- or world-readable.
7. Verify with section 5 checks 1–5 (metadata only; the helper stays unexecuted).

> **STOP — gate G2.** Credential/secret enrollment is a separate approval from
> G1. Installing helper code is not handling secrets; this step is.

Enrollment or provisioning of the credential material the helper will later
return is performed by Ko, using approved secret tooling, outside this runbook.
No credential value or helper output is exposed to any agent, prompt, shell
history, log file, evidence artifact, or Git. The helper's contract is: emit
exactly one JSON object on stdout with non-empty string `username` and
`password` fields, and nothing else. It must be idempotent and side-effect free,
since one accepted transport replay may invoke it a second time.

### Task 4C — Hermes registration and catalog discovery

> **STOP — gate G3.** `hermes mcp add` **changes the live Hermes profile**, and
> `hermes mcp test` **starts the Croton process** for catalog discovery. Both
> need approval separate from G1 and G2 for that reason alone.

Catalog-only `add`/`test`/`list` does **not** invoke the credential helper, does
not authenticate, does not dial Bridge, and does not access mail. Constructing
the Bridge adapter only validates configuration; the dial, the helper execution,
and the authentication happen lazily inside the session the adapter opens, and a
session is opened only when a Croton mail tool operation runs. That first
execution belongs to Task 4D under its own gate G5.

4C assumes 4B was completed and accepted. It installs no files: its entire scope
is the approved Hermes profile registration plus catalog discovery. Registration
is exactly the command documented in
[Hermes registration](MCP.md#hermes-registration); `--args` must come last, and
everything after it is passed to Croton:

```sh
hermes mcp add croton --connect-timeout 60 \
  --command /srv/agents/croton/bin/croton-mcp \
  --args --config /srv/agents/croton/config/croton.json
```

No `--env` and no secrets on the command line. To rehearse without touching the
live profile, set `HERMES_HOME` to a fresh `mktemp -d` directory first — that
still starts Croton, so it is still behind G3.

Verification is catalog-only:

```sh
hermes mcp test croton
hermes mcp list
```

`hermes mcp test croton` reports the six **raw** tool names Croton advertises:

- `list_folders`
- `search_mail`
- `get_message`
- `get_thread`
- `list_attachments`
- `select_digest_candidates`

`hermes mcp list` confirms the `croton` server is registered and enabled. Neither
command is expected to print `mcp__`-prefixed names. The model-facing mapping is
deterministic: Hermes prefixes each raw name with the server name, giving the six
`mcp__croton__*` names documented in [Hermes registration](MCP.md#hermes-registration)
and proven by the merged integration test. For evidence, record the six raw names
observed plus that documented prefixed mapping; do not invent a CLI to print the
prefixed names, and do not send an LLM prompt or tool call to elicit them.

Also confirm no resources and no prompts are advertised. **Do not invoke any
Croton mail tool in 4C.**

### Task 4D — first live read-only tool call

> **STOP — gate G5.** A live tool call touches the mailbox. It is also the point
> at which Croton first executes the credential helper and first opens the
> outbound Bridge TLS connection and authenticates — none of which happened in
> 4C. It needs its own separately named approval identifying the single tool and
> its arguments in advance. Successful registration in 4C does not authorize it.

One bounded, read-only invocation (`list_folders` is the smallest). Record only
the outcome and counts — never folder names, subjects, addresses, bodies,
identifiers, protocol frames, or unredacted stderr.

### Bridge and service changes

Never performed by this runbook. Bridge login, Bridge configuration, and any
service restart are gate G4 and belong to a separate, separately approved task.
No `systemctl`, restart, firewall, or exposure command appears here by design.

## 5. Verification commands

All print only hashes, modes, owners, paths, or process/catalog metadata.

1. **Modes, owners, and file types** (no contents):

   ```sh
   stat -c '%n %F %U:%G %a' \
     "$CROTON_ROOT" "$CROTON_ROOT/bin" "$CROTON_ROOT/config" \
     "$CROTON_ROOT/trust" "$CROTON_ROOT/bin/croton-mcp" \
     "$CROTON_ROOT/bin/croton-credentials" \
     "$CROTON_ROOT/config/croton.json" "$CROTON_ROOT/trust/bridge-leaf.pem"
   ```

   Expect `0750` directories and binary, `0700` helper, `0600` config and trust
   file, one consistent `CROTON_USER:CROTON_GROUP`, and `regular file` for all
   four files.

2. **No symlinks under the root** (scoped to `$CROTON_ROOT` only):

   ```sh
   find "$CROTON_ROOT" -type l -print
   ```

   Expect no output.

3. **Binary and helper integrity**:

   ```sh
   sha256sum "$CROTON_ROOT/bin/croton-mcp" "$CROTON_ROOT/bin/croton-credentials"
   ```

   Must equal the candidate hashes recorded in preflight step 6.

4. **Config size bound, without printing contents**:

   ```sh
   stat -c '%s' "$CROTON_ROOT/config/croton.json"
   ```

   Expect at most 65536. Do not `cat`, `jq`, or `grep` the file: it names the
   helper path and may name trust material.

5. **Trust material fingerprint** (reads a local file, authenticates nothing):

   ```sh
   openssl x509 -in "$CROTON_ROOT/trust/bridge-leaf.pem" -noout -fingerprint -sha256
   ```

   For an SPKI-pin deployment, the equivalent evidence is the SPKI digest from
   the same PEM:

   ```sh
   openssl x509 -in "$CROTON_ROOT/trust/bridge-leaf.pem" -noout -pubkey \
     | openssl pkey -pubin -outform der \
     | openssl dgst -sha256
   ```

6. **Posture**:

   ```sh
   ss -lnt 'sport = :1143'
   pgrep -a -x croton-mcp || echo "no croton-mcp process"
   ```

   Bridge must be bound to loopback only. Croton runs only as a Hermes stdio
   child, on demand, so no persistent process is expected between calls.

7. **Catalog metadata**: `hermes mcp test croton` (six raw names, no resources,
   no prompts) and `hermes mcp list` (server registered and enabled).

Prohibited in verification: anything that prints the config file, executes or
captures the credential helper, dumps protocol frames, authenticates to Bridge
(`openssl s_client`, `curl`, IMAP clients), or copies stderr unredacted.

## 6. Rollback

Ordered so Croton can no longer be started before its files change. Bridge and
unrelated host state are never touched.

> **STOP — gate G6.** Approval required before changing the Hermes profile.

1. Remove the registration, then confirm:

   ```sh
   hermes mcp remove croton
   hermes mcp list
   ```

   `hermes mcp remove` asks for interactive confirmation. Afterwards the
   `croton` registration must be gone.

2. Prove no Croton child process survives:

   ```sh
   pgrep -a -x croton-mcp || echo "no croton-mcp process"
   ```

   Expect the "no process" line. If one persists, stop and escalate rather than
   killing unrelated processes.

> **STOP — gate G7.** Approval required before restoring or deleting any file,
> and it must name the exact `BACKUP_ID` and the exact paths.

3. Process the pre-deployment manifest **one target at a time**, independently.
   The install may have been mixed, so the manifest entry — not the install as a
   whole — decides each path's treatment:

   - *Entry says `existed`:* copy that artifact back from its named backup in
     `"$CROTON_ROOT/rollback/$BACKUP_ID"` to its exact original path, restoring
     the recorded owner and mode, then verify its recorded pre-deployment
     SHA-256.
   - *Entry says `absent`:* that target was created by G1, so remove exactly that
     one final target path, by explicit literal path, one at a time — for
     example `rm -i -- "$CROTON_ROOT/config/croton.json"`. Remove no directory
     yet.

   Never use `rm -rf`, never use a wildcard, never recurse into a directory.
   Anything absent from the manifest is out of scope and left untouched. Retain
   every backup file through step 4.

4. Rollback-specific verification. Do **not** rerun the deployment checks: they
   assert the deployed state, which is exactly what rollback undid.

   - Registration: `hermes mcp list` only, confirming `croton` is absent. Never
     run `hermes mcp test` after removal.
   - Each `absent` manifest target must now be gone, including as a dangling
     symlink. Run only the line for each target whose manifest entry is
     `absent`; the four possible exact checks are:

     ```sh
     test ! -e "$CROTON_ROOT/bin/croton-mcp" && test ! -L "$CROTON_ROOT/bin/croton-mcp"
     test ! -e "$CROTON_ROOT/bin/croton-credentials" && test ! -L "$CROTON_ROOT/bin/croton-credentials"
     test ! -e "$CROTON_ROOT/config/croton.json" && test ! -L "$CROTON_ROOT/config/croton.json"
     test ! -e "$CROTON_ROOT/trust/bridge-leaf.pem" && test ! -L "$CROTON_ROOT/trust/bridge-leaf.pem"
     ```

     A target whose manifest entry is `existed` must remain present and is
     checked below instead.
   - Each `existed` manifest target: compare its current SHA-256, file type,
     owner, and mode to the **recorded pre-deployment evidence** — not to the
     candidate hash, which is what rollback removed.
   - Process posture: `pgrep -a -x croton-mcp`.
   - If `$CROTON_ROOT` still exists, rerun the root-scoped symlink check
     `find "$CROTON_ROOT" -type l -print` and the planned-path
     `namei -om "$path"` check from preflight step 2; record the outcome.
   - Bridge loopback posture (`ss -lnt 'sport = :1143'`) may be rechecked as
     context. Bridge itself is untouched by rollback.

   Record the resulting state per target: absent, or restored to its
   pre-deployment hash, type, owner, and mode.

   If any target fails verification, stop and retain every backup file.

5. After step 4 passes for **every** target, gate G7 permits deleting the exact
   backup file for each entry marked `existed`. Run only the matching lines:

   ```sh
   rm -i -- "$CROTON_ROOT/rollback/$BACKUP_ID/croton-mcp"
   rm -i -- "$CROTON_ROOT/rollback/$BACKUP_ID/croton-credentials"
   rm -i -- "$CROTON_ROOT/rollback/$BACKUP_ID/croton.json"
   rm -i -- "$CROTON_ROOT/rollback/$BACKUP_ID/bridge-leaf.pem"
   ```

   Then remove the now-empty backup directory:

   ```sh
   rmdir -- "$CROTON_ROOT/rollback/$BACKUP_ID"
   ```

   If `rmdir` finds unexpected content, stop rather than forcing removal. Use
   further `rmdir` commands only for exact directories recorded as created by
   this deployment.

6. Bridge, its configuration, its credentials, and all other `/srv/agents`
   content are untouched at every step. If rollback appears to need a Bridge or
   service change, stop: that is gate G4 and a separate task.

## 7. Redacted evidence checklist

Record exactly these per deployment or rollback attempt.

- [ ] Candidate commit SHA and merge commit SHA.
- [ ] `sha256` of the installed binary and helper, and confirmation each matches
      its pre-install candidate hash.
- [ ] Path / type / owner / mode table from the `stat -c '%n %F %U:%G %a'` check.
- [ ] Result of the root-scoped symlink check (expected: empty).
- [ ] Bridge loopback address and port as observed (`127.0.0.1:1143` and/or
      `[::1]:1143`), plus confirmation that no `croton-mcp` process persists.
- [ ] Bridge leaf SHA-256 fingerprint, or the SPKI SHA-256 pin digest.
- [ ] The six raw tool names observed from `hermes mcp test croton`, their
      documented `mcp__croton__*` mapping, that `hermes mcp list` shows the
      server enabled, and that no resources or prompts are present.
- [ ] `BACKUP_ID` and the four-entry pre-deployment manifest: each target marked
      `existed` with its backup path/hash/type/owner/mode, or marked `absent`.
- [ ] Which approval gates were granted, by whom, and when.
- [ ] Rollback outcome: registration removed, no Croton process, files restored
      or deleted by exact path, re-verification result, and the backup cleanup
      outcome (exact backups deleted after verification, or retained on
      failure), plus the result of each `rmdir`.

Never store, paste, or attach: credential-helper output, any credential or
mail-account identifier, the contents of `config/croton.json`, mailbox data of
any kind, protocol frames, or unredacted stderr. Croton's audit lines are
limited to the fields `event`, `tool`, `outcome`, `code`, `truncated`; nothing
beyond that vocabulary belongs in evidence.

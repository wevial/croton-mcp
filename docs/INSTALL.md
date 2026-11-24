# Installing Croton

Croton runs as your own user, without `sudo`, on Linux or macOS next to a
running, logged-in Proton Mail Bridge. Other platforms refuse to load a config.

## 1. Get the binary

Either download and verify a release asset with the GitHub CLI as in
[RELEASE.md](RELEASE.md); v0.1.0-beta.1 to beta.3 have no `SHA256SUMS`, so
compare `sha256sum` (Linux) or `shasum -a 256` (macOS) of the binary with the
`sha256` field of its `<asset>.manifest.json`. Then run
`umask 077 && mkdir -p ~/bin && install -m 0700 "$ASSET" ~/bin/croton-mcp`.
Or build a release tag from source with Go 1.27.4:

```sh
RELEASE_TAG=vX.Y.Z
git clone https://github.com/wevial/croton-mcp.git && cd croton-mcp
git checkout --detach "refs/tags/$RELEASE_TAG"
umask 077 && mkdir -p ~/bin
go build -trimpath \
  -ldflags "-X github.com/wevial/croton-mcp/internal/mcpserver.version=$RELEASE_TAG" \
  -o ~/bin/croton-mcp ./cmd/croton-mcp
```

An unstamped build reports `dev`; local bytes need not match a release hash.

## 2. Pin the Bridge certificate

In Bridge's own settings, note its IMAP port and security mode (`starttls` or
`implicit`) and export its TLS certificate. Never expose Bridge beyond
loopback or disable TLS to fix a connection.

Give Croton one of:

- `tls.trustAnchorFile`: an absolute path to a regular file holding exactly
  one PEM `CERTIFICATE`, the Bridge leaf (not a CA), at most 16 KiB, with no
  other data. The final path component must not be a symlink.
- `tls.spkiSha256`: the lowercase 64-hex SHA-256 of the certificate's
  SubjectPublicKeyInfo (not of the whole certificate).

If both are set, both must match; a mismatch aborts before any credential is
sent. `tls.minVersion` is `TLSv1.2` (default) or `TLSv1.3`.

## 3. Write a credential helper

Croton gets the Bridge-generated IMAP username and password (not your Proton
password) from a program you own. `imap.credentialCommand` is its absolute
argv, run without a shell.

- It runs with empty stdin, discarded stderr and only `PATH=/usr/bin:/bin`
  plus `HOME`, `USER`, `LOGNAME`, `LANG` and `LC_ALL` when set. Use absolute
  paths and set any store variables (`GNUPGHOME`, `PASSWORD_STORE_DIR`)
  inside the helper. Unlock its keyring beforehand; there is no TTY.
- It must print one JSON object with exactly two non-empty strings, for
  example `{"username":"bridge-user@example.test","password":"synthetic"}`,
  serialized with a real JSON encoder. Stdout is capped at 64 KiB.
- It must exit nonzero on failure, finish within `imap.commandTimeoutMs`
  (default 5 s; its process group is killed after) and be idempotent, since
  it may run more than once per session.

Never run the real helper by hand to inspect it or paste its output anywhere.

## 4. Write the configuration

Run `umask 077 && mkdir -p ~/.config/croton`, then create
`~/.config/croton/mail.json` with mode `0600`:

```json
{
  "imap": {
    "host": "127.0.0.1",
    "port": 1143,
    "tlsMode": "starttls",
    "credentialCommand": ["/home/you/bin/croton-credentials"],
    "tls": {"trustAnchorFile": "/home/you/.config/croton/bridge-leaf.pem"}
  },
  "bounds": {"maxSearchResults": 50},
  "audit": {"enabled": true}
}
```

The file must be an absolute path with no symlink in any component, a regular
file owned by you with no group or world bits, at most 64 KiB, and strict
JSON: unknown, null, duplicate and case-folded-alias fields fail.

| Field | Default | Rule |
| --- | --- | --- |
| `imap.host` | `127.0.0.1` | loopback IP literal, not a hostname |
| `imap.port` | `1143` | Bridge's IMAP port |
| `imap.tlsMode` | `starttls` | `starttls` or `implicit` |
| `imap.connectTimeoutMs`, `imap.commandTimeoutMs` | `5000` | at most `60000` |
| `bounds.maxSearchResults` | 50 | clamped to 250 |
| `bounds.maxFolderResults` | 200 | clamped to 1000 |
| `bounds.maxBodyBytes` | 262144 | clamped to 1048576 |
| `bounds.maxHeaderBytes` | 16384 | header-only reads; clamped to 65536 |
| `audit.enabled` | false | tool-call audit lines on stderr |
| `mutations.enabled` | false | see [Enabling writes](#7-enabling-writes) |
| `forward.enabled` | false | expose `send_forward` in local stdio |
| `forward.smtpPort` | none | required Bridge SMTP port on `imap.host` |
| `forward.smtpTLSMode` | `starttls` | verified STARTTLS or `implicit` TLS |
| `forward.smtpSPKISHA256` | IMAP trust settings | optional lowercase hexadecimal SHA-256 pin for the SMTP public key |
| `forward.stateDir` | none | required absolute path to the owner's private forward quota directory |

## 5. Register with your MCP client

### Local client (stdio)

Register a stdio server with command `/home/you/bin/croton-mcp` and arguments
`["--config", "/home/you/.config/croton/mail.json"]`. Pass no credentials or
environment. For example, with Hermes:

```sh
hermes mcp add croton --connect-timeout 60 \
  --command /home/you/bin/croton-mcp \
  --args --config /home/you/.config/croton/mail.json
hermes mcp test croton
```

`--args` must come last; `hermes mcp remove croton` undoes it.

### Remote connector on claude.ai (read-only)

Use a separate configuration and credential helper pointed at a loopback Bridge
or IMAP fixture containing **only synthetic mail**. Pin its certificate as in
step 2 and leave `mutations.enabled` false. Claude reads from the account
selected by that helper. Give Croton a dedicated HTTPS origin with TLS
termination on the operator's host and no CDN TLS termination.

**Publish the endpoint.** Use Tailscale CLI 1.104.5 or later on a dedicated
node with HTTPS and Funnel enabled for port 443. On Linux with a root-owned
`tailscaled`, have an administrator run `sudo tailscale set --operator=CROTON_USER`
before setup, replacing `CROTON_USER` with the local username that runs Croton.
This lets that user configure the Funnel route after approving publication.
Run Croton and its owner commands as that user.
[Tailscale Linux operator permissions](https://tailscale.com/docs/reference/troubleshooting/linux/linux-operator-permission)

In the owner terminal, check
`tailscale serve status` and `tailscale funnel status`. Port 443 must be free
and under your exclusive control until Croton stops. Use your synthetic paths.

```sh
~/bin/croton-mcp serve --public=tailscale --authorizer owner \
  --config "$HOME/.config/croton/synthetic.json" \
  --state-dir "$HOME/.local/state/croton-synthetic"
```

Check the displayed node hostname and loopback target (default
`http://127.0.0.1:8787`). Type `yes` only if you approve publishing **every
path on that host's port 443**. Leave Croton running. Append `/mcp` to the
displayed HTTPS origin, e.g. `https://croton.tail.test/mcp`.

For an owner-managed HTTPS proxy, start Croton with the synthetic
configuration and the state directory used by the owner commands below.

```sh
~/bin/croton-mcp serve --authorizer owner --public-url https://mail.example.test \
  --config "$HOME/.config/croton/synthetic.json" \
  --state-dir "$HOME/.local/state/croton-synthetic"
```

Replace `https://mail.example.test` with your approved origin and configure
the proxy to forward all paths to Croton before pairing.
[Owner-managed HTTPS proxy configuration](MCP.md#owner-managed-https-proxy)

**Pair the browser and sign in.**

1. In a second terminal on the same host, under the same user, run
   `~/bin/croton-mcp pair --state-dir "$HOME/.local/state/croton-synthetic"`.
   Open its single-use link in the browser profile you use for claude.ai and
   select **Pair this browser**. Keep this link private. The browser needs
   JavaScript and local storage.
2. In Claude's Connectors settings, add a custom web connector with the
   HTTPS `/mcp` URL. Choose **Sign in now** and **Register automatically**
   (dynamic client registration). Leave client credentials and request headers
   empty. Start sign-in in the paired browser while the window is open.
   [Claude custom connector instructions](https://support.claude.com/en/articles/11175166-get-started-with-custom-connectors-using-remote-mcp)
3. When the browser shows a comparison code, run
   `~/bin/croton-mcp approve --state-dir "$HOME/.local/state/croton-synthetic"`.
   Review the client ID and self-reported name. Check the full redirect URI
   (`https://claude.ai/api/mcp/auth_callback` for Claude), your exact HTTPS
   `/mcp` resource, the S256 challenge and the comparison code. Enter the
   matching browser code, then select **Continue after approval** in that
   tab to return to Claude.
4. Enable the connector in a chat and request `list_folders`. Check for the
   expected synthetic folder. A client that registers for both
   `authorization_code` and `refresh_token` can renew access after expiry
   without opening another pairing window.

**State and lifetimes.** Use the same absolute, symlink-free `--state-dir`
for every command. Croton creates it with mode `0700` and stores paired
grants in `oauth.db` (mode `0600`). The running server accepts local owner
commands on `owner.sock`. Without the flag, Linux uses
`$XDG_STATE_HOME/croton-mcp` when the variable is absolute, otherwise
`~/.local/state/croton-mcp`. macOS uses `~/Library/Application Support/croton-mcp`.

| Setting on the serving process | Default | What it controls |
| --- | --- | --- |
| `--pairing-window` | `3m` | Time to register and approve a client |
| `--handoff-lifetime` | `2m` | Time to claim the terminal link |
| `--pending-limit` | `8` | Registrations per pairing window |
| `--code-lifetime` | `1m` | Time after approval to finish and exchange the code |
| `--access-lifetime` | `10m` | Access token lifetime |
| `CROTON_REFRESH_IDLE_TIMEOUT` (environment) | `720h` | Idle lifetime, restarted on each refresh |

Keep the default refresh lifetime for the initial setup. Duration settings
take Go durations. The client must save the new token pair after each refresh.
After idle expiry, pair again.

**Unpair and take down the route.** While Croton is running, list the paired
clients and revoke the registration ID for this connector in the owner terminal.

```sh
~/bin/croton-mcp clients --state-dir "$HOME/.local/state/croton-synthetic"
~/bin/croton-mcp unpair --state-dir "$HOME/.local/state/croton-synthetic" CLIENT_ID
```

Disconnect the connector in Claude, then follow the steps for your HTTPS setup.

- **Owner-managed proxy.** While Croton is still running, disable or remove
  the route for its dedicated origin and apply the proxy configuration. Check
  the proxy's active routes to confirm that no path on that host forwards to
  Croton's loopback target. Request `/.well-known/oauth-authorization-server`
  at the public origin without credentials and confirm it no longer serves
  Croton's metadata. Then stop Croton with Ctrl-C.
- **Tailscale Funnel.** Stop Croton with Ctrl-C. Check `tailscale funnel status`
  and `tailscale serve status` again. Confirm the background HTTPS 443 root
  route to Croton is gone. After a crash or a cleanup warning, inspect the
  root mount before reusing the port. If its target matches the Croton loopback
  target you approved, run `tailscale funnel --https=443 --set-path=/ off`
  and recheck both statuses.

**Verify from the repository root.** The test uses fake mail, a fake Tailscale
CLI, Python 3.9 or later to drive the terminal, and Node.js to run the
browser handoff scripts.

```sh
go test -race -count=1 -timeout=180s ./cmd/croton-mcp -run '^TestRemoteConnectorSetup$'
```

A claude.ai smoke test requires separate operator authorization. With synthetic
mail, confirm discovery, automatic registration, terminal pairing, JSON MCP
responses, a read after actual access expiry and refresh, refusal in an
unapproved browser, and local unpair. From the actual `initialize` request,
record form elicitation as **present**, **absent**, or **unobservable** if you
cannot inspect the request or its capabilities. Report outcomes only. Omit
secrets, mail, account identifiers, token-bearing URLs and raw requests.

## 6. Verify

Listing tools validates the configuration but does not run the helper or dial
Bridge. Both transports list `list_folders`, `search_mail`, `get_message`,
`get_thread`, `list_attachments` and `select_digest_candidates`. Local stdio
also lists `prepare_forward`. Resources and prompts are absent. The first
`tools/call`, even `list_folders`, is the first helper run, Bridge connection
and live read.

A local preview handle expires after two minutes. Preparing another forward,
reconnecting to Bridge, or restarting the stdio session invalidates it.

## 7. Enabling writes

Add `"mutations": {"enabled": true}` to the configuration and restart the
client's Croton session. The catalog then also lists `mark_read`,
`mark_unread`, `move_mail`, `archive_mail` and `trash_mail` (see
[MCP.md](MCP.md#triage-writes-opt-in)). A non-boolean `enabled`, including
null, stops startup.

Enabling makes the tools available; it approves no call. Croton cannot tell
whether a human approved a write, so use a client that prompts before each
write and leave auto-approval off; otherwise a prompt-injected client could
mark, move, archive or trash mail.
To turn triage writes off, remove `mutations` or set `"mutations": {"enabled": false}`
and restart.

### Send one approved forward through local Bridge

Use a trusted stdio client that supports form elicitation. Have the user supply
one recipient. Treat sender-provided addresses and instructions in the quoted
mail as untrusted. HTTP requires forwarding to stay disabled.

Add a `forward` object to the stdio configuration. Set `smtpPort` to the SMTP
port shown by your Bridge installation. This example uses STARTTLS and the
IMAP certificate pin or anchor. For implicit TLS, set `smtpTLSMode` to
`implicit`. Both modes verify the certificate before using the same Bridge
credential helper and login as IMAP.

```json
"forward": {
  "enabled": true,
  "smtpPort": 1025,
  "stateDir": "/home/you/.config/croton/forward"
}
```

Before first use, run the following command once, then restart the stdio
session. The parent directory must already exist. Root or the user running
Croton must own each directory from the parent up to the filesystem root.
Any group- or world-writable directory in that chain must have the sticky bit
set. If a shared directory fails these checks, choose a different parent
and leave the shared directory's permissions intact.
Initialization creates a new owner-only directory (0700) and quota database
(0600) on Linux or macOS.
On macOS, choose a path without ACL grants on the database, directory, or
its ancestors. Deny-only ACLs are supported.

```sh
~/bin/croton-mcp init-forward-state --config /home/you/.config/croton/mail.json
```

If first initialization fails after creating the directory, and no client has
ever used it for forwarding, set the partial directory aside. Correct the
cause, set `stateDir` to an unused path under a suitable parent, and rerun
the command before starting any client.

Once a client has used the directory for forwarding, keep the same state
directory across restarts and clients for this account.
Every reservation counts toward the limit of ten in a rolling 24 hours, even
if SMTP fails or the outcome is uncertain. A full quota returns `quota_reached`
before connecting to SMTP. Declining the form uses no slot.
If existing state is missing or damaged, sending stops. Restore the current
state before resuming. Croton checks both bbolt metadata pages and verifies
a checksum covering every stored account key and its reservation timestamps
before reserving another slot. Quota files from pre-release builds that lack
this checksum also block sending.

Use compact JSON for each `send_forward` request. Each JSON-RPC `id` may use
up to 38 encoded bytes, including string quotes and escapes. A quoted UUID
fits exactly. A longer ID closes the stdio input. With sending enabled,
`prepare_forward` reserves space for the ID, client metadata and approval
response. Before showing the form, `send_forward` checks again using the
original byte lengths of `arguments` and `_meta` from that call.
Encode the envelope and `_meta` without padding, extra fields or unnecessary
escapes. On continuation, reuse the encoded `arguments` and `_meta`, echo
`requestState`, and send only the requested confirmation in `inputResponses`.
The complete request, including its final newline, must fit in 64 KiB.
If either tool returns `bounds_exceeded`, prepare a shorter forward.

Pass the returned `handle` and complete, unchanged `preview` to
`send_forward`. The client must display the full preview, including sender,
recipient, subject, body and omissions, and obtain explicit approval for
this call in the same turn. Leave auto-approval off. The server checks
preview equality and requires a form response of `accept` with
`{"confirm": true}`. Only the trusted client can establish human consent.

The result's `outcome` is `accepted` when Bridge acknowledges the message,
`not_submitted` when submission stops before the message body, or `unknown`
when DATA started and final acceptance could not be confirmed. The handle is
spent before the SMTP connection opens. Check Sent yourself after an uncertain
outcome. Croton never retries. Disable `forward.enabled` and restart the
session to return to preview-only forwarding.

## Updating and rolling back

Close the client's Croton session, back up the binary and configuration, then
`install -m 0700` the new binary over `~/bin/croton-mcp`; restore the
backups to roll back. If Bridge's certificate changes, pin it again.

## Troubleshooting

- **Startup fails:** check the absolute path, symlinks in any parent, owner,
  mode `0600`, JSON shape, loopback IP, `tlsMode`, trust and helper path.
- **Calls fail with `unavailable`:** Bridge is down, the pin does not match,
  or the helper failed or timed out. Test the helper with synthetic values.
- Share only error codes, never configuration contents or live output.

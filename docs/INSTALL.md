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
node with HTTPS and Funnel enabled for port 443. In the owner terminal, check
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
displayed HTTPS origin, e.g. `https://croton.tail.test/mcp`. For a dedicated
HTTPS reverse proxy on the host, follow the [owner-managed proxy setup](MCP.md#owner-managed-https-proxy)
before pairing.

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

Disconnect the connector in Claude and stop the serving process with Ctrl-C.
Check `tailscale funnel status` and `tailscale serve status` again. Confirm
the background HTTPS 443 root route to Croton is gone. After a crash or a
cleanup warning, inspect the current target before reusing the port. If
you verify the remaining route is yours, run `tailscale funnel --https=443 off`
and recheck both statuses. Leave unrelated routes in place.

**Verify from the repository root.** The test uses fake mail, a fake Tailscale
CLI and Python 3 to drive the terminal.

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
Bridge. Expect exactly `list_folders`, `search_mail`, `get_message`,
`get_thread`, `list_attachments` and `select_digest_candidates`, and no
resources or prompts. The first `tools/call`, even `list_folders`, is the
first helper run, Bridge connection and live read.

## 7. Enabling writes

Add `"mutations": {"enabled": true}` to the configuration and restart the
client's Croton session. The catalog then also lists `mark_read`,
`mark_unread`, `move_mail`, `archive_mail` and `trash_mail` (see
[MCP.md](MCP.md#triage-writes-opt-in)). A non-boolean `enabled`, including
null, stops startup.

Enabling makes the tools available; it approves no call. Croton cannot tell
whether a human approved a write, so use a client that prompts before each
write and leave auto-approval off; otherwise a prompt-injected client could
mark, move, archive or trash mail. Nothing is sent or permanently deleted.
To turn writes off, remove `mutations` or set `"mutations": {"enabled": false}`
and restart.

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

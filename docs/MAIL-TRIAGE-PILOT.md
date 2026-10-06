# Mail triage pilot runbook

This runbook describes a later, bounded M1 pilot of the opt-in Mail triage
tools on the operator's own Proton account. It is a non-executing procedure:
this document grants no installation, enablement, account access or live-write
authority, and nothing in it has been run. Each gate below needs its own
separate, explicit operator approval at the time it is performed.

Synthetic triage proofs in the repository do not establish live Proton
behavior or usability. The pilot exists to observe both on a small set of
self-sent test messages before anyone considers real-inbox authorization,
which this runbook does not grant.

The [Mail triage design](design/0005-mail-triage.md) and the
[MCP contract](MCP.md#triage-tools) remain authoritative for tool behavior.
The pilot's confirmation rule is intentionally stricter than the design's
direct-request rule. All paths, folder names and identities below are role
placeholders; keep real values private and never paste them into tickets,
agent conversations or the public evidence template.

## Release selection prerequisite

The pilot is blocked until a suitable tagged release is published. Before
install approval, the operator records privately:

1. An operator-selected published git tag in the public repository.
2. A matching GitHub release for that same tag. A tag without a matching
   published release, or a release without the tag, stops the pilot.
3. The resolved full revision: the full 40-hex commit SHA the tag points to,
   reviewed by the maintainer. Use it as `REVIEWED_REVISION` in the source
   build; never build a moving branch or an unresolved tag name.
4. The staged-manifest SHA-256: build the candidate with the installation
   guide's staging helper into a new candidate directory, outside the install
   layout, and record the `sha256` from its `manifest.json`, whose `revision`
   must equal the resolved full revision.
5. A staged-binary checksum: compute the SHA-256 of the candidate directory's
   `croton-mcp` with the platform's local hash utility and compare it with the
   manifest SHA-256. A mismatch stops the pilot.

Staging a candidate installs nothing and changes no installed executable. The
independent installed-binary checksum cannot exist yet: a first install has no
executable, and an upgrade's existing executable is the previous release. It is
verified after installation instead, as described under Installation.

Do not download a release binary; build from the resolved revision. Checksums
provide integrity, not signatures or provenance attestation.

## Authorization gates

The pilot has four gates, performed in order. Approval of one gate grants
nothing for the next, and each gate stops on any failure:

1. **Install approval:** the operator explicitly approves installing the
   selected release with `mutations` absent.
2. **Rollback-rehearsal approval:** the operator explicitly approves rehearsing
   rollback and reinstalling the selected release.
3. **Enablement approval:** the operator explicitly approves setting
   `mutations.enabled` to true in the local configuration.
4. **Bounded live read/write approval:** the operator explicitly authorizes the
   named bounded reads of the pilot folders, and every write additionally needs
   per-payload confirmation of the displayed exact payload.

## Installation

Install by following the conservative
[user-owned installation guide](USER-INSTALL.md) exactly, using the resolved
full revision as the reviewed revision. This runbook does not rewrite or relax
that guide: its source build, staging, layout, configuration, credential-helper
and update procedures apply unchanged, and its Distribution limitations still
hold. Leave `mutations` absent for the install gate.

After the authorized installation, and before any later gate, verify the
independent installed-binary checksum: compute the SHA-256 of the final
installed executable with the platform's local hash utility and compare it
with the manifest SHA-256. A mismatch stops the pilot; keep launches blocked
and restore the prior set with the installation guide's rollback step.

## Catalog and rollback rehearsal

Use catalog-only verification from the installation guide, restricted to
initialization and `tools/list` without automatic tool execution.

- With `mutations` absent or false, the new release's disabled catalog is the
  six-tool set: `list_folders`, `search_mail`, `get_message`, `get_thread`,
  `list_attachments` and `select_digest_candidates`.
- After the enablement gate, the enabled catalog is the eleven-tool set: those
  six plus `mark_read`, `mark_unread`, `move_mail`, `archive_mail` and
  `trash_mail`.

Rehearse rollback before enablement with the installation guide's Update and
rollback procedure. The rehearsal restores the complete matched artifact set
recorded before the install, or, for a first install, restores first-install
absence by removing only the exact targets recorded as previously absent.
After restoring, check the catalog against the prior release's own documented
catalog. Do not assume an older binary has eleven tools or any triage tool;
after a first-install rollback there is no Croton catalog to check. Then
reinstall the selected release, verify the independent installed-binary
checksum again against the manifest SHA-256, and repeat the six-tool check.

Only after both installed-binary checks match, enable by staging a reviewed
configuration with `mutations.enabled` set to true through the same update
procedure, then confirm the eleven-tool catalog.
Catalog checks do not invoke the helper, connect to Bridge or read mail.

## Pilot selection and approval

- Own-account scope: use only the operator's own Proton account.
- Send about ten self-sent messages with synthetic subjects and bodies and no
  attachments, personal data or forwarded content.
- Using the Proton app, not Croton, create one initial pilot folder under
  `Folders/` and move the test messages there. Also create one empty pilot
  destination folder under `Folders/` for `move_mail`.
- Real-inbox prohibition: never read from, write to or move into INBOX or any
  folder holding real mail. Only the pilot folders and the `\Archive` and
  `\Trash` holders are in scope, and only for pilot messages.
- Five-UID limit: each triage call carries at most five UIDs, below the
  server's batch maximum of 50.
- Exact payload confirmation: before every call the client displays the exact
  payload, meaning the tool, source mailbox, UIDVALIDITY, ordered UIDs and the
  move destination when present, and the operator explicitly confirms that
  payload. A direct request is not enough, and there is no reusable session
  approval.

Obtain identities only from a separately authorized fresh read of the pilot
folder, such as a bounded `search_mail`, and keep them private. When pilot
messages are in Archive or Trash, which also hold real mail, restrict that read
with a `search_mail` subject filter matching only the synthetic pilot subjects.

## Action procedure

Run each action once, in table order, on pilot messages only. Before and after
each call, privately compare the Proton app state described in its row, and
record only the outcome category in the template below.

| Tool | Pilot payload | Private Proton-app state check |
| --- | --- | --- |
| `mark_read` | 1 to 5 unread UIDs from the pilot folder | Proton app shows those messages read and still in the pilot folder |
| `mark_unread` | 1 to 5 read UIDs from the pilot folder | Proton app shows those messages unread and still in the pilot folder |
| `move_mail` | 1 to 5 UIDs from the pilot folder; `destination` is the exact pilot destination folder under `Folders/`, valid under the existing MCP contract | Proton app shows them in the pilot destination folder and absent from the pilot folder |
| `archive_mail` | 1 to 5 UIDs from the pilot folder; no destination argument | Proton app shows them in Archive and absent from the pilot folder |
| `trash_mail` | 1 to 5 UIDs from the pilot folder; no destination argument | Proton app shows them in Trash, not permanently deleted, and absent from the pilot folder |

Destinations follow the existing MCP contract only: `move_mail` names an
existing selectable `Folders/` entry, while `archive_mail` and `trash_mail`
map to the single `\Archive` or `\Trash` holder.

## Definitive refusal observation

Observe one definitive refusal with a safe source-equals-destination probe: a
single-message `move_mail` call whose destination equals the pilot folder it is
taken from. The client displays that exact payload and the operator gives
explicit payload confirmation before the call. The expected result is
`refused` with no write.

Observe the refusal privately and confirm in the Proton app that the message's
state is unchanged. Do not provoke refusals any other way: no forced fault,
invalid credential, invented UID or mixed batch.

## Unknown outcome and recovery

On any `unknown` or unexpected outcome:

1. STOP: issue no further triage call.
2. No replay: never resend the same payload.
3. Reconcile only through a separately authorized fresh readback of the pilot
   folders and a private Proton-app check.
4. Any further write needs a new approval of a freshly displayed exact payload.

After any MOVE, including `archive_mail` and `trash_mail`, identities are
gone from the source. Obtain fresh destination-source identities, meaning the
destination mailbox, its UIDVALIDITY and its UIDs, from a separately authorized
fresh read before any later action on those messages.

## Trash and cleanup

Retention warning: Proton may empty Trash automatically after its retention
period, so a trashed pilot message may later be permanently deleted by Proton.
No-purge boundary: never purge or empty Trash, permanently delete or expunge
anything during the pilot; Croton has no such tool.

Optional return moves, such as moving pilot messages back to the pilot folder,
are separate freshly identified and approved actions under the rules above.
They are not installation rollback, and installation rollback does not move
mail back.

## Public evidence template

Status: NOT RUN. This template is unfilled and claims no live outcome.

Record only outcome categories from this allowlist: `NOT RUN`, `applied`,
`refused`, `unknown`, `not_attempted`, `matched`, `mismatched` and `stopped`.
Never include credentials, account identifiers, addresses, subjects, folder
names, UIDs, UIDVALIDITY values, message content, screenshots, protocol
captures or unredacted diagnostics. Synthetic-only proof limitation: the
repository's automated witnesses use synthetic fixtures only and do not prove
live success or client consent enforcement.

<!-- mail-triage-pilot-evidence -->
| Step | Outcome | Proton-app state |
| --- | --- | --- |
| `mark_read` | NOT RUN | NOT RUN |
| `mark_unread` | NOT RUN | NOT RUN |
| `move_mail` | NOT RUN | NOT RUN |
| `archive_mail` | NOT RUN | NOT RUN |
| `trash_mail` | NOT RUN | NOT RUN |
| refusal probe | NOT RUN | NOT RUN |
| rollback rehearsal | NOT RUN | NOT RUN |

Usability prompts, answered in a sentence each without private data:

- Was each displayed payload clear enough to confirm?
- Did any confirmation feel redundant or easy to approve by mistake?
- Did results match what the Proton app showed?
- Was any step confusing or slow?

## Structured contract

This block is the machine-checked form of the boundaries above, verified by
`scripts/verify_docs_mail_triage_pilot.py`. It is a documentation witness and
does not enforce anything at runtime.

<!-- mail-triage-pilot-contract -->
```json
{
  "status": "not_run",
  "grants_authority": false,
  "release": {
    "published_git_tag": "operator_selected",
    "matching_github_release": true,
    "full_revision": "resolved_and_reviewed",
    "manifest_sha256": "recorded_from_staged_manifest",
    "staged_binary_sha256": "compared_before_install_approval",
    "installed_binary_sha256": "verified_after_install_and_reinstall_before_enablement",
    "pilot_blocked_until_published": true
  },
  "gates": ["install", "rollback_rehearsal", "local_enablement", "bounded_live_read_write"],
  "installation": "docs/USER-INSTALL.md",
  "catalog": {
    "disabled_tools": 6,
    "enabled_tools": 11,
    "rollback": "prior_release_documented_catalog",
    "first_install_rollback": "restore_absence"
  },
  "scope": {
    "account": "operator_own",
    "messages": "about_ten_self_sent",
    "initial_pilot_folders": 1,
    "max_uids_per_call": 5,
    "confirmation": "each_displayed_exact_payload",
    "session_approval": false,
    "real_inbox": "prohibited"
  },
  "actions": ["mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"],
  "refusal": {
    "probe": "move_mail_source_equals_destination_single_message",
    "confirmation": "explicit_exact_payload",
    "prohibited": ["forced_fault", "invalid_credential", "invented_uid", "mixed_batch"]
  },
  "recovery": ["stop", "no_replay", "separately_authorized_fresh_readback", "new_approval", "fresh_identities_after_move"],
  "cleanup": {
    "trash_retention_warning": true,
    "purge": false,
    "return_moves": "separate_fresh_identified_approved_actions"
  },
  "evidence": {
    "status": "NOT RUN",
    "fields": "outcome_only_allowlist",
    "private_data": "excluded",
    "proof": "synthetic_only"
  }
}
```

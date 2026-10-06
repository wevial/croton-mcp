# Mail label design

This record defines a planned, separately scoped contract for adding and
removing existing Proton Mail labels through Proton Mail Bridge. It is a design
and proof contract only. It registers no tool, changes no configuration schema,
installed configuration or adapter, and enables no live write. Label support is
planned and not shipped.

The [Mail triage design](0005-mail-triage.md) stays the historical and current
record for the shipped triage tools, and its Labels (later) section is retained
unchanged as a guard. The shipped Seen `UID STORE` and native `UID MOVE`
contract is not a generic mutation permission and does not authorize anything
in this record.

`scripts/test_mail_labels_design.py` checks the visible prose of this record
and the README link to it. It is a documentation witness: it cannot prove human
approval, Bridge behavior or the absence of runtime data loss.

## Tag semantics

Proton labels are tags, not folders. A message lives in exactly one folder and
can carry several labels at once; Bridge shows each label as a mailbox under
`Labels/` whose entries are views of messages that remain in their folders.

Adding an existing label preserves the message's source folder, its
source-folder UID and Seen state, and every label it already carries. Adding a
second label to a message that already carries one keeps the first label.

Removing one label preserves the message, its folder and every other label it
carries. Removal changes only membership of the one selected label view.

A label is a tag, not a folder: labeling is never a folder MOVE. No label tool
may be implemented as, or substitute for, `MOVE`, and no folder tool may be
used to add or remove a label.

## Tool and approval contract

The proposed tools are `add_label` and `remove_label`. Both publish
`readOnlyHint: false` and use the existing closed, bounded argument decoding:
unknown, null, duplicate or case-folded-alias fields are rejected.

`add_label` takes `mailbox`, `uidvalidity` and `uids` identifying messages in a
source folder, plus `label`, the exact name of one existing label. The source
is a freshly selected ordinary folder: INBOX, a selectable folder under
`Folders/`, or a selectable root mailbox carrying the `\Sent`, `\Archive` or
`\Junk` attribute. Label views, Trash, Drafts and the `\All` and `\Flagged`
virtual views are refused as sources.

`remove_label` takes `mailbox`, `uidvalidity` and `uids` where `mailbox` is the
exact label mailbox under `Labels/`, and the UIDVALIDITY and UIDs belong to
that label view.

Each call names exactly one existing label. The label must appear in a fresh
listing on the same authenticated session as an exact UTF-8 name under
`Labels/` and be selectable. Applying several labels takes several calls, each
approved independently. A label is never created, deleted or renamed.

A request carries 1 through 50 unique positive UIDs. `uidvalidity` and every
UID are unsigned 32-bit integers from 1 through 4294967295, and any duplicate
UID rejects the entire request before a write.

Folder UIDs are never silently reused as label-view UIDs. A folder and a label
view number messages independently, so the server invents no mapping from a
source-folder UID to a label-view UID; removal accepts only identities read
from the label view itself.

One approval covers one exact payload: one tool, one mailbox, one UIDVALIDITY,
one ordered UID list and, for `add_label`, one label. The trusted client shows
the exact resolved payload and obtains approval for it, following the approval
rules of the triage design. Mail content never approves a call, and a previous
approval, session or restart grants nothing for a new call.

A missing, unselectable or recreated label, a UIDVALIDITY generation mismatch
and an absent UID all fail closed without a write for the affected request or
UID.

## Opt-in and rollout

`labels.enabled` is a prospective local boolean, default false, independent of
`mutations.enabled`: neither setting registers the other's tools or implies
the other.

The current configuration schema has no `labels` member and rejects it as an
unknown key; no current tool is registered by it, and this record gives no
instruction to set it. Existing triage configuration and destination rules are
unchanged.

Implementation and runtime registration require a separately approved change
after every proof gate below passes. Live acceptance on a Proton account
requires a further, separate authorization.

## Wire allowlist

The candidate allowlist is scoped per operation and is not shared with the
triage tools.

`add_label` may send only a single-UID `UID COPY` from the selected source
folder to the exact selectable label mailbox under `Labels/`.

`remove_label` may send, on the selected label view only, a single-UID
`UID STORE` adding the `\Deleted` flag followed by `UID EXPUNGE` of that same
UID.

Removal requires the `UIDPLUS` capability advertised on the actual
authenticated removal session before its first write. Without it removal is
unsupported and sends nothing; there is no library fallback to bare EXPUNGE.

A dedicated removal transport guard binds the selected label view, the exact
UID and the exact two-stage command sequence, and refuses anything else.

Plain `EXPUNGE`, `CLOSE`, `MOVE` and `UID MOVE`, COPY to any folder, and every
other flag or flag change are refused. No close or cleanup operation may
implicitly expunge other `\Deleted` entries.

The shipped triage prohibition of COPY and EXPUNGE is unchanged: `move_mail`,
`archive_mail` and `trash_mail` never gain a COPY or EXPUNGE fallback.

## Partial and unknown outcomes

Each call processes its input UIDs serially, one at a time, and reports
`applied`, `refused`, `not_attempted` or `unknown` per UID in input order. No
write is replayed after a possible write.

A timeout, disconnect or otherwise uncertain response to the `\Deleted` STORE
or to UID EXPUNGE makes that UID `unknown`: the call stops, nothing is replayed
and the transport is aborted. Every remaining UID is `not_attempted`.

If the `\Deleted` STORE succeeds and UID EXPUNGE then fails definitively, the
removal is a partial write, never a safe refusal. The UID is reported
`unknown` with the bounded code `partial`, and the call stops.

Marking `\Deleted` is a partial side effect, not approval of permanent
deletion.

Cleanup after a partial or unknown removal requires a separate read that
returns a fresh label-view identity and a new explicit approval of a new exact
payload.

## Upstream evidence

The candidate semantics rest on pinned public source: ProtonMail/proton-bridge
commit b9c5dac1651437100c40896dacd778a0518a26f2,
`internal/services/imapservice/connector.go`, whose `go.mod` pins Gluon
7e800978ab4a.

In that Gluon revision `handle_copy.go` calls `mailbox.Copy`, and
`handleUIDExpunge` passes the UID sequence to `mailbox.Expunge`, whereas plain
EXPUNGE supplies nil and so expunges every `\Deleted` entry in the selected
mailbox. In the Bridge connector, removal from a label unlabels the selected
label, while the Trash and Drafts paths can call `DeleteMessage`.

This is static source evidence, not proof of the installed Bridge version or of
live semantics. It does not replace the proof gates below.

## Proof gates

No label tool is registered at runtime until every proof case below passes.
The proofs are real public-MCP, configuration-to-CLI, verified-TLS synthetic
wire and state witnesses, including collateral-deletion tests. The closed
proof cases are:

- Multiple labels and folder retention: adding a second label keeps the first
  label, the source folder, its UID and Seen state, and removing one keeps the
  message, its folder and the other label.
- Colliding folder and label UIDs: equal UID numbers in a folder and a label
  view never cross, and a folder UID never reaches the label view.
- Cross-view Deleted isolation: a `\Deleted` mark in the label view never
  affects the same message in its folder or another label view.
- Unrelated Deleted label entries: other entries already marked `\Deleted` in
  the label view survive removal untouched.
- Missing UIDPLUS: without `UIDPLUS` on the removal session removal sends no
  write.
- Deleted-marker partial failure: a definitive UID EXPUNGE failure after a
  successful `\Deleted` STORE reports the partial outcome and stops.
- Lost responses at each stage: a lost COPY, STORE or UID EXPUNGE response
  yields `unknown`, a transport abort and no replay.
- Label rename and recreate races: a label renamed, deleted or recreated
  between listing and write fails closed.

If those proofs cannot establish a safe scoped removal, removal remains
unavailable rather than substituting MOVE or generic EXPUNGE. Installed-version
capability checks and a live Proton pilot are separately authorized later.

## Status

Planned, not shipped. No label tool, configuration member or adapter command
exists. Mail stays read-only unless `mutations.enabled` registers the shipped
triage tools, which remain limited to Seen `UID STORE` and native `UID MOVE`.

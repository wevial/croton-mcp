# Proposed Mail mutation boundary

## Invariant

No mutation is authorized by this record. The shipped Mail implementation
remains read-only. Bridge connections remain loopback-only with TLS.

## Proposed operations

These are proposed semantics only, not registered tools:

| Operation | Proposed behavior |
| --- | --- |
| flag | Set or clear the IMAP Flagged flag (`\Flagged`). |
| mark read | Set or clear the IMAP Seen flag (`\Seen`). |
| move | Move explicitly selected messages to an existing destination mailbox. |

Identity must be UID-based and bound to the source mailbox and UIDVALIDITY.
Check UIDVALIDITY against a fresh server value before any mutation; a changed
generation invalidates the selected identity. Message sequence numbers must
not substitute for UIDs. Revalidate the existing destination before a move.

Excluded operations: sending, drafts, permanent deletion, arbitrary flags and
implicit expunge. No fallback copy/delete/expunge sequence is prescribed.
An unavailable move primitive is an unsupported operation, not permission to
assemble a different mutation sequence.

## Fail-closed behavior

All of the following are requirements for a future implementation:

- No valid approval: fail closed before any mutation. This record grants none;
  an unresolved approval model cannot be treated as approval.
- Stale identity: fail closed before any mutation, including a UIDVALIDITY
  mismatch, missing UID or identity no longer bound to the selected mailbox.
- Invalid destination: fail closed before any mutation if the destination
  does not exist, cannot be validated or is not the explicitly selected mailbox.
- Unsupported operation: fail closed before any mutation, including excluded
  operations or missing server support for the requested operation.
- Ambiguous completion: fail closed without reporting success. A disconnect or
  timeout after dispatch may leave the result unknown; report that uncertainty.
  An ambiguous mutation must not be blindly replayed. Reconciliation and any
  further action require separately established identity and valid approval;
  this record does not specify a retry or recovery mechanism.

## Proof requirements

Future synthetic tests must prove the proposed boundary before writes can be
enabled. These tests are not claimed to exist today. Use synthetic `.test`
fixtures with no live credentials, account identifiers or mailbox content.

Test setting and clearing Flagged and Seen without changing other flags, and
moving only explicitly selected UIDs to an existing destination. Test mailbox
binding and fresh UIDVALIDITY checks, including generation changes and missing
UIDs. Each refusal case above must have a test: no valid approval, stale
identity, invalid destination, unsupported operation and ambiguous completion.
For pre-mutation refusals assert no write was dispatched. Simulate loss of the
completion response after dispatch; assert no success claim and no blind replay.
Assert excluded operations and fallback copy/delete/expunge are never emitted.

The documentation verifier is a structural check, not proof that a future
IMAP mutation implementation is safe. A future implementation must supply its
own deterministic tests and an explicitly decided approval contract.

## Reserved

- Approval authority remains undecided: who may authorize a mutation.
- Approval lifetime remains undecided: per-call versus session approval.
- User-visible confirmation surface remains undecided: where and how the user
  would see and confirm the selected operation, messages and destination.

The approval model is Reserved. Neither this record nor engineering choices
select an approval mechanism or change config behavior.

## Status

Proposed. Mail remains read-only. No Mail mutation tools are registered by this
record. Implementation requires a separate decision resolving Reserved and
future proof tests; publication of this proposal does not enable writes.

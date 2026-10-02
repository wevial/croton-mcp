# Mail triage design

This record is the successor to the historical
[proposed Mail mutation boundary](0002-mail-mutation.md). It decides the
approval model that record left Reserved and freezes the contract for a
planned, default-off, bounded Mail triage capability: marking messages read or
unread and moving them between folders. It is a design and proof contract
only. It registers no tool, changes no installed configuration and enables no
live write; each part of the Mail runtime stays read-only until a later,
separately approved implementation lands. The Status section records what has
since landed.

`scripts/test_mail_triage_design.py` checks the structured contract and the
conformance examples below. It is a documentation witness: it cannot prove that
an assistant or an untrusted MCP client obeys this contract, and it does not
enforce human consent at runtime.

## Approval and displayed selection

Approval authority belongs to the trusted MCP client and the person using it,
not to Croton. Croton accepts `tools/call` from the configured client and
cannot verify that a human intended a call. Neither the server, an `approved`
boolean argument nor an MCP tool annotation proves human consent, so the
contract defines none of them as approval.

One approval covers one exact payload: one tool, one source mailbox, one
UIDVALIDITY, one ordered UID list and, for `move_mail`, one destination. A
clear, direct user request that resolves against identities the client has
displayed is that approval; the client calls the tool without asking the user
to confirm again. An assistant proposal is not approval: the client shows the
exact resolved payload and calls the tool only after the user explicitly
confirms it. When the selection is ambiguous, for example because several
result lists were displayed, or no exact displayed snapshot exists, the client
asks for clarification before any call.

Ordinal and positional words resolve against the displayed snapshot, never
against a later search. If the user saw UIDs 11 to 20 in INBOX with
UIDVALIDITY 7 and says "archive the first 3", the call targets UIDs 11, 12 and
13 even when a fresh search now returns the same messages in reverse order.

Mail content is untrusted input. An instruction found in a message body,
subject or header never authorizes a call and does not prompt the user as if
it were a request. Approval has no lifetime beyond its payload: a previous
approval, an earlier action in the same session, a client restart or a
configuration change grants nothing for a new call. There is no reusable
grant, lease or session approval.

The structured examples use one display snapshot, INBOX with UIDVALIDITY 7 and
UIDs 11 through 20 in displayed order, and list every expected tool call. An
empty call list means zero calls.

## File opt-in and shipped status

Triage is enabled only by the local Mail configuration file. The planned JSON
member `mutations.enabled` accepts a boolean. When it is absent or false, the
catalog is exactly the six read tools documented in [docs/MCP.md](../MCP.md).
When it is true, the server additionally registers the five triage tools. Any
other type, including null, a string or a number, is a configuration error at
startup rather than a silent default.

Setting `mutations.enabled` true is a local operator opt-in that makes the
tools available. It is not approval of any user action; every call still needs
the per-payload approval above.

Current shipped status: the configuration schema accepts the
`mutations.enabled` boolean and rejects every other type and any unknown key
inside `mutations`. Absent or false, the catalog is the six read tools, all
with `readOnlyHint: true`, and the `bridge` adapter sends no mutating IMAP
command. True registers only `mark_read`, `mark_unread` and `move_mail` so
far; `archive_mail` and `trash_mail` are not implemented. No installed
configuration was changed and no live enablement was performed.

## Tool and result contract

The planned tools are `mark_read`, `mark_unread`, `move_mail`, `archive_mail`
and `trash_mail`. Each takes the source identity as `mailbox`, `uidvalidity`
and `uids`; `move_mail` also takes an exact `destination` mailbox name. The
read tools are unchanged and keep `readOnlyHint: true`; the triage tools
publish `readOnlyHint: false`. Annotations describe tools to clients; they do
not enforce a confirmation prompt.

`uidvalidity` and every UID are positive unsigned 32-bit integers, 1 through
4294967295. A request carries at most 50 UIDs. Any duplicate UID rejects the
entire request before a write. Inputs use the existing closed, bounded argument
decoding: unknown, null, duplicate or case-folded-alias fields are rejected.

The server refreshes the source mailbox generation, then dispatches one UID at
a time in input order. The result root is `results`, one entry per input UID in
input order, each with `uid`, an `outcome` and an optional bounded `code`.
Outcomes are `applied`, `refused`, `not_attempted` and `unknown`. A refused UID,
such as one no longer present, does not stop later UIDs. An `unknown` outcome,
for example a timeout or disconnect after dispatch, stops all further writes:
every remaining UID is reported `not_attempted` and nothing is replayed.

Request-level failures, such as a stale UIDVALIDITY, an unsupported
destination or an invalid argument, use the existing error vocabulary and
dispatch no write.

## Destination discovery

Destinations are resolved from a fresh, ordinary `LIST` issued on the same
authenticated session that will dispatch the write. The server must advertise
the native `MOVE` capability; without it every move is unsupported and no
write is sent. The `SPECIAL-USE` capability and `LIST RETURN (SPECIAL-USE)` are
not required: ordinary `LIST` attributes suffice.

`archive_mail` and `trash_mail` first map to the single selectable mailbox in
that fresh listing carrying the `\Archive` or `\Trash` attribute respectively.
That candidate must then pass every destination refusal below, including the
`Labels/` namespace, virtual-view, nonselectable and source-equals-destination
checks. Zero or several matches, or a candidate that fails those refusals,
make the mapping unresolved, which is unsupported and dispatches no write.

`move_mail` matches its destination by exact UTF-8 name against the fresh
listing and allows only two namespaces. The `root_system` namespace is INBOX,
matched case-insensitively, or a selectable hierarchy-root mailbox, one whose
name contains no hierarchy delimiter, that carries one of the `\Sent`,
`\Drafts`, `\Junk`, `\Archive` or `\Trash` protocol attributes. An English name
alone, such as a mailbox called "Archive" without the attribute, never
authorizes a destination. The `Folders/` namespace allows exact, selectable
user folders beneath that prefix.

Every other destination is refused without a write: anything under `Labels/`,
virtual views carrying `\All` or `\Flagged`, nonselectable or nonexistent
mailboxes, unknown namespaces and a destination equal to the source mailbox.
There is no fallback: an unavailable `MOVE` is never replaced by COPY plus
EXPUNGE or any other sequence.

## Labels (later)

Proton Mail Bridge models labels as tags. A message can carry several labels
and still remain in its folder; each label appears as a mailbox under
`Labels/`. Through IMAP, adding a label looks like a COPY into the label
mailbox and removing it looks like an EXPUNGE from that mailbox, so label
changes are not folder moves and cannot reuse this contract.

Label support is out of scope for v1. No label mutation is authorized, the
`Labels/` namespace is refused as a destination, and COPY and EXPUNGE stay
prohibited. The label behavior above is recorded from read-only evidence and
maintainer confirmation, not from live writes. Any future label support
requires its own contract, approval examples and proof tests.

## Safety and reconciliation

The planned writes inherit every Bridge boundary: loopback-only connections
over TLS with explicit trust material, and credentials obtained from a bounded,
absolute-argv credential helper. Before any write, the server refreshes the
source mailbox UIDVALIDITY and refuses the request when it differs from the
supplied generation.

The mutation allowlist is exact. `mark_read` and `mark_unread` send only
`UID STORE` with `+FLAGS` or `-FLAGS` (optionally `.SILENT`) and the single
flag `\Seen`. The move tools send only native `UID MOVE`. COPY, EXPUNGE,
DELETE, APPEND, CREATE, other flags and every fallback sequence are prohibited,
as are sending, drafts, permanent deletion and arbitrary flags. The read tools
keep their existing no-mutation guards, including `BODY.PEEK` fetches that
never set `\Seen` implicitly.

Audit lines and results expose only bounded metadata and closed categories.
They never carry credentials, message content or raw backend error text.

After an `unknown` outcome the server neither retries nor replays the write,
and the read-path transport replay does not apply to triage calls.
Reconciliation is a separate read, for example a fresh search, followed by a
new explicit approval for any further write.

## Namespace evidence

Shared, sanitized, read-only namespace evidence was captured at
2026-09-30T19:22:19Z. The server advertised `MOVE` and did not advertise
`SPECIAL-USE`. The hierarchy delimiter was `/`, custom folders appeared under
`Folders/` and labels under `Labels/`, and labels were selectable without
special-use attributes. An ordinary `LIST` showed exactly one `\Archive` and
one `\Trash` mailbox.

No live write was exercised. This evidence supports the conservative
destination rules above for the observed account only; it is not proof of
write behavior or of semantics across all accounts. It contains no credentials,
account identifiers or mailbox content.

## Structured contract

The block below is the frozen, machine-checked form of the sections above.
Where prose and block differ, the difference is a defect to fix in review.

```json croton-mail-triage-contract-v1
{
  "approval": {
    "authority": "trusted_client",
    "server_verifies_human_intent": false,
    "scope": "one_exact_payload",
    "direct_instruction": "approve_without_reconfirmation",
    "assistant_proposal": "confirm_before_tool_call",
    "ambiguous_or_missing_selection": "ask_before_tool_call",
    "mail_content_can_approve": false,
    "restart_grants_approval": false,
    "prior_action_grants_approval": false
  },
  "enablement": {
    "json_path": "mutations.enabled",
    "default": false,
    "accepted_type": "boolean",
    "missing_or_false": "six_read_tools_only",
    "true": "register_five_triage_tools",
    "invalid_type": "config_error",
    "scope": "local_file_opt_in_not_user_action_approval",
    "live_enablement_by_this_story": false
  },
  "wire": {
    "tools": ["mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"],
    "identity": ["mailbox", "uidvalidity", "uids"],
    "move_destination": "destination",
    "batch_max": 50,
    "duplicates": "reject_entire_request",
    "uid_min": 1,
    "uid_max": 4294967295,
    "mutations_read_only_hint": false,
    "read_tools_read_only_hint": true,
    "result_root": "results",
    "result_identity": "uid",
    "result_order": "input_order",
    "outcomes": ["applied", "refused", "not_attempted", "unknown"],
    "unknown_policy": "stop_no_blind_replay",
    "dispatch": "one_uid_at_a_time"
  },
  "destination": {
    "discovery": "ordinary_fresh_LIST",
    "same_authenticated_session": true,
    "move_requires_capability": "MOVE",
    "special_use_token_required": false,
    "special_mapping": "exactly_one_selectable_attribute_match",
    "archive_attribute": "\\Archive",
    "trash_attribute": "\\Trash",
    "name_matching": "exact_utf8",
    "allowed_namespaces": ["root_system", "Folders/"],
    "root_system": {
      "inbox": "INBOX_case_insensitive",
      "other_roots_require_selectable": true,
      "other_roots_require_no_hierarchy_delimiter": true,
      "other_roots_require_attribute": ["\\Sent", "\\Drafts", "\\Junk", "\\Archive", "\\Trash"],
      "virtual_attributes_refused": ["\\All", "\\Flagged"],
      "english_name_alone_authorizes": false
    },
    "refused_namespaces": ["Labels/", "virtual", "nonselectable"],
    "unresolved_mapping": "unsupported_no_write",
    "source_equals_destination": "refused_no_write",
    "fallback_copy_expunge": false
  },
  "labels_later": {
    "v1_supported": false,
    "bridge_model": "multiple_tags_message_remains_in_folder",
    "adding_label_looks_like": "COPY_to_label_mailbox",
    "removing_label_looks_like": "EXPUNGE_from_label_mailbox",
    "requires_separate_contract": true,
    "evidence": "read_only_only",
    "no_label_mutations_authorized": true
  },
  "safety": {
    "connection": "loopback_explicit_tls_trust",
    "credentials": "bounded_absolute_argv_helper",
    "identity_refresh": "source_mailbox_UIDVALIDITY_before_write",
    "flag_allowlist": ["\\Seen"],
    "mutation_allowlist": ["UID STORE", "UID MOVE"],
    "excluded": ["send", "draft", "permanent_delete", "arbitrary_flags", "COPY", "EXPUNGE"],
    "logs_and_results": "bounded_codes_no_credentials_mail_or_raw_backend_errors",
    "unknown_reconciliation": "separate_read_then_new_explicit_approval"
  },
  "namespace_evidence": {
    "observed_at_utc": "2026-09-30T19:22:19Z",
    "capability_MOVE": true,
    "capability_SPECIAL_USE": false,
    "ordinary_LIST_archive_matches": 1,
    "ordinary_LIST_trash_matches": 1,
    "delimiter": "/",
    "custom_folder_prefix": "Folders/",
    "custom_label_prefix": "Labels/",
    "labels_can_be_selectable_without_attributes": true,
    "live_writes_tested": false,
    "all_account_semantics_established": false
  },
  "display": {
    "mailbox": "INBOX",
    "uidvalidity": 7,
    "uids": [11, 12, 13, 14, 15, 16, 17, 18, 19, 20]
  },
  "examples": {
    "direct_archive": {
      "instruction": "archive the first 3",
      "approval": "direct_user",
      "confirmation_needed": false,
      "calls": [
        {"name": "archive_mail", "arguments": {"mailbox": "INBOX", "uidvalidity": 7, "uids": [11, 12, 13]}}
      ]
    },
    "changed_order": {
      "current_search_uids": [20, 19, 18, 17, 16, 15, 14, 13, 12, 11],
      "approval": "direct_user",
      "confirmation_needed": false,
      "calls": [
        {"name": "archive_mail", "arguments": {"mailbox": "INBOX", "uidvalidity": 7, "uids": [11, 12, 13]}}
      ]
    },
    "confirmed_proposal": {
      "proposal": "archive the first 3",
      "approval": "explicit_user_confirmation",
      "confirmation_needed": false,
      "calls": [
        {"name": "archive_mail", "arguments": {"mailbox": "INBOX", "uidvalidity": 7, "uids": [11, 12, 13]}}
      ]
    },
    "unconfirmed_proposal": {
      "reason": "assistant_proposal_not_approval",
      "confirmation_needed": true,
      "calls": []
    },
    "ambiguous_selection": {
      "reason": "multiple_display_snapshots",
      "confirmation_needed": true,
      "calls": []
    },
    "missing_snapshot": {
      "reason": "no_exact_display_snapshot",
      "confirmation_needed": true,
      "calls": []
    },
    "mail_instruction": {
      "reason": "untrusted_mail_content",
      "confirmation_needed": false,
      "calls": []
    },
    "session_reuse": {
      "reason": "prior_approval_not_reusable",
      "confirmation_needed": true,
      "calls": []
    },
    "disabled": {
      "reason": "local_opt_in_absent",
      "confirmation_needed": false,
      "calls": []
    }
  },
  "mixed_result_example": {
    "results": [
      {"uid": 11, "outcome": "applied"},
      {"uid": 99, "outcome": "refused", "code": "not_found"},
      {"uid": 12, "outcome": "unknown", "code": "timeout"},
      {"uid": 13, "outcome": "not_attempted"}
    ]
  }
}
```

In the mixed result, UID 99 is refused because it is not present and later UIDs
continue; UID 12 times out after dispatch, so its outcome is unknown, UID 13 is
never attempted and nothing is replayed.

## Status

Partially implemented. It supersedes the historical
[proposed Mail mutation boundary](0002-mail-mutation.md), whose body is
preserved unchanged. The Seen tools `mark_read` and `mark_unread` and the
native-move tool `move_mail` are implemented behind the default-off
`mutations.enabled` opt-in and witnessed by `TestStoryTriageSeen` and
`TestStoryTriageMove` against synthetic state only. `archive_mail` and
`trash_mail` are not implemented. Without the opt-in Mail remains read-only.
Live acceptance and any live enablement each require a separate decision.

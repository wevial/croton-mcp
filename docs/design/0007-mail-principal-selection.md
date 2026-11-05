# Mail principal and selection-epoch feasibility

This record decides whether Croton can bind an authoritative, private Proton
account identity to its authenticated Bridge session, and freezes the proposed
read-only client epoch handshake that would depend on it. It is a design and
feasibility record only. It registers no tool, changes no credential helper or
configuration schema, creates no binding file and grants no live account
authority.

`scripts/test_mail_principal_selection_design.py` checks the visible sections
and tables of this record. It is a documentation witness: it checks record
consistency and citation form, not upstream semantics, Bridge behavior or the
absence of secrets in arbitrary text. Human review evaluates the cited source.

## Problem

A trusted client approves one exact Mail payload against identities it has
displayed. Those identities are a mailbox name, a UIDVALIDITY and UIDs, and two
Proton accounts can produce colliding mailbox/UIDVALIDITY/UID tuples. If the
credential helper later yields another account, a reconnect can silently move
an old selection and its approval to that other account.

Successful IMAP login plus TLS identity does not prove that a later connection
belongs to the previously selected Proton account. Public UID fields and a
random epoch cannot substitute for private principal verification.

## Current surface

The baseline is Croton commit 103557c17d75a7db52214fbcefe5b383c0327fca. None of
the current surface is an authoritative-principal source or a public epoch API.
`Adapter.openSession` in `bridge/bridge.go` dials, authenticates and caches one
session; its read retry loop in `Adapter.execute` is not write retry authority
and performs no identity comparison.

| Path | Symbol | Citation | Finding | Principal source | Public epoch |
| --- | --- | --- | --- | --- | --- |
| bridge/credentials.go | Credentials, parseCredentials | croton@103557c17d75a7db52214fbcefe5b383c0327fca:bridge/credentials.go | The helper JSON accepts only username and password and rejects every other field. A principal field is not a supported helper feature. | none | none |
| bridge/imapclient.go | imapSession.Authenticate, waitForCapabilities | croton@103557c17d75a7db52214fbcefe5b383c0327fca:bridge/imapclient.go | LOGIN authenticates the session, then NOOP and the capability refresh complete; there is no principal query. | none | none |
| bridge/search.go | encodeMessageID, decodeMessageID, newAdapterIDKey | croton@103557c17d75a7db52214fbcefe5b383c0327fca:bridge/search.go | Message IDs are process-local HMAC IDs over mailbox, UIDVALIDITY and UID under a random per-adapter key; they become stale_id in a new process and carry no account identity. | none | none |

## Candidate evidence

The candidate set is closed: native authenticated-session identity, trusted
Bridge-local metadata bound to that session, and privileged helper/config
attestation. Upstream evidence is source inspection of public ProtonMail/proton-bridge
commit b9c5dac1651437100c40896dacd778a0518a26f2 and its `go.mod` Gluon pin
7e800978ab4a9942829eb6116e33ea0278c03e91. These commits do not describe the
installed Bridge version. No executed evidence exists for any row.

| Candidate | Verdict | Trust and connection binding | Evidence kind | Citation | Symbol | Limit | Executed evidence |
| --- | --- | --- | --- | --- | --- | --- | --- |
| native authenticated-session identity | unsupported | Gluon binds the session to a private userID in server state, but the IMAP connection never receives it. | source inspection | ProtonMail/gluon@7e800978ab4a9942829eb6116e33ea0278c03e91:internal/session/handle_login.go; ProtonMail/gluon@7e800978ab4a9942829eb6116e33ea0278c03e91:internal/backend/backend.go; ProtonMail/gluon@7e800978ab4a9942829eb6116e33ea0278c03e91:internal/session/handle_id.go | Session.handleLogin, Backend.getUserID, Session.handleIDGet | LOGIN replies only OK with CAPABILITY items; ID returns server version info; no session command exposes the userID or a keyed derivative. | NOT RUN |
| trusted Bridge-local metadata bound to that session | unsupported | The gRPC frontend is a separate server-token channel; nothing ties one of its answers to Croton's IMAP connection. | source inspection | ProtonMail/proton-bridge@b9c5dac1651437100c40896dacd778a0518a26f2:internal/frontend/grpc/service_user.go; ProtonMail/proton-bridge@b9c5dac1651437100c40896dacd778a0518a26f2:internal/frontend/grpc/utils.go; ProtonMail/proton-bridge@b9c5dac1651437100c40896dacd778a0518a26f2:internal/frontend/grpc/service.go | Service.GetUserList, grpcUserFromInfo, validateServerToken | GetUserList returns user IDs with usernames, addresses and the Bridge password, so using it would expose credentials and account data, and a lookup by credential match is a time-of-check race with the IMAP login. | NOT RUN |
| privileged helper/config attestation | unsupported | The helper is privileged configuration, but its output is not bound to the account that Bridge authenticates on the connection. | source inspection | croton@103557c17d75a7db52214fbcefe5b383c0327fca:bridge/credentials.go | parseCredentials | The current helper JSON rejects any extra field; an attested principal would merely repeat a configured name that Bridge never confirms. | NOT RUN |

In Bridge, `Connector.Authorize` in `internal/services/imapservice/connector.go`
calls `State.CheckAuth` in `internal/services/useridentity/state.go`, which
matches the Bridge password and any enabled address case-insensitively. Gluon
`Backend.getUserID` in `internal/backend/backend.go` returns the first user
whose connector authorizes the credentials. That mapping stays inside Bridge.

## Insufficient claims

Each of these claims alone is insufficient evidence of account identity and is
never accepted as a principal source or a fallback.

| Claim | Verdict | Reason |
| --- | --- | --- |
| username-only | insufficient | A helper can return any address the Bridge accepts, and an address can move between accounts or be shared by aliases. |
| TLS-only | insufficient | The pinned Bridge certificate identifies the local Bridge process, not the account selected by LOGIN. |
| credential-fingerprint-only | insufficient | A Bridge password identifies a credential, not an account; rotation changes it and an unkeyed fingerprint must not be published. |
| UID-tuple-only | insufficient | Mailbox, UIDVALIDITY and UID tuples collide across accounts. |
| opaque-ID-only | insufficient | Message IDs are process-local HMAC encodings of public tuples and carry no account identity. |

## Decision

| Field | Value |
| --- | --- |
| decision | blocked |
| design candidate | none |
| runtime implementation | unavailable |
| runtime proof | unavailable |
| fallback | none |
| account data returned | none |
| missing evidence | No inspected source exposes a session-bound private principal on Croton's IMAP connection, and no inspected Bridge-local interface binds an account answer to that connection without returning credentials or account data. |

Integration is blocked. Unsupported is a valid completed investigation
outcome, not an invented successful API. No fallback is enabled and no new
account data is returned.

## Blockers

| Requirement | Missing source or binding evidence |
| --- | --- |
| session-bound principal | No IMAP response on the authenticated Gluon session carries the private userID or a keyed derivative of it. |
| Bridge-local binding | No Bridge-local interface answers which account authenticated a given IMAP connection; GetUserList is connection-independent and returns credentials and addresses. |
| helper attestation | No helper output field is confirmed by Bridge; a configured principal name is not authoritative. |

## Proposed handshake

This proposal is frozen for a later, separately approved implementation and is
gated on a real authoritative principal source. While the decision is blocked
it cannot be implemented. The catalog remains unchanged until a separate
approved implementation registers anything.

| Field | Value |
| --- | --- |
| tool | get_mail_context |
| input | empty closed object |
| readOnlyHint | true |
| side effects | none |
| response fields | selectionEpoch, accountContinuity |
| selectionEpoch | opaque 64-character lowercase-hex token, not derived from the principal |
| accountContinuity | literal verified |
| response bound | 512 bytes |
| principal evidence bound | 256 bytes |
| binding record bound | 4096 bytes |
| deadline | existing configured command deadline |
| success precondition | authoritative private principal verification |
| refusal | unsupported, unavailable or stale_id with no epoch and no account data |
| raw-tuple writes | unchanged |
| catalog | unchanged |

`get_mail_context` takes an empty closed input object, publishes
`readOnlyHint: true` and has no mutation or preparation side effect.

A successful response contains only `selectionEpoch` and `accountContinuity`.
`selectionEpoch` is an opaque 64-character lowercase-hex token that is not
derived from the principal. `accountContinuity` is the literal `verified`. The
complete serialized result is at most 512 bytes.

A successful response is permitted only after authoritative private principal
verification. Without it the call returns the existing static `unsupported`,
`unavailable` or `stale_id` category as applicable, with no epoch and no
account data.

No response, error string or audit record contains a principal, address,
credential, endpoint detail, unkeyed principal fingerprint or mailbox content.

Raw mailbox/UIDVALIDITY/UID write inputs stay unchanged under M2.

## Binding state

One owner-pinned account binding exists per configured adapter. Private
principal evidence is at most 256 bytes, and any protected persistent binding
record is at most 4096 bytes. No mail content or credential is retained.

Any eventual persisted binding uses an owner-only, no-follow, regular-file
policy. Enrollment, rotation and revocation are trusted operator actions
specified separately from model input; a tool call never enrolls a principal.

This ticket creates no such file. If a principal source cannot support these
constraints, the design stays blocked rather than adopting a larger unbounded
fallback.

## Epoch lifecycle

A new process invalidates old snapshots and prepared handles.

Reconnect verifies the same pinned principal before any account data or write.

Verified same-principal credential rotation does not itself change account
identity.

Mismatch or unavailable continuity aborts access, clears client approvals and
prepared objects, and never silently enrolls the new principal.

A new random epoch cannot legitimize an unverified account.

Context acquisition uses the existing finite configured command deadline, with
no unbounded polling or discovery and no automatic retry after a possible
mutation.

## Enforcement split

Server-enforced: principal verification before any account data or write,
refusal without an epoch when continuity is unavailable, the response bounds,
and a new epoch after process restart or principal change.

Client gate: the trusted client compares the context epoch, invalidates old
ordinal snapshots and approvals, and serializes context changes versus
dispatch.

Raw tuples alone do not prove selection age; the server cannot enforce that
an approval was made under the current epoch while writes keep raw tuples.

A future read-only metadata call still requires separately authorized setup
and account access; this design grants none.

## Future acceptance matrix

Every case requires verified-TLS, public-MCP and configured-client tests with
independent state and identity oracles. Zero wrong-account disclosure and zero
wrong-account writes must be observed, not inferred from a transcript refusal.
Synthetic roles accountA and accountB stand for two distinct test accounts. All
statuses remain NOT RUN.

| Case | Setup | Before data/write assertion | Client invalidation | Mutation replay | Status |
| --- | --- | --- | --- | --- | --- |
| colliding-tuples | accountA and accountB expose the same mailbox, UIDVALIDITY and UID | No accountB data or write occurs under an accountA selection or approval | required | none | NOT RUN |
| helper-switch-reconnect | the helper switches from accountA to accountB across a reconnect | Principal mismatch is detected before any data or write on the new session | required | none | NOT RUN |
| restart-old-selection | the process restarts while the client holds an accountA selection | Old IDs return stale_id and no write uses the old selection | required | none | NOT RUN |
| same-principal-rotation | the accountA Bridge password rotates and the principal is verified unchanged | Continuity stays verified without re-enrollment and no other account is reached | required when the epoch changes | none | NOT RUN |
| principal-unavailable | the principal source is unavailable on reconnect | Refusal returns no epoch, no account data and no write | required | none | NOT RUN |
| principal-mismatch | the reconnect principal differs from the pinned accountA principal | Access aborts with no data or write and accountB is not enrolled | required | none | NOT RUN |
| context-dispatch-race | a context read races a pending dispatch | Dispatch is serialized against the context change and no write crosses epochs | required | none | NOT RUN |
| stale-prepared-handle | a prepared handle from an older epoch is used | The stale handle is refused before any write | required | none | NOT RUN |

## Investigation

Offline source inspection on 2026-11-05 UTC, within the 45-minute bound. The
two public upstream commits above were fetched by exact hash into a scratch
directory and read with no build, execution or account access. No Bridge RPC
query, helper execution, credential load, IMAP login or account operation was
performed. Examples use synthetic roles only.

## Status

Blocked; design only. No get_mail_context implementation, registration or
proof exists, and Mail authority is unchanged.

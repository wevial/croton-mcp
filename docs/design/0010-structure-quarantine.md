# Structure-frame quarantine

This record fixes the contract of the private structure-frame quarantine
reader in `bridge/structure_quarantine.go` and of the dedicated session in
`bridge/structure_session.go` that installs it. Neither has a production
caller, registers a tool, changes an existing read budget, output limit or
search path, or by itself completes guard acceptance.
`TestStructureQuarantinePrimitive` in
`bridge/structure_quarantine_internal_test.go` exercises the reader over
`net.Pipe` with synthetic input only.

## Problem

The existing `readBudgetConn` forwards bytes as it reads them and fails only
after its budget is spent, so go-imap has already parsed an unvalidated prefix
of the failing response. Its SEARCH observer watches literals after the bytes
are forwarded under mutable begin/end epochs; it cannot recall bytes or decide
that a whole frame may be released.

The pinned go-imap client also records the target of a UID FETCH once and
routes each untagged FETCH by UID. A FLAGS-only FETCH for the target UID that
arrives while the BODYSTRUCTURE fetch is pending takes that single delivery,
and the real structure that follows has nowhere to go.

## Reader lifetime

One reader serves one dedicated session for one selected UID. The selected UID
is fixed at construction and must be positive. `beginDispatch` records the
single structure dispatch; a second call is a sticky protocol refusal. The
reader keeps three dispatch states: idle, active and delivered. None of its
counters is reset during its lifetime and no arm/disarm epoch exists.

Read serializes source reads, never returns bytes together with an error, and
returns approved bytes before any terminal error. Bytes of a refused or
unfinished frame are never returned. The reader never drains, resynchronizes,
rewrites or passes through input.

## Fixed limits

| Limit | Value | Counted |
| --- | --- | --- |
| FETCH frame wire bytes | 65536 | every byte from `*` through CRLF, including quoted syntax, escapes, literal declarations, literal payload and the terminator |
| list depth | 64 | the FETCH attribute list is depth 1 |
| lexical tokens per FETCH | 4096 | each parenthesis, atom or number, quoted string and literal; a literal declaration and its payload are one token |
| atom bytes | 4096 | one atom, number or flag inside a FETCH |
| control line bytes | 4096 | one whole control line including CRLF |
| session wire bytes | 131072 | every byte read, including consumed FLAGS-only frames |
| session responses | 64 | every control, released FETCH and consumed FLAGS-only FETCH |
| session work units | 8192 | every FETCH token, including those of consumed FLAGS-only frames, plus one unit per control line |

Quoted strings and literal payloads have no separate ceiling. Their maximum is
the remaining allowance of the complete frame, so neither can exceed 65536
bytes. A literal declaration is refused as soon as its declared size, plus the
closing of every open list and the CRLF, cannot fit the remaining frame or
session allowance, before any payload is read.

Each limit is checked before the byte that would exceed it is buffered or
released. Within a FETCH the reader also refuses as soon as the least possible
completion of the frame, closing every open list and its CRLF, cannot fit a
budget, so no read waits for input that could not be admitted. Raw storage
holds only the current frame and never grows past 65536 bytes; a refusal drops
it. Overage is a sticky `bounds_exceeded`.

A valid wide structure may exceed 4096 tokens or the frame allowance and is
then refused before any MIME projection. This is independent resource policy,
not a promise to admit every message with 200 parts. A longer admissible
quoted or literal name still reaches the unchanged downstream 255/1024
filename truncate-and-flag; no output cap is raised.

## Framing

The reader is iterative and byte-linear. It keeps no token tree and does not
recurse. Each response is framed completely before it is classified or
released.

Lines end only at CRLF. A bare LF, a CR not followed by LF, and a CR or LF in a
quoted string are protocol refusals. Quoted strings accept only `\"` and `\\`
escapes. Literals are classic `{n}` CRLF declarations with a uint32 size of at
most ten digits; their payload is opaque, so a fake FETCH, CRLF or parenthesis
inside it is ordinary data. Literal8, non-synchronizing, negative, empty and
overflowing declarations are refused, as are an unterminated quote or escape
and a truncated literal.

A FETCH is `* n FETCH (` with a positive canonical uint32 sequence number,
single spaces between elements and a CRLF right after its closing parenthesis.
A list may directly follow a closed list only inside a BODYSTRUCTURE. A line
whose second word starts with a digit and whose third word is FETCH is framed
as a FETCH; if it fails this grammar, including a zero, leading-zero or
overflowing sequence number, it is a protocol refusal and never falls back to
control handling. Status prose such as `* OK FETCH completed` is a control,
and a FETCH without a sequence number is an unknown control and refused.

Controls are whole CRLF lines of at most 4096 bytes and carry no literals:

| Control | Admitted form |
| --- | --- |
| untagged status | `* OK`, `* NO`, `* BAD`, `* BYE`, `* PREAUTH`, optional text |
| CAPABILITY | `* CAPABILITY` and one or more atoms, no `{` or `}` |
| FLAGS | `* FLAGS` and a parenthesized list of flag atoms, no `{` or `}` |
| EXISTS, RECENT | `* n EXISTS`, `* n RECENT` with n zero or more |
| EXPUNGE | `* n EXPUNGE` with n positive |
| SEARCH, ESEARCH | printable ASCII, complete quoted strings, no `{` or `}` even when quoted |
| continuation | `+`, optionally followed by a space and text |
| tagged status | tag, then `OK`, `NO` or `BAD`, optional text |

Status and continuation text is opaque, so a brace in it is text. Text that
starts with a response-code bracket is admitted only when that bracket closes
on the same line. Non-text controls refuse a brace or literal declaration and
an unfinished quote. Any other control is a protocol refusal.

## FETCH classification and FLAGS routing

A FETCH may carry only `UID`, `FLAGS` and `BODYSTRUCTURE`, each at most once,
in any order. Bare `BODY`, `BODY[...]` sections, `BINARY`, `BINARY.SIZE` and
every other item are protocol refusals; this holds after an apparent FLAGS-only
prefix too, because no frame is classified before it is complete.

A complete frame with one BODYSTRUCTURE list is released byte for byte only
when its UID is present and equals the selected UID and the dispatch is
active; release marks the dispatch delivered. An early structure, a second
structure, and a wrong, missing or duplicate UID are protocol refusals.

A complete frame with exactly one FLAGS list of valid flag atoms, zero or one
positive UID for any message, and nothing else is consumed explicitly: the
reader returns none of its bytes, it does not touch the dispatch record, and
the later structure stays deliverable. This holds before, during and after the
dispatch. The frame still counts its wire bytes against the frame and session
byte budgets, its tokens against the per-FETCH and session work budgets, and
one session response. The reader never mutates flags, logs content or
suppresses anything on normal pooled sessions; consumption exists only inside
this dedicated reader. Any other frame without a structure is a protocol
refusal.

## Terminal behavior

The first refusal or abort is sticky. A later abort, dispatch, read or input
never changes it. Approved bytes that were complete before a refusal may still
emerge; zero delivery is per refused frame, and bounded semantic MIME errors
of an approved structure may still reach the pinned decoder.

`abort` ends an unrefused reader with the existing fixed context category,
`operation_canceled` for cancellation, raw or already mapped, and
`command_timed_out` otherwise, and leaves an earlier refusal unchanged. Input
that arrives after an abort is discarded. A source error that is a context
error is such an abort. Any other source error inside a response is a
protocol refusal and one between responses is `bridge_unreachable`. Error
values carry only their stable code, never raw input or source error text.

## Dedicated session stack

`newStructureSessionOver` takes the established, verified and decrypted
transport and builds, from the bottom up:

1. `operationConn` over the transport, which turns every cleared or later
   deadline into the operation deadline and reports a read that fails after
   the operation ended as its context error;
2. `deadlineConn` over it, with the operation deadline set and a context
   watcher that closes the transport for the whole lifetime;
3. `quarantineConn`, whose Read is the structure quarantine for the one
   selected positive UID;
4. `readBudgetConn`, the unchanged per-command budget and SEARCH observer;
5. after STARTTLS only, `syntheticGreetingConn` with the fixed local greeting;
6. `imapclient.New`, whose reader starts at once.

The quarantine is therefore the only reader of server bytes from before the
client exists until the transport closes. The synthetic greeting is a
bounded local constant served above it, so it never carries server bytes
around the quarantine, and it is neither counted nor released by it. There is
no arm, disarm or pool-return interval: the session is never pooled and the
reader's counters span its whole lifetime. A private test seam may place a
tap between the quarantine and `readBudgetConn`; production passes nil.

The pooled constructor `newIMAPSessionOver` shares only `startClient`, which
constructs the client and waits for its greeting and capabilities; its
`deadlineConn`, `dispatchGuardConn` and `readBudgetConn` stack and every
`readSession` method are unchanged.

The session advances once through authenticate, select and dispatch and then
closes. Examine is a read-only EXAMINE and one exact UID SEARCH for the
selected UID. The dispatch records `beginDispatch` and then sends the
session's only FETCH, `UID FETCH <uid> (UID BODYSTRUCTURE)`; its response must
carry that UID once and one extended structure, and may carry FLAGS, in any
order. A repeated or out-of-order call is refused before any write. Close is
one LOGOUT under the operation deadline; Abort ends the quarantine, closes the
transport without draining it and waits for the client reader. Neither
retries or falls back to another session. A failed stage returns the
quarantine's first terminal category when it has one, even if the operation
was canceled after it, and otherwise the mapped client error. `readSelectedStructure` runs one
such session under a single deadline that covers connect, the credential
helper, authentication, selection, the fetch and close.

## Acceptance boundary

`TestStructureSessionLifetime` in
`bridge/structure_session_internal_test.go` proves the stack placement, the
lifetime and the single dispatch, and that validated FLAGS-only frames for an
absent, other or target UID, before and during the dispatch, are consumed,
counted against the session budgets and never take the client's delivery,
with the target structure in UID-first and UID-last order.
`TestStructureSessionTLS` in `bridge/structure_session_tls_test.go` runs the
actual stack and pinned client over verified STARTTLS and implicit TLS
against a private bounded peer, with a literal parameter holding fake FETCH
syntax, CRLF, quotes and parentheses, a quoted parameter over 4096 bytes and
target-UID FLAGS-only frames before the structure, and against the testkit
typed fixture. The decoder-facing tap equals the approved responses exactly.

`TestStructureSessionRefusal` in `bridge/structure_session_internal_test.go`
completes the guard with negative lifecycle schedules over a pipe, with taps
on both sides of the quarantine and channel barriers instead of sleeps:

- `pending_read`: the decoder-facing Read and the quarantine's source Read
  are both outstanding before the target dispatch. A control fragmented
  across writes is withheld until complete and, coalesced with a refused
  prefix, released alone. Write and Close of the transport and inspection of
  the reader state complete while both Reads are outstanding, then the
  refused frame, cancellation, Close, or a source Read returning an approved
  control and an incomplete FETCH together with an error leaves the decoder
  with exactly the approved bytes. The Read issued before the dispatch still
  delivers a fragmented target exactly once.
- `terminal_state`: a bounds refusal held at the decoder boundary while the
  operation is canceled keeps `bounds_exceeded`; cancellation first keeps
  `operation_canceled` through later aborts, dispatches and reads; Abort with
  a mapped category records it; the peer observes the transport close with
  no LOGOUT, drain or replay; a later read of the same endpoint has no
  connection to reuse and a fresh endpoint gets a fresh quarantine; and the
  transport deadline is the operation deadline at every set from
  construction through close.
- `negative_control`: the same tap type below the normal pooled stack
  records the refused frame's bytes, so the zero-delivery witness observes
  real input.

`TestStructureSessionTLSRefusal` in `bridge/structure_session_tls_test.go`
runs `readSelectedStructure`, the actual stack and the pinned client over
verified STARTTLS and implicit TLS against private adverse schedules. Each
refused response follows a consumed FLAGS-only frame and an approved control
coalesced with its first fragment, and is otherwise sent in three fragments.
The cases are wrong, missing and duplicate UID, missing, duplicate, early and
second structure, duplicate FLAGS, bare BODY, BODY sections, BINARY,
BINARY.SIZE, an unknown attribute, a late BODY after a FLAGS-only prefix, a
leading unclosed response-code bracket, malformed FETCH prefixes, truncation,
cancellation during an incomplete FETCH, and one over the depth, token,
atom, frame (quoted and literal), control, session byte, response and work
limits, including a literal declaration sent without payload. For each the
decoder-facing tap equals the approved responses the peer sent, any accepted
target included, with zero bytes of the refused response; the quarantine
consumed exactly the bytes before it plus the offending prefix through the
threshold derived from the limits above; its first category is the returned
code; and the peer observes the client close the transport after the refused
command and nothing more. At-limit responses for depth, tokens, atoms,
frame-filling quoted and literal values, a control, and whole-session
responses and work are released byte for byte and decoded.

This evidence is per refused frame over these executed schedules. It is not
parser equivalence with go-imap and not a MIME-safety claim: earlier
approved frames, and resource-bounded but semantically invalid structures,
still reach the pinned decoder.

## Groundwork and integration boundary

The reader alone is proved in isolation. The dedicated session above wires
it into the client and its lifetime, and the refusal proofs above complete
the guard through the actual client. No public listing uses it yet; that
consumer is separate work.

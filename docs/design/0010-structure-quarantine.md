# Structure-frame quarantine

This record fixes the contract of the private structure-frame quarantine
reader in `bridge/structure_quarantine.go`. The reader is groundwork: it has no
production caller, registers no tool, changes no existing read budget, output
limit or search path, and does not by itself complete guard acceptance.
`TestStructureQuarantinePrimitive` in
`bridge/structure_quarantine_internal_test.go` exercises it over `net.Pipe`
with synthetic input only.

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
that starts like a FETCH but fails this grammar, including a missing, zero,
leading-zero or overflowing sequence number, is a protocol refusal; it never
falls back to control handling.

Controls are whole CRLF lines of at most 4096 bytes and carry no literals:

| Control | Admitted form |
| --- | --- |
| untagged status | `* OK`, `* NO`, `* BAD`, `* BYE`, `* PREAUTH`, optional text |
| CAPABILITY | `* CAPABILITY` and one or more atoms |
| FLAGS | `* FLAGS` and a parenthesized list of flag atoms |
| EXISTS, RECENT | `* n EXISTS`, `* n RECENT` with n zero or more |
| EXPUNGE | `* n EXPUNGE` with n positive |
| SEARCH, ESEARCH | printable ASCII, complete quoted strings, no brace |
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
`operation_canceled` for cancellation and `command_timed_out` otherwise, and
leaves an earlier refusal unchanged. Input that arrives after an abort is
discarded. A source error inside a response is a protocol refusal and one
between responses is `bridge_unreachable`. Error values carry only their
stable code, never raw input or source error text.

## Groundwork and integration boundary

This change proves the reader in isolation. It does not construct a dedicated
session, wrap a TLS connection, handle credentials, synchronize a pending
client read or change any production path. The dedicated-session integration
that follows wires the reader into a client and its lifetime; only that
integration's actual-client negative and pending-read TLS proof completes the
guard, and both must land before any public listing uses it.

# Download confinement

## Invariant

A Drive download must never create, truncate, replace, or write a file outside
an explicitly allowed destination directory. Every destination component must
be resolved without following symlinks, and the authority established by that
resolution must remain bound to descriptors through the write and publication.
An empty allowlist permits no downloads. Uncertain confinement means refusal,
not a best-effort path check.

This extends the [threat model](../THREAT_MODEL.md#drive-cli-subprocess): the
client has a download command, but the server registers no download tool;
local download confinement and write approval are reserved and not shipped.

## Mechanisms

- **Linux:** pin the allowed root with a directory descriptor obtained through
  no-follow traversal. Resolve the relative destination with `openat2`, using
  `RESOLVE_BENEATH | RESOLVE_NO_SYMLINKS` and `O_NOFOLLOW`. `RESOLVE_BENEATH`
  confines resolution to that root; `RESOLVE_NO_SYMLINKS` rejects intermediate
  symlinks as well as the final one, which `O_NOFOLLOW` alone cannot do.
  Keep the resulting descriptors for subsequent I/O. Unsupported syscalls or
  flags and unresolved races are refusals; never retry with weaker flags.
  See the [Linux openat2 manual](https://man7.org/linux/man-pages/man2/openat2.2.html).
- **macOS:** use descriptor-relative `openat`, one component at a time, with
  `O_NOFOLLOW` on every open and `O_DIRECTORY` on directory opens. Reject
  absolute destination paths and `..` components before traversal; each next
  component is opened relative to the pinned preceding directory descriptor.
  Retain the descriptors needed for the write and publication. This follows
  the existing Linux/macOS config loader's component traversal.
- **Unavailable primitives:** a platform without the required descriptor-relative,
  no-follow primitives fails closed before creating or writing anything. This
  includes Linux environments where the required `openat2` operation is
  unavailable. There is no fallback to canonicalize-and-open.

These primitives bind resolution and I/O to objects; they do not freeze the
directory namespace. In particular, neither `openat2` nor `openat` prevents a
directory from being renamed outside the allowed tree after opening. The future
implementation must also prevent an adversary from relocating the destination
or its ancestors throughout writing and publication, or refuse that destination
before writing. A post-write path check or advisory lock is not that guarantee.
If the environment cannot enforce it, downloads there remain unavailable.

## Decision

Choose descriptor-bound confinement: Linux uses the constrained `openat2`
operation above; macOS uses component-wise, no-follow `openat`. Carry the opened
authority through all I/O instead of reopening a checked pathname. The future
tool must control the writer through that authority; passing a validated path
to the existing CLI download command, which resolves it again, is insufficient.

The precedent is [`internal/config/open_unix.go`](../../internal/config/open_unix.go),
which traverses the `--config` path relative to directory descriptors with
`O_NOFOLLOW`, then validates the opened file through descriptor-based `Stat` in
[`internal/config/config.go`](../../internal/config/config.go). The
[README platform contract](../../README.md#platform-support) documents this
discipline; [`open_other.go`](../../internal/config/open_other.go) refuses
unsupported platforms. Reading one already-open config file does not establish
the stronger lifetime guarantee required for a download destination.

Reject check-then-open, including `Lstat`, `EvalSymlinks`, or a cleaned path's
allowlist-prefix check followed by a separate open. A symlink substitution or
rename between those operations can redirect the write. Checking again after
opening or writing cannot undo an escaped write.

## Proof test

The implementation ticket must ship deterministic Go tests on Linux and macOS
using synthetic temporary allowed and outside directories. Synchronization
barriers must place attacks at the relevant operations; timing sleeps are not
proof. Both attacks must be refused:

1. **Symlinked component:** insert an intermediate symlink to the outside
   directory, and separately a final-component symlink to an outside sentinel.
   Include replacement after destination selection but before opening. The
   download must fail without creating, truncating, or modifying outside files.
2. **Rename during the write:** pause a multi-chunk download after its first
   write and attempt to move the destination directory outside the allowed
   root, replacing its old name with a link to an outside directory. The
   confinement mechanism must refuse the escaping rename; if the environment
   permits such relocation, the download must instead have been refused before
   its first write. Continuing to write through a descriptor into a relocated
   directory, or reporting an error only after escape, fails this test. Exercise
   the same constraint on ancestors and through final publication.

Assert outside sentinels are unchanged and no outside output appears, including
partial files. Include an ordinary in-root success case and an unavailable-
primitive refusal so that rejecting every request cannot satisfy the proof.
These are requirements for the future tool, not tests claimed to exist today.

## Reserved

- **Approval model:** not decided here. Who approves a download, when approval
  occurs, and how it is represented remain product decisions; engineering
  process must not silently choose them.
- **Overwrite policy:** not decided here. Handling existing files, collisions,
  and replacement remains reserved.
- **Size and time caps:** not decided here. Byte limits, duration limits, and
  their values remain reserved.

This record changes neither config-key behavior nor any approval mechanism.

## Status

Proposed. This is the design-only record for KO-449; no download tool ships with
it. A follow-up implementation ticket, **Descriptor-confined Drive download
tool** (issue identifier not yet assigned), must implement these mechanisms and
ship the proof tests before registering a download tool. That ticket also needs
explicit product decisions for the reserved questions; this record supplies none.

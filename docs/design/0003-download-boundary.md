# Resolution-time Drive download boundary

## Invariant

A Drive download requires resolution-time confinement beneath an explicitly
allowed root. Every destination component must be resolved without following
symlinks. Output I/O must use the descriptors obtained at resolution, without
pathname re-resolution, through writing and publication. An empty allowlist
permits no downloads. Uncertain resolution-time confinement means refusal.

## Mechanisms

Linux must pin the allowed root through no-follow directory traversal and use
constrained `openat2` with `RESOLVE_BENEATH`, `RESOLVE_NO_SYMLINKS` and
`O_NOFOLLOW` for relative destination resolution. Retain the resulting
descriptors for output I/O.

macOS must use component-wise, descriptor-relative `openat` with `O_NOFOLLOW`
on every open and `O_DIRECTORY` on directory opens. Each component must be
opened relative to the pinned preceding directory descriptor. Reject absolute
destination paths and `..` components before traversal.

Unavailable required primitives, unsupported syscalls or flags, and unresolved
resolution races must cause refusal before any output mutation or writer
invocation. Never retry with weaker flags or fall back to canonicalize-and-open.

## Decision

The maintainer accepts resolution-time confinement and descriptor-bound output
for a single-operator host. This decision supersedes KO-449's unconditional
pre-write refusal gate and relocation-backend prerequisite. Only the lifetime
relocation guarantee is superseded; no-follow resolution, descriptor-bound
output and unavailable-primitive refusal remain required.

Relocation of the allowed root or its ancestors after opening is outside the
threat model and is the operator's responsibility. Croton does not promise to
prevent directory relocation. Descriptors do not prevent rename syscalls or
freeze the filesystem namespace. A descriptor continues to identify its opened
object even if that object is renamed; this is not a lifetime pathname-location
guarantee.

A validated destination pathname must never be reopened, including through the
CLI download command. Reject check-then-open via `Lstat`, `EvalSymlinks` or an
allowlist-prefix check followed by a separate open. Post-write checks cannot
undo an escaped write. The historical requirements remain recorded in
[KO-449](0001-download-confinement.md).

## Proof test

Future implementation proofs must use deterministic Go tests on Linux and
macOS with synthetic temporary allowed and outside directories. These tests
are required future work, not tests already shipped. Use explicit synchronization
barriers at resolution and write boundaries, not timing sleeps.

- Intermediate symlink: refuse traversal of a component pointing to the outside
  directory, including substitution after destination selection before opening.
- Final symlink: refuse a final-component symlink pointing to an outside
  sentinel, including substitution before opening.
- Sub-root rename: pause after resolution, rename an opened subdirectory, and
  replace its old pathname with a different directory or a symlink to an outside
  directory. Refuse to follow the replacement pathname or substituted symlink;
  any continued output must use the original descriptors. This case does not
  require preventing the rename syscall or freezing the namespace.
- Outside sentinel checks: assert outside sentinels remain unchanged and no
  output, including partial files, appears through replacement paths or symlinks.
  Movement of an already-open object is not evidence of following its replacement
  pathname; do not assert lifetime location protection for that object.
- Unavailable primitives: refuse before output mutation or writer invocation
  when required primitives are unavailable.
- Ordinary in-root success: complete output through the resolved descriptors
  when required primitives are available. Rejecting every request cannot satisfy
  this proof.

The documentation verifier witnesses these named requirements structurally;
passing it is not a runtime security proof.

## Reserved

The approval model remains undecided and Reserved. The overwrite policy remains
undecided and Reserved. Size caps remain undecided and Reserved. Time caps remain
undecided and Reserved. These unresolved policies block tool registration.
This record selects none of these policies and changes no config behavior.

## Status

Accepted as a maintainer-approved design decision, with no implementation
shipped. This record registers no download tool and implements no opener.
A future implementation must satisfy the proof requirements and resolve Reserved
before tool registration. The server remains read-only.

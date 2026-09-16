#!/usr/bin/env python3
"""Witness the resolution-time download design, not runtime security.

Uses the existing Markdown parser and comment filter. Requirements are local to
named sections, outside comments and fenced examples. This closed list of
required statements is normalized for whitespace, case and inline code only;
changing the contract vocabulary requires review.
"""

import pathlib
import re
import sys

from verify_docs_design_mail import normalize, prose, visible_lines

DOC = pathlib.Path("docs/design/0003-download-boundary.md")
OLD = pathlib.Path("docs/design/0001-download-confinement.md")
DRIVE = pathlib.Path("docs/DRIVE-MCP.md")
HEADINGS = ("Invariant", "Mechanisms", "Decision", "Proof test", "Reserved", "Status")
REQUIREMENTS = {
    "Invariant": {
        "resolution-time confinement": "A Drive download requires resolution-time confinement beneath an explicitly allowed root.",
        "no symlink following": "Every destination component must be resolved without following symlinks.",
        "descriptor-bound output without pathname re-resolution": "Output I/O must use the descriptors obtained at resolution, without pathname re-resolution, through writing and publication.",
        "empty allowlist refusal": "An empty allowlist permits no downloads.",
        "uncertain confinement refusal": "Uncertain resolution-time confinement means refusal.",
    },
    "Mechanisms": {
        "Linux constrained openat2 and required flags": "Linux must pin the allowed root through no-follow directory traversal and use constrained openat2 with RESOLVE_BENEATH, RESOLVE_NO_SYMLINKS and O_NOFOLLOW for relative destination resolution.",
        "retained descriptors": "Retain the resulting descriptors for output I/O.",
        "macOS component-wise openat and required flags": "macOS must use component-wise, descriptor-relative openat with O_NOFOLLOW on every open and O_DIRECTORY on directory opens.",
        "pinned component traversal": "Each component must be opened relative to the pinned preceding directory descriptor.",
        "absolute and parent traversal refusal": "Reject absolute destination paths and `..` components before traversal.",
        "unavailable-primitive refusal": "Unavailable required primitives, unsupported syscalls or flags, and unresolved resolution races must cause refusal before any output mutation or writer invocation.",
        "no weakened fallback": "Never retry with weaker flags or fall back to canonicalize-and-open.",
    },
    "Decision": {
        "accepted single-operator boundary": "The maintainer accepts resolution-time confinement and descriptor-bound output for a single-operator host.",
        "superseded refusal gate and relocation prerequisite": "This decision supersedes KO-449's unconditional pre-write refusal gate and relocation-backend prerequisite.",
        "limited supersession": "Only the lifetime relocation guarantee is superseded; no-follow resolution, descriptor-bound output and unavailable-primitive refusal remain required.",
        "operator responsibility for root and ancestor relocation": "Relocation of the allowed root or its ancestors after opening is outside the threat model and is the operator's responsibility.",
        "no relocation prevention promise": "Croton does not promise to prevent directory relocation.",
        "no namespace freeze": "Descriptors do not prevent rename syscalls or freeze the filesystem namespace.",
        "no CLI pathname reopening": "A validated destination pathname must never be reopened, including through the CLI download command.",
    },
    "Proof test": {
        "future synthetic platform proofs": "Future implementation proofs must use deterministic Go tests on Linux and macOS with synthetic temporary allowed and outside directories.",
        "no shipped proof claim": "These tests are required future work, not tests already shipped.",
        "deterministic synchronization without sleeps": "Use explicit synchronization barriers at resolution and write boundaries, not timing sleeps.",
        "intermediate symlink refusal": "Intermediate symlink: refuse traversal of a component pointing to the outside directory, including substitution after destination selection before opening.",
        "final symlink refusal": "Final symlink: refuse a final-component symlink pointing to an outside sentinel, including substitution before opening.",
        "sub-root rename and replacement setup": "Sub-root rename: pause after resolution, rename an opened subdirectory, and replace its old pathname with a different directory or a symlink to an outside directory.",
        "replacement-path refusal": "Refuse to follow the replacement pathname or substituted symlink; any continued output must use the original descriptors.",
        "rename proof scope": "This case does not require preventing the rename syscall or freezing the namespace.",
        "outside sentinel checks": "Outside sentinel checks: assert outside sentinels remain unchanged and no output, including partial files, appears through replacement paths or symlinks.",
        "unavailable primitives case": "Unavailable primitives: refuse before output mutation or writer invocation when required primitives are unavailable.",
        "ordinary in-root success": "Ordinary in-root success: complete output through the resolved descriptors when required primitives are available.",
        "no refuse-all proof": "Rejecting every request cannot satisfy this proof.",
        "structural witness only": "The documentation verifier witnesses these named requirements structurally; passing it is not a runtime security proof.",
    },
    "Reserved": {
        "undecided approval model": "The approval model remains undecided and Reserved.",
        "undecided overwrite policy": "The overwrite policy remains undecided and Reserved.",
        "undecided size caps": "Size caps remain undecided and Reserved.",
        "undecided time caps": "Time caps remain undecided and Reserved.",
        "registration blocked": "These unresolved policies block tool registration.",
        "no policy or config change": "This record selects none of these policies and changes no config behavior.",
    },
    "Status": {
        "accepted design without implementation": "Accepted as a maintainer-approved design decision, with no implementation shipped.",
        "no tool or opener": "This record registers no download tool and implements no opener.",
        "future registration prerequisites": "A future implementation must satisfy the proof requirements and resolve Reserved before tool registration.",
        "read-only server": "The server remains read-only.",
    },
}


def sections(lines):
    headings = []
    bodies = {}
    current = None
    for line in lines:
        heading = line.heading()
        if heading is not None:
            current = None
            level, title = heading
            if level >= 2:
                headings.append(heading)
                current = title
                bodies.setdefault(title, [])
        elif current is not None:
            bodies[current].append(line)

    return headings, bodies


def verify(doc=DOC, old=OLD, drive=DRIVE):
    failures = []
    contents = {}
    for path in (doc, old, drive):
        try:
            contents[path] = visible_lines(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError) as error:
            failures.append(f"{path}: cannot read documentation ({type(error).__name__})")

    if failures:
        return failures

    headings, bodies = sections(contents[doc])
    if headings != [(2, title) for title in HEADINGS]:
        failures.append(f"{doc}: expected exactly six ordered ## headings: {', '.join(HEADINGS)}")

    for section, requirements in REQUIREMENTS.items():
        body = prose(bodies.get(section, []))
        for name, statement in requirements.items():
            if normalize(statement) not in body:
                failures.append(f"{doc} {section}: missing {name}")

    _, old_bodies = sections(contents[old])
    status = prose(old_bodies.get("Status", []))
    if "superseded-in-part" not in status:
        failures.append(f"{old} Status: missing superseded-in-part status")
    if not re.search(r"\[[^\]]+\]\(0003-download-boundary\.md\)", status):
        failures.append(f"{old} Status: missing successor link")

    _, drive_bodies = sections(contents[drive])
    # Check the actual configuration paragraph, not an unrelated page mention.
    running = "\n".join(
        line.text if line.fence is None and not line.opens else ""
        for line in drive_bodies.get("Running", [])
    )
    paragraphs = [normalize(p) for p in re.split(r"\n\s*\n", running)]
    paragraph = next((p for p in paragraphs if p.startswith("alloweddownloaddirectories")), "")
    drive_requirements = {
        "reserved keys and no shipped tools": "allowedDownloadDirectories and writes.enabled are reserved: the server registers no download or write tools.",
        "operator responsibility for root and ancestor relocation": "Under the accepted design, relocation of the allowed root or its ancestors after opening is the operator's responsibility, outside Croton's threat model.",
        "no shipped download implementation": "no download implementation ships.",
    }
    for name, statement in drive_requirements.items():
        if normalize(statement) not in paragraph:
            failures.append(f"{drive} allowedDownloadDirectories paragraph: missing {name}")
    if not re.search(r"\[[^\]]+\]\(design/0003-download-boundary\.md\)", paragraph):
        failures.append(f"{drive} allowedDownloadDirectories paragraph: missing successor link")

    return failures


def main():
    failures = verify()
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("docs download boundary design OK (structural witness only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

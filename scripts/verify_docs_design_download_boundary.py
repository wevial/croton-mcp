#!/usr/bin/env python3
"""Witness the resolution-time download design, not runtime security.

Uses the existing Markdown parser and comment filter. Requirements are local to
named sections, outside comments and fenced examples. Closed sets of complete
statements reject extra or qualified claims. The contract vocabulary is normalized
for whitespace, case and inline code only;
changing the contract vocabulary requires review.
"""

import pathlib
import re
import sys

from verify_docs_design_mail import normalize, visible_lines

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
        "client-managed per-call confirmation": "Operators must configure client-managed per-call confirmation before enabling downloads.",
        "configured-client trust boundary": "Croton accepts tools/call from the configured client and cannot verify that a human confirmed.",
        "no server prompt": "Croton never prompts for confirmation itself.",
        "annotations do not enforce confirmation": "The annotations readOnlyHint false and destructiveHint false do not force a client prompt or prove confirmation.",
        "auto-approval permits unattended writes": "An auto-approving client permits unattended local writes inside the allowed root.",
        "visible source destination and known size": "The human-facing request must identify the Drive source path, destination under the allowed root and size when metadata supplies it.",
        "no request secrets": "The human-facing request must never include credentials or share passwords.",
        "no agent consent argument": "There is no approved tool argument or other agent-supplied consent assertion.",
        "no consent audit field": "There is no consent audit field.",
        "disabled by default": "The download.enabled setting defaults to false.",
        "no disabled tool": "While disabled, no download tool is registered.",
        "explicit operator opt-in": "Explicit operator opt-in accepts the configured-client trust boundary.",
        "pre-registration startup refusal": "Until the registration implementation ships, download.enabled true must fail startup rather than enable a partial capability.",
        "accepted disabled config": "The current config schema accepts download.enabled, download.maxBytes and download.timeoutSeconds, but rejects enabled downloads with a static ErrConfigInvalid before CLI startup, regardless of allowed roots.",
        "parsed limit defaults": "Omitted limits default to 256 MiB and 120 seconds.",
        "parsed limit bounds": "Explicit limits must be positive signed-64-bit integers; timeout conversion to time.Duration must not overflow.",
        "no limit clamping": "Invalid limits are rejected, never clamped.",
        "refuse on exists": "Downloads must refuse an existing destination.",
        "no overwrite flag": "There is no overwrite flag.",
        "configurable byte cap": "The configurable byte cap defaults to 256 MiB.",
        "configurable time cap": "The configurable time cap defaults to 120 seconds.",
        "known-limit precheck": "Known limits must be checked before the first write.",
        "mid-download limit cleanup": "A limit reached mid-download must stop the download and delete partial output.",
        "future local audit line": "A future local audit line per download must identify source, destination and outcome.",
        "no audit secrets or consent claim": "That audit line must never claim consent or include credentials or share passwords.",
        "path privacy": "Source and destination paths can reveal sensitive names and activity; operators must protect access to and retention of these future local logs.",
        "future runtime controls": "These controls require future runtime implementation and are not supplied by WriteFresh.",
    },
    "Proof test": {
        "future synthetic platform proofs": "Future download integration proofs must use deterministic Go tests on Linux and macOS with synthetic temporary allowed and outside directories.",
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
        "only per-session approval reserved": "Only per-session approval remains Reserved.",
    },
    "Status": {
        "accepted policy record": "Accepted as a maintainer-approved design and policy record.",
        "shipped confined opener": "The internal confined opener WriteFresh has shipped without a production call site.",
        "unshipped registration and enforcement": "Download tool registration and policy enforcement have not shipped.",
        "record shipment before registration": "Registration remains held until this record ships and a later integration proves the required controls.",
        "descriptor-bound writer and cleanup proof": "Before registration, integration must prove a supported descriptor-bound writer and temporary-publication path, partial-output cleanup and all required runtime controls without reopening the validated destination pathname.",
        "WriteFresh is not cleanup proof": "WriteFresh alone does not provide partial-output cleanup or publication.",
        "CLI download disabled": "The existing CLI Download method remains disabled by the invocation allowlist.",
        "read-only server": "The server remains read-only.",
    },
}


CONTEXT = {
    "Decision": (
        "A descriptor continues to identify its opened object even if that object is renamed; this is not a lifetime pathname-location guarantee.",
        "Reject check-then-open via `Lstat`, `EvalSymlinks` or an allowlist-prefix check followed by a separate open.",
        "Post-write checks cannot undo an escaped write.",
        "The historical requirements remain recorded in [KO-449](0001-download-confinement.md).",
    ),
    "Proof test": (
        "Movement of an already-open object is not evidence of following its replacement pathname; do not assert lifetime location protection for that object.",
    ),
}
OLD_REQUIREMENTS = {
    "superseded-in-part status and successor link": "Superseded-in-part by the maintainer-approved [resolution-time Drive download boundary](0003-download-boundary.md).",
    "superseded lifetime contract": "The successor replaces the lifetime relocation guarantee, unconditional pre-write refusal gate and relocation-backend prerequisite with resolution-time confinement and descriptor-bound output.",
    "historical body": "The body above preserves the historical KO-449 proposal; its superseded requirements are not the current boundary.",
    "no shipped tool or opener": "No download tool or opener ships with either record.",
    "reserved policies block registration": "Approval, overwrite, size and time cap policies remain Reserved and block tool registration.",
}
DRIVE_REQUIREMENTS = {
    "reserved keys and no shipped tools": "allowedDownloadDirectories and writes.enabled are reserved: the server registers no download or write tools.",
    "operator responsibility for root and ancestor relocation": "Under the accepted design, relocation of the allowed root or its ancestors after opening is the operator's responsibility, outside Croton's threat model.",
    "successor link and no shipped download tool": "See the [resolution-time download boundary](design/0003-download-boundary.md) for the accepted confinement and download policies; no download tool ships.",
}
DRIVE_CONTEXT = (
    "The file carries no credentials and the server never reads any; authentication is the CLI's own concern, and a CLI that reports it needs authentication surfaces as `unavailable`.",
)


def check_statements(lines, requirements, context, label, failures):
    # Strip only line-leading list markers; keep negations and qualifications.
    body = normalize(" ".join(
        re.sub(r"^\s*[-+*]\s+", "", line.text)
        for line in lines if line.fence is None and not line.opens
    ))
    # A terminator is a single period followed by whitespace or end of prose.
    # Dots inside links, writes.enabled and the `..` component are not boundaries.
    statements = set(re.split(r"(?<=[^.]\.)\s+", body)) - {""}
    allowed = {normalize(statement) for statement in context}
    for name, statement in requirements.items():
        expected = normalize(statement)
        allowed.add(expected)
        if expected not in statements:
            failures.append(f"{label}: missing {name}")

    if statements - allowed:
        failures.append(f"{label}: unexpected contract statement; only approved complete statements are allowed")


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
        check_statements(
            bodies.get(section, []), requirements, CONTEXT.get(section, ()),
            f"{doc} {section}", failures,
        )

    _, old_bodies = sections(contents[old])
    check_statements(
        old_bodies.get("Status", []), OLD_REQUIREMENTS, (),
        f"{old} Status", failures,
    )

    _, drive_bodies = sections(contents[drive])
    # Check the actual configuration paragraph, not an unrelated page mention.
    running = "\n".join(
        line.text if line.fence is None and not line.opens else ""
        for line in drive_bodies.get("Running", [])
    )
    paragraphs = re.split(r"\n\s*\n", running)
    matching = [p for p in paragraphs if normalize(p).startswith("alloweddownloaddirectories")]
    if len(matching) != 1:
        failures.append(f"{drive}: expected exactly one allowedDownloadDirectories paragraph")

    check_statements(
        visible_lines(matching[0] if matching else ""), DRIVE_REQUIREMENTS, DRIVE_CONTEXT,
        f"{drive} allowedDownloadDirectories paragraph", failures,
    )

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

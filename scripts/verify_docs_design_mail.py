#!/usr/bin/env python3
"""Structurally check the Mail proposal; this does not prove mutation safety.

Run from the repository root. Required statements are normalized for whitespace,
case and inline code, but must occur in their named section outside examples and
HTML comments. Failures name the missing contract requirement.
Invariant, Reserved and Status use closed sets of complete sentences to reject
contradictory claims;
changing that vocabulary requires review of this structural contract.
"""

import pathlib
import re
import sys

from verify_docs_architecture import fence_marker, parse, table_rows

DOC = pathlib.Path("docs/design/0002-mail-mutation.md")
MCP = pathlib.Path("docs/MCP.md")
HEADINGS = (
    "Invariant", "Proposed operations", "Fail-closed behavior",
    "Proof requirements", "Reserved", "Status",
)
OPERATIONS = {
    "flag": "Set or clear the IMAP Flagged flag",
    "mark read": "Set or clear the IMAP Seen flag",
    "move": "Move explicitly selected messages to an existing destination mailbox",
}
REQUIREMENTS = {
    "Invariant": {
        "no authorization": "No mutation is authorized by this record.",
        "shipped read-only": "The shipped Mail implementation remains read-only.",
    },
    "Proposed operations": {
        "UID identity": "Identity must be UID-based and bound to the source mailbox and UIDVALIDITY.",
        "UIDVALIDITY check": "Check UIDVALIDITY against a fresh server value before any mutation",
        "exclusions": "Excluded operations: sending, drafts, permanent deletion, arbitrary flags and implicit expunge.",
        "no fallback": "No fallback copy/delete/expunge sequence is prescribed.",
    },
    "Fail-closed behavior": {
        "no valid approval": "No valid approval: fail closed before any mutation.",
        "stale identity": "Stale identity: fail closed before any mutation",
        "invalid destination": "Invalid destination: fail closed before any mutation",
        "unsupported operation": "Unsupported operation: fail closed before any mutation",
        "ambiguous completion": "Ambiguous completion: fail closed without reporting success.",
        "no blind replay": "An ambiguous mutation must not be blindly replayed.",
    },
    "Proof requirements": {
        "future synthetic tests": "Future synthetic tests must prove the proposed boundary before writes can be enabled.",
        "no existing coverage claim": "These tests are not claimed to exist today.",
    },
    "Reserved": {
        "approval authority": "Approval authority remains undecided: who may authorize a mutation.",
        "approval lifetime": "Approval lifetime remains undecided: per-call versus session approval.",
        "confirmation surface": (
            "User-visible confirmation surface remains undecided: where and how the user "
            "would see and confirm the selected operation, messages and destination."
        ),
        "Reserved model": "The approval model is Reserved.",
    },
    "Status": {
        "proposed read-only status": "Proposed. Mail remains read-only.",
        "no registration": "No Mail mutation tools are registered by this record.",
    },
}
STATUS_CONTEXT = (
    "Implementation requires a separate decision resolving Reserved and future "
    "proof tests; publication of this proposal does not enable writes."
)

CLOSED_CONTEXT = {
    "Invariant": ("Bridge connections remain loopback-only with TLS.",),
    "Reserved": (
        "Neither this record nor engineering choices select an approval mechanism "
        "or change config behavior.",
    ),
    "Status": (STATUS_CONTEXT,),
}


def visible_lines(text):
    """Strip real comments while preserving literal comment markers in code."""
    visible = []
    fence = None
    offset = 0
    while offset < len(text):
        if offset == 0 or text[offset - 1] == "\n":
            end = text.find("\n", offset)
            end = len(text) if end == -1 else end + 1
            raw = text[offset:end]
            marker = fence_marker(raw)
            if fence is not None or marker is not None:
                if fence is None:
                    fence = marker
                elif marker is not None and marker[0] == fence[0] and marker[1] >= fence[1] and not marker[2]:
                    fence = None

                visible.append(raw)
                offset = end
                continue

        if text.startswith("<!--", offset):
            end = text.find("-->", offset + 4)
            end = len(text) if end == -1 else end + 3
            # Preserve boundaries so hidden comments cannot manufacture rows.
            visible.append("\n" * text[offset:end].count("\n"))
            offset = end
            continue

        if text[offset] == "\\" and offset + 1 < len(text) and text[offset + 1] in "`\\":
            visible.append(text[offset:offset + 2])
            offset += 2
            continue

        if text[offset] == "`":
            opener = re.match(r"`+", text[offset:])[0]
            end = offset + len(opener)
            closer = re.search(r"(?<!`)" + opener + r"(?!`)", text[end:])
            if closer is not None:
                end += closer.end()

            visible.append(text[offset:end])
            offset = end
            continue

        visible.append(text[offset])
        offset += 1

    return parse("".join(visible))


def normalize(text):
    return " ".join(text.replace("`", "").lower().split())


def prose(lines):
    return normalize(" ".join(line.text for line in lines if line.fence is None and not line.opens))


def verify(doc=DOC, mcp=MCP):
    failures = []
    contents = {}
    for path in (doc, mcp):
        try:
            contents[path] = visible_lines(path.read_text(encoding="utf-8"))
        except (OSError, UnicodeError) as error:
            failures.append(f"{path}: cannot read documentation ({type(error).__name__})")

    if failures:
        return failures

    headings = []
    sections = {}
    current = None
    for line in contents[doc]:
        heading = line.heading()
        if heading is not None:
            level, title = heading
            current = None
            if level >= 2:
                headings.append((level, title))
                current = title
                sections.setdefault(title, [])
        elif current is not None:
            sections[current].append(line)

    if headings != [(2, title) for title in HEADINGS]:
        failures.append(f"{doc}: headings: expected exactly six ordered ## headings: {', '.join(HEADINGS)}")

    for section, requirements in REQUIREMENTS.items():
        body = prose(sections.get(section, []))
        # Authorization-sensitive sections have closed vocabularies: substring
        # matches cannot reject negation, qualifications or appended write claims.
        # Strip only line-leading list markers before checking complete sentences.
        if section in CLOSED_CONTEXT:
            body = normalize(" ".join(
                re.sub(r"^\s*[-+*]\s+", "", line.text)
                for line in sections.get(section, [])
                if line.fence is None and not line.opens
            ))

        sentences = re.findall(r"[^.]+(?:\.|$)", body)
        sentences = [sentence.strip() for sentence in sentences]
        for name, statement in requirements.items():
            expected = normalize(statement)
            present = expected in body
            if section in CLOSED_CONTEXT:
                present = all(part.strip() + "." in sentences for part in expected.split(".") if part.strip())

            if not present:
                failures.append(f"{doc} {section}: missing {name}")

        if section in CLOSED_CONTEXT:
            allowed = {normalize(statement) for statement in CLOSED_CONTEXT[section]}
            for statement in requirements.values():
                allowed.update(part.strip() + "." for part in normalize(statement).split(".") if part.strip())

            if any(sentence not in allowed for sentence in sentences):
                label = "status" if section == "Status" else "authorization"
                failures.append(f"{doc} {section}: unexpected {label} statement; only the proposed read-only contract is allowed")

    rows = table_rows(sections.get("Proposed operations", []))
    names = [normalize(row[0]) for row in rows]
    if sorted(names) != sorted(OPERATIONS):
        failures.append(f"{doc} Proposed operations: operation rows must be exactly flag, mark read, move")

    for name, meaning in OPERATIONS.items():
        matching = [row for row in rows if normalize(row[0]) == name]
        if len(matching) != 1 or len(matching[0]) != 2 or normalize(meaning) not in normalize(matching[0][1]):
            failures.append(f"{doc} Proposed operations: missing or invalid {name} row")

    # Require the proposal label in the link itself, not unrelated page prose.
    links = re.findall(r"\[([^\]]+)\]\(design/0002-mail-mutation\.md\)", prose(contents[mcp]))
    if not any(re.search(r"\bproposed\b", label) for label in links):
        failures.append(f"{mcp}: missing proposed Mail design link to design/0002-mail-mutation.md")

    return failures


def main(doc=DOC, mcp=MCP):
    failures = verify(doc, mcp)
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("docs Mail mutation proposal OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

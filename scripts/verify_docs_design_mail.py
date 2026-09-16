#!/usr/bin/env python3
"""Structurally check the Mail proposal; this does not prove mutation safety.

Run from the repository root. Required statements are normalized for whitespace,
case and inline code, but must occur in their named section outside examples and
HTML comments. Failures name the missing contract requirement.
"""

import pathlib
import re
import sys

from verify_docs_architecture import parse, table_rows

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
        "confirmation surface": "User-visible confirmation surface remains undecided:",
        "Reserved model": "The approval model is Reserved.",
    },
    "Status": {
        "proposed read-only status": "Proposed. Mail remains read-only.",
        "no registration": "No Mail mutation tools are registered by this record.",
    },
}


def visible_lines(text):
    # Preserve line boundaries so hiding comments cannot manufacture table rows.
    text = re.sub(r"<!--.*?(?:-->|\Z)", lambda m: "\n" * m[0].count("\n"), text, flags=re.S)
    return parse(text)


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
        for name, statement in requirements.items():
            if normalize(statement) not in body:
                failures.append(f"{doc} {section}: missing {name}")

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

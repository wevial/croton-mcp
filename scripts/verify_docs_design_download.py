#!/usr/bin/env python3
"""Verify the structure of docs/design/0001-download-confinement.md.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import sys

from verify_docs_architecture import parse

DOC = pathlib.Path("docs/design/0001-download-confinement.md")
HEADINGS = ("Invariant", "Mechanisms", "Decision", "Proof test", "Reserved", "Status")
MECHANISM_TOKENS = ("Linux", "openat2", "RESOLVE_BENEATH", "macOS", "openat", "O_NOFOLLOW", "fails closed")
RESERVED_WORDS = ("approval", "overwrite", "size", "time", "caps")


def main():
    failures = []
    if not DOC.is_file():
        print(f"FAIL: {DOC}: file not found", file=sys.stderr)
        return 1

    lines = parse(DOC.read_text(encoding="utf-8"))
    headings = []
    sections = {}
    current = None
    for line in lines:
        heading = line.heading()
        if heading is not None:
            level, title = heading
            current = None
            if level >= 2:
                headings.append((level, title))
                current = title
                sections.setdefault(title, [])
        elif current is not None and line.fence is None and not line.opens:
            sections[current].append(line.text)

    if headings != [(2, title) for title in HEADINGS]:
        failures.append(f"{DOC}: expected exactly these six ## headings in order: {', '.join(HEADINGS)}; found {headings!r}")

    mechanisms = " ".join(" ".join(sections.get("Mechanisms", [])).split())
    for token in MECHANISM_TOKENS:
        if not re.search(r"\b" + re.escape(token) + r"\b", mechanisms):
            failures.append(f"{DOC} Mechanisms: missing {token!r}")

    reserved = " ".join(sections.get("Reserved", [])).lower()
    for word in RESERVED_WORDS:
        if not re.search(r"\b" + word + r"\b", reserved):
            failures.append(f"{DOC} Reserved: missing {word!r}")

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("docs download confinement design OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

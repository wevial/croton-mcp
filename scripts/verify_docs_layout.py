#!/usr/bin/env python3
"""Verify that the repository docs describe the tree.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import subprocess
import sys

STALE_README_PHRASES = ("currently empty", "scaffold")
STALE_FACADE_PHRASE = "package-private read-only IMAP facade"
EXECUTABLES = ("croton-mcp", "croton-drive-mcp")


def go_package_dirs():
    out = subprocess.run(
        ["git", "ls-files", "--", "*.go"], check=True, capture_output=True, text=True
    ).stdout
    return sorted({str(pathlib.PurePosixPath(f).parent) for f in out.split() if f})


def section(text, heading_re, name, failures):
    match = re.search(heading_re, text, re.MULTILINE)
    if match is None:
        failures.append(f"{name}: heading matching {heading_re!r} not found")
        return ""
    rest = text[match.end():]
    nxt = re.search(r"^## ", rest, re.MULTILINE)
    return rest if nxt is None else rest[: nxt.start()]


def check_readme(failures):
    text = pathlib.Path("README.md").read_text(encoding="utf-8")
    for phrase in STALE_README_PHRASES:
        if phrase in text:
            failures.append(f"README.md: stale phrase {phrase!r} present")
    layout = section(text, r"^## Layout\s*$", "README.md", failures)
    bullets = {}
    for m in re.finditer(r"^- `([^`]+)`:(.*)$", layout, re.MULTILINE):
        bullets[m.group(1)] = m.group(2).strip()
    for d in go_package_dirs():
        if d not in bullets:
            failures.append(f"README.md Layout: no bullet for package directory {d!r}")
        elif not bullets[d]:
            failures.append(f"README.md Layout: bullet for {d!r} has an empty role")


def check_dependencies(failures):
    text = pathlib.Path("docs/DEPENDENCIES.md").read_text(encoding="utf-8")
    sec = section(text, r"^## .*go-imap.*$", "docs/DEPENDENCIES.md", failures)
    if not sec:
        return
    if "`bridge`" not in sec:
        failures.append("docs/DEPENDENCIES.md go-imap section: does not name `bridge`")
    if STALE_FACADE_PHRASE in sec:
        failures.append(
            f"docs/DEPENDENCIES.md go-imap section: stale phrase {STALE_FACADE_PHRASE!r} present"
        )


def check_agents(failures):
    lines = pathlib.Path("AGENTS.md").read_text(encoding="utf-8").splitlines()
    opening = next((l for l in lines if l.strip() and not l.startswith("#")), "")
    first_sentence = re.split(r"(?<=[.!?])\s", opening.strip(), maxsplit=1)[0]
    for name in EXECUTABLES:
        if not re.search(r"\b" + re.escape(name) + r"\b", first_sentence):
            failures.append(f"AGENTS.md: opening sentence does not name {name!r}")


def main():
    failures = []
    check_readme(failures)
    check_dependencies(failures)
    check_agents(failures)
    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("docs layout OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

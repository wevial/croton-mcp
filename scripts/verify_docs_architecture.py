#!/usr/bin/env python3
"""Verify the structure of docs/ARCHITECTURE.md against the tree.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import subprocess
import sys

DOC = pathlib.Path("docs/ARCHITECTURE.md")
SECTIONS = ("Product form", "Processes", "Packages", "Vision to tree", "Outside the module")
PROCESSES = ("croton-mcp", "croton-drive-mcp")
VISION_ANCHORS = ("stdio", "separately runnable", "MCP-neutral", "unofficial", "account material")


def go_package_dirs():
    out = subprocess.run(
        ["git", "ls-files", "--", "*.go"], check=True, capture_output=True, text=True
    ).stdout
    return sorted({str(pathlib.PurePosixPath(f).parent) for f in out.split() if f})


def section(text, title, failures):
    match = re.search(r"^## " + re.escape(title) + r"\s*$", text, re.MULTILINE)
    if match is None:
        failures.append(f"{DOC}: heading '## {title}' not found")
        return None
    rest = text[match.end():]
    nxt = re.search(r"^#{1,2} ", rest, re.MULTILINE)
    return rest if nxt is None else rest[: nxt.start()]


def subsections(text):
    """Split a section into (heading, body) pairs for each ### heading."""
    parts = re.split(r"^### (.*)$", text, flags=re.MULTILINE)
    return [(parts[i].strip(), parts[i + 1]) for i in range(1, len(parts), 2)]


def check_mermaid(name, body, failures):
    lines = body.splitlines()
    opening = next((i for i, l in enumerate(lines) if re.match(r"^```mermaid\s*$", l)), None)
    if opening is None:
        failures.append(f"{DOC} {name}: no ```mermaid fence")
        return
    inner = []
    closed = False
    for line in lines[opening + 1 :]:
        if line.startswith("#"):
            break
        if re.match(r"^```\s*$", line):
            closed = True
            break
        inner.append(line)
    if not closed:
        failures.append(f"{DOC} {name}: mermaid fence is not closed before the next heading")
    nonblank = [l for l in inner if l.strip()]
    if not nonblank or nonblank[0].strip() != "flowchart LR":
        failures.append(f"{DOC} {name}: mermaid fence does not open with 'flowchart LR'")
    if not any("-->" in l for l in inner):
        failures.append(f"{DOC} {name}: mermaid fence has no '-->' edge")


def check_processes(text, failures):
    sec = section(text, "Processes", failures)
    if sec is None:
        return
    subs = subsections(sec)
    for name in PROCESSES:
        matches = [
            (h, b) for h, b in subs if re.search(r"(?<![\w-])" + re.escape(name) + r"(?![\w-])", h)
        ]
        if not matches:
            failures.append(f"{DOC} Processes: no ### subsection naming {name!r}")
            continue
        check_mermaid(f"### {matches[0][0]}", matches[0][1], failures)


def table_rows(sec):
    """Collect table rows from a section, ignoring anything inside a backtick or tilde code fence."""
    rows = []
    fence = None  # (character, length) of the open fence, per CommonMark
    for line in sec.splitlines():
        stripped = line.strip()
        m = re.match(r"^(`{3,}|~{3,})", stripped)
        if m:
            marker = m.group(1)
            if fence is None:
                fence = (marker[0], len(marker))
                continue
            if marker[0] == fence[0] and len(marker) >= fence[1]:
                fence = None
                continue
        if fence is not None or not stripped.startswith("|"):
            continue
        cells = [c.strip() for c in stripped.strip("|").split("|")]
        if all(re.fullmatch(r":?-{3,}:?", c) for c in cells if c):
            continue
        rows.append(cells)
    return rows[1:] if rows else []


def check_packages(text, failures):
    sec = section(text, "Packages", failures)
    if sec is None:
        return
    rows = table_rows(sec)
    for d in go_package_dirs():
        matching = [r for r in rows if r and f"`{d}`" in r[0]]
        if not matching:
            failures.append(f"{DOC} Packages: no table row for package directory {d!r}")
            continue
        row = matching[0]
        if len(row) < 3 or not row[1]:
            failures.append(f"{DOC} Packages: row for {d!r} has an empty role cell")
        if len(row) < 3 or not row[2]:
            failures.append(f"{DOC} Packages: row for {d!r} has an empty 'must never' cell")


def check_vision(text, failures):
    sec = section(text, "Vision to tree", failures)
    if sec is None:
        return
    rows = table_rows(sec)
    for anchor in VISION_ANCHORS:
        matching = [r for r in rows if r and anchor in r[0]]
        if not matching:
            failures.append(f"{DOC} Vision to tree: no table row containing {anchor!r}")
            continue
        row = matching[0]
        if len(row) < 2 or not row[1]:
            failures.append(f"{DOC} Vision to tree: row for {anchor!r} has an empty 'where' cell")


def main():
    failures = []
    if not DOC.is_file():
        print(f"FAIL: {DOC} not found", file=sys.stderr)
        return 1
    text = DOC.read_text(encoding="utf-8")
    for title in SECTIONS:
        section(text, title, failures)
    check_processes(text, failures)
    check_packages(text, failures)
    check_vision(text, failures)
    failures = list(dict.fromkeys(failures))
    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("docs architecture OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

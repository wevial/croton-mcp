#!/usr/bin/env python3
"""Verify the structure of docs/ARCHITECTURE.md against the tree.

Run from the repository root. Exits nonzero and names every failure.

Headings, tables and diagrams are only recognized outside enclosing code
fences, so a document whose structure has been turned into literal text by
a surrounding fence fails rather than passes. Tables must be real GFM
tables: a header row followed by a delimiter row with the same column count.
"""

import pathlib
import re
import subprocess
import sys

DOC = pathlib.Path("docs/ARCHITECTURE.md")
SECTIONS = ("Product form", "Processes", "Packages", "Vision to tree", "Outside the module")
PROCESSES = ("croton-mcp", "croton-drive-mcp")
VISION_ANCHORS = ("stdio", "separately runnable", "MCP-neutral", "unofficial", "account material")

FENCE = re.compile(r"^(`{3,}|~{3,})(.*)$")
HEADING = re.compile(r"^(#{1,6}) (.*?)\s*#*\s*$")
DELIMITER_CELL = re.compile(r":?-+:?")


def go_package_dirs():
    out = subprocess.run(
        ["git", "ls-files", "--", "*.go"], check=True, capture_output=True, text=True
    ).stdout
    return sorted({str(pathlib.PurePosixPath(f).parent) for f in out.split() if f})


def fence_marker(line):
    """Return (character, length, info string) when a line opens or closes a code fence."""
    m = FENCE.match(line.strip())
    if m is None:
        return None
    marker, info = m.group(1), m.group(2).strip()
    if marker[0] == "`" and "`" in info:
        return None  # CommonMark: a backtick fence's info string may not contain backticks
    return marker[0], len(marker), info


class Line:
    """One source line with its CommonMark fence context.

    ``fence`` is the (character, length, info) of the fence enclosing this
    line, or None at top level. ``opens`` is set on the line that opens a
    fence and ``closes`` on the line that closes one.
    """

    __slots__ = ("text", "fence", "opens", "closes")

    def __init__(self, text, fence, opens, closes):
        self.text, self.fence, self.opens, self.closes = text, fence, opens, closes

    def heading(self):
        """Return (level, title) when this line is a top-level ATX heading."""
        if self.fence is not None or self.opens:
            return None
        m = HEADING.match(self.text)
        return (len(m.group(1)), m.group(2)) if m else None


def parse(text):
    """Annotate every line with the fence enclosing it, per CommonMark."""
    lines = []
    fence = None
    for raw in text.splitlines():
        marker = fence_marker(raw)
        if fence is None:
            if marker is not None:
                fence = marker
                lines.append(Line(raw, None, True, False))
                continue
            lines.append(Line(raw, None, False, False))
        else:
            char, length, _ = fence
            if marker is not None and marker[0] == char and marker[1] >= length and not marker[2]:
                lines.append(Line(raw, fence, False, True))
                fence = None
                continue
            lines.append(Line(raw, fence, False, False))
    return lines


def section(lines, title, failures):
    """Return the lines under '## <title>' up to the next # or ## heading."""
    start = None
    for i, line in enumerate(lines):
        h = line.heading()
        if h is not None and h[0] == 2 and h[1] == title:
            start = i + 1
            break
    if start is None:
        failures.append(f"{DOC}: heading '## {title}' not found (outside any code fence)")
        return None
    end = len(lines)
    for i in range(start, len(lines)):
        h = lines[i].heading()
        if h is not None and h[0] <= 2:
            end = i
            break
    return lines[start:end]


def subsections(lines):
    """Split a section into (heading, body lines) pairs for each ### heading."""
    subs = []
    for i, line in enumerate(lines):
        h = line.heading()
        if h is not None and h[0] == 3:
            subs.append((h[1], i + 1))
    return [
        (title, lines[start : subs[j + 1][1] - 1 if j + 1 < len(subs) else len(lines)])
        for j, (title, start) in enumerate(subs)
    ]


def check_mermaid(name, body, failures):
    """Require a real, top-level ```mermaid fence: closed, 'flowchart LR' first, one edge."""
    opening = None
    for i, line in enumerate(body):
        if line.opens and line.fence is None and fence_marker(line.text) == ("`", 3, "mermaid"):
            opening = i
            break
    if opening is None:
        failures.append(f"{DOC} {name}: no top-level ```mermaid fence (a fence inside another fence is literal text)")
        return
    inner = []
    closed = False
    for line in body[opening + 1 :]:
        if line.closes:
            closed = True
            break
        if HEADING.match(line.text):
            break  # a heading swallowed by an open fence: treat the fence as unclosed here
        inner.append(line.text)
    if not closed:
        failures.append(f"{DOC} {name}: mermaid fence is not closed before the next heading")
    nonblank = [l for l in inner if l.strip()]
    if not nonblank or nonblank[0].strip() != "flowchart LR":
        failures.append(f"{DOC} {name}: mermaid fence does not open with 'flowchart LR'")
    if not any("-->" in l for l in inner):
        failures.append(f"{DOC} {name}: mermaid fence has no '-->' edge")


def check_processes(lines, failures):
    sec = section(lines, "Processes", failures)
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


def split_cells(text):
    stripped = text.strip()
    if not stripped.startswith("|"):
        return None
    return [c.strip() for c in stripped.strip("|").split("|")]


def is_delimiter_row(cells):
    return cells is not None and all(DELIMITER_CELL.fullmatch(c) for c in cells)


def table_rows(sec):
    """Collect body rows from every real GFM table in a section.

    A table is a header row followed by a delimiter row of the same width,
    outside any code fence; its body is the run of following pipe rows.
    Pipe-prefixed prose without a delimiter row is not a table.
    """
    rows = []
    i = 0
    while i < len(sec) - 1:
        header, delim = sec[i], sec[i + 1]
        if header.fence is not None or header.opens or delim.fence is not None or delim.opens:
            i += 1
            continue
        hcells, dcells = split_cells(header.text), split_cells(delim.text)
        if hcells is None or not is_delimiter_row(dcells) or len(hcells) != len(dcells):
            i += 1
            continue
        i += 2
        while i < len(sec) and sec[i].fence is None and not sec[i].opens:
            cells = split_cells(sec[i].text)
            if cells is None:
                break
            rows.append(cells)
            i += 1
    return rows


def check_packages(lines, failures):
    sec = section(lines, "Packages", failures)
    if sec is None:
        return
    rows = table_rows(sec)
    if not rows:
        failures.append(f"{DOC} Packages: no Markdown table (header row followed by a delimiter row)")
        return
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


def check_vision(lines, failures):
    sec = section(lines, "Vision to tree", failures)
    if sec is None:
        return
    rows = table_rows(sec)
    if not rows:
        failures.append(f"{DOC} Vision to tree: no Markdown table (header row followed by a delimiter row)")
        return
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
    lines = parse(DOC.read_text(encoding="utf-8"))
    for title in SECTIONS:
        section(lines, title, failures)
    check_processes(lines, failures)
    check_packages(lines, failures)
    check_vision(lines, failures)
    failures = list(dict.fromkeys(failures))
    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("docs architecture OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

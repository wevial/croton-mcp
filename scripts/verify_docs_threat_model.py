#!/usr/bin/env python3
"""Verify the structure and required statements of docs/THREAT_MODEL.md.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import subprocess
import sys

DOC = pathlib.Path("docs/THREAT_MODEL.md")
REQUIRED_HEADINGS = (
    "## Assets",
    "## Attacker model",
    "## Trust boundaries",
    "## Residual risks",
    "## Out of scope",
)
MIN_BOUNDARIES = 6
MCP_TOKENS = ("64 KiB", "internal/mcpserver/stdio.go", "internal/drivemcp/server.go")
CLI_TOKENS = ("internal/drivecli/client.go",)
CLI_WORDS = ("compatibility", "provenance")
EMAIL_RE = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def tracked_files():
    out = subprocess.run(
        ["git", "ls-files"], check=True, capture_output=True, text=True
    ).stdout
    return [f for f in out.split("\n") if f]


def is_tracked_path(token, files):
    if token in files:
        return True
    prefix = token.rstrip("/") + "/"
    return any(f.startswith(prefix) for f in files)


def boundaries_section(text, failures):
    match = re.search(r"^## Trust boundaries\s*$", text, re.MULTILINE)
    if match is None:
        return None
    rest = text[match.end():]
    nxt = re.search(r"^## ", rest, re.MULTILINE)
    return rest if nxt is None else rest[: nxt.start()]


def split_subsections(section):
    parts = re.split(r"^### (.*)$", section, flags=re.MULTILINE)
    return [(parts[i].strip(), parts[i + 1]) for i in range(1, len(parts), 2)]


def check_subsection(heading, body, files, failures):
    tokens = re.findall(r"`([^`\n]+)`", body)
    if not any(is_tracked_path(t, files) for t in tokens):
        failures.append(
            f"{DOC} boundary {heading!r}: no backticked token names a tracked file or directory"
        )
    if "MCP" in heading:
        for token in MCP_TOKENS:
            if token not in body:
                failures.append(f"{DOC} boundary {heading!r}: missing token {token!r}")
    if "CLI" in heading:
        for token in CLI_TOKENS:
            if token not in body:
                failures.append(f"{DOC} boundary {heading!r}: missing token {token!r}")
        for word in CLI_WORDS:
            if not re.search(r"\b" + word + r"\b", body):
                failures.append(f"{DOC} boundary {heading!r}: missing word {word!r}")


def main():
    failures = []
    if not DOC.is_file():
        print(f"FAIL: {DOC}: file not found", file=sys.stderr)
        return 1
    text = DOC.read_text(encoding="utf-8")
    headings = {line.rstrip() for line in text.splitlines() if line.startswith("## ")}
    for heading in REQUIRED_HEADINGS:
        if heading not in headings:
            failures.append(f"{DOC}: missing heading {heading!r}")
    section = boundaries_section(text, failures)
    if section is not None:
        subsections = split_subsections(section)
        if len(subsections) < MIN_BOUNDARIES:
            failures.append(
                f"{DOC}: {len(subsections)} boundary subsections under '## Trust boundaries', expected at least {MIN_BOUNDARIES}"
            )
        files = tracked_files()
        mcp = cli = False
        for heading, body in subsections:
            mcp = mcp or "MCP" in heading
            cli = cli or "CLI" in heading
            check_subsection(heading, body, files, failures)
        if not mcp:
            failures.append(f"{DOC}: no boundary subsection heading contains 'MCP'")
        if not cli:
            failures.append(f"{DOC}: no boundary subsection heading contains 'CLI'")
    for number, line in enumerate(text.splitlines(), 1):
        for match in EMAIL_RE.finditer(line):
            failures.append(f"{DOC}:{number}: email-shaped token {match.group(0)!r}")
    if failures:
        for f in failures:
            print(f"FAIL: {f}", file=sys.stderr)
        return 1
    print("docs threat model OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

#!/usr/bin/env python3
"""Verify that the repository docs describe the tree.

Run from the repository root. Exits nonzero and names every failure. The
README checks also run against scratch copies that must fail, so the stale
phrases and the `--config` rule are witnessed without touching the real file.
"""

import pathlib
import re
import subprocess
import sys

STALE_README_PHRASES = ("currently empty", "scaffold", "read-only IMAP adapter", "/usr/local/go/bin/go")
GO_RUN = "go run ./cmd/"
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
        return None
    rest = text[match.end():]
    nxt = re.search(r"^## ", rest, re.MULTILINE)
    return rest if nxt is None else rest[: nxt.start()]


def check_readme(failures):
    text = pathlib.Path("README.md").read_text(encoding="utf-8")
    check_readme_text(text, failures)
    self_test_readme(text, failures)


def check_readme_text(text, failures):
    for phrase in STALE_README_PHRASES:
        if phrase in text:
            failures.append(f"README.md: stale phrase {phrase!r} present")
    for line in text.splitlines():
        if GO_RUN in line and "--config" not in line:
            failures.append(f"README.md: {line.strip()!r} names no --config")

    layout = section(text, r"^## Layout\s*$", "README.md", failures)
    if layout is None:
        return
    bullets = {}
    for m in re.finditer(r"^- `([^`]+)`:(.*)$", layout, re.MULTILINE):
        bullets.setdefault(m.group(1), []).append(m.group(2).strip())
    for d in go_package_dirs():
        roles = bullets.get(d, [])
        if not roles:
            failures.append(f"README.md Layout: no bullet for package directory {d!r}")
            continue
        if len(roles) > 1:
            failures.append(
                f"README.md Layout: {len(roles)} bullets for package directory {d!r}, expected one"
            )
        if not all(roles):
            failures.append(f"README.md Layout: bullet for {d!r} has an empty role")


def reword(text, phrase, replacement):
    if phrase not in text:
        raise ValueError(f"self-test phrase absent from README.md: {phrase!r}")

    return text.replace(phrase, replacement)


def self_test_readme(text, failures):
    run = "go run ./cmd/croton-mcp --config /absolute/path/to/croton.json"
    cases = (
        ("read-only bridge", reword(text, "IMAP adapter over", "read-only IMAP adapter over"),
         "stale phrase 'read-only IMAP adapter'"),
        ("fixed go path", reword(text, run, "/usr/local/go/bin/" + run),
         "stale phrase '/usr/local/go/bin/go'"),
        ("run without config", reword(text, run, "go run ./cmd/croton-mcp"), "names no --config"),
    )

    for name, candidate, expected in cases:
        errors = []
        check_readme_text(candidate, errors)
        if not any(expected in error for error in errors):
            failures.append(f"README.md self-test {name}: unexpected result {errors}")


def check_dependencies(failures):
    text = pathlib.Path("docs/DEPENDENCIES.md").read_text(encoding="utf-8")
    sec = section(text, r"^## .*go-imap.*$", "docs/DEPENDENCIES.md", failures)
    if sec is None:
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
    try:
        check_readme(failures)
    except ValueError as error:
        failures.append(str(error))
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

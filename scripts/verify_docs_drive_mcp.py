#!/usr/bin/env python3
"""Verify that docs/DRIVE-MCP.md matches the Drive server's code and tests.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import subprocess
import sys

PAGE = pathlib.Path("docs/DRIVE-MCP.md")
TOOLS_GO = pathlib.Path("internal/drivemcp/tools.go")
TEST_GLOBS = ("internal/drivemcp/*_test.go", "cmd/croton-drive-mcp/*_test.go")
HEADINGS = ("Running", "Tools", "Arguments", "Output", "Errors", "Audit", "Sharing")
BOUND_TOKENS = ("24 KiB", "1024", "200", "100 000")
MIN_CITED_TESTS = 8
# The witnesses the ticket names for the catalog, invalid-argument, oversize-output,
# error-mapping, audit and both sharing claims; each must be cited by name.
REQUIRED_WITNESSES = (
    "TestNewNegotiatesCurrentProtocolWithTheReadOnlyDriveCatalog",
    "TestDriveToolsRejectInvalidArgumentsWithoutExecutingTheCLI",
    "TestEncodeBoundedShrinksOversizeListResultsIntoValidJSON",
    "TestMapDriveErrorCoversEveryAdapterCode",
    "TestDriveAuditRecordsOnlyToolNameAndOutcome",
    "TestGetDriveSharingStatusReportsSharedUnsharedAndCommandErrors",
    "TestGetDriveSharingStatusBoundsMembersAndKeepsAuditPayloadFree",
    "TestStdioDriveToolsServeFrozenDataAfterSuccessfulNegotiation",
)
TEST_NAME_RE = re.compile(r"Test[A-Za-z0-9_]+")


def section(text, name, failures):
    match = re.search(r"^## " + re.escape(name) + r"\s*$", text, re.MULTILINE)
    if match is None:
        failures.append(f"{PAGE}: heading '## {name}' not found")
        return None
    rest = text[match.end():]
    nxt = re.search(r"^## ", rest, re.MULTILINE)
    return rest if nxt is None else rest[: nxt.start()]


def backticked(text):
    return set(re.findall(r"`([^`]+)`", text))


def code_tool_names():
    source = TOOLS_GO.read_text(encoding="utf-8")
    start = source.find("func toolDefinitions()")
    if start < 0:
        return None
    end = source.find("\nfunc ", start + 1)
    body = source[start:] if end < 0 else source[start:end]
    return set(re.findall(r'name:\s*"([a-z_]+)"', body))


def code_error_codes():
    source = TOOLS_GO.read_text(encoding="utf-8")
    return set(re.findall(r'\berr[A-Za-z]+\s*=\s*"([a-z_]+)"', source))


def declared_tests():
    out = subprocess.run(
        ["git", "ls-files", "--", *TEST_GLOBS], check=True, capture_output=True, text=True
    ).stdout
    names = set()
    for path in out.split():
        names |= set(re.findall(r"func (Test[A-Za-z0-9_]+)\(", pathlib.Path(path).read_text(encoding="utf-8")))
    return names


def check_page(failures):
    if not PAGE.is_file():
        failures.append(f"{PAGE}: missing")
        return
    text = PAGE.read_text(encoding="utf-8")
    sections = {name: section(text, name, failures) for name in HEADINGS}

    tools_section = sections["Tools"]
    if tools_section is not None:
        documented = set()
        for line in tools_section.splitlines():
            cells = [cell.strip() for cell in line.strip().strip("|").split("|")]
            if len(cells) >= 2 and cells[0].startswith("`"):
                documented |= backticked(cells[0])
        expected = code_tool_names()
        if expected is None:
            failures.append(f"{TOOLS_GO}: toolDefinitions() not found")
        else:
            for name in sorted(expected - documented):
                failures.append(f"{PAGE} Tools table: tool {name!r} registered in {TOOLS_GO} but not documented")
            for name in sorted(documented - expected):
                failures.append(f"{PAGE} Tools table: tool {name!r} documented but not registered in {TOOLS_GO}")
            if not expected:
                failures.append(f"{TOOLS_GO}: no tool names found in toolDefinitions()")

    errors_section = sections["Errors"]
    if errors_section is not None:
        documented = {code for code in backticked(errors_section) if re.fullmatch(r"[a-z_]+", code)}
        expected = code_error_codes()
        for code in sorted(expected - documented):
            failures.append(f"{PAGE} Errors: code {code!r} declared in {TOOLS_GO} but not documented")
        for code in sorted(documented - expected):
            failures.append(f"{PAGE} Errors: code {code!r} documented but not declared in {TOOLS_GO}")
        if not expected:
            failures.append(f"{TOOLS_GO}: no err... constants found")

    cited = set(TEST_NAME_RE.findall(text))
    declared = declared_tests()
    for name in sorted(cited - declared):
        failures.append(f"{PAGE}: cited test {name!r} is not declared in a tracked Drive _test.go file")
    if len(cited) < MIN_CITED_TESTS:
        failures.append(f"{PAGE}: cites {len(cited)} distinct tests, want at least {MIN_CITED_TESTS}")
    for name in REQUIRED_WITNESSES:
        if name not in cited:
            failures.append(f"{PAGE}: required witness test {name!r} is not cited")

    sharing_section = sections["Sharing"]
    if sharing_section is not None and "customPassword" not in sharing_section:
        failures.append(f"{PAGE} Sharing: does not mention customPassword")

    for token in BOUND_TOKENS:
        if token not in text:
            failures.append(f"{PAGE}: bound token {token!r} not found")


def main():
    failures = []
    check_page(failures)
    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1
    print("docs/DRIVE-MCP.md OK")
    return 0


if __name__ == "__main__":
    sys.exit(main())

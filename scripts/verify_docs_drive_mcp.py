#!/usr/bin/env python3
"""Verify that docs/DRIVE-MCP.md matches the Drive server's code and tests.

Run from the repository root. Exits nonzero and names every failure.
"""

import pathlib
import re
import subprocess
import sys

from verify_docs_design_download_boundary import (
    check_statements,
    sections as contract_sections,
    visible_lines,
)

DOWNLOAD_REQUIREMENTS = {
    "future policy link": "Planned downloads follow the accepted [resolution-time download boundary](design/0003-download-boundary.md).",
    "unshipped registration and controls": "The confined opener has shipped, but download registration and policy enforcement have not shipped.",
    "client-managed per-call confirmation": "Operators must configure client-managed per-call confirmation before enabling downloads.",
    "configured-client trust boundary": "Croton accepts tools/call from the configured client and cannot verify that a human confirmed.",
    "no server prompt": "Croton never prompts for confirmation itself.",
    "annotations do not enforce confirmation": "The annotations readOnlyHint false and destructiveHint false do not force a client prompt or prove confirmation.",
    "auto-approval permits unattended writes": "An auto-approving client permits unattended local writes inside the allowed root.",
    "disabled by default": "The download.enabled setting defaults to false.",
    "no disabled tool": "While disabled, no download tool is registered.",
    "explicit operator opt-in": "Explicit operator opt-in accepts the configured-client trust boundary.",
    "pre-registration startup refusal": "Until the registration implementation ships, download.enabled true must fail startup rather than enable a partial capability.",
    "accepted disabled config": "The current config schema accepts download.enabled, download.maxBytes and download.timeoutSeconds, but rejects enabled downloads with a static ErrConfigInvalid before CLI startup, regardless of allowed roots.",
    "parsed limit defaults": "Omitted limits default to 256 MiB and 120 seconds.",
    "parsed limit bounds": "Explicit limits must be positive signed-64-bit integers; timeout conversion to time.Duration must not overflow.",
    "no limit clamping": "Invalid limits are rejected, never clamped.",
    "writer and cleanup gate": "Registration requires later integration proof of a supported descriptor-bound writer and temporary-publication path, cleanup and runtime controls without reopening the validated destination pathname.",
    "future audit privacy": "Future download audit lines identify source, destination and outcome, never claimed consent, credentials or share passwords; paths can reveal sensitive names and activity and require protected log access and retention.",
    "current audit unchanged": "This future path-logging policy does not change the current audit vocabulary.",
    "structural witness only": "Documentation verifiers witness these policy statements structurally, not runtime enforcement.",
}

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
    "TestAllowlistMiddlewareAnswersMethodNotFound",
    "TestRunToolRecoversAPanicAsInternal",
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
    body = code_tool_definitions_body(TOOLS_GO.read_text(encoding="utf-8"))
    if body is None:
        return None
    return set(re.findall(r'name:\s*"([a-z_]+)"', body))


def code_error_codes():
    source = TOOLS_GO.read_text(encoding="utf-8")
    return set(re.findall(r'\berr[A-Za-z]+\s*=\s*"([a-z_]+)"', source))


NOT_WITNESSED = "Not yet witnessed"
WITNESSED_SENTENCE = "Every current runtime claim on this page is witnessed by a tracked test."


def check_not_witnessed(text, failures):
    """Runtime claims are witnessed; planned enforcement must remain future work."""
    body = section(text, NOT_WITNESSED, failures)
    if body is None:
        return
    if WITNESSED_SENTENCE not in " ".join(body.split()):
        failures.append(f"{PAGE} {NOT_WITNESSED}: missing witnessed sentence")
    if "Planned download policies are structurally witnessed by documentation verifiers; their runtime enforcement remains future work." not in " ".join(body.split()):
        failures.append(f"{PAGE} {NOT_WITNESSED}: missing future policy enforcement distinction")
    if "|" in body:
        failures.append(f"{PAGE} {NOT_WITNESSED}: must not contain a table")


def code_tool_definitions_body(source):
    start = source.find("func toolDefinitions()")
    if start < 0:
        return None
    end = source.find("\nfunc ", start + 1)
    return source[start:] if end < 0 else source[start:end]


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
    _, policy_sections = contract_sections(visible_lines(text))
    check_statements(
        policy_sections.get("Planned downloads", []), DOWNLOAD_REQUIREMENTS, (),
        f"{PAGE} Planned downloads", failures,
    )

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

    check_not_witnessed(text, failures)


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

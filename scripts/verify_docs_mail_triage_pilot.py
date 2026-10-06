#!/usr/bin/env python3
"""Offline witness for the Mail triage pilot runbook; uses repository text only.

It checks documented boundaries and synthetic negative fixtures. It never
launches Croton, runs a helper, opens an account connection or records a
pilot outcome, and it cannot prove live behavior or human consent.
"""

import argparse
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
GUIDE = ROOT / "docs/MAIL-TRIAGE-PILOT.md"
LINK = "docs/MAIL-TRIAGE-PILOT.md"
GUIDE_LINKS = ("USER-INSTALL.md", "MCP.md", "design/0005-mail-triage.md")
CONTRACT = re.compile(r"<!-- mail-triage-pilot-contract -->\s*```json\n(.*?)\n```", re.S)
EVIDENCE = re.compile(r"<!-- mail-triage-pilot-evidence -->\n((?:\|[^\n]*\n)+)")
ACTIONS = ["mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"]
EVIDENCE_STEPS = ACTIONS + ["refusal probe", "rollback rehearsal"]
# Checksum steps must appear in this order: staged comparison before install
# approval, installed verification after installation and reinstallation, then
# enablement. A first install has no executable to hash beforehand.
ORDER = ("Before install approval, the operator records privately",
         "A staged-binary checksum", "1. Install approval:",
         "verify the independent installed-binary checksum:",
         "verify the independent installed-binary checksum again",
         "Only after both installed-binary checks match, enable")
PREAMBLE = ("non-executing procedure",
            "grants no installation, enablement, account access or live-write authority",
            "nothing in it has been run", "intentionally stricter")
EXPECTED = {
    "status": "not_run",
    "grants_authority": False,
    "release": {
        "published_git_tag": "operator_selected",
        "matching_github_release": True,
        "full_revision": "resolved_and_reviewed",
        "manifest_sha256": "recorded_from_staged_manifest",
        "staged_binary_sha256": "compared_before_install_approval",
        "installed_binary_sha256": "verified_after_install_and_reinstall_before_enablement",
        "pilot_blocked_until_published": True,
    },
    "gates": ["install", "rollback_rehearsal", "local_enablement", "bounded_live_read_write"],
    "installation": "docs/USER-INSTALL.md",
    "catalog": {
        "disabled_tools": 6,
        "enabled_tools": 11,
        "rollback": "prior_release_documented_catalog",
        "first_install_rollback": "restore_absence",
    },
    "scope": {
        "account": "operator_own",
        "messages": "about_ten_self_sent",
        "initial_pilot_folders": 1,
        "max_uids_per_call": 5,
        "confirmation": "each_displayed_exact_payload",
        "session_approval": False,
        "real_inbox": "prohibited",
    },
    "actions": ACTIONS,
    "refusal": {
        "probe": "move_mail_source_equals_destination_single_message",
        "confirmation": "explicit_exact_payload",
        "prohibited": ["forced_fault", "invalid_credential", "invented_uid", "mixed_batch"],
    },
    "recovery": ["stop", "no_replay", "separately_authorized_fresh_readback",
                 "new_approval", "fresh_identities_after_move"],
    "cleanup": {
        "trash_retention_warning": True,
        "purge": False,
        "return_moves": "separate_fresh_identified_approved_actions",
    },
    "evidence": {
        "status": "NOT RUN",
        "fields": "outcome_only_allowlist",
        "private_data": "excluded",
        "proof": "synthetic_only",
    },
}
REQUIRED = {
    "Release selection prerequisite": (
        "blocked until a suitable tagged release is published",
        "operator-selected published git tag", "matching GitHub release",
        "Before install approval", "resolved full revision", "full 40-hex commit SHA",
        "staged-manifest SHA-256", "A staged-binary checksum",
        "compare it with the manifest SHA-256", "A mismatch stops the pilot",
        "Staging a candidate installs nothing", "cannot exist yet",
        "Do not download a release binary"),
    "Authorization gates": (
        "four gates", "Approval of one gate grants nothing for the next",
        "Install approval:", "Rollback-rehearsal approval:", "Enablement approval:",
        "Bounded live read/write approval:", "per-payload confirmation"),
    "Installation": (
        "[user-owned installation guide](USER-INSTALL.md)",
        "does not rewrite or relax that guide", "Leave `mutations` absent",
        "After the authorized installation", "independent installed-binary checksum",
        "final installed executable"),
    "Catalog and rollback rehearsal": (
        "disabled catalog is the six-tool set", "enabled catalog is the eleven-tool set",
        "complete matched artifact set", "first-install absence",
        "prior release's own documented catalog",
        "Do not assume an older binary has eleven tools",
        "verify the independent installed-binary checksum again",
        "Only after both installed-binary checks match"),
    "Pilot selection and approval": (
        "Own-account scope", "about ten self-sent messages", "one initial pilot folder",
        "Real-inbox prohibition", "Five-UID limit", "at most five UIDs",
        "Exact payload confirmation",
        "the tool, source mailbox, UIDVALIDITY, ordered UIDs and the move destination when present",
        "no reusable session approval"),
    "Action procedure": ("existing MCP contract", "Proton app state"),
    "Definitive refusal observation": (
        "safe source-equals-destination probe", "single-message `move_mail` call",
        "explicit payload confirmation", "Observe the refusal privately",
        "state is unchanged", "no forced fault, invalid credential, invented UID or mixed batch"),
    "Unknown outcome and recovery": (
        "STOP", "No replay", "separately authorized fresh readback", "new approval",
        "fresh destination-source identities"),
    "Trash and cleanup": (
        "Retention warning", "No-purge boundary", "never purge or empty Trash",
        "separate freshly identified and approved actions", "not installation rollback"),
    "Public evidence template": (
        "Status: NOT RUN", "claims no live outcome", "outcome categories from this allowlist",
        "Never include credentials, account identifiers", "message content",
        "Synthetic-only proof limitation",
        "do not prove live success or client consent enforcement", "Usability prompts"),
}


def sections_of(text):
    return dict(re.findall(r"^## ([^\n]+)\n(.*?)(?=^## |\Z)", text, re.M | re.S))


def normalize(text):
    return " ".join(text.split()).replace("**", "")


def table_rows(block):
    rows = []
    for line in block.splitlines():
        if not line.startswith("|") or re.fullmatch(r"[|\s:-]+", line):
            continue
        rows.append([cell.strip() for cell in line.strip().strip("|").split("|")])

    return rows[1:]


def flatten(value, prefix=""):
    if isinstance(value, dict):
        items = {}
        for key, child in value.items():
            items.update(flatten(child, f"{prefix}{key}."))
        return items

    return {prefix[:-1]: value}


def contract_errors(raw):
    try:
        contract = json.loads(raw)
    except ValueError:
        return ["contract: invalid JSON"]
    if not isinstance(contract, dict):
        return ["contract: invalid JSON"]

    failures = []
    actual = flatten(contract)
    for path, expected in flatten(EXPECTED).items():
        if path not in actual:
            failures.append(f"contract: {path} missing")
        elif actual[path] != expected or type(actual[path]) is not type(expected):
            failures.append(f"contract: {path} changed")
    for path in sorted(set(actual) - set(flatten(EXPECTED))):
        failures.append(f"contract: unexpected {path}")

    return failures


def order_errors(guide):
    normalized = normalize(guide)
    positions = [normalized.find(marker) for marker in ORDER]
    if -1 in positions:
        return [f"order: checksum step missing: {ORDER[positions.index(-1)]!r}"]

    failures = []
    for earlier, later, first, second in zip(ORDER, ORDER[1:], positions, positions[1:]):
        if first >= second:
            failures.append(f"order: {earlier!r} must precede {later!r}")

    return failures


def action_errors(section):
    failures = []
    rows = table_rows(section)
    tools = [row[0].strip("`") for row in rows]
    if tools != ACTIONS:
        failures.append(f"actions: expected rows {ACTIONS}, found {tools}")

    for row in rows:
        tool = row[0].strip("`")
        if len(row) != 3 or "Proton app" not in row[2]:
            failures.append(f"actions: {tool} lacks a private Proton-app state check")
        elif tool == "move_mail" and not (
                "`Folders/`" in row[1] and "existing MCP contract" in row[1]):
            failures.append("actions: move_mail destination outside the existing MCP contract")

    return failures


def evidence_errors(guide, section):
    failures = []
    blocks = EVIDENCE.findall(guide)
    if len(blocks) != 1:
        return ["evidence: expected one marked outcome table"]

    rows = table_rows(blocks[0])
    steps = [row[0].strip("`") for row in rows]
    if steps != EVIDENCE_STEPS:
        failures.append(f"evidence: expected rows {EVIDENCE_STEPS}, found {steps}")
    for row in rows:
        if len(row) != 3 or row[1:] != ["NOT RUN", "NOT RUN"]:
            failures.append(f"evidence: row {row[0].strip('`')!r} claims a live outcome")

    prompts = section.split("Usability prompts", 1)[-1]
    count = len(re.findall(r"^- .+\?$", prompts, re.M))
    if not 3 <= count <= 6:
        failures.append(f"evidence: expected 3 to 6 usability prompts, found {count}")

    return failures


def verify(guide, readme, exists):
    failures = []
    preamble = normalize(guide.split("\n## ", 1)[0])
    for phrase in PREAMBLE:
        if phrase not in preamble:
            failures.append(f"preamble: missing contract phrase {phrase!r}")

    sections = sections_of(guide)
    for heading, phrases in REQUIRED.items():
        if heading not in sections:
            failures.append(f"section missing: {heading}")
            continue
        normalized = normalize(sections[heading])
        for phrase in phrases:
            if phrase not in normalized:
                failures.append(f"{heading}: missing contract phrase {phrase!r}")

    failures.extend(order_errors(guide))

    contracts = CONTRACT.findall(guide)
    if len(contracts) != 1:
        failures.append("contract: expected one marked JSON fence")
    else:
        failures.extend(contract_errors(contracts[0]))

    failures.extend(action_errors(sections.get("Action procedure", "")))
    failures.extend(evidence_errors(guide, sections.get("Public evidence template", "")))

    for target in GUIDE_LINKS:
        if f"]({target}" not in guide:
            failures.append(f"guide: link to {target} missing")
        elif not exists(f"docs/{target}"):
            failures.append(f"guide: link target missing: docs/{target}")
    if not re.search(r"\[[^\]]+\]\(docs/MAIL-TRIAGE-PILOT\.md\)", readme):
        failures.append("README: pilot guide link missing")
    elif not exists(LINK):
        failures.append("README: pilot guide link does not resolve")

    return failures


def repository_file(relative):
    path = ROOT / relative
    return path.is_file() and path.resolve().is_relative_to(ROOT)


def self_test(guide, readme):
    sections = sections_of(guide)
    contract = CONTRACT.search(guide)
    if contract is None:
        raise ValueError("self-test requires the baseline contract")

    def edit(heading, old, new=""):
        body = sections.get(heading, "")
        # Prose phrases may wrap, so whitespace inside a target matches any run.
        pattern = re.escape(old) if old.endswith("\n") else r"\s+".join(
            map(re.escape, old.split()))
        changed, count = re.subn(pattern, lambda _: new, body, count=1)
        if count != 1:
            raise ValueError(f"self-test mutation target absent: {heading}: {old!r}")

        start = guide.index(body)
        return guide[:start] + changed + guide[start + len(body):]

    def edit_contract(change):
        data = json.loads(contract[1])
        change(data)
        return guide[:contract.start(1)] + json.dumps(data, indent=2) + guide[contract.end(1):]

    def phrase_case(name, heading, phrase):
        return (name, edit(heading, phrase), readme, repository_file,
                f"missing contract phrase {phrase!r}")

    def row(tool):
        line = next(l for l in sections["Action procedure"].splitlines()
                    if l.startswith(f"| `{tool}` |"))
        return line + "\n"

    release = "Release selection prerequisite"
    gates = "Authorization gates"
    catalog = "Catalog and rollback rehearsal"
    scope = "Pilot selection and approval"
    refusal = "Definitive refusal observation"
    recovery = "Unknown outcome and recovery"
    cleanup = "Trash and cleanup"
    evidence = "Public evidence template"
    cases = [
        ("baseline", guide, readme, repository_file, None),
        phrase_case("missing release publication", release, "matching GitHub release"),
        phrase_case("missing tag selection", release, "operator-selected published git tag"),
        phrase_case("missing full revision", release, "full 40-hex commit SHA"),
        phrase_case("missing manifest checksum", release, "staged-manifest SHA-256"),
        phrase_case("missing staged-binary checksum", release, "A staged-binary checksum"),
        phrase_case("missing installed-binary checksum", "Installation",
                    "independent installed-binary checksum"),
        phrase_case("missing reinstall checksum", catalog,
                    "verify the independent installed-binary checksum again"),
        phrase_case("enablement not after checksums", catalog,
                    "Only after both installed-binary checks match"),
        ("installed hash before install approval", edit(
            release, "Do not download a release binary",
            "Then verify the independent installed-binary checksum: hash the installed "
            "executable. Do not download a release binary"), readme, repository_file,
         "order: '1. Install approval:' must precede 'verify the independent installed-binary"),
        ("contract without publication gate", edit_contract(
            lambda c: c["release"].pop("matching_github_release")), readme, repository_file,
         "contract: release.matching_github_release missing"),
        ("contract without binary comparison", edit_contract(
            lambda c: c["release"].update(installed_binary_sha256="trusted")), readme,
         repository_file, "contract: release.installed_binary_sha256 changed"),
        ("contract installed hash before install", edit_contract(
            lambda c: c["release"].update(installed_binary_sha256="recorded_before_install")),
         readme, repository_file, "contract: release.installed_binary_sha256 changed"),
        phrase_case("missing install approval", gates, "Install approval:"),
        phrase_case("missing enablement approval", gates, "Enablement approval:"),
        phrase_case("missing per-payload confirmation", gates, "per-payload confirmation"),
        phrase_case("installation not linked", "Installation",
                    "[user-owned installation guide](USER-INSTALL.md)"),
        ("installation link target missing", guide, readme,
         lambda path: path != "docs/USER-INSTALL.md",
         "guide: link target missing: docs/USER-INSTALL.md"),
        phrase_case("missing disabled six-tool set", catalog, "disabled catalog is the six-tool set"),
        phrase_case("missing enabled eleven-tool set", catalog,
                    "enabled catalog is the eleven-tool set"),
        phrase_case("missing prior-release catalog rule", catalog,
                    "prior release's own documented catalog"),
        phrase_case("older binary assumed eleven tools", catalog,
                    "Do not assume an older binary has eleven tools"),
        phrase_case("missing first-install absence", catalog, "first-install absence"),
        ("contract enabled catalog changed", edit_contract(
            lambda c: c["catalog"].update(enabled_tools=6)), readme, repository_file,
         "contract: catalog.enabled_tools changed"),
        phrase_case("missing own-account scope", scope, "Own-account scope"),
        phrase_case("missing about ten messages", scope, "about ten self-sent messages"),
        phrase_case("missing single pilot folder", scope, "one initial pilot folder"),
        phrase_case("missing five-UID limit", scope, "Five-UID limit"),
        phrase_case("missing exact payload confirmation", scope, "Exact payload confirmation"),
        phrase_case("missing real-inbox prohibition", scope, "Real-inbox prohibition"),
        ("contract batch limit widened", edit_contract(
            lambda c: c["scope"].update(max_uids_per_call=50)), readme, repository_file,
         "contract: scope.max_uids_per_call changed"),
        ("contract session approval allowed", edit_contract(
            lambda c: c["scope"].update(session_approval=True)), readme, repository_file,
         "contract: scope.session_approval changed"),
    ]
    for tool in ACTIONS:
        cases.append((f"missing {tool} row", edit("Action procedure", row(tool)), readme,
                      repository_file, "actions: expected rows"))
    cases += [
        ("row without Proton-app check", edit(
            "Action procedure", "Proton app shows them in Archive", "Archive holds them"),
         readme, repository_file, "actions: archive_mail lacks a private Proton-app state check"),
        ("move destination outside contract", edit(
            "Action procedure", "the exact pilot destination folder under `Folders/`",
            "any folder"), readme, repository_file,
         "actions: move_mail destination outside the existing MCP contract"),
        phrase_case("missing source-equals-destination probe", refusal,
                    "safe source-equals-destination probe"),
        phrase_case("missing probe confirmation", refusal, "explicit payload confirmation"),
        phrase_case("missing unchanged state", refusal, "state is unchanged"),
        phrase_case("forced refusals allowed", refusal,
                    "no forced fault, invalid credential, invented UID or mixed batch"),
        phrase_case("missing STOP", recovery, "STOP"),
        phrase_case("missing no replay", recovery, "No replay"),
        phrase_case("missing fresh readback", recovery, "separately authorized fresh readback"),
        phrase_case("missing new approval", recovery, "new approval"),
        phrase_case("missing fresh identities after MOVE", recovery,
                    "fresh destination-source identities"),
        phrase_case("missing retention warning", cleanup, "Retention warning"),
        phrase_case("missing no-purge boundary", cleanup, "No-purge boundary"),
        phrase_case("return move as rollback", cleanup, "not installation rollback"),
        ("contract purge allowed", edit_contract(lambda c: c["cleanup"].update(purge=True)),
         readme, repository_file, "contract: cleanup.purge changed"),
        phrase_case("missing NOT RUN status", evidence, "Status: NOT RUN"),
        phrase_case("missing outcome-only allowlist", evidence,
                    "outcome categories from this allowlist"),
        phrase_case("missing private-data exclusions", evidence,
                    "Never include credentials, account identifiers"),
        phrase_case("missing synthetic-only limitation", evidence,
                    "Synthetic-only proof limitation"),
        ("template claims live outcome", edit(
            evidence, "| `trash_mail` | NOT RUN |", "| `trash_mail` | applied |"), readme,
         repository_file, "evidence: row 'trash_mail' claims a live outcome"),
        ("template missing rollback row", edit(
            evidence, "| rollback rehearsal | NOT RUN | NOT RUN |\n"), readme,
         repository_file, "evidence: expected rows"),
        ("template without usability prompts", re.sub(
            r"^- .+\?\n", "", guide, flags=re.M), readme, repository_file,
         "evidence: expected 3 to 6 usability prompts, found 0"),
        ("contract evidence claims run", edit_contract(
            lambda c: c["evidence"].update(status="RUN")), readme, repository_file,
         "contract: evidence.status changed"),
        ("unexpected contract field", edit_contract(
            lambda c: c["scope"].update(real_inbox_allowed=True)), readme, repository_file,
         "contract: unexpected scope.real_inbox_allowed"),
        ("malformed contract", guide[:contract.start(1)] + "{" + guide[contract.end(1):],
         readme, repository_file, "contract: invalid JSON"),
        ("missing README link", guide, readme.replace(LINK, "docs/MISSING.md"),
         repository_file, "README: pilot guide link missing"),
        ("README link target missing", guide, readme, lambda path: path != LINK,
         "README: pilot guide link does not resolve"),
    ]

    executed = 0
    failed = []
    for name, candidate, candidate_readme, exists, expected in cases:
        errors = verify(candidate, candidate_readme, exists)
        executed += 1
        if (expected is None and errors) or (expected is not None and not any(
                expected in error for error in errors)):
            failed.append(f"self-test {name}: unexpected result {errors}")

    print(f"mail-triage-pilot self-test: {executed} cases executed")
    if executed == 0:
        failed.append("self-test executed zero cases")
    return failed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()

    try:
        guide = GUIDE.read_text(encoding="utf-8")
        readme = (ROOT / "README.md").read_text(encoding="utf-8")
        failures = verify(guide, readme, repository_file)
        if args.self_test:
            failures.extend(self_test(guide, readme))
    except (OSError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("mail-triage-pilot docs OK (offline synthetic checks only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

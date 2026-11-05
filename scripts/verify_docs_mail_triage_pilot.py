#!/usr/bin/env python3
"""Offline witness for the Mail triage pilot runbook; uses repository text only.

It checks documented boundaries and synthetic negative fixtures. Prose is
matched in visible text: HTML comments and top-level fenced code blocks are
removed, and whitespace is collapsed so wrapped lines still match. The marked
JSON contract is extracted separately. It never launches Croton, runs a helper,
calls git, gh or the network, opens an account connection or records a pilot
outcome. It inspects repository text and synthetic fixtures only and does not
demonstrate a real release, human consent, installed bytes or live mail
behavior.
"""

import argparse
import json
from pathlib import Path
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
GUIDE = ROOT / "docs/MAIL-TRIAGE-PILOT.md"
LINK = "docs/MAIL-TRIAGE-PILOT.md"
GUIDE_LINKS = ("USER-INSTALL.md", "RELEASE.md", "MCP.md", "design/0005-mail-triage.md")
CONTRACT = re.compile(r"<!-- mail-triage-pilot-contract -->\s*```json\n(.*?)\n```", re.S)
EVIDENCE = re.compile(r"<!-- mail-triage-pilot-evidence -->\n((?:\|[^\n]*\n)+)")
READ_TOOLS = ["list_folders", "search_mail", "get_message", "get_thread",
              "list_attachments", "select_digest_candidates"]
ACTIONS = ["mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"]
EVIDENCE_STEPS = ACTIONS + ["refusal probe", "rollback rehearsal"]
POLICY = ("Pilot acceptance installs only the selected published native binary asset verified against "
          "both the published manifest and release-note SHA-256; a local source build is not a pilot artifact.")
# Companion guides must state the same visible policy and keep the development
# source-build path, which never satisfies pilot acceptance.
COMPANIONS = {
    "docs/RELEASE.md": ("Integrity limitations", (
        POLICY,
        "If the platform is unsupported or no native asset is published for it, STOP the binary path",
    ), None),
    "docs/USER-INSTALL.md": ("Source build", (
        POLICY,
        "If the platform is unsupported or no native asset is published for it, STOP the binary path",
        "A source build and published-byte acceptance are different paths.",
    ), "python3 scripts/stage_mail_candidate.py"),
}
# Each selection failure must STOP without a rebuild fallback.
STOPS = (
    ("missing tag", "Without a selected published tag, STOP."),
    ("missing publication",
     "For a tag without a matching published release, a draft release, or a release without the tag, STOP."),
    ("unresolved full revision", "For an unresolved tag name, a moving branch or an untagged SHA, STOP."),
    ("missing native pair", "If either asset of the native binary/manifest pair is missing, STOP."),
    ("platform mismatch", "On a platform mismatch or any other identity mismatch, STOP."),
    ("checksum mismatch", "On a checksum mismatch with either value, STOP."),
)
# Visible pilot instructions that would make a local source build the artifact.
SOURCE_BUILD = (
    r"\bdo not download (?:a|the) (?:[\w-]+ )?(?:release )?binary",
    r"\bbuild (?:it |the (?:candidate|binary) )?(?:locally )?from (?:the )?(?:resolved revision|tag|source)",
    r"\bstaging helper\b",
    r"\bstage_mail_candidate\b",
    r"`reviewed_revision` in the source build",
    r"\bfall(?:s|ing)? back to (?:a )?(?:local )?(?:source )?(?:build|rebuild)",
)
# Checksum steps must appear in this order: published-byte comparison before
# install approval, installed verification after installation and
# reinstallation, then enablement. A first install has no executable to hash
# beforehand.
ORDER = ("Before install approval, the operator records privately",
         "A published-binary checksum", "1. Install approval:",
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
        "pilot_artifact": "published_native_binary",
        "manifest_sha256": "recorded_from_published_manifest",
        "published_binary_sha256": "compared_with_release_notes_and_manifest_before_install_approval",
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
    "identities": {
        "search_mail_supplies_uids": False,
        "source": "blocked_until_release_documents_supported_source",
        "decode_or_guess": False,
        "subject_marker": "per_run_unique",
        "result_check": "known_pilot_messages_or_stop",
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
        "blocked until a suitable tagged release is published", POLICY,
        "[Mail release guide](RELEASE.md)",
        "operator-selected published git tag", "matching published non-draft GitHub release",
        "`tagName` exactly equals the tag and `isDraft` is false",
        "Before install approval", "resolved full revision", "full 40-hex commit SHA",
        "`targetCommitish` alone is not proof",
        "the host `GOHOSTOS` and `GOHOSTARCH`, not the configured `GOOS` and `GOARCH`",
        "its `revision` equals the resolved full revision", "its `toolchain` is `go1.26.10`",
        "its `GOOS` and `GOARCH` equal the native platform",
        "The published manifest SHA-256", "binary `sha256` field, not a hash of the manifest file",
        "A published-binary checksum",
        "compare it with both the release-note SHA-256 and the published manifest SHA-256 "
        "before install approval",
        *(phrase for _, phrase in STOPS),
        "there is no rebuild fallback", "never substitute a local source build",
        "grants no install authority", "needs the separate install approval",
        "cannot exist yet"),
    "Authorization gates": (
        "four gates", "Approval of one gate grants nothing for the next",
        "Install approval:", "Rollback-rehearsal approval:", "Enablement approval:",
        "Bounded live read/write approval:", "per-payload confirmation"),
    "Installation": (
        "[user-owned installation guide](USER-INSTALL.md)",
        "does not rewrite or relax that guide", "Published-byte entry", "Leave `mutations` absent",
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
        "no reusable session approval", "no shipped read tool supplies triage identities",
        "opaque HMAC-bound message ids", "triage tools accept no message id",
        "blocked until a published release documents a supported, bounded source",
        "Never decode, derive or guess identities", "Put one per-run unique marker",
        "not a pilot-only boundary", "compare every result with the known pilot messages",
        "stop if any result does not match"),
    "Action procedure": ("existing MCP contract", "Proton app state",
                         "only after the supported identity source above exists"),
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


def visible(text):
    text = re.sub(r"<!--.*?(?:-->|\Z)", "", text, flags=re.S)
    lines, fence = [], None
    for line in text.splitlines():
        marker = re.match(r" {0,3}(`{3,}|~{3,})", line)
        closing = re.fullmatch(r" {0,3}(`{3,}|~{3,})[ \t]*", line)
        if fence is None and marker:
            fence = marker.group(1)
        elif fence is not None and closing and closing.group(1).startswith(fence):
            fence = None
        elif fence is None:
            lines.append(line)

    return "\n".join(lines)


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


def unique_object(pairs):
    keys = [key for key, _ in pairs]
    if len(set(keys)) != len(keys):
        raise ValueError("duplicate JSON key")

    return dict(pairs)


def shape_errors(actual, expected, path=""):
    # Walk the nested objects directly, so a dotted key cannot stand in for a nested one.
    if not isinstance(expected, dict):
        if actual != expected or type(actual) is not type(expected):
            return [f"contract: {path} changed"]
        return []
    if not isinstance(actual, dict):
        return [f"contract: {path} changed"]

    failures = []
    for key, child in expected.items():
        child_path = f"{path}.{key}" if path else key
        if key not in actual:
            failures.append(f"contract: {child_path} missing")
        else:
            failures.extend(shape_errors(actual[key], child, child_path))
    for key in sorted(set(actual) - set(expected)):
        failures.append(f"contract: unexpected {path + '.' if path else ''}{key}")

    return failures


def contract_errors(raw):
    try:
        contract = json.loads(raw, object_pairs_hook=unique_object)
    except ValueError:
        return ["contract: invalid JSON"]
    if not isinstance(contract, dict):
        return ["contract: invalid JSON"]

    return shape_errors(contract, EXPECTED)


def source_build_errors(guide):
    prose = normalize(visible(guide)).lower()
    return [f"source build: pilot instruction {pattern!r}" for pattern in SOURCE_BUILD
            if re.search(pattern, prose)]


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


def catalog_errors(section):
    bullets = re.findall(r"^- (.*?)(?=^- |^\s*$|\Z)", section, re.M | re.S)
    catalogs = (("disabled", "disabled catalog is the six-tool set", "", READ_TOOLS),
                ("enabled", "enabled catalog is the eleven-tool set", "those six plus", ACTIONS))

    failures = []
    for name, label, base, members in catalogs:
        bullet = next((normalize(b) for b in bullets if label in normalize(b)), None)
        if bullet is None:
            failures.append(f"catalog: {name} bullet missing")
            continue
        listed = [tool for tool in re.findall(r"`([^`]+)`", bullet) if tool != "mutations"]
        if listed != members or base not in bullet:
            failures.append(f"catalog: {name} members expected {members}, found {listed}")

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


def companion_errors(companions):
    failures = []
    for path, (heading, phrases, recipe) in COMPANIONS.items():
        text = companions.get(path, "")
        body = sections_of(visible(text)).get(heading)
        if body is None:
            failures.append(f"{path}: visible section missing: {heading}")
            continue
        normalized = normalize(body)
        for phrase in phrases:
            if phrase not in normalized:
                failures.append(f"{path} {heading}: missing contract phrase {phrase!r}")
        # The development recipe is a fenced example, read as text and never executed.
        if recipe is not None and recipe not in sections_of(text).get(heading, ""):
            failures.append(f"{path} {heading}: development source-build recipe missing")

    return failures


def verify(guide, readme, exists, companions):
    failures = []
    shown = visible(guide)
    preamble = normalize(shown.split("\n## ", 1)[0])
    for phrase in PREAMBLE:
        if phrase not in preamble:
            failures.append(f"preamble: missing contract phrase {phrase!r}")

    sections = sections_of(shown)
    for heading, phrases in REQUIRED.items():
        if heading not in sections:
            failures.append(f"section missing: {heading}")
            continue
        normalized = normalize(sections[heading])
        for phrase in phrases:
            if phrase not in normalized:
                failures.append(f"{heading}: missing contract phrase {phrase!r}")

    failures.extend(source_build_errors(guide))
    failures.extend(order_errors(shown))
    failures.extend(companion_errors(companions))

    contracts = CONTRACT.findall(guide)
    if len(contracts) != 1:
        failures.append("contract: expected one marked JSON fence")
    else:
        failures.extend(contract_errors(contracts[0]))

    failures.extend(catalog_errors(sections.get("Catalog and rollback rehearsal", "")))
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


def self_test(guide, readme, companions):
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

    def companion_case(name, path, old, new, expected):
        # Prose phrases may wrap, so whitespace inside a target matches any run.
        pattern = r"\s+".join(map(re.escape, old.split()))
        changed, count = re.subn(pattern, lambda _: new, companions[path], count=1)
        if count != 1:
            raise ValueError(f"self-test mutation target absent: {path}: {old!r}")

        return (name, guide, readme, repository_file, expected, {**companions, path: changed})

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
        phrase_case("missing release publication", release, "matching published non-draft GitHub release"),
        phrase_case("missing draft check", release,
                    "`tagName` exactly equals the tag and `isDraft` is false"),
        phrase_case("missing tag selection", release, "operator-selected published git tag"),
        phrase_case("missing full revision", release, "full 40-hex commit SHA"),
        phrase_case("targetCommitish trusted", release, "`targetCommitish` alone is not proof"),
        phrase_case("configured target platform", release,
                    "the host `GOHOSTOS` and `GOHOSTARCH`, not the configured `GOOS` and `GOARCH`"),
        phrase_case("missing manifest revision identity", release,
                    "its `revision` equals the resolved full revision"),
        phrase_case("missing manifest toolchain identity", release, "its `toolchain` is `go1.26.10`"),
        phrase_case("missing manifest platform identity", release,
                    "its `GOOS` and `GOARCH` equal the native platform"),
        phrase_case("missing manifest checksum", release, "The published manifest SHA-256"),
        phrase_case("manifest file hash used", release,
                    "binary `sha256` field, not a hash of the manifest file"),
        phrase_case("missing published-binary checksum", release, "A published-binary checksum"),
        ("release-note comparison dropped", edit(
            release, "both the release-note SHA-256 and the published manifest SHA-256",
            "the published manifest SHA-256"), readme, repository_file,
         "missing contract phrase 'compare it with both the release-note SHA-256"),
        ("manifest comparison dropped", edit(
            release, "both the release-note SHA-256 and the published manifest SHA-256",
            "the release-note SHA-256"), readme, repository_file,
         "missing contract phrase 'compare it with both the release-note SHA-256"),
        phrase_case("missing no rebuild fallback", release, "there is no rebuild fallback"),
        phrase_case("missing separate install authority", release, "grants no install authority"),
        phrase_case("missing separate install approval", release, "needs the separate install approval"),
        phrase_case("missing published policy", release, POLICY),
        ("policy only in HTML comment", edit(release, POLICY, f"<!-- {POLICY} -->"), readme,
         repository_file, "missing contract phrase 'Pilot acceptance installs only"),
        ("policy only in code fence", edit(release, POLICY, f"```text\n{POLICY}\n```"), readme,
         repository_file, "missing contract phrase 'Pilot acceptance installs only"),
        ("rewrapped visible policy", edit(release, POLICY, POLICY.replace(" verified ", "\nverified ").replace(
            " a local ", "\na local ")), readme, repository_file, None),
        ("source-built pilot instruction", edit(
            release, "there is no rebuild fallback",
            "there is no rebuild fallback. Do not download a release binary; build from the resolved revision"),
         readme, repository_file, "source build: pilot instruction"),
        ("missing native asset rebuilt locally", edit(
            release, STOPS[3][1], "If either asset of the native binary/manifest pair is missing, "
            "build it locally from the resolved revision instead."), readme, repository_file,
         "source build: pilot instruction"),
        ("source build via staging helper", edit(
            "Installation", "with the accepted published asset as the candidate.",
            "with the staging helper output as the candidate."), readme, repository_file,
         "source build: pilot instruction"),
        phrase_case("missing installed-binary checksum", "Installation",
                    "independent installed-binary checksum"),
        phrase_case("missing reinstall checksum", catalog,
                    "verify the independent installed-binary checksum again"),
        phrase_case("enablement not after checksums", catalog,
                    "Only after both installed-binary checks match"),
        ("installed hash before install approval", edit(
            release, "there is no rebuild fallback",
            "there is no rebuild fallback. Then verify the independent installed-binary checksum: "
            "hash the installed executable"), readme, repository_file,
         "order: '1. Install approval:' must precede 'verify the independent installed-binary"),
        ("binary-only rollback", edit(
            catalog, "restores the complete matched artifact set", "restores only the prior binary"),
         readme, repository_file, "missing contract phrase 'complete matched artifact set'"),
        phrase_case("missing published-byte install entry", "Installation", "Published-byte entry"),
        ("contract without publication gate", edit_contract(
            lambda c: c["release"].pop("matching_github_release")), readme, repository_file,
         "contract: release.matching_github_release missing"),
        ("contract source-built artifact", edit_contract(
            lambda c: c["release"].update(pilot_artifact="source_build")), readme, repository_file,
         "contract: release.pilot_artifact changed"),
        ("contract staged manifest", edit_contract(
            lambda c: c["release"].update(manifest_sha256="recorded_from_staged_manifest")), readme,
         repository_file, "contract: release.manifest_sha256 changed"),
        ("contract without published hash", edit_contract(
            lambda c: c["release"].pop("published_binary_sha256")), readme, repository_file,
         "contract: release.published_binary_sha256 missing"),
        ("contract published hash vs manifest only", edit_contract(
            lambda c: c["release"].update(published_binary_sha256="compared_with_manifest_before_install_approval")),
         readme, repository_file, "contract: release.published_binary_sha256 changed"),
        ("contract rebuild fallback", edit_contract(
            lambda c: c["release"].update(rebuild_fallback={})), readme, repository_file,
         "contract: unexpected release.rebuild_fallback"),
        ("contract obsolete staged hash", edit_contract(
            lambda c: c["release"].update(staged_binary_sha256="compared_before_install_approval")), readme,
         repository_file, "contract: unexpected release.staged_binary_sha256"),
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
        ("disabled members omitted", edit(
            catalog, ": `list_folders`, `search_mail`, `get_message`, `get_thread`, "
            "`list_attachments` and `select_digest_candidates`."), readme, repository_file,
         "catalog: disabled members expected"),
        ("disabled member missing", edit(catalog, "`get_thread`, "), readme,
         repository_file, "catalog: disabled members expected"),
        ("enabled members omitted", edit(
            catalog, ": those six plus `mark_read`, `mark_unread`, `move_mail`, "
            "`archive_mail` and `trash_mail`."), readme, repository_file,
         "catalog: enabled members expected"),
        ("enabled member missing", edit(catalog, "`mark_unread`, "), readme,
         repository_file, "catalog: enabled members expected"),
        ("enabled base set omitted", edit(catalog, "those six plus", "plus"), readme,
         repository_file, "catalog: enabled members expected"),
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
        phrase_case("search_mail assumed to supply identities", scope,
                    "no shipped read tool supplies triage identities"),
        phrase_case("missing identity-source block", scope,
                    "blocked until a published release documents a supported, bounded source"),
        phrase_case("identity decoding allowed", scope, "Never decode, derive or guess identities"),
        phrase_case("missing per-run marker", scope, "Put one per-run unique marker"),
        phrase_case("missing known-pilot result check", scope,
                    "compare every result with the known pilot messages"),
        phrase_case("missing mismatch stop", scope, "stop if any result does not match"),
        phrase_case("actions not blocked on identity source", "Action procedure",
                    "only after the supported identity source above exists"),
        ("contract search_mail supplies identities", edit_contract(
            lambda c: c["identities"].update(search_mail_supplies_uids=True)), readme,
         repository_file, "contract: identities.search_mail_supplies_uids changed"),
        ("contract result check omitted", edit_contract(
            lambda c: c["identities"].pop("result_check")), readme, repository_file,
         "contract: identities.result_check missing"),
    ]
    for path in COMPANIONS:
        missing = f"{path} {COMPANIONS[path][0]}: missing contract phrase 'Pilot acceptance installs only"
        cases += [
            companion_case(f"{path} policy removed", path, POLICY, "", missing),
            companion_case(f"{path} policy only in HTML comment", path, POLICY, f"<!-- {POLICY} -->", missing),
            companion_case(f"{path} policy only in code fence", path, POLICY,
                           f"\n```text\n{POLICY}\n```\n", missing),
            companion_case(f"{path} rewrapped visible policy", path, POLICY,
                           POLICY.replace(" verified ", "\nverified ").replace(" a local ", "\na local "), None),
            companion_case(f"{path} missing native asset rebuilt locally", path,
                           "no native asset is published for it, STOP the binary path",
                           "no native asset is published for it, build it locally from the tag instead",
                           f"{path} {COMPANIONS[path][0]}: missing contract phrase 'If the platform is unsupported"),
        ]
    cases += [
        companion_case("development source-build path dropped", "docs/USER-INSTALL.md",
                       "A source build and published-byte acceptance are different paths.", "",
                       "missing contract phrase 'A source build and published-byte acceptance"),
        companion_case("development recipe removed", "docs/USER-INSTALL.md",
                       'python3 scripts/stage_mail_candidate.py --revision "$REVIEWED_REVISION"', "",
                       "development source-build recipe missing"),
        ("install guide missing", guide, readme, repository_file,
         "docs/USER-INSTALL.md: visible section missing: Source build",
         {**companions, "docs/USER-INSTALL.md": ""}),
        ("contract dotted-key collision", edit_contract(lambda c: (
            c["release"].update(pilot_artifact="source_build"),
            c.update({"release.pilot_artifact": "published_native_binary"}))), readme, repository_file,
         "contract: release.pilot_artifact changed"),
        ("contract dotted key", edit_contract(
            lambda c: c.update({"release.pilot_artifact": "published_native_binary"})), readme,
         repository_file, "contract: unexpected release.pilot_artifact"),
        ("contract duplicate key", guide.replace(
            '"pilot_artifact": "published_native_binary",',
            '"pilot_artifact": "source_build",\n    "pilot_artifact": "published_native_binary",', 1),
         readme, repository_file, "contract: invalid JSON"),
        ("contract object replaced by value", edit_contract(lambda c: c.update(release="published")),
         readme, repository_file, "contract: release changed"),
    ]
    for name, phrase in STOPS:
        cases.append(phrase_case(f"{name} without STOP", release, phrase))
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
    for name, candidate, candidate_readme, exists, expected, *bundle in cases:
        errors = verify(candidate, candidate_readme, exists, bundle[0] if bundle else companions)
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
        companions = {path: (ROOT / path).read_text(encoding="utf-8") for path in COMPANIONS}
        failures = verify(guide, readme, repository_file, companions)
        if args.self_test:
            failures.extend(self_test(guide, readme, companions))
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

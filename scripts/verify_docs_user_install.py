#!/usr/bin/env python3
"""Offline witness for the user-owned Mail guide; uses repository text only."""

import argparse
import ipaddress
import json
from pathlib import Path, PurePosixPath
import re
import sys

ROOT = Path(__file__).resolve().parents[1]
GUIDE = ROOT / "docs/USER-INSTALL.md"
LINK = "docs/USER-INSTALL.md"
FIXTURE = re.compile(r"<!-- user-install-config -->\s*```json\n(.*?)\n```", re.S)
REQUIRED = {
    "Source build": ("REVIEWED_REVISION", "checkout --detach", "go1.26.6",
                     "./cmd/croton-mcp", "go build", "go vet", "go test -race"),
    "User-owned layout": ("absolute", "operator-owned", "0700", "umask 077",
                          "croton-mcp.candidate", "SHA-256"),
    "TLS and configuration prerequisites": (
        "Separately", "Bridge", "regular owner-only file", "effective user",
        "0600", "no group or world permission bits", "no-follow",
        "every path component", "symlinked parents", "Explicit trust",
        "non-CA Bridge leaf", "SubjectPublicKeyInfo", "No inline credentials"),
    "Credential helper contract": (
        "bridge/credentials.go", "internal/config", "pass-backed",
        "absolute `credentialCommand[0]`", "argv without a shell",
        "PATH=/usr/bin:/bin", "`HOME`", "`USER`", "`LOGNAME`", "`LANG`",
        "`LC_ALL`", "when those variables exist", "no arbitrary environment",
        "`GNUPGHOME` and `PASSWORD_STORE_DIR` are not inherited",
        "explicit environment", "absolute programs", "JSON serializer",
        "json.dumps", "quotes, backslashes and control characters",
        "Bridge-generated IMAP username and password", "not the Proton account password",
        "exactly two non-empty string fields", "`username`", "`password`",
        "no interactive stdin", "pre-unlocked", "stderr is discarded",
        "64 KiB", "timeout", "idempotent", "Never print real helper output",
        "synthetic stubs"),
    "Verification and stdio registration": (
        "--self-test", "argument array", "same operator account", "stdout protocol-only",
        "Catalog-only verification", "tools/list", "does not invoke the helper",
        "Explicitly authorized live read", "separately authorizes", "tools/call",
        "no validated Claude Code or Codex compatibility"),
    "Update and rollback": (
        "reviewed revision", "backup", "hash", "permissions", "rename",
        "rename the staged executable and every staged config, helper and trust file",
        "Do not reopen the client session until the complete matched set is installed and verified",
        "Keep launches blocked until the complete prior set is restored and verified",
        "Restore", "previously absent", "catalog-only", "user-service",
        "self-update", "transient", "no commands to modify or restart running services"),
    "Troubleshooting": ("Configuration unreadable", "Catalog succeeds", "synthetic stubs"),
    "Distribution limitations": ("source-build", "does not currently provide published binary releases",
                                 "install packages", "release updater"),
}


def strict_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate JSON key")
        result[key] = value
    return result


def fixture_errors(raw):
    failures = []
    try:
        cfg = json.loads(raw, object_pairs_hook=strict_object)
        # Closed shapes also reject credentials hidden in extra fixture fields.
        if set(cfg) != {"imap", "bounds", "audit"}:
            raise ValueError("unexpected top-level fields")
        imap = cfg["imap"]
        if set(imap) != {"host", "port", "tlsMode", "credentialCommand", "tls"}:
            raise ValueError("unexpected IMAP fields or inline credentials")
        address = ipaddress.ip_address(imap["host"])
        address = getattr(address, "ipv4_mapped", None) or address
        if not address.is_loopback:
            failures.append("fixture: IMAP must be loopback")
        if type(imap["port"]) is not int or not 1 <= imap["port"] <= 65535:
            failures.append("fixture: invalid IMAP port")
        if imap["tlsMode"] not in ("starttls", "implicit"):
            failures.append("fixture: TLS mode required")
        command = imap["credentialCommand"]
        if (not isinstance(command, list) or len(command) != 1
                or not isinstance(command[0], str)
                or not PurePosixPath(command[0]).is_absolute()):
            failures.append("fixture: absolute credential helper required")
        tls = imap["tls"]
        if not isinstance(tls, dict) or set(tls) - {"trustAnchorFile", "spkiSha256"}:
            raise ValueError("unexpected TLS fields")
        anchor = tls.get("trustAnchorFile", "")
        pin = tls.get("spkiSha256", "")
        if not anchor and not pin:
            failures.append("fixture: explicit trust required")
        if anchor and (not isinstance(anchor, str) or not PurePosixPath(anchor).is_absolute()):
            failures.append("fixture: absolute trust file required")
        if pin and (not isinstance(pin, str) or not re.fullmatch(r"[0-9a-f]{64}", pin)):
            failures.append("fixture: invalid SPKI pin")
        if cfg["bounds"] != {"maxSearchResults": 50} or cfg["audit"] != {"enabled": True}:
            failures.append("fixture: unexpected bounds or audit fields")
    except (ValueError, TypeError, KeyError, AttributeError):
        failures.append("fixture: invalid JSON configuration shape")

    return failures


def verify(guide, readme):
    failures = []
    sections = dict(re.findall(r"^## ([^\n]+)\n(.*?)(?=^## |\Z)", guide, re.M | re.S))
    for heading, phrases in REQUIRED.items():
        if heading not in sections:
            failures.append(f"section missing: {heading}")
            continue
        normalized = " ".join(sections[heading].split()).replace("**", "")
        for phrase in phrases:
            if phrase not in normalized:
                failures.append(f"{heading}: missing contract phrase {phrase!r}")

    fixtures = FIXTURE.findall(guide)
    if len(fixtures) != 1:
        failures.append("fixture: expected one marked JSON fence")
    else:
        failures.extend(fixture_errors(fixtures[0]))

    if not re.search(r"\[[^\]]+\]\(docs/USER-INSTALL\.md\)", readme):
        failures.append("README: guide link missing")
    if not (ROOT / LINK).is_file() or (ROOT / LINK).resolve() != GUIDE.resolve():
        failures.append("README: guide link does not resolve")

    return failures


def self_test(guide, readme):
    match = FIXTURE.search(guide)
    if match is None:
        raise ValueError("self-test requires the baseline fixture")

    def mutate_config(change):
        cfg = json.loads(match[1])
        change(cfg)
        return guide[:match.start(1)] + json.dumps(cfg) + guide[match.end(1):]

    cases = [
        ("baseline", guide, readme, None),
        ("IPv6 loopback", mutate_config(lambda c: c["imap"].update(host="::1")),
         readme, None),
        ("SPKI trust", mutate_config(lambda c: c["imap"].update(
            tls={"spkiSha256": "a" * 64})), readme, None),
        ("missing trust", mutate_config(lambda c: c["imap"].update(tls={})),
         readme, "explicit trust required"),
        ("relative helper", mutate_config(lambda c: c["imap"].update(
            credentialCommand=["bin/helper"])), readme, "absolute credential helper"),
        ("missing update/rollback", re.sub(
            r"^## Update and rollback\n.*?(?=^## |\Z)", "", guide, flags=re.M | re.S),
         readme, "section missing: Update and rollback"),
        ("binary-only installation", guide.replace(
            "rename the staged executable and every staged config, helper and trust file",
            "rename only the staged executable"),
         readme, "missing contract phrase 'rename the staged executable and every staged config, helper and trust file'"),
        ("non-loopback", mutate_config(lambda c: c["imap"].update(host="192.0.2.1")),
         readme, "IMAP must be loopback"),
        ("inline credentials", mutate_config(lambda c: c["imap"].update(password="synthetic")),
         readme, "invalid JSON configuration shape"),
        ("missing link", guide, readme.replace(LINK, "docs/MISSING.md"), "guide link missing"),
        ("missing no-follow caution", guide.replace("no-follow", "ordinary"),
         readme, "missing contract phrase 'no-follow'"),
        ("missing environment contract", guide.replace("PATH=/usr/bin:/bin", "PATH=other"),
         readme, "missing contract phrase 'PATH=/usr/bin:/bin'"),
        ("malformed JSON", guide[:match.start(1)] + "{" + guide[match.end(1):],
         readme, "invalid JSON configuration shape"),
    ]
    executed = 0
    failed = []
    for name, candidate, candidate_readme, expected in cases:
        errors = verify(candidate, candidate_readme)
        executed += 1
        if (expected is None and errors) or (expected is not None and not any(
                expected in error for error in errors)):
            failed.append(f"self-test {name}: unexpected result {errors}")

    print(f"user-install self-test: {executed} cases executed")
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
        failures = verify(guide, readme)
        if args.self_test:
            failures.extend(self_test(guide, readme))
    except (OSError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("user-install docs OK (offline synthetic checks only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

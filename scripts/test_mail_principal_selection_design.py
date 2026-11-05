#!/usr/bin/env python3
"""Offline checks: the Mail principal and selection-epoch design record.

The record must state its contract in visible sections and GFM tables. HTML
comments, including an unterminated one, and top-level fenced code blocks are
removed before matching, and whitespace is collapsed so that wrapped prose
still matches. The checks cover record consistency and citation form only:
they do not prove upstream semantics, Bridge behavior, human approval or the
absence of secrets in arbitrary text.

Run without arguments to verify the real record. Run with --self-test to also
check synthetic fixtures derived from it.
"""
from pathlib import Path
import re
import sys
import unittest


REPO = Path(__file__).resolve().parent.parent
RECORD_NAME = "docs/design/0007-mail-principal-selection.md"
RECORD = REPO / RECORD_NAME

BASELINE = "103557c17d75a7db52214fbcefe5b383c0327fca"
PINS = {
    "croton": BASELINE,
    "ProtonMail/proton-bridge": "b9c5dac1651437100c40896dacd778a0518a26f2",
    "ProtonMail/gluon": "7e800978ab4a9942829eb6116e33ea0278c03e91",
}
CITATION = re.compile(r"([A-Za-z]+(?:/[A-Za-z-]+)?)@([0-9a-f]{40}):([A-Za-z0-9_./-]+\.go)")

CURRENT_SURFACE = {
    "bridge/credentials.go": "only username and password",
    "bridge/imapclient.go": "there is no principal query",
    "bridge/search.go": "process-local HMAC IDs",
}
CANDIDATES = (
    "native authenticated-session identity",
    "trusted Bridge-local metadata bound to that session",
    "privileged helper/config attestation",
)
CANDIDATE_VERDICTS = ("source-supported", "unsupported")
EXCLUSIONS = ("username-only", "TLS-only", "credential-fingerprint-only", "UID-tuple-only", "opaque-ID-only")

DECISION_FIELDS = (
    "decision", "design candidate", "runtime implementation", "runtime proof",
    "fallback", "account data returned", "missing evidence",
)
HANDSHAKE = {
    "tool": "get_mail_context",
    "input": "empty closed object",
    "readOnlyHint": "true",
    "side effects": "none",
    "response fields": "selectionEpoch, accountContinuity",
    "selectionEpoch": "opaque 64-character lowercase-hex token, not derived from the principal",
    "accountContinuity": "literal verified",
    "response bound": "512 bytes",
    "principal evidence bound": "256 bytes",
    "binding record bound": "4096 bytes",
    "deadline": "existing configured command deadline",
    "success precondition": "authoritative private principal verification",
    "refusal": "unsupported, unavailable or stale_id with no epoch and no account data",
    "raw-tuple writes": "unchanged",
    "catalog": "unchanged",
}
CASES = (
    "colliding-tuples", "helper-switch-reconnect", "restart-old-selection", "same-principal-rotation",
    "principal-unavailable", "principal-mismatch", "context-dispatch-race", "stale-prepared-handle",
)

PROSE = {
    "Problem": (
        "Successful IMAP login plus TLS identity does not prove that a later connection belongs to the "
        "previously selected Proton account.",
        "Public UID fields and a random epoch cannot substitute for private principal verification.",
    ),
    "Current surface": (
        "None of the current surface is an authoritative-principal source or a public epoch API.",
        "its read retry loop in `Adapter.execute` is not write retry authority",
    ),
    "Candidate evidence": (
        "These commits do not describe the installed Bridge version.",
        "No executed evidence exists for any row.",
    ),
    "Proposed handshake": (
        "gated on a real authoritative principal source.",
        "The catalog remains unchanged until a separate approved implementation registers anything.",
        "`get_mail_context` takes an empty closed input object, publishes `readOnlyHint: true` and has no "
        "mutation or preparation side effect.",
        "A successful response contains only `selectionEpoch` and `accountContinuity`.",
        "`selectionEpoch` is an opaque 64-character lowercase-hex token that is not derived from the principal.",
        "`accountContinuity` is the literal `verified`.",
        "The complete serialized result is at most 512 bytes.",
        "A successful response is permitted only after authoritative private principal verification.",
        "Without it the call returns the existing static `unsupported`, `unavailable` or `stale_id` category "
        "as applicable, with no epoch and no account data.",
        "No response, error string or audit record contains a principal, address, credential, endpoint "
        "detail, unkeyed principal fingerprint or mailbox content.",
        "Raw mailbox/UIDVALIDITY/UID write inputs stay unchanged under M2.",
    ),
    "Binding state": (
        "One owner-pinned account binding exists per configured adapter.",
        "Private principal evidence is at most 256 bytes, and any protected persistent binding record is at "
        "most 4096 bytes.",
        "No mail content or credential is retained.",
        "Any eventual persisted binding uses an owner-only, no-follow, regular-file policy.",
        "a tool call never enrolls a principal.",
        "This ticket creates no such file.",
        "the design stays blocked rather than adopting a larger unbounded fallback.",
    ),
    "Epoch lifecycle": (
        "A new process invalidates old snapshots and prepared handles.",
        "Reconnect verifies the same pinned principal before any account data or write.",
        "Verified same-principal credential rotation does not itself change account identity.",
        "Mismatch or unavailable continuity aborts access, clears client approvals and prepared objects, and "
        "never silently enrolls the new principal.",
        "A new random epoch cannot legitimize an unverified account.",
        "Context acquisition uses the existing finite configured command deadline, with no unbounded polling "
        "or discovery and no automatic retry after a possible mutation.",
    ),
    "Enforcement split": (
        "Server-enforced: principal verification before any account data or write, refusal without an epoch "
        "when continuity is unavailable, the response bounds, and a new epoch after process restart or "
        "principal change.",
        "Client gate: the trusted client compares the context epoch, invalidates old ordinal snapshots and "
        "approvals, and serializes context changes versus dispatch.",
        "Raw tuples alone do not prove selection age;",
        "this design grants none.",
    ),
    "Future acceptance matrix": (
        "verified-TLS, public-MCP and configured-client tests with independent state and identity oracles.",
        "Zero wrong-account disclosure and zero wrong-account writes must be observed, not inferred from a "
        "transcript refusal.",
        "All statuses remain NOT RUN.",
    ),
    "Status": ("design only. No get_mail_context implementation, registration or proof exists",),
}
BLOCKED_PROSE = (
    "Unsupported is a valid completed investigation outcome, not an invented successful API.",
    "No fallback is enabled and no new account data is returned.",
)

FORBIDDEN_FIELD = re.compile(r"principal|address|credential|password|username|subject|body|mailbox|account(?!Continuity)",
                             re.I)
EMAIL = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9-]+(?:\.[A-Za-z0-9-]+)*\.[A-Za-z]{2,}")
CLAIMS = re.compile(
    r"\bget_mail_context`? (?:is|was|has been) (?:now )?(?:registered|implemented|shipped|available)"
    r"|\b(?:is|was|has been) installed\b|\binstallation (?:is|was) (?:complete|performed)"
    r"|\blive (?:account )?(?:proof|test|run|authority) (?:passed|succeeded|was granted|is granted|exists)"
    r"|\bstatus:? (?:PASS|PASSED|EXECUTED|LIVE)\b",
    re.I,
)


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


def sections(text):
    """Map each visible level-two heading to the list of its raw bodies."""
    parts = re.split(r"^## +(.+?)\s*$", visible(text), flags=re.M)
    found = {}
    for i in range(1, len(parts), 2):
        found.setdefault(parts[i], []).append(parts[i + 1])
    return found


def cells(line):
    return [cell.strip() for cell in line.strip().strip("|").split("|")]


def table(body):
    """Return (header, rows) of the one GFM table in body, or None."""
    lines = body.splitlines()
    starts = [i for i, line in enumerate(lines) if line.startswith("|")
              and (i == 0 or not lines[i - 1].startswith("|"))]
    if len(starts) != 1 or starts[0] + 1 >= len(lines):
        return None

    start = starts[0]
    header = cells(lines[start])
    delimiter = cells(lines[start + 1])
    if len(delimiter) != len(header) or not all(re.fullmatch(r":?-+:?", cell) for cell in delimiter):
        return None
    rows = []
    for line in lines[start + 2:]:
        if not line.startswith("|"):
            break
        row = cells(line)
        if len(row) != len(header):
            return None
        rows.append(dict(zip(header, row)))

    return header, rows


def citation_errors(where, value):
    if not value:
        return [f"{where}: citation missing"]

    errors = []
    for citation in (part.strip() for part in value.split(";")):
        match = CITATION.fullmatch(citation)
        if match is None:
            errors.append(f"{where}: citation {citation!r} is not repository@commit:file")
        elif PINS.get(match.group(1)) != match.group(2):
            errors.append(f"{where}: citation {citation!r} is not at an identified immutable pin")
    return errors


class Record:
    def __init__(self, text):
        self.text = text
        self.found = sections(text)
        self.errors = []

    def body(self, heading):
        bodies = self.found.get(heading, [])
        if len(bodies) != 1:
            self.errors.append(f"need one visible {heading!r} section")
            return None
        return bodies[0]

    def rows(self, heading, columns):
        body = self.body(heading)
        if body is None:
            return []
        parsed = table(body)
        if parsed is None:
            self.errors.append(f"{heading}: need one well-formed table")
            return []
        header, rows = parsed
        if tuple(header) != tuple(columns):
            self.errors.append(f"{heading}: table columns must be {list(columns)}")
            return []
        return rows

    def keyed(self, heading, key, columns, expected):
        rows = self.rows(heading, columns)
        names = [row[key] for row in rows]
        if names and sorted(names) != sorted(expected):
            self.errors.append(f"{heading}: rows must be exactly {sorted(expected)}, found {names}")
        return {row[key]: row for row in rows}


def check_clauses(record, heading, clauses):
    body = record.body(heading)
    if body is None:
        return
    flat = " ".join(body.split())
    for clause in clauses:
        if " ".join(clause.split()) not in flat:
            record.errors.append(f"{heading}: missing clause {clause!r}")


def check_current_surface(record):
    columns = ("Path", "Symbol", "Citation", "Finding", "Principal source", "Public epoch")
    rows = record.keyed("Current surface", "Path", columns, CURRENT_SURFACE)
    for path, row in rows.items():
        if path not in CURRENT_SURFACE:
            continue
        where = f"Current surface {path}"
        if not row["Symbol"]:
            record.errors.append(f"{where}: symbol missing")
        record.errors.extend(citation_errors(where, row["Citation"]))
        if not row["Citation"].endswith(":" + path) or not row["Citation"].startswith("croton@"):
            record.errors.append(f"{where}: citation must name this file at the Croton baseline")
        if CURRENT_SURFACE[path] not in row["Finding"]:
            record.errors.append(f"{where}: finding must record {CURRENT_SURFACE[path]!r}")
        if row["Principal source"] != "none" or row["Public epoch"] != "none":
            record.errors.append(f"{where}: must not be relabeled as a principal source or public epoch API")


def check_candidates(record):
    columns = ("Candidate", "Verdict", "Trust and connection binding", "Evidence kind", "Citation", "Symbol",
               "Limit", "Executed evidence")
    rows = record.keyed("Candidate evidence", "Candidate", columns, CANDIDATES)
    for name, row in rows.items():
        where = f"Candidate {name}"
        if row["Verdict"] not in CANDIDATE_VERDICTS:
            record.errors.append(f"{where}: verdict {row['Verdict']!r} is not one of {list(CANDIDATE_VERDICTS)}")
        if not row["Trust and connection binding"]:
            record.errors.append(f"{where}: trust and connection binding missing")
        if row["Evidence kind"] != "source inspection":
            record.errors.append(f"{where}: evidence kind must be source inspection")
        record.errors.extend(citation_errors(where, row["Citation"]))
        if not row["Symbol"]:
            record.errors.append(f"{where}: symbol missing")
        if not row["Limit"]:
            record.errors.append(f"{where}: limit missing")
        if row["Executed evidence"] != "NOT RUN":
            record.errors.append(f"{where}: executed evidence must be NOT RUN")

    exclusions = record.keyed("Insufficient claims", "Claim", ("Claim", "Verdict", "Reason"), EXCLUSIONS)
    for name, row in exclusions.items():
        if row["Verdict"] != "insufficient" or not row["Reason"]:
            record.errors.append(f"Insufficient claims {name}: must be insufficient with a reason")

    return rows


def check_decision(record, candidates):
    rows = record.keyed("Decision", "Field", ("Field", "Value"), DECISION_FIELDS)
    if not all(field in rows for field in DECISION_FIELDS):
        return
    value = {field: rows[field]["Value"] for field in DECISION_FIELDS}

    for field in ("runtime implementation", "runtime proof"):
        if value[field] != "unavailable":
            record.errors.append(f"Decision: {field} must be unavailable")
    for field in ("fallback", "account data returned"):
        if value[field] != "none":
            record.errors.append(f"Decision: {field} must be none")

    supported = [name for name, row in candidates.items() if row["Verdict"] == "source-supported"]
    if value["decision"] == "blocked":
        if value["design candidate"] != "none":
            record.errors.append("Decision: blocked names no design candidate")
        if value["missing evidence"] in ("", "none"):
            record.errors.append("Decision: blocked needs concrete missing evidence")
        if supported:
            record.errors.append("Decision: blocked contradicts a source-supported candidate")
        check_clauses(record, "Decision", BLOCKED_PROSE)
        blockers = record.rows("Blockers", ("Requirement", "Missing source or binding evidence"))
        if not blockers or any(not row["Missing source or binding evidence"] for row in blockers):
            record.errors.append("Blockers: blocked needs concrete missing source or binding evidence")
    elif value["decision"] == "design-only":
        if value["design candidate"] not in supported:
            record.errors.append("Decision: design-only must name a source-supported candidate")
        if value["missing evidence"] != "none":
            record.errors.append("Decision: design-only records no missing evidence")
    else:
        record.errors.append(f"Decision: {value['decision']!r} is not design-only or blocked")


def check_handshake(record):
    rows = record.keyed("Proposed handshake", "Field", ("Field", "Value"), HANDSHAKE)
    for field, expected in HANDSHAKE.items():
        if field in rows and rows[field]["Value"] != expected:
            record.errors.append(f"Proposed handshake: {field} must be {expected!r}")
    fields = rows.get("response fields", {}).get("Value", "")
    for name in fields.split(","):
        if FORBIDDEN_FIELD.search(name):
            record.errors.append(f"Proposed handshake: response field {name.strip()!r} exposes account data")


def check_matrix(record):
    columns = ("Case", "Setup", "Before data/write assertion", "Client invalidation", "Mutation replay", "Status")
    rows = record.keyed("Future acceptance matrix", "Case", columns, CASES)
    for name, row in rows.items():
        where = f"Future acceptance matrix {name}"
        if not row["Setup"] or not row["Before data/write assertion"]:
            record.errors.append(f"{where}: setup and before data/write assertion required")
        if not row["Client invalidation"].startswith("required"):
            record.errors.append(f"{where}: client invalidation must be required")
        if row["Mutation replay"] != "none":
            record.errors.append(f"{where}: mutation replay must be none")
        if row["Status"] != "NOT RUN":
            record.errors.append(f"{where}: status must be NOT RUN")
    names = " ".join(row["Setup"] for row in rows.values())
    if "accountA" not in names or "accountB" not in names:
        record.errors.append("Future acceptance matrix: synthetic roles accountA and accountB required")


def check_provenance(record):
    """Reject private-looking identities anywhere and live-authority claims in visible text."""
    for address in EMAIL.findall(record.text):
        record.errors.append(f"example identity {address!r} must be a synthetic role")
    for claim in CLAIMS.finditer(" ".join(visible(record.text).split())):
        record.errors.append(f"claim {claim.group(0)!r} asserts registration, installation or live authority")


def record_errors(text):
    record = Record(text)
    for heading, clauses in PROSE.items():
        check_clauses(record, heading, clauses)
    check_current_surface(record)
    candidates = check_candidates(record)
    check_decision(record, candidates)
    check_handshake(record)
    check_matrix(record)
    check_provenance(record)
    return record.errors


def repository_errors():
    """The proposed tool stays unregistered and no other document claims it."""
    errors = []
    for path in sorted(REPO.rglob("*.go")):
        if "get_mail_context" in path.read_text(encoding="utf-8"):
            errors.append(f"{path.relative_to(REPO)}: get_mail_context must not be registered")
    for path in sorted([REPO / "README.md", *REPO.glob("docs/**/*.md")]):
        relative = str(path.relative_to(REPO))
        if relative != RECORD_NAME and "get_mail_context" in path.read_text(encoding="utf-8"):
            errors.append(f"{relative}: get_mail_context is documented outside its design record")
    return errors


def replace_row(text, key, new_row):
    pattern = re.compile(r"^\| " + re.escape(key) + r" \|.*$", re.M)
    mutated, count = pattern.subn(lambda _: new_row, text)
    assert count == 1, key
    return mutated


def drop_row(text, key):
    mutated, count = re.subn(r"^\| " + re.escape(key) + r" \|.*\n", "", text, flags=re.M)
    assert count == 1, key
    return mutated


def set_cell(text, key, column, value):
    """Set one cell of the row whose first cell is key, by zero-based column."""
    line = re.search(r"^\| " + re.escape(key) + r" \|.*$", text, flags=re.M).group(0)
    row = cells(line)
    row[column] = value
    return replace_row(text, key, "| " + " | ".join(row) + " |")


def substitute(text, clause, replacement):
    """Replace a prose clause even when the record wraps it across lines."""
    mutated, count = re.subn(r"\s+".join(map(re.escape, clause.split())), lambda _: replacement, text)
    assert count > 0, clause
    return mutated


def design_only(text):
    """A synthetic, evidence-supported design-only bundle derived from the real record."""
    text = set_cell(text, "native authenticated-session identity", 1, "source-supported")
    text = set_cell(text, "decision", 1, "design-only")
    text = set_cell(text, "design candidate", 1, "native authenticated-session identity")
    text = set_cell(text, "missing evidence", 1, "none")
    return text


class PrincipalSelectionDesign(unittest.TestCase):
    def setUp(self):
        self.assertTrue(RECORD.is_file(), "principal selection design record is absent")
        self.text = RECORD.read_text(encoding="utf-8")

    def assertRejected(self, text, fragment):
        errors = record_errors(text)
        self.assertTrue(any(fragment in error for error in errors), (fragment, errors))

    def test_real_record_accepted(self):
        self.assertEqual(record_errors(self.text), [])
        self.assertEqual(repository_errors(), [])

    def test_current_surface(self):
        for path in CURRENT_SURFACE:
            with self.subTest(dropped=path):
                self.assertRejected(drop_row(self.text, path), "Current surface: rows must be exactly")
            with self.subTest(relabeled=path):
                self.assertRejected(set_cell(self.text, path, 4, "authoritative"), "must not be relabeled")
                self.assertRejected(set_cell(self.text, path, 5, "selectionEpoch"), "must not be relabeled")
            with self.subTest(finding=path):
                self.assertRejected(set_cell(self.text, path, 3, "Exposes the account."), "finding must record")
            with self.subTest(citation=path):
                self.assertRejected(set_cell(self.text, path, 2, "croton@main:" + path), "repository@commit:file")
        extra = self.text.replace("| bridge/search.go |", "| bridge/bridge.go | openSession | "
                                  "croton@" + BASELINE + ":bridge/bridge.go | x | none | none |\n| bridge/search.go |")
        self.assertRejected(extra, "Current surface: rows must be exactly")

    def test_candidate_matrix(self):
        for name in CANDIDATES:
            with self.subTest(dropped=name):
                self.assertRejected(drop_row(self.text, name), "Candidate evidence: rows must be exactly")
            with self.subTest(verified=name):
                self.assertRejected(set_cell(self.text, name, 1, "verified"), "is not one of")
            for column, fragment in ((2, "binding missing"), (5, "symbol missing"), (6, "limit missing")):
                with self.subTest(name=name, column=column):
                    self.assertRejected(set_cell(self.text, name, column, ""), fragment)
            for citation in ("", "ProtonMail/proton-bridge@master:internal/x.go",
                             "ProtonMail/gluon@" + "0" * 40 + ":internal/x.go"):
                with self.subTest(name=name, citation=citation):
                    self.assertRejected(set_cell(self.text, name, 4, citation), "citation")
            with self.subTest(executed=name):
                self.assertRejected(set_cell(self.text, name, 7, "PASSED"), "NOT RUN")
        for claim in EXCLUSIONS:
            with self.subTest(exclusion=claim):
                self.assertRejected(drop_row(self.text, claim), "Insufficient claims: rows must be exactly")
                self.assertRejected(set_cell(self.text, claim, 1, "sufficient"), "must be insufficient")

    def test_decision_states(self):
        self.assertEqual(record_errors(design_only(self.text)), [], "positive design-only bundle")
        self.assertRejected(set_cell(design_only(self.text), "runtime proof", 1, "available"),
                            "runtime proof must be unavailable")
        self.assertRejected(set_cell(design_only(self.text), "runtime implementation", 1, "available"),
                            "runtime implementation must be unavailable")
        self.assertRejected(set_cell(self.text, "missing evidence", 1, "none"), "concrete missing evidence")
        self.assertRejected(set_cell(self.text, "missing evidence", 1, ""), "concrete missing evidence")
        start = self.text.index("\n## Blockers\n")
        no_blockers = self.text[:start] + self.text[self.text.index("\n## ", start + 1):]
        self.assertRejected(no_blockers, "need one visible 'Blockers' section")
        self.assertRejected(set_cell(self.text, "session-bound principal", 1, ""), "concrete missing source")
        self.assertRejected(set_cell(self.text, "fallback", 1, "username"), "fallback must be none")
        self.assertRejected(set_cell(self.text, "account data returned", 1, "address"),
                            "account data returned must be none")
        self.assertRejected(set_cell(self.text, "decision", 1, "verified"), "is not design-only or blocked")
        unsupported_design = set_cell(set_cell(set_cell(self.text, "decision", 1, "design-only"),
                                               "design candidate", 1, CANDIDATES[0]), "missing evidence", 1, "none")
        self.assertRejected(unsupported_design, "must name a source-supported candidate")
        self.assertRejected(set_cell(self.text, CANDIDATES[1], 1, "source-supported"),
                            "blocked contradicts a source-supported candidate")
        for clause in BLOCKED_PROSE:
            with self.subTest(clause=clause):
                self.assertRejected(substitute(self.text, clause, ""), "Decision: missing clause")

    def test_handshake(self):
        for field in HANDSHAKE:
            with self.subTest(dropped=field):
                self.assertRejected(drop_row(self.text, field), "Proposed handshake: rows must be exactly")
            with self.subTest(changed=field):
                self.assertRejected(set_cell(self.text, field, 1, "unbounded"), f"{field} must be")
        for name in ("principalId", "address", "credentialHash", "mailboxSubject"):
            with self.subTest(field=name):
                self.assertRejected(set_cell(self.text, "response fields", 1,
                                             "selectionEpoch, accountContinuity, " + name), "exposes account data")
        for heading in ("Proposed handshake", "Binding state", "Epoch lifecycle", "Enforcement split"):
            for clause in PROSE[heading]:
                with self.subTest(heading=heading, clause=clause):
                    self.assertRejected(substitute(self.text, clause, ""), f"{heading}: missing clause")

    def test_acceptance_matrix(self):
        for case in CASES:
            with self.subTest(dropped=case):
                self.assertRejected(drop_row(self.text, case), "Future acceptance matrix: rows must be exactly")
            with self.subTest(status=case):
                self.assertRejected(set_cell(self.text, case, 5, "PASSED"), "status must be NOT RUN")
            with self.subTest(replay=case):
                self.assertRejected(set_cell(self.text, case, 4, "once"), "mutation replay must be none")
            with self.subTest(invalidation=case):
                self.assertRejected(set_cell(self.text, case, 3, "optional"), "client invalidation must be required")
            with self.subTest(assertion=case):
                self.assertRejected(set_cell(self.text, case, 2, ""), "before data/write assertion required")
        self.assertRejected(self.text.replace("accountB", "second account"), "accountA and accountB")

    def test_visibility_and_provenance(self):
        headings = ("Current surface", "Candidate evidence", "Insufficient claims", "Decision", "Blockers",
                    "Proposed handshake", "Epoch lifecycle", "Future acceptance matrix")
        for heading in headings:
            start = self.text.index(f"\n## {heading}\n")
            end = self.text.find("\n## ", start + 1)
            for name, hidden in (
                ("comment", self.text[:start] + "\n<!--" + self.text[start:end] + "\n-->" + self.text[end:]),
                ("unterminated comment", self.text[:start] + "\n<!--" + self.text[start:]),
                ("fence", self.text[:start] + "\n```text" + self.text[start:end] + "\n```\n" + self.text[end:]),
            ):
                with self.subTest(heading=heading, hidden=name):
                    self.assertRejected(hidden, f"need one visible {heading!r} section")
        for clause in PROSE["Epoch lifecycle"]:
            for name, wrap in (("comment", "<!-- {} -->"), ("fence", "\n```text\n{}\n```\n")):
                with self.subTest(clause=clause, hidden=name):
                    self.assertRejected(substitute(self.text, clause, wrap.format(clause)), "missing clause")
        hidden_row = self.text.replace("| catalog | unchanged |", "<!-- | catalog | unchanged | -->")
        self.assertRejected(hidden_row, "Proposed handshake: rows must be exactly")

        for example in ("alice@proton.me", "owner@example.com"):
            with self.subTest(example=example):
                self.assertRejected(self.text.replace("accountA and accountB expose",
                                                      example + " and accountB expose"), "synthetic role")
                self.assertRejected(self.text + f"\n<!-- {example} -->\n", "synthetic role")
        for claim in ("`get_mail_context` is registered.", "get_mail_context has been implemented.",
                      "The binding file is installed.", "Live account proof passed."):
            with self.subTest(claim=claim):
                self.assertRejected(self.text.replace("## Status\n", "## Status\n\n" + claim + "\n"), "asserts")


def main(arguments):
    if arguments == ["--self-test"]:
        suite = unittest.defaultTestLoader.loadTestsFromTestCase(PrincipalSelectionDesign)
        result = unittest.TextTestRunner(verbosity=2).run(suite)
        return 0 if result.wasSuccessful() else 1
    if arguments:
        print("usage: test_mail_principal_selection_design.py [--self-test]", file=sys.stderr)
        return 2

    if not RECORD.is_file():
        print(f"{RECORD_NAME}: missing", file=sys.stderr)
        return 1
    errors = record_errors(RECORD.read_text(encoding="utf-8")) + repository_errors()
    for error in errors:
        print(f"{RECORD_NAME}: {error}", file=sys.stderr)
    if errors:
        return 1

    print(f"{RECORD_NAME}: ok")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))

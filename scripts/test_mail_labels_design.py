#!/usr/bin/env python3
"""Structural checks: the planned Mail label contract is recorded in visible prose.

The label design must state each required clause in the visible prose of its
named section. HTML comments, including an unterminated one, and top-level
fenced code blocks are removed before matching, and whitespace is collapsed so
that wrapped lines still match. Every test also deletes or hides each clause in
a copy of the record and requires the check to reject it. These are offline
documentation witnesses: they do not prove human approval, Bridge behavior or
the absence of runtime data loss.
"""
from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parent.parent
RECORD = REPO / "docs/design/0006-mail-labels.md"
TRIAGE = REPO / "docs/design/0005-mail-triage.md"
README = REPO / "README.md"
LINK = "docs/design/0006-mail-labels.md"

TAG = ("Tag semantics", (
    "Adding an existing label preserves the message's source folder, its "
    "source-folder UID and Seen state, and every label it already carries.",
    "Adding a second label to a message that already carries one keeps the first label.",
    "Removing one label preserves the message, its folder and every other label it carries.",
    "A label is a tag, not a folder: labeling is never a folder MOVE.",
    "No label tool may be implemented as, or substitute for, `MOVE`, and no "
    "folder tool may be used to add or remove a label.",
))
IDENTITY = ("Tool and approval contract", (
    "`add_label` takes `mailbox`, `uidvalidity` and `uids` identifying messages "
    "in a source folder, plus `label`, the exact name of one existing label.",
    "`remove_label` takes `mailbox`, `uidvalidity` and `uids` where `mailbox` is "
    "the exact label mailbox under `Labels/`, and the UIDVALIDITY and UIDs belong "
    "to that label view.",
    "Each call names exactly one existing label.",
    "A request carries 1 through 50 unique positive UIDs.",
    "any duplicate UID rejects the entire request before a write.",
    "Folder UIDs are never silently reused as label-view UIDs.",
    "the server invents no mapping from a source-folder UID to a label-view UID",
    "One approval covers one exact payload: one tool, one mailbox, one "
    "UIDVALIDITY, one ordered UID list and, for `add_label`, one label.",
    "The trusted client shows the exact resolved payload and obtains approval for it",
    "A missing, unselectable or recreated label, a UIDVALIDITY generation mismatch "
    "and an absent UID all fail closed without a write",
))
WIRE = ("Wire allowlist", (
    "`add_label` may send only a single-UID `UID COPY` from the selected source "
    "folder to the exact selectable label mailbox under `Labels/`.",
    "`remove_label` may send, on the selected label view only, a single-UID "
    "`UID STORE` adding the `\\Deleted` flag followed by `UID EXPUNGE` of that same UID.",
    "Removal requires the `UIDPLUS` capability advertised on the actual "
    "authenticated removal session before its first write.",
    "Without it removal is unsupported and sends nothing; there is no library "
    "fallback to bare EXPUNGE.",
    "A dedicated removal transport guard binds the selected label view, the exact "
    "UID and the exact two-stage command sequence, and refuses anything else.",
    "Plain `EXPUNGE`, `CLOSE`, `MOVE` and `UID MOVE`, COPY to any folder, and "
    "every other flag or flag change are refused.",
    "No close or cleanup operation may implicitly expunge other `\\Deleted` entries.",
    "The shipped triage prohibition of COPY and EXPUNGE is unchanged: `move_mail`, "
    "`archive_mail` and `trash_mail` never gain a COPY or EXPUNGE fallback.",
))
PARTIAL = ("Partial and unknown outcomes", (
    "reports `applied`, `refused`, `not_attempted` or `unknown` per UID in input order.",
    "No write is replayed after a possible write.",
    "A timeout, disconnect or otherwise uncertain response to the `\\Deleted` "
    "STORE or to UID EXPUNGE makes that UID `unknown`: the call stops, nothing is "
    "replayed and the transport is aborted.",
    "If the `\\Deleted` STORE succeeds and UID EXPUNGE then fails definitively, "
    "the removal is a partial write, never a safe refusal.",
    "Marking `\\Deleted` is a partial side effect, not approval of permanent deletion.",
    "Cleanup after a partial or unknown removal requires a separate read that "
    "returns a fresh label-view identity and a new explicit approval of a new "
    "exact payload.",
))
EVIDENCE = ("Upstream evidence", (
    "ProtonMail/proton-bridge commit b9c5dac1651437100c40896dacd778a0518a26f2, "
    "`internal/services/imapservice/connector.go`, whose `go.mod` pins Gluon 7e800978ab4a.",
    "In that Gluon revision `handle_copy.go` calls `mailbox.Copy`, and "
    "`handleUIDExpunge` passes the UID sequence to `mailbox.Expunge`, whereas "
    "plain EXPUNGE supplies nil",
    "This is static source evidence, not proof of the installed Bridge version "
    "or of live semantics.",
))
PROOF = ("Proof gates", (
    "No label tool is registered at runtime until every proof case below passes.",
    "Multiple labels and folder retention: adding a second label keeps the first "
    "label, the source folder, its UID and Seen state, and removing one keeps the "
    "message, its folder and the other label.",
    "Colliding folder and label UIDs: equal UID numbers in a folder and a label "
    "view never cross, and a folder UID never reaches the label view.",
    "Cross-view Deleted isolation: a `\\Deleted` mark in the label view never "
    "affects the same message in its folder or another label view.",
    "Unrelated Deleted label entries: other entries already marked `\\Deleted` in "
    "the label view survive removal untouched.",
    "Missing UIDPLUS: without `UIDPLUS` on the removal session removal sends no write.",
    "Deleted-marker partial failure: a definitive UID EXPUNGE failure after a "
    "successful `\\Deleted` STORE reports the partial outcome and stops.",
    "Lost responses at each stage: a lost COPY, STORE or UID EXPUNGE response "
    "yields `unknown`, a transport abort and no replay.",
    "Label rename and recreate races: a label renamed, deleted or recreated "
    "between listing and write fails closed.",
    "If those proofs cannot establish a safe scoped removal, removal remains "
    "unavailable rather than substituting MOVE or generic EXPUNGE.",
))
ROLLOUT = ("Opt-in and rollout", (
    "`labels.enabled` is a prospective local boolean, default false, independent "
    "of `mutations.enabled`: neither setting registers the other's tools or "
    "implies the other.",
    "The current configuration schema has no `labels` member and rejects it as an "
    "unknown key; no current tool is registered by it, and this record gives no "
    "instruction to set it.",
    "Implementation and runtime registration require a separately approved change "
    "after every proof gate below passes.",
    "Live acceptance on a Proton account requires a further, separate authorization.",
))
STATUS = ("Status", ("Planned, not shipped.",))
SPECS = (TAG, IDENTITY, WIRE, PARTIAL, EVIDENCE, PROOF, ROLLOUT, STATUS)

HISTORICAL_LABELS = ("Labels (later)", (
    "Label support is out of scope for v1.",
    "No label mutation is authorized, the `Labels/` namespace is refused as a "
    "destination, and COPY and EXPUNGE stay prohibited.",
))
HISTORICAL_SAFETY = ("Safety and reconciliation", (
    "COPY, EXPUNGE, DELETE, APPEND, CREATE, other flags and every fallback "
    "sequence are prohibited",
))


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
    parts = re.split(r"^## +(.+?)\s*$", visible(text), flags=re.M)
    found = {}
    for i in range(1, len(parts), 2):
        found.setdefault(parts[i], []).append(" ".join(parts[i + 1].split()))
    return found


def missing(text, spec):
    """Return the clauses of spec absent from its one visible section."""
    heading, clauses = spec
    bodies = sections(text).get(heading, [])
    if len(bodies) != 1:
        return [f"need one visible {heading!r} section"]

    return [clause for clause in clauses if " ".join(clause.split()) not in bodies[0]]


def clause_pattern(clause):
    return re.compile(r"\s+".join(map(re.escape, clause.split())))


def readme_errors(readme, exists):
    paragraphs = [" ".join(p.split()) for p in re.split(r"\n\s*\n", visible(readme))]
    linked = [p for p in paragraphs if re.search(r"\[[^\]]+\]\(" + re.escape(LINK) + r"\)", p)]
    if not linked:
        return ["README: label design link missing"]
    if not exists(LINK):
        return ["README: label design link target missing"]
    if not any("planned, not shipped" in p for p in linked):
        return ["README: label design link not marked planned, not shipped"]

    return []


def repository_file(path):
    return (REPO / path).is_file()


def registered_label_tools():
    found = []
    for path in sorted(REPO.rglob("*.go")):
        text = path.read_text(encoding="utf-8")
        if '"add_label"' in text or '"remove_label"' in text:
            found.append(str(path.relative_to(REPO)))
    return found


class LabelDesign(unittest.TestCase):
    def setUp(self):
        self.assertTrue(RECORD.is_file(), "label design record is absent")
        self.text = RECORD.read_text(encoding="utf-8")

    def assert_contract(self, spec, fragments=()):
        self.assertEqual(missing(self.text, spec), [])

        for clause in spec[1]:
            with self.subTest(omitted=clause):
                mutated, count = clause_pattern(clause).subn("", self.text)
                self.assertGreater(count, 0)
                self.assertIn(clause, missing(mutated, spec))
        for fragment in fragments:
            with self.subTest(fragment=fragment):
                mutated, count = clause_pattern(fragment).subn("", self.text)
                self.assertGreater(count, 0)
                self.assertNotEqual(missing(mutated, spec), [])

    def test_tag_preservation(self):
        self.assert_contract(TAG, fragments=(
            "source folder,", "source-folder UID", "Seen state,", "keeps the first label",
            "the message,", "its folder", "every other label", "never a folder MOVE"))

        historical = TRIAGE.read_text(encoding="utf-8")
        self.assertEqual(missing(historical, HISTORICAL_LABELS), [],
                         "historical Labels (later) guard changed")

    def test_exact_identity_and_approval(self):
        self.assert_contract(IDENTITY, fragments=(
            "the exact name of one existing label", "exact label mailbox under `Labels/`",
            "exactly one existing label", "1 through 50 unique positive", "never silently reused",
            "one exact payload", "exact resolved payload"))

    def test_scoped_wire_and_refusals(self):
        self.assert_contract(WIRE, fragments=(
            "single-UID `UID COPY`", "exact selectable label mailbox", "on the selected label view only",
            "`UIDPLUS`", "before its first write", "no library fallback to bare EXPUNGE",
            "Plain `EXPUNGE`,", "`CLOSE`,", "`MOVE` and `UID MOVE`,", "COPY to any folder,",
            "every other flag or flag change", "implicitly expunge other"))

        historical = TRIAGE.read_text(encoding="utf-8")
        self.assertEqual(missing(historical, HISTORICAL_SAFETY), [],
                         "shipped COPY/EXPUNGE prohibition changed")

    def test_partial_and_unknown(self):
        self.assert_contract(PARTIAL, fragments=(
            "makes that UID `unknown`", "the call stops,", "nothing is replayed",
            "the transport is aborted", "never a safe refusal", "not approval of permanent deletion",
            "fresh label-view identity", "new explicit approval"))

    def test_evidence_and_proof_gates(self):
        self.assert_contract(EVIDENCE, fragments=(
            "b9c5dac1651437100c40896dacd778a0518a26f2", "7e800978ab4a",
            "static source evidence", "not proof of the installed Bridge version"))
        self.assert_contract(PROOF, fragments=(
            "until every proof case below passes", "COPY, STORE or UID EXPUNGE",
            "rather than substituting MOVE or generic EXPUNGE"))

    def test_rollout_and_visibility(self):
        self.assert_contract(ROLLOUT, fragments=(
            "default false", "independent of `mutations.enabled`", "no `labels` member",
            "gives no instruction to set it", "separately approved change", "separate authorization"))
        self.assert_contract(STATUS)
        self.assertEqual(registered_label_tools(), [], "label tools must not be registered")

        for spec in SPECS:
            heading = spec[0]
            start = self.text.index(f"\n## {heading}\n")
            end = self.text.find("\n## ", start + 1)
            end = len(self.text) if end < 0 else end
            for name, hidden in (
                ("closed comment", self.text[:start] + "\n<!--" + self.text[start:end] + "\n-->" + self.text[end:]),
                ("unterminated comment", self.text[:start] + "\n<!--" + self.text[start:]),
            ):
                with self.subTest(heading=heading, hidden=name):
                    self.assertEqual(missing(hidden, spec), [f"need one visible {heading!r} section"])
            for clause in spec[1]:
                pattern = clause_pattern(clause)
                for name, wrap in (("comment", "<!-- {} -->"), ("fence", "\n```text\n{}\n```\n")):
                    with self.subTest(clause=clause, hidden=name):
                        hidden = pattern.sub(lambda m: wrap.format(m.group(0)), self.text)
                        self.assertIn(clause, missing(hidden, spec))

        readme = README.read_text(encoding="utf-8")
        self.assertEqual(readme_errors(readme, repository_file), [])
        paragraph = next(p for p in re.split(r"\n\s*\n", readme) if LINK in p)
        for name, text, exists, error in (
            ("link hidden", readme.replace(paragraph, "<!--\n" + paragraph + "\n-->"),
             repository_file, "README: label design link missing"),
            ("link unterminated comment", readme.replace(paragraph, "<!--\n" + paragraph),
             repository_file, "README: label design link missing"),
            ("link retargeted", readme.replace(LINK, "docs/design/MISSING.md"),
             repository_file, "README: label design link missing"),
            ("target missing", readme, lambda path: path != LINK,
             "README: label design link target missing"),
            ("not marked planned", readme.replace("planned, not shipped", "available"),
             repository_file, "README: label design link not marked planned, not shipped"),
        ):
            with self.subTest(readme=name):
                self.assertEqual(readme_errors(text, exists), [error])


if __name__ == "__main__":
    unittest.main(verbosity=2)

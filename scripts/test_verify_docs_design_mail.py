#!/usr/bin/env python3
"""Synthetic temporary-file tests for the Mail design verifier."""

import contextlib
import io
import pathlib
import tempfile
import unittest

import verify_docs_design_mail as verifier


RECORD = r"""# Synthetic Mail proposal

## Invariant

No mutation is authorized by this record.
The shipped Mail implementation remains read-only.

## Proposed operations

| Operation | Proposed behavior |
| --- | --- |
| flag | Set or clear the IMAP Flagged flag (`\Flagged`). |
| mark read | Set or clear the IMAP Seen flag (`\Seen`). |
| move | Move explicitly selected messages to an existing destination mailbox. |

Identity must be UID-based and bound to the source mailbox and UIDVALIDITY.
Check UIDVALIDITY against a fresh server value before any mutation.
Excluded operations: sending, drafts, permanent deletion, arbitrary flags and implicit expunge.
No fallback copy/delete/expunge sequence is prescribed.

## Fail-closed behavior

No valid approval: fail closed before any mutation.
Stale identity: fail closed before any mutation.
Invalid destination: fail closed before any mutation.
Unsupported operation: fail closed before any mutation.
Ambiguous completion: fail closed without reporting success.
An ambiguous mutation must not be blindly replayed.

## Proof requirements

Future synthetic tests must prove the proposed boundary before writes can be enabled.
These tests are not claimed to exist today.
Use synthetic mail.test fixtures.

## Reserved

Approval authority remains undecided: who may authorize a mutation.
Approval lifetime remains undecided: per-call versus session approval.
User-visible confirmation surface remains undecided: the future UI.
The approval model is Reserved.

## Status

Proposed. Mail remains read-only.
No Mail mutation tools are registered by this record.
"""
MCP = "Mail remains read-only. See the [proposed Mail mutation boundary](design/0002-mail-mutation.md).\n"


class MailDesignTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        root = pathlib.Path(self.directory.name)
        self.doc = root / "0002-mail-mutation.md"
        self.mcp = root / "MCP.md"

    def check(self, record=RECORD, mcp=MCP):
        self.doc.write_text(record, encoding="utf-8")
        self.mcp.write_text(mcp, encoding="utf-8")
        return verifier.verify(self.doc, self.mcp)

    def reject(self, record, diagnostic, mcp=MCP):
        failures = self.check(record, mcp)
        self.assertTrue(any(diagnostic in failure for failure in failures), failures)

    def remove_statement(self, section, name):
        statement = verifier.REQUIREMENTS[section][name]
        self.assertIn(statement, RECORD)
        self.reject(RECORD.replace(statement, ""), f"{section}: missing {name}")

    def test_valid_synthetic_record(self):
        self.assertEqual(self.check(), [])

    def test_missing_heading(self):
        for heading in verifier.HEADINGS:
            with self.subTest(heading=heading):
                self.reject(RECORD.replace(f"## {heading}", ""), "headings:")

    def test_heading_order_and_extra_heading(self):
        self.reject(RECORD.replace("## Invariant", "## Extra\n\n## Invariant"), "headings:")
        changed = RECORD.replace("## Invariant", "## TEMP").replace("## Reserved", "## Invariant").replace("## TEMP", "## Reserved")
        self.reject(changed, "headings:")

    def test_missing_operation_rows(self):
        for operation in ("flag", "mark read", "move"):
            with self.subTest(operation=operation):
                changed = "\n".join(line for line in RECORD.splitlines() if not line.startswith(f"| {operation} |"))
                self.reject(changed, f"invalid {operation} row")

    def test_extra_duplicate_and_invalid_operation_rows(self):
        self.reject(RECORD.replace("| flag |", "| send |"), "operation rows must be exactly")
        row = "| flag | Set or clear the IMAP Flagged flag. |\n"
        self.reject(RECORD.replace("| mark read |", row + "| mark read |"), "operation rows must be exactly")
        for old, new, name in (("Flagged flag", "Deleted flag", "flag"), ("Seen flag", "Answered flag", "mark read"), ("existing destination", "new destination", "move")):
            with self.subTest(operation=name):
                self.reject(RECORD.replace(old, new), f"invalid {name} row")

    def test_operation_table_requires_delimiter(self):
        self.reject(RECORD.replace("| --- | --- |", ""), "operation rows must be exactly")

    def test_missing_no_valid_approval(self):
        self.remove_statement("Fail-closed behavior", "no valid approval")

    def test_missing_stale_identity(self):
        self.remove_statement("Fail-closed behavior", "stale identity")

    def test_missing_invalid_destination(self):
        self.remove_statement("Fail-closed behavior", "invalid destination")

    def test_missing_unsupported_operation(self):
        self.remove_statement("Fail-closed behavior", "unsupported operation")

    def test_missing_ambiguous_completion(self):
        self.remove_statement("Fail-closed behavior", "ambiguous completion")

    def test_missing_no_blind_replay(self):
        self.remove_statement("Fail-closed behavior", "no blind replay")

    def test_missing_reservations(self):
        for name in verifier.REQUIREMENTS["Reserved"]:
            with self.subTest(reservation=name):
                self.remove_statement("Reserved", name)

    def test_chosen_approval_model(self):
        self.reject(RECORD.replace("remains undecided", "is selected"), "Reserved: missing")

    def test_shipped_status_substitutions(self):
        for old, new in (("Proposed.", "Shipped."), ("Mail remains read-only.", "Mail supports writes."), ("No Mail mutation tools are registered", "Mail mutation tools are registered")):
            with self.subTest(substitution=new):
                self.reject(RECORD.replace(old, new), "Status: missing")

    def test_missing_invariants_identity_and_future_proof(self):
        for section in ("Invariant", "Proposed operations", "Proof requirements"):
            for name in verifier.REQUIREMENTS[section]:
                with self.subTest(section=section, requirement=name):
                    self.remove_statement(section, name)

    def test_requirements_are_section_local(self):
        for section, requirements in verifier.REQUIREMENTS.items():
            for name, statement in requirements.items():
                with self.subTest(section=section, requirement=name):
                    moved = statement + "\n\n" + RECORD.replace(statement, "")
                    self.reject(moved, f"{section}: missing {name}")

    def test_missing_proposal_link(self):
        self.reject(RECORD, "missing proposed Mail design link", "Mail remains read-only.")

    def test_link_must_be_labeled_proposed(self):
        self.reject(RECORD, "missing proposed Mail design link", MCP.replace("proposed", "shipped"))

    def test_requirements_only_in_fenced_examples_fail(self):
        for marker in ("```", "~~~~"):
            with self.subTest(marker=marker):
                self.reject(f"{marker}markdown\n{RECORD}\n{marker}\n", "headings:")
                for section, requirements in verifier.REQUIREMENTS.items():
                    for name, statement in requirements.items():
                        hidden = RECORD.replace(statement, f"{marker}\n{statement}\n{marker}")
                        self.reject(hidden, f"{section}: missing {name}")
                self.reject(RECORD, "missing proposed Mail design link", f"{marker}\n{MCP}{marker}")
                table = RECORD[RECORD.index("| Operation |"):RECORD.index("\n\nIdentity")]
                self.reject(RECORD.replace(table, f"{marker}\n{table}\n{marker}"), "operation rows must be exactly")

    def test_requirements_only_in_html_comments_fail(self):
        self.reject(f"<!--\n{RECORD}\n-->", "headings:")
        for section, requirements in verifier.REQUIREMENTS.items():
            for name, statement in requirements.items():
                with self.subTest(section=section, requirement=name):
                    self.reject(RECORD.replace(statement, f"<!-- {statement} -->"), f"{section}: missing {name}")
        for operation in ("flag", "mark read", "move"):
            row = next(line for line in RECORD.splitlines() if line.startswith(f"| {operation} |"))
            self.reject(RECORD.replace(row, f"<!--\n{row}\n-->"), f"invalid {operation} row")
        self.reject(RECORD, "missing proposed Mail design link", f"<!-- {MCP} -->")

    def test_missing_file_and_cli_diagnostics(self):
        self.check()
        self.doc.unlink()
        stderr = io.StringIO()
        with contextlib.redirect_stderr(stderr):
            self.assertEqual(verifier.main(self.doc, self.mcp), 1)
        self.assertIn("FAIL:", stderr.getvalue())
        self.assertIn("cannot read documentation", stderr.getvalue())

        self.check(RECORD.replace("Proposed.", "Shipped."))
        with contextlib.redirect_stderr(stderr):
            self.assertEqual(verifier.main(self.doc, self.mcp), 1)
        self.assertIn("Status: missing proposed read-only status", stderr.getvalue())

    def test_cli_success(self):
        self.check()
        stdout = io.StringIO()
        with contextlib.redirect_stdout(stdout):
            self.assertEqual(verifier.main(self.doc, self.mcp), 0)
        self.assertIn("proposal OK", stdout.getvalue())


if __name__ == "__main__":
    unittest.main()

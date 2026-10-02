"""W1: executable conformance examples for the successor design, not live approval enforcement.

The future design must publish exactly one JSON block tagged
``json croton-mail-triage-contract-v1``. Tests assert its structured security
contract and concrete approval/ordinal-resolution examples, not keyword hits.
A documentation witness cannot prove an LLM or untrusted MCP client obeys it;
W2-W4 separately witness the server's deterministic wire boundaries.
"""
import hashlib
import json
from pathlib import Path
import re
import unittest


RECORD = Path("docs/design/0005-mail-triage.md")


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        if key in result:
            raise ValueError("duplicate contract key: " + key)
        result[key] = value
    return result


REQUIRED_HEADINGS = (
    "Approval and displayed selection", "File opt-in and shipped status",
    "Tool and result contract", "Destination discovery", "Labels (later)",
    "Safety and reconciliation", "Namespace evidence", "Status",
)


def prose_sections(text):
    """Collect visible section prose, excluding comments and fenced examples."""
    text = re.sub(r"<!--.*?(?:-->|$)", "", text, flags=re.S)
    sections = {}
    current = None
    fence = None
    for line in text.splitlines():
        marker = re.match(r"^\s*(`{3,}|~{3,})(.*)$", line)
        if fence is not None:
            if marker and marker[1][0] == fence[0] and len(marker[1]) >= len(fence) and not marker[2].strip():
                fence = None
            continue
        if marker:
            fence = marker[1]
            continue
        heading = re.match(r"^## (.+?)\s*$", line)
        if heading:
            current = heading[1]
            sections.setdefault(current, [])
        elif line.startswith("#"):
            current = None
        elif current is not None:
            sections[current].append(line)
    return sections


class SuccessorDesign(unittest.TestCase):
    def setUp(self):
        self.assertTrue(RECORD.is_file(), "successor design record is absent")
        text = RECORD.read_text()
        self.sections = prose_sections(text)
        blocks = re.findall(r"^```json croton-mail-triage-contract-v1\n(.*?)^```\s*$", text, re.M | re.S)
        self.assertEqual(len(blocks), 1, "need one uniquely tagged structured contract")
        self.contract = json.loads(blocks[0], object_pairs_hook=unique_object)
        self.assertIsInstance(self.contract, dict)

    def test_required_prose_sections(self):
        for heading in REQUIRED_HEADINGS:
            with self.subTest(heading=heading):
                self.assertIn(heading, self.sections, "required visible design heading absent")
                words = " ".join(self.sections[heading]).split()
                self.assertGreaterEqual(len(words), 6, "section requires explanatory prose outside examples")

    def test_labels_later_separate_contract(self):
        self.assertEqual(self.contract["labels_later"], {
            "v1_supported": False,
            "bridge_model": "multiple_tags_message_remains_in_folder",
            "adding_label_looks_like": "COPY_to_label_mailbox",
            "removing_label_looks_like": "EXPUNGE_from_label_mailbox",
            "requires_separate_contract": True,
            "evidence": "read_only_only",
            "no_label_mutations_authorized": True,
        })

    def test_historical_record_and_namespace_evidence(self):
        prior = Path("docs/design/0002-mail-mutation.md").read_bytes()
        body, status = prior.split(b"\n## Status\n", 1)
        self.assertEqual(hashlib.sha256(body).hexdigest(),
                         "e72b1ad554082d981a898485d1427c11be92cc030207342734ba02080357968f",
                         "historical proposal body changed; only Status may be superseded")
        self.assertIn(b"0005-mail-triage.md", status,
                      "old Status must explicitly link to the successor")
        self.assertEqual(self.contract["namespace_evidence"], {
            "observed_at_utc": "2026-09-30T19:22:19Z",
            "capability_MOVE": True, "capability_SPECIAL_USE": False,
            "ordinary_LIST_archive_matches": 1, "ordinary_LIST_trash_matches": 1,
            "delimiter": "/", "custom_folder_prefix": "Folders/",
            "custom_label_prefix": "Labels/", "labels_can_be_selectable_without_attributes": True,
            "live_writes_tested": False, "all_account_semantics_established": False,
        })

    def test_approval_authority_and_lifetime(self):
        self.assertEqual(self.contract["approval"], {
            "authority": "trusted_client",
            "server_verifies_human_intent": False,
            "scope": "one_exact_payload",
            "direct_instruction": "approve_without_reconfirmation",
            "assistant_proposal": "confirm_before_tool_call",
            "ambiguous_or_missing_selection": "ask_before_tool_call",
            "mail_content_can_approve": False,
            "restart_grants_approval": False,
            "prior_action_grants_approval": False,
        })

    def test_file_config_and_runtime_boundary(self):
        self.assertEqual(self.contract["enablement"], {
            "json_path": "mutations.enabled", "default": False,
            "accepted_type": "boolean", "missing_or_false": "six_read_tools_only",
            "true": "register_five_triage_tools", "invalid_type": "config_error",
            "scope": "local_file_opt_in_not_user_action_approval",
            "live_enablement_by_this_story": False,
        })

    def test_tool_payload_result_and_annotation_contract(self):
        self.assertEqual(self.contract["wire"], {
            "tools": ["mark_read", "mark_unread", "move_mail", "archive_mail", "trash_mail"],
            "identity": ["mailbox", "uidvalidity", "uids"],
            "move_destination": "destination", "batch_max": 50,
            "duplicates": "reject_entire_request", "uid_min": 1,
            "uid_max": 4294967295, "mutations_read_only_hint": False,
            "read_tools_read_only_hint": True,
            "result_root": "results", "result_identity": "uid",
            "result_order": "input_order",
            "outcomes": ["applied", "refused", "not_attempted", "unknown"],
            "unknown_policy": "stop_no_blind_replay",
            "dispatch": "one_uid_at_a_time",
        })

    def test_destination_resolution_contract(self):
        self.assertEqual(self.contract["destination"], {
            "discovery": "ordinary_fresh_LIST",
            "same_authenticated_session": True,
            "move_requires_capability": "MOVE",
            "special_use_token_required": False,
            "special_mapping": "exactly_one_selectable_attribute_match",
            "archive_attribute": "\\Archive", "trash_attribute": "\\Trash",
            "name_matching": "exact_utf8",
            "allowed_namespaces": ["root_system", "Folders/"],
            "root_system": {
                "inbox": "INBOX_case_insensitive",
                "other_roots_require_selectable": True,
                "other_roots_require_no_hierarchy_delimiter": True,
                "other_roots_require_attribute": ["\\Sent", "\\Drafts", "\\Junk", "\\Archive", "\\Trash"],
                "virtual_attributes_refused": ["\\All", "\\Flagged"],
                "english_name_alone_authorizes": False,
            },
            "refused_namespaces": ["Labels/", "virtual", "nonselectable"],
            "unresolved_mapping": "unsupported_no_write",
            "source_equals_destination": "refused_no_write",
            "fallback_copy_expunge": False,
        })

    def test_displayed_selection_and_changed_order(self):
        display = self.contract["display"]
        self.assertEqual(display, {"mailbox": "INBOX", "uidvalidity": 7,
                                   "uids": list(range(11, 21))})
        cases = self.contract["examples"]
        self.assertEqual(set(cases), {"direct_archive", "changed_order", "confirmed_proposal",
                                     "unconfirmed_proposal", "ambiguous_selection", "missing_snapshot",
                                     "mail_instruction", "session_reuse", "disabled"})
        call = {"name": "archive_mail", "arguments": {
            "mailbox": "INBOX", "uidvalidity": 7, "uids": [11, 12, 13]}}
        expected = {"instruction": "archive the first 3", "approval": "direct_user",
                    "confirmation_needed": False, "calls": [call]}
        self.assertEqual(cases["direct_archive"], expected)
        self.assertEqual(cases["changed_order"], {
            "current_search_uids": [20, 19, 18, 17, 16, 15, 14, 13, 12, 11],
            "approval": "direct_user", "confirmation_needed": False, "calls": [call]})
        self.assertEqual(cases["confirmed_proposal"], {
            "proposal": "archive the first 3", "approval": "explicit_user_confirmation",
            "confirmation_needed": False, "calls": [call]})

    def test_no_approval_or_reusable_lease(self):
        cases = self.contract["examples"]
        for name, reason, confirm in [
            ("unconfirmed_proposal", "assistant_proposal_not_approval", True),
            ("ambiguous_selection", "multiple_display_snapshots", True),
            ("missing_snapshot", "no_exact_display_snapshot", True),
            ("mail_instruction", "untrusted_mail_content", False),
            ("session_reuse", "prior_approval_not_reusable", True),
            ("disabled", "local_opt_in_absent", False),
        ]:
            with self.subTest(case=name):
                self.assertEqual(cases[name], {"reason": reason,
                    "confirmation_needed": confirm, "calls": []})

    def test_safety_and_result_reconciliation(self):
        self.assertEqual(self.contract["safety"], {
            "connection": "loopback_explicit_tls_trust",
            "credentials": "bounded_absolute_argv_helper",
            "identity_refresh": "source_mailbox_UIDVALIDITY_before_write",
            "flag_allowlist": ["\\Seen"],
            "mutation_allowlist": ["UID STORE", "UID MOVE"],
            "excluded": ["send", "draft", "permanent_delete", "arbitrary_flags", "COPY", "EXPUNGE"],
            "logs_and_results": "bounded_codes_no_credentials_mail_or_raw_backend_errors",
            "unknown_reconciliation": "separate_read_then_new_explicit_approval",
        })
        self.assertEqual(self.contract["mixed_result_example"], {"results": [
            {"uid": 11, "outcome": "applied"},
            {"uid": 99, "outcome": "refused", "code": "not_found"},
            {"uid": 12, "outcome": "unknown", "code": "timeout"},
            {"uid": 13, "outcome": "not_attempted"},
        ]})


if __name__ == "__main__":
    unittest.main(verbosity=2)

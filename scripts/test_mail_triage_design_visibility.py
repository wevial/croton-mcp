"""Regression tests: W1 reads its structured contract from visible text only.

Each case points the W1 module's record path at a temporary copy of the real
successor design record and runs the unmodified W1 suite against it.
"""
import importlib.util
import io
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch


REPO = Path(__file__).resolve().parent.parent
RECORD = REPO / "docs/design/0005-mail-triage.md"
FENCE = "```json croton-mail-triage-contract-v1\n"
MESSAGE = "need one uniquely tagged structured contract"


def load_w1():
    spec = importlib.util.spec_from_file_location(
        "w1_mail_triage_design", REPO / "scripts/test_mail_triage_design.py")
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


def contract_span(text):
    start = text.index(FENCE)
    end = text.index("\n```\n", start + len(FENCE)) + len("\n```\n")
    return start, end


class VisibleContract(unittest.TestCase):
    def run_w1(self, text):
        w1 = load_w1()

        with tempfile.TemporaryDirectory() as tmp:
            copy = Path(tmp) / "0005-mail-triage.md"
            copy.write_text(text)
            suite = unittest.defaultTestLoader.loadTestsFromTestCase(w1.SuccessorDesign)
            with patch.object(w1, "RECORD", copy):
                result = unittest.TextTestRunner(stream=io.StringIO(), verbosity=0).run(suite)

        return result

    def assert_contract_not_found(self, text):
        result = self.run_w1(text)

        self.assertEqual(result.testsRun, 10)
        self.assertEqual(result.errors, [])
        self.assertEqual(len(result.failures), 10, "every W1 test must fail in setUp")
        for _, trace in result.failures:
            self.assertIn(MESSAGE, trace)

    def test_contract_in_closed_comment_is_not_found(self):
        text = RECORD.read_text()
        self.assertEqual(text.count(FENCE), 1)
        start, end = contract_span(text)

        hidden = text[:start] + "<!--\n" + text[start:end] + "-->\n" + text[end:]

        self.assert_contract_not_found(hidden)

    def test_contract_after_unterminated_comment_is_not_found(self):
        text = RECORD.read_text()
        self.assertEqual(text.count(FENCE), 1)
        start, _ = contract_span(text)

        hidden = text[:start] + "<!--\n" + text[start:]

        self.assert_contract_not_found(hidden)

    def test_unmodified_record_passes_all_w1_tests(self):
        result = self.run_w1(RECORD.read_text())

        self.assertEqual(result.testsRun, 10)
        self.assertTrue(result.wasSuccessful(), result.failures + result.errors)


if __name__ == "__main__":
    unittest.main(verbosity=2)

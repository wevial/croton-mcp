#!/usr/bin/env python3
"""Synthetic subprocess witnesses only; no real build, config or credentials."""

import hashlib
import io
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

import stage_mail_candidate as helper


REVISION = "a" * 40
BINARY = b"synthetic Mail binary.test\x00\xff"


class CandidateTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.parent = Path(self.temp.name)
        self.root = self.parent / "checkout"
        self.root.mkdir()
        (self.root / "untracked.test").write_text("not exported")
        (self.root / "ignored.test").write_text("not exported")
        self.output = self.parent / "candidate"
        self.head = REVISION
        self.dirty = ""
        self.info = dict(GOVERSION="go1.26.6", GOOS="linux", GOARCH="amd64",
                         GOHOSTOS="linux", GOHOSTARCH="amd64")
        self.calls = []
        self.fail_build = False
        self.race_output = False
        self.addCleanup(patch.stopall)
        patch.object(helper.sys, "platform", "linux").start()
        patch.object(helper.subprocess, "run", side_effect=self.stub).start()

    def stub(self, argv, **kwargs):
        self.calls.append(argv)
        self.assertEqual(kwargs["stdin"], subprocess.DEVNULL)
        self.assertEqual(kwargs["stderr"], subprocess.PIPE)
        self.assertFalse(kwargs["check"])
        self.assertEqual(kwargs["env"]["GOWORK"], "off")
        self.assertEqual(kwargs["env"]["GOFLAGS"], "")
        self.assertEqual(kwargs["env"]["GOTOOLCHAIN"], "go1.26.6")
        result = ""
        code = 0
        if argv == ["git", "rev-parse", "--show-toplevel"]:
            result = str(self.root)
        elif argv == ["git", "rev-parse", "HEAD"]:
            result = self.head
        elif argv == ["git", "status", "--porcelain", "--untracked-files=no"]:
            result = self.dirty
        elif argv == ["git", "archive", "--format=tar", REVISION]:
            with helper.tarfile.open(fileobj=kwargs["stdout"], mode="w") as archive:
                content = b"synthetic tracked source.test"
                entry = helper.tarfile.TarInfo("tracked.test")
                entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
        elif argv == ["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH", "GOHOSTOS", "GOHOSTARCH"]:
            result = json.dumps(self.info)
        elif argv[:4] == ["go", "build", "-trimpath", "-o"]:
            self.assertEqual(argv[5:], ["./cmd/croton-mcp"])
            source = kwargs["cwd"]
            self.assertNotEqual(source, self.root)
            self.assertEqual([p.name for p in source.iterdir()], ["tracked.test"])
            Path(argv[4]).write_bytes(BINARY)
            code = 1 if self.fail_build else 0
            if self.race_output:
                self.output.mkdir()
        else:
            self.fail(f"unexpected subprocess: {argv}")

        return subprocess.CompletedProcess(argv, code, result.encode(), b"synthetic diagnostic.test")

    def run_helper(self, revision=REVISION):
        with patch("sys.stdout", new_callable=io.StringIO), patch("sys.stderr", new_callable=io.StringIO):
            return helper.main(["--revision=" + revision, "--output", str(self.output)])

    def assert_no_build(self):
        self.assertFalse(any(c[:2] == ["go", "build"] for c in self.calls))

    def assert_cleaned(self):
        self.assertEqual(list(self.parent.glob(".mail-candidate-*")), [])

    def test_success_manifest_hash_and_tracked_export(self):
        self.assertEqual(self.run_helper(), 0)
        self.assertEqual({p.name for p in self.output.iterdir()}, {"croton-mcp", "manifest.json"})
        binary = (self.output / "croton-mcp").read_bytes()
        self.assertEqual(binary, BINARY)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual(manifest, dict(revision=REVISION, toolchain="go1.26.6", GOOS="linux",
                                       GOARCH="amd64", binary="croton-mcp",
                                       sha256=hashlib.sha256(binary).hexdigest()))
        self.assert_cleaned()

    def test_native_macos(self):
        self.info.update(GOOS="darwin", GOHOSTOS="darwin", GOARCH="arm64", GOHOSTARCH="arm64")
        with patch.object(helper.sys, "platform", "darwin"):
            self.assertEqual(self.run_helper(), 0)
        manifest = json.loads((self.output / "manifest.json").read_text())
        self.assertEqual((manifest["GOOS"], manifest["GOARCH"]), ("darwin", "arm64"))

    def test_mismatched_revision(self):
        self.assertEqual(self.run_helper("b" * 40), 1)
        self.assert_no_build()
        self.assertFalse(self.output.exists())

    def test_full_revision_required(self):
        for revision in ("HEAD", "a" * 7, "--help", ""):
            with self.subTest(revision=revision):
                self.assertEqual(self.run_helper(revision), 1)
        self.assert_no_build()

    def test_dirty_tracked_files(self):
        for status in (" M tracked.test", "M  tracked.test", "A  staged.test", " D tracked.test"):
            with self.subTest(status=status):
                self.dirty = status
                self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()
        self.assertFalse(self.output.exists())

    def test_unsupported_host(self):
        with patch.object(helper.sys, "platform", "win32"):
            self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_unsupported_target_platform(self):
        self.info["GOOS"] = "windows"
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()
        self.assert_cleaned()

    def test_cross_architecture_rejected(self):
        self.info["GOARCH"] = "arm64"
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_wrong_toolchain(self):
        self.info["GOVERSION"] = "go1.26.0"
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_existing_output_untouched(self):
        self.output.mkdir()
        marker = self.output / "keep.test"
        marker.write_bytes(b"unchanged")
        self.assertEqual(self.run_helper(), 1)
        self.assertEqual(marker.read_bytes(), b"unchanged")
        self.assert_no_build()

    def test_existing_empty_output_untouched(self):
        self.output.mkdir()
        self.assertEqual(self.run_helper(), 1)
        self.assertEqual(list(self.output.iterdir()), [])
        self.assert_no_build()

    def test_dangling_output_symlink(self):
        self.output.symlink_to(self.parent / "absent.test")
        self.assertEqual(self.run_helper(), 1)
        self.assertTrue(self.output.is_symlink())
        self.assert_no_build()

    def test_output_inside_checkout(self):
        self.output = self.root / "candidate"
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_output_through_checkout_alias(self):
        alias = self.parent / "alias"
        alias.symlink_to(self.root, target_is_directory=True)
        self.output = alias / "candidate"
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_relative_output(self):
        self.output = Path("candidate.test")
        self.assertEqual(self.run_helper(), 1)
        self.assert_no_build()

    def test_failed_build_cleanup(self):
        self.fail_build = True
        self.assertEqual(self.run_helper(), 1)
        self.assertTrue(any(c[:2] == ["go", "build"] for c in self.calls))
        self.assertFalse(self.output.exists())
        self.assert_cleaned()

    def test_output_created_during_build_is_not_replaced(self):
        self.race_output = True
        self.assertEqual(self.run_helper(), 1)
        self.assertEqual(list(self.output.iterdir()), [])
        self.assert_cleaned()

    def test_guide_contract(self):
        guide = (Path(__file__).resolve().parents[1] / "docs/USER-INSTALL.md").read_text()
        for statement in ("python3 scripts/stage_mail_candidate.py", '--revision "$REVIEWED_REVISION"',
                          '--output "$MAIL_CANDIDATE_DIR"', "Nothing is published",
                          "Checksums provide integrity", "not signatures", "provenance attestation",
                          "not hermetic", "manual installation", "manifest.json", "Python 3.12"):
            with self.subTest(statement=statement):
                self.assertIn(statement, guide)


if __name__ == "__main__":
    suite = unittest.defaultTestLoader.loadTestsFromTestCase(CandidateTests)
    count = suite.countTestCases()
    print(f"Mail candidate synthetic tests: {count} named cases", flush=True)
    if not count:
        raise SystemExit("FAIL: zero cases")

    result = unittest.TextTestRunner(verbosity=2).run(suite)
    raise SystemExit(0 if result.wasSuccessful() else 1)

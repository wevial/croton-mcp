#!/usr/bin/env python3
"""Synthetic temporary-file regression tests for the download boundary verifier."""

import contextlib
import io
import pathlib
import re
import tempfile
import unittest
from unittest import mock

import verify_docs_design_download_boundary as verifier
import verify_docs_drive_mcp as drive_verifier
import verify_docs_threat_model as threat_verifier


RECORD = """# Resolution-time Drive download boundary

## Invariant

A Drive download requires resolution-time confinement beneath an explicitly allowed root. Every destination component must be resolved without following symlinks. Output I/O must use the descriptors obtained at resolution, without pathname re-resolution, through writing and publication. An empty allowlist permits no downloads. Uncertain resolution-time confinement means refusal.

## Mechanisms

Linux must pin the allowed root through no-follow directory traversal and use constrained `openat2` with `RESOLVE_BENEATH`, `RESOLVE_NO_SYMLINKS` and `O_NOFOLLOW` for relative destination resolution. Retain the resulting descriptors for output I/O.

macOS must use component-wise, descriptor-relative `openat` with `O_NOFOLLOW` on every open and `O_DIRECTORY` on directory opens. Each component must be opened relative to the pinned preceding directory descriptor. Reject absolute destination paths and `..` components before traversal.

Unavailable required primitives, unsupported syscalls or flags, and unresolved resolution races must cause refusal before any output mutation or writer invocation. Never retry with weaker flags or fall back to canonicalize-and-open.

## Decision

The maintainer accepts resolution-time confinement and descriptor-bound output for a single-operator host. This decision supersedes KO-449's unconditional pre-write refusal gate and relocation-backend prerequisite. Only the lifetime relocation guarantee is superseded; no-follow resolution, descriptor-bound output and unavailable-primitive refusal remain required.

Relocation of the allowed root or its ancestors after opening is outside the threat model and is the operator's responsibility. Croton does not promise to prevent directory relocation. Descriptors do not prevent rename syscalls or freeze the filesystem namespace. A descriptor continues to identify its opened object even if that object is renamed; this is not a lifetime pathname-location guarantee.

A validated destination pathname must never be reopened, including through the CLI download command. Reject check-then-open via `Lstat`, `EvalSymlinks` or an allowlist-prefix check followed by a separate open. Post-write checks cannot undo an escaped write. The historical requirements remain recorded in [KO-449](0001-download-confinement.md).

Operators must configure client-managed per-call confirmation before enabling downloads.

Croton accepts tools/call from the configured client and cannot verify that a human confirmed.

Croton never prompts for confirmation itself.

The annotations readOnlyHint false and destructiveHint false do not force a client prompt or prove confirmation.

An auto-approving client permits unattended local writes inside the allowed root.

The human-facing request must identify the Drive source path, destination under the allowed root and size when metadata supplies it.

The human-facing request must never include credentials or share passwords.

There is no approved tool argument or other agent-supplied consent assertion.

There is no consent audit field.

The download.enabled setting defaults to false.

While disabled, no download tool is registered.

Explicit operator opt-in accepts the configured-client trust boundary.

Until the registration implementation ships, download.enabled true must fail startup rather than enable a partial capability.

The current config schema accepts download.enabled, download.maxBytes and download.timeoutSeconds, but rejects enabled downloads with a static ErrConfigInvalid before CLI startup, regardless of allowed roots.

Omitted limits default to 256 MiB and 120 seconds.

Explicit limits must be positive signed-64-bit integers; timeout conversion to time.Duration must not overflow.

Invalid limits are rejected, never clamped.

Downloads must refuse an existing destination.

There is no overwrite flag.

The configurable byte cap defaults to 256 MiB.

The configurable time cap defaults to 120 seconds.

Known limits must be checked before the first write.

A limit reached mid-download must stop the download and delete partial output.

A future local audit line per download must identify source, destination and outcome.

That audit line must never claim consent or include credentials or share passwords.

Source and destination paths can reveal sensitive names and activity; operators must protect access to and retention of these future local logs.

These controls require future runtime implementation and are not supplied by WriteFresh.

## Proof test

Future download integration proofs must use deterministic Go tests on Linux and macOS with synthetic temporary allowed and outside directories. These tests are required future work, not tests already shipped. Use explicit synchronization barriers at resolution and write boundaries, not timing sleeps.

- Intermediate symlink: refuse traversal of a component pointing to the outside directory, including substitution after destination selection before opening.

- Final symlink: refuse a final-component symlink pointing to an outside sentinel, including substitution before opening.

- Sub-root rename: pause after resolution, rename an opened subdirectory, and replace its old pathname with a different directory or a symlink to an outside directory. Refuse to follow the replacement pathname or substituted symlink; any continued output must use the original descriptors. This case does not require preventing the rename syscall or freezing the namespace.

- Outside sentinel checks: assert outside sentinels remain unchanged and no output, including partial files, appears through replacement paths or symlinks. Movement of an already-open object is not evidence of following its replacement pathname; do not assert lifetime location protection for that object.

- Unavailable primitives: refuse before output mutation or writer invocation when required primitives are unavailable.

- Ordinary in-root success: complete output through the resolved descriptors when required primitives are available. Rejecting every request cannot satisfy this proof.

The documentation verifier witnesses these named requirements structurally; passing it is not a runtime security proof.

## Reserved

Only per-session approval remains Reserved.

## Status

Accepted as a maintainer-approved design and policy record.

The internal confined opener WriteFresh has shipped without a production call site.

Download tool registration and policy enforcement have not shipped.

Registration remains held until this record ships and a later integration proves the required controls.

Before registration, integration must prove a supported descriptor-bound writer and temporary-publication path, partial-output cleanup and all required runtime controls without reopening the validated destination pathname.

WriteFresh alone does not provide partial-output cleanup or publication.

The existing CLI Download method remains disabled by the invocation allowlist.

The server remains read-only."""
OLD = """## Status

Superseded-in-part by the maintainer-approved [resolution-time Drive download boundary](0003-download-boundary.md). The successor replaces the lifetime relocation guarantee, unconditional pre-write refusal gate and relocation-backend prerequisite with resolution-time confinement and descriptor-bound output. The body above preserves the historical KO-449 proposal; its superseded requirements are not the current boundary. No download tool or opener ships with either record. Approval, overwrite, size and time cap policies remain Reserved and block tool registration.
"""
DRIVE = """## Running

`allowedDownloadDirectories` and `writes.enabled` are reserved: the server registers no download or write tools. Under the accepted design, relocation of the allowed root or its ancestors after opening is the operator's responsibility, outside Croton's threat model. See the [resolution-time download boundary](design/0003-download-boundary.md) for the accepted confinement and download policies; no download tool ships. The file carries no credentials and the server never reads any; authentication is the CLI's own concern, and a CLI that reports it needs authentication surfaces as `unavailable`.
"""


class DownloadBoundaryTests(unittest.TestCase):
    def setUp(self):
        directory = tempfile.TemporaryDirectory()
        self.addCleanup(directory.cleanup)
        root = pathlib.Path(directory.name)
        self.paths = [root / name for name in ("boundary.md", "old.md", "drive.md")]

    def check(self, texts):
        for path, text in zip(self.paths, texts):
            path.write_text(text, encoding="utf-8")

        return verifier.verify(*self.paths)

    def contracts(self):
        for section, requirements in verifier.REQUIREMENTS.items():
            yield 0, section, requirements

        yield 1, "Status", verifier.OLD_REQUIREMENTS
        yield 2, "allowedDownloadDirectories paragraph", verifier.DRIVE_REQUIREMENTS

    def test_valid_fixtures_and_normalization(self):
        self.assertEqual(self.check([RECORD, OLD, DRIVE]), [])
        normalized = [
            "\n".join(line if line.startswith("#") else line.replace(" ", "  ") for line in text.splitlines())
            for text in (RECORD, OLD, DRIVE)
        ]
        self.assertEqual(self.check(normalized), [])

    def test_appended_contradictions(self):
        for index, section, requirements in self.contracts():
            for claim in (
                "Downloads are now enabled.",
                "Validated paths may be reopened.",
                "The approval model is automatic approval.",
            ):
                with self.subTest(section=section, claim=claim):
                    texts = [RECORD, OLD, DRIVE]
                    statement = next(iter(requirements.values()))
                    # Fixtures retain inline code; normalization removes only backticks.
                    texts[index] = texts[index].replace("`", "")
                    texts[index] = texts[index].replace(statement.replace("`", ""), statement.replace("`", "") + " " + claim)
                    failures = self.check(texts)
                    self.assertTrue(any(f"{section}: unexpected contract statement" in f for f in failures), failures)

    def test_requirements_reject_negation_qualification_hiding_and_movement(self):
        for index, section, requirements in self.contracts():
            for name, statement in requirements.items():
                statement = statement.replace("`", "")
                for change in (
                    lambda s: "Not " + s,
                    lambda s: s[:-1] + "; except when downloads are enabled.",
                    lambda s: "<!-- " + s + " -->",
                    lambda s: "\n\n```\n" + s + "\n```\n\n",
                    lambda s: "\n\n~~~~\n" + s + "\n~~~~\n\n",
                    lambda s: "",
                ):
                    with self.subTest(section=section, name=name, change=change):
                        texts = [RECORD, OLD, DRIVE]
                        texts[index] = texts[index].replace("`", "")
                        self.assertIn(statement, texts[index])
                        texts[index] = texts[index].replace(statement, change(statement))
                        # Even a visible copy outside its section cannot satisfy it.
                        texts[index] = statement + "\n\n" + texts[index]
                        failures = self.check(texts)
                        self.assertTrue(any(f"{section}: missing {name}" in f for f in failures), failures)

    def test_hidden_contradictions_are_ignored(self):
        for index, section, requirements in self.contracts():
            for hidden in (
                "<!-- Downloads are now enabled. -->",
                "\n\n```\nDownloads are now enabled.\n```\n\n",
                "\n\n~~~~\nDownloads are now enabled.\n~~~~\n\n",
            ):
                with self.subTest(section=section, hidden=hidden):
                    texts = [RECORD, OLD, DRIVE]
                    if index == 2:
                        # Fenced examples are separate paragraphs in Running.
                        texts[index] += hidden + "\n"
                    else:
                        texts[index] = texts[index].replace(f"## {section}\n", f"## {section}\n\n{hidden}\n")
                    self.assertEqual(self.check(texts), [])

    def test_ordered_headings_and_links(self):
        self.assertTrue(self.check([RECORD.replace("## Mechanisms", "## Extra"), OLD, DRIVE]))
        for index, target in ((1, "0003-download-boundary.md"), (2, "design/0003-download-boundary.md")):
            texts = [RECORD, OLD, DRIVE]
            texts[index] = texts[index].replace(target, "wrong.md")
            self.assertTrue(self.check(texts))

    def test_public_docs_require_their_own_visible_policy_statements(self):
        for module, attribute, section in (
            (drive_verifier, "PAGE", "Planned downloads"),
            (threat_verifier, "DOC", "Planned Drive downloads"),
        ):
            source = getattr(module, attribute).read_text(encoding="utf-8")
            for token in ("tools/call", "download.enabled", "download.maxBytes", "download.timeoutSeconds",
                          "ErrConfigInvalid", "time.Duration", "readOnlyHint false", "destructiveHint false"):
                source = source.replace(f"`{token}`", token)
            path = self.paths[0]
            path.write_text(source, encoding="utf-8")
            with mock.patch.object(module, attribute, path):
                with contextlib.redirect_stdout(io.StringIO()):
                    self.assertEqual(module.main(), 0)

                for name, statement in module.DOWNLOAD_REQUIREMENTS.items():
                    pattern = r"`?" + r"`?\s+`?".join(
                        re.escape(word) for word in statement.split()
                    ) + r"`?"
                    for replacement in ("", "<!-- " + statement + " -->",
                                        "Not " + statement,
                                        "\n\n```\n" + statement + "\n```\n\n"):
                        with self.subTest(document=module.__name__, name=name,
                                          replacement=replacement):
                            changed, count = re.subn(pattern, lambda _: replacement,
                                                     source, count=1)
                            self.assertEqual(count, 1)
                            # A visible copy outside the policy section cannot satisfy it.
                            path.write_text(statement + "\n\n" + changed, encoding="utf-8")
                            errors = io.StringIO()
                            with contextlib.redirect_stderr(errors):
                                self.assertEqual(module.main(), 1)
                            self.assertIn(f"{section}: missing {name}", errors.getvalue())

    def test_duplicate_drive_summary_is_rejected(self):
        self.assertTrue(self.check([RECORD, OLD, DRIVE + "\n" + DRIVE.split("\n\n", 1)[1]]))


if __name__ == "__main__":
    unittest.main()

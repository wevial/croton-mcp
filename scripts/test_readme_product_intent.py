#!/usr/bin/env python3
"""Structural checks: the README states Croton's purpose and trust boundary.

Only visible prose counts. HTML comments, including an unterminated one, and
top-level fenced code blocks are removed before matching, and whitespace is
collapsed so that wrapped lines still match. A fence opens on a line starting
with three or more backticks or tildes, optionally followed by an info string,
and closes only on a line of at least as many of the same character and nothing
else. Nested constructs (fences in blockquotes or list items, indented code,
raw HTML blocks) are out of scope; this is not a Markdown parser. These are
documentation witnesses, not runtime proof of consent or confidentiality.
"""
from pathlib import Path
import re
import unittest


REPO = Path(__file__).resolve().parent.parent
README = REPO / "README.md"

PURPOSE = (
    "Croton makes Proton usable by AI assistants without handing them "
    "unrestricted access to your account.",
    "Privacy is the purpose; prompt-injection resistance is a supporting safeguard.",
)
DISCLOSURE = (
    "Content returned to a cloud-backed assistant can reach that assistant's "
    "model provider.",
    "Croton limits disclosure; it does not guarantee confidentiality after that "
    "handoff or that returned content stays on your machine.",
    "Croton does not protect a compromised operating system, user account, "
    "Proton account or Bridge.",
    "Croton cannot verify that a human approved a tool call.",
)
RETAINED = (
    "read-only by default",
    "`mutations.enabled`",
    "no live account write has been exercised",
    "](SECURITY.md)",
    "](docs/THREAT_MODEL.md)",
    "](docs/USER-INSTALL.md)",
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
    parts = re.split(r"^## +(.+?)\s*$", text, flags=re.M)
    return [(parts[i], " ".join(parts[i + 1].split())) for i in range(1, len(parts), 2)]


class ReadmeProductIntent(unittest.TestCase):
    def setUp(self):
        self.text = visible(README.read_text(encoding="utf-8"))
        self.sections = sections(self.text)
        self.headings = [heading for heading, _ in self.sections]

    def section(self, heading):
        self.assertEqual(self.headings.count(heading), 1, f"need one visible {heading!r} section")

        return dict(self.sections)[heading]

    def test_purpose_before_status(self):
        body = self.section("Why Croton")
        self.section("Status")

        self.assertLess(self.headings.index("Why Croton"), self.headings.index("Status"))
        for statement in PURPOSE:
            self.assertIn(statement, body)

    def test_disclosure_and_trust_boundaries(self):
        body = self.section("Security and privacy")

        for statement in DISCLOSURE:
            self.assertIn(statement, body)

    def test_existing_defaults_and_links_remain(self):
        prose = " ".join(self.text.split())

        for item in RETAINED:
            self.assertIn(item, prose)


if __name__ == "__main__":
    unittest.main(verbosity=2)

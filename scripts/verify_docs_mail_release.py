#!/usr/bin/env python3
"""Offline witness for the tagged Mail release guide; uses repository text only.

Clauses are matched in visible prose: HTML comments, including an unterminated
one, and top-level fenced code blocks are removed, and whitespace is collapsed
so wrapped lines still match. The marked tagged shell recipe in Source build is
inspected separately as text, as are the marked download recipe in the release
guide and the first-install copy fences in User-owned layout. Nothing here calls
git, gh, the network, a build or an installer, and passing is not evidence that
any release exists.
"""

import argparse
from pathlib import Path
import re
import sys

import verify_docs_user_install as installer

ROOT = Path(__file__).resolve().parents[1]
PATHS = {"release": "docs/RELEASE.md", "guide": "docs/USER-INSTALL.md", "readme": "README.md"}
RECIPE = re.compile(r"<!-- mail-release-tagged-recipe -->\s*```sh\n(.*?)\n```", re.S)
DOWNLOAD = re.compile(r"<!-- mail-release-download-recipe -->\s*```sh\n(.*?)\n```", re.S)
FENCE = re.compile(r"^```sh\n(.*?)\n```", re.S | re.M)

# (document, section, clause name, exact phrase) per witness.
CLAUSES = {
    "test_publication_boundary": (
        ("release", "Publication boundary", "tag plus published release",
         "A Mail release is a reviewed git tag plus a matching published non-draft GitHub release for that exact tag."),
        ("release", "Publication boundary", "non-release forms",
         "A moving branch, an untagged commit SHA, a draft release, or a tag with no published release is not a release."),
        ("release", "Publication boundary", "absent publication STOP",
         "If no matching published non-draft GitHub release exists for the selected tag, STOP"),
        ("release", "Publication boundary", "no existing release claim",
         "completing this documentation does not create or claim an existing release."),
        ("release", "Publication boundary", "separate operator actions",
         "running a live triage pilot each remain separate authorized operator actions."),
    ),
    "test_manifest_and_published_bytes": (
        ("release", "Maintainer release checklist", "new immutable tag",
         "Create a new immutable tag at that SHA"),
        ("release", "Maintainer release checklist", "manifest revision",
         "`revision` equals the tag-resolved SHA"),
        ("release", "Maintainer release checklist", "manifest toolchain", "`toolchain` is `go1.26.6`"),
        ("release", "Maintainer release checklist", "manifest platform",
         "`GOOS` and `GOARCH` name the native build platform"),
        ("release", "Maintainer release checklist", "manifest binary filename",
         "`binary` is `croton-mcp`"),
        ("release", "Maintainer release checklist", "manifest sha256",
         "`sha256` is the staged binary's SHA-256."),
        ("release", "Maintainer release checklist", "independent staged hash",
         "Independently hash the staged binary with the platform's local hash utility and compare it with the manifest `sha256`."),
        ("release", "Maintainer release checklist", "no checksum editing",
         "never edit the manifest or an expected checksum to make it match."),
        ("release", "Maintainer release checklist", "platform-identified binary asset",
         "The binary asset is `croton-mcp-<RELEASE_TAG>-<GOOS>-<GOARCH>`, a byte-identical copy of the staged `croton-mcp` named by the manifest `binary` field."),
        ("release", "Maintainer release checklist", "platform-identified manifest asset",
         "The manifest asset is `croton-mcp-<RELEASE_TAG>-<GOOS>-<GOARCH>.manifest.json`, a byte-identical copy of the staged `manifest.json`."),
        ("release", "Maintainer release checklist", "unchanged manifest schema",
         "the manifest content and schema are not rewritten."),
        ("release", "Maintainer release checklist", "release-note fields and checksum",
         "the manifest's full `revision`, `toolchain`, `GOOS` and `GOARCH`, the exact published binary asset filename, its staged manifest `binary` name, and the binary SHA-256."),
        ("release", "Maintainer release checklist", "platform-identified asset pair",
         "Upload each platform's binary and manifest assets together as a platform-identified pair, then publish the release as non-draft."),
        ("release", "Maintainer release checklist", "no private release content",
         "No secrets, private paths, account metadata or mail content belong in release notes or assets."),
        ("release", "Maintainer release checklist", "tagName and isDraft check",
         "`tagName` exactly equals the tag and `isDraft` is false."),
        ("release", "Maintainer release checklist", "tag resolution not targetCommitish",
         "`targetCommitish` alone is not proof of the immutable tag's resolved SHA."),
        ("release", "Maintainer release checklist", "independent published hash",
         "independently hash the published bytes, and compare them with the release notes and the published manifest `sha256`."),
        ("release", "Maintainer release checklist", "no in-place replacement",
         "Do not replace accepted assets in place or retarget the tag"),
        ("release", "Accepting published bytes", "host platform identity",
         "Identify the host platform with `go env GOHOSTOS GOHOSTARCH`"),
        ("release", "Accepting published bytes", "configured target is not the host",
         "Do not use `go env GOOS GOARCH`: those report the configured build target"),
        ("release", "Accepting published bytes", "absent downloaded asset STOP",
         "if either native asset is absent, STOP the binary path."),
        ("release", "Accepting published bytes", "manifest matches host platform",
         "`go1.26.6` and the host `GOHOSTOS` and `GOHOSTARCH`."),
    ),
    "test_tagged_selection": (
        ("guide", "Source build", "published RELEASE_TAG start",
         "Select a release only by an operator-selected published `RELEASE_TAG`"),
        ("guide", "Source build", "no moving branch or untagged SHA",
         "Do not build a moving branch or a directly chosen untagged SHA."),
        ("guide", "Source build", "publication check",
         "Check that the GitHub release `tagName` exactly equals `RELEASE_TAG` and `isDraft` is false; otherwise STOP."),
        ("guide", "Source build", "exact tag resolution",
         "Resolve that exact tag to its full commit SHA as `REVIEWED_REVISION`"),
        ("guide", "Source build", "recorded full SHA equals HEAD",
         "Record the printed full SHA and confirm HEAD equals it."),
    ),
    "test_integrity_limitations": (
        ("release", "Integrity limitations", "checksum is not a signature",
         "Checksums provide integrity, not signatures or provenance."),
        ("release", "Integrity limitations", "rebuild not assumed equal",
         "Locally rebuilt bytes are not assumed equal to a release asset."),
        ("release", "Integrity limitations", "commit identity is not enough",
         "Never compare only the commit identity."),
        ("release", "Integrity limitations", "pilot requires published bytes",
         "Pilot acceptance requires the selected published staged bytes whose SHA-256 matches the release notes and the published manifest."),
        ("release", "Integrity limitations", "absent platform asset STOP",
         "If the platform is unsupported or no native asset is published for it, STOP the binary path"),
        ("release", "Integrity limitations", "no signing while sole installer",
         "No signing is required while Ko is the only installer."),
        ("release", "Accepting published bytes", "published bytes not rebuild",
         "Pilot acceptance installs the published staged binary for the operator's native platform, not a local rebuild."),
        ("release", "Accepting published bytes", "acceptance mismatch STOP",
         "On any mismatch, STOP; do not edit expected checksums."),
        ("release", "Accepting published bytes", "published-byte first install",
         "it copies the accepted asset to `croton-mcp.candidate` exactly once instead of the source-build copy"),
        ("guide", "User-owned layout", "occupied candidate STOP",
         "if either is occupied, STOP rather than overwrite it."),
        ("guide", "User-owned layout", "published-byte entry skips source copy",
         "Published-byte entry: for pilot acceptance, skip the source-build copy"),
        ("guide", "User-owned layout", "published candidate hash before rename",
         "confirm it equals the release-note SHA-256 and the published manifest `sha256` before renaming it."),
        ("guide", "Source build", "retained checksum is not a signature",
         "Checksums provide integrity, not signatures or provenance attestation."),
        ("guide", "Source build", "source build distinguished",
         "A source build and published-byte acceptance are different paths."),
        ("guide", "Source build", "guide rebuild not assumed equal",
         "Locally rebuilt bytes are not assumed equal to a release asset"),
        ("guide", "Source build", "guide pilot requires published bytes",
         "Pilot acceptance requires the selected published staged bytes whose SHA-256 matches the release notes"),
        ("guide", "Source build", "guide absent platform asset STOP",
         "If the platform is unsupported or no native asset is published for it, STOP the binary path."),
    ),
    "test_update_boundaries": (
        ("release", "Updates and authority", "tagged update",
         "Updates select another published release tag"),
        ("release", "Updates and authority", "no untagged update",
         "Never update from a moving branch or an untagged SHA."),
        ("release", "Updates and authority", "separate approval",
         "Publication grants no authority to install, enable mutations or make a live `tools/call`; each needs separate explicit operator approval."),
        ("guide", "Update and rollback", "guide tagged update",
         "Select another published release tag, never a moving branch or an untagged SHA"),
        ("guide", "Update and rollback", "guide separate approval",
         "Publication grants no authority to install or enable anything; the update and any live read need separate explicit operator approval."),
        ("guide", "Update and rollback", "launch blocking",
         "Keep launches blocked while installing the complete matched artifact set"),
        ("guide", "Update and rollback", "complete prior set",
         "Keep launches blocked until the complete prior set is restored and verified"),
    ),
    "test_links_and_status": (
        ("guide", "Distribution limitations", "tagged source path",
         "This is a source-build path from a published release tag, with optional acceptance of platform-identified staged binaries published with a matching release"),
        ("guide", "Distribution limitations", "no published-state claim",
         "This guide does not claim that any release has been published"),
        ("guide", "Distribution limitations", "no signing or installer automation",
         "The repository does not provide supported install packages, a release updater, binary signing, installer automation"),
        ("readme", "User-owned Mail installation", "README release guide summary",
         "for the manual tagged-release checklist, staged per-platform checksums and acceptance of published bytes."),
    ),
}
RELEASE_HEADINGS = ("Publication boundary", "Maintainer release checklist", "Accepting published bytes",
                    "Integrity limitations", "Updates and authority", "Offline verification")
# Affirmative claims of publication, signing, automation or reproducibility.
FABRICATED = (
    r"\b(?:the|a|our|this|first|latest|current|initial|mail)(?: [\w-]+){0,2} releases? "
    r"(?:is|are|has been|have been|was|were) (?:now )?(?:published|available|out)\b",
    r"\bdownload the latest release\b",
    r"\b(?:binaries|releases|assets|binary) (?:are|is) signed\b",
    r"\bsigned (?:binaries|releases|assets)\b",
    r"\binstaller automation (?:is|exists|installs)\b",
    r"\bautomatically (?:installs|publishes|updates)\b",
    r"\b(?:are|is) (?:guaranteed )?reproducible\b",
)
# Each link: (document, relative target, owning directory of the document).
LINKS = (("readme", "docs/RELEASE.md", ""), ("guide", "RELEASE.md", "docs"),
         ("release", "USER-INSTALL.md", "docs"))
RECIPE_ORDER = (
    ("selected tag", "RELEASE_TAG='<published-release-tag>'"),
    ("publication check", 'gh release view "$RELEASE_TAG" --repo wevial/croton-mcp --json tagName,isDraft'),
    ("fresh clone", "git clone https://github.com/wevial/croton-mcp.git /absolute/operator/source/croton-mcp"),
    ("tag resolution", 'REVIEWED_REVISION="$(git rev-parse --verify "refs/tags/$RELEASE_TAG^{commit}")"'),
    ("detached checkout", 'git checkout --detach "$REVIEWED_REVISION"'),
    ("HEAD check", 'test "$(git rev-parse HEAD)" = "$REVIEWED_REVISION"'),
    ("race tests", "go test -race ./..."),
    ("unchanged helper",
     'python3 scripts/stage_mail_candidate.py --revision "$REVIEWED_REVISION" --output "$MAIL_CANDIDATE_DIR"'),
)
DOWNLOAD_LINES = (
    ("host platform", 'HOST_PLATFORM="$(go env GOHOSTOS)-$(go env GOHOSTARCH)"'),
    ("platform asset name", 'MAIL_RELEASE_ASSET="croton-mcp-$RELEASE_TAG-$HOST_PLATFORM"'),
    ("download directory", "MAIL_RELEASE_DIR=/absolute/operator/candidates/mail-release"),
    ("absent download directory", 'test ! -e "$MAIL_RELEASE_DIR"'),
    ("two-asset download",
     'gh release download "$RELEASE_TAG" --repo wevial/croton-mcp --dir "$MAIL_RELEASE_DIR" '
     '--pattern "$MAIL_RELEASE_ASSET" --pattern "$MAIL_RELEASE_ASSET.manifest.json"'),
)
TARGET = re.compile(r"\bgo env (?:GOOS|GOARCH)\b|\$\(go env (?:GOOS|GOARCH)\)")
SOURCE_COPY = 'install -m 0700 "$MAIL_CANDIDATE_DIR/croton-mcp" "$CROTON_BIN_DIR/croton-mcp.candidate"'
PUBLISHED_COPY = 'install -m 0700 "$MAIL_RELEASE_DIR/$MAIL_RELEASE_ASSET" "$CROTON_BIN_DIR/croton-mcp.candidate"'
MOVING = re.compile(r"\b(?:main|master|trunk|develop)\b|origin/|refs/heads/|HEAD[~^]|@\{|FETCH_HEAD"
                    r"|--branch\b|\b[0-9a-f]{40}\b|\b[0-9a-f]{64}\b|<full-reviewed-commit-sha>")


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


def normalize(text):
    return " ".join(text.split()).replace("**", "")


def sections(text):
    parts = re.split(r"^## +(.+?)\s*$", text, flags=re.M)
    found = {}
    for index in range(1, len(parts), 2):
        found.setdefault(parts[index], []).append(parts[index + 1])

    return found


def check_clauses(docs, witness):
    failures = []
    for doc, heading, name, phrase in CLAUSES[witness]:
        bodies = sections(visible(docs[doc])).get(heading, [])
        if len(bodies) != 1:
            failures.append(f"{PATHS[doc]}: need one visible {heading!r} section")
            continue
        if normalize(phrase) not in normalize(bodies[0]):
            failures.append(f"{PATHS[doc]} {heading}: missing {name}")

    return failures


def fabricated(docs, names):
    failures = []
    for doc in names:
        prose = normalize(visible(docs[doc])).lower()
        for pattern in FABRICATED:
            if re.search(pattern, prose):
                failures.append(f"{PATHS[doc]}: fabricated release, signing or automation claim {pattern!r}")

    return failures


def test_publication_boundary(docs):
    failures = check_clauses(docs, "test_publication_boundary")

    found = sections(visible(docs["release"]))
    if tuple(found) != RELEASE_HEADINGS or any(len(bodies) != 1 for bodies in found.values()):
        failures.append(f"{PATHS['release']}: expected ordered sections {', '.join(RELEASE_HEADINGS)}")

    failures.extend(fabricated(docs, ("release",)))
    return failures


def download_errors(release):
    bodies = sections(release).get("Accepting published bytes", [])
    recipes = DOWNLOAD.findall(release)
    if len(bodies) != 1 or len(recipes) != 1 or recipes[0] not in bodies[0]:
        return ["download recipe: expected one marked recipe in Accepting published bytes"]

    failures = []
    lines = [line.strip() for line in recipes[0].splitlines() if line.strip()]
    positions = []
    for name, expected in DOWNLOAD_LINES:
        if lines.count(expected) != 1:
            failures.append(f"download recipe: missing {name}")
        else:
            positions.append(lines.index(expected))
    if positions != sorted(positions):
        failures.append("download recipe: steps out of order")

    # The directory checked for absence and passed to --dir is assigned only once.
    if any(re.match(r"(?:export\s+)?MAIL_RELEASE_DIR=", line) and line != DOWNLOAD_LINES[2][1] for line in lines):
        failures.append("download recipe: download directory assigned other than once")
    if any(TARGET.search(line) for line in lines):
        failures.append("download recipe: configured target used as host platform")

    return failures


def test_manifest_and_published_bytes(docs):
    return check_clauses(docs, "test_manifest_and_published_bytes") + download_errors(docs["release"])


def recipe_errors(guide):
    bodies = sections(guide).get("Source build", [])
    recipes = RECIPE.findall(guide)
    if len(bodies) != 1 or len(recipes) != 1 or recipes[0] not in bodies[0]:
        return ["recipe: expected one marked tagged shell recipe in Source build"]

    failures = []
    lines = [line.strip() for line in recipes[0].splitlines()
             if line.strip() and not line.strip().startswith("#")]
    if not lines or lines[0] != RECIPE_ORDER[0][1]:
        failures.append("recipe: must start from an operator-selected published RELEASE_TAG")

    positions = []
    for name, expected in RECIPE_ORDER:
        if lines.count(expected) != 1:
            failures.append(f"recipe: missing {name}")
        else:
            positions.append(lines.index(expected))
    if positions != sorted(positions):
        failures.append("recipe: steps out of order")

    for line in lines:
        if re.match(r"(?:export\s+)?(?:REVIEWED_REVISION|RELEASE_TAG)=", line) and line not in (
                RECIPE_ORDER[0][1], RECIPE_ORDER[3][1]):
            failures.append("recipe: revision or tag chosen other than by exact tag resolution")
        if re.match(r"git\s+(?:checkout|switch|pull|reset|merge|rebase)\b", line) and line != RECIPE_ORDER[4][1]:
            failures.append("recipe: unexpected checkout or branch update")
        if MOVING.search(line):
            failures.append("recipe: moving branch or directly chosen SHA")

    return failures


def test_tagged_selection(docs):
    return check_clauses(docs, "test_tagged_selection") + recipe_errors(docs["guide"])


def first_install_errors(guide):
    bodies = sections(guide).get("User-owned layout", [])
    if len(bodies) != 1:
        return ["first install: need one User-owned layout section"]

    # Each entry has its own fence so the published candidate is never overwritten by a source copy.
    blocks = [[line.strip() for line in block.splitlines()] for block in FENCE.findall(bodies[0])]
    source = [block for block in blocks if SOURCE_COPY in block]
    published = [block for block in blocks if PUBLISHED_COPY in block]
    if len(source) != 1 or SOURCE_COPY not in source[0] or PUBLISHED_COPY in source[0]:
        return ["first install: expected one separate source-build copy"]
    if len(published) != 1 or SOURCE_COPY in published[0] or published[0].count(PUBLISHED_COPY) != 1:
        return ["first install: expected one separate published-byte copy"]

    return []


def test_integrity_limitations(docs):
    return check_clauses(docs, "test_integrity_limitations") + first_install_errors(docs["guide"])


def test_update_boundaries(docs):
    failures = check_clauses(docs, "test_update_boundaries")

    # The existing installer witness keeps the TLS, helper, config and rollback guards.
    failures.extend(f"installer: {error}" for error in installer.verify(docs["guide"], docs["readme"]))
    return failures


def test_links_and_status(docs):
    failures = check_clauses(docs, "test_links_and_status")

    for doc, target, base in LINKS:
        pattern = r"\[[^\]]+\]\(" + re.escape(target) + r"\)"
        if not re.search(pattern, visible(docs[doc])):
            failures.append(f"{PATHS[doc]}: link to {target} missing")
        elif str(Path(base, target)) not in docs["files"]:
            failures.append(f"{PATHS[doc]}: link to {target} does not resolve")

    failures.extend(fabricated(docs, ("guide", "readme")))
    return failures


TESTS = (test_publication_boundary, test_manifest_and_published_bytes, test_tagged_selection,
         test_integrity_limitations, test_update_boundaries, test_links_and_status)


def verify(docs):
    failures = []
    for test in TESTS:
        failures.extend(f"{test.__name__}: {error}" for error in test(docs))

    return failures


def reword(docs, doc, phrase, replacement):
    # Prose wraps, so match the phrase across any whitespace.
    pattern = r"\s+".join(map(re.escape, phrase.split()))
    text, count = re.subn(pattern, lambda _: replacement, docs[doc])
    if count == 0:
        raise ValueError(f"self-test phrase absent from {PATHS[doc]}: {phrase!r}")

    return {**docs, doc: text}


def self_test(docs):
    resolve = RECIPE_ORDER[3][1]
    cases = [("baseline", docs, None, None)]
    for witness, clauses in CLAUSES.items():
        test = globals()[witness]
        for doc, _, name, phrase in clauses:
            cases.append((f"remove {name}", reword(docs, doc, phrase, "Synthetic removed clause."),
                          test, f"missing {name}"))
    cases += [
        ("release-notes GOARCH removed", reword(
            docs, "release", "`toolchain`, `GOOS` and `GOARCH`, the exact", "`toolchain`, `GOOS`, the exact"),
         test_manifest_and_published_bytes, "missing release-note fields and checksum"),
        ("release-notes checksum removed", reword(
            docs, "release", "its staged manifest `binary` name, and the binary SHA-256.",
            "and its staged manifest `binary` name."),
         test_manifest_and_published_bytes, "missing release-note fields and checksum"),
        ("STOP only in a comment", reword(
            docs, "release", "If no matching published non-draft GitHub release exists for the selected tag, STOP",
            "<!-- If no matching published non-draft GitHub release exists for the selected tag, STOP -->"),
         test_publication_boundary, "missing absent publication STOP"),
        ("publication check omitted", reword(docs, "guide", RECIPE_ORDER[1][1], ""),
         test_tagged_selection, "recipe: missing publication check"),
        ("tag resolution omitted", reword(docs, "guide", resolve, ""),
         test_tagged_selection, "recipe: missing tag resolution"),
        ("directly chosen SHA", reword(docs, "guide", resolve, "REVIEWED_REVISION='<full-reviewed-commit-sha>'"),
         test_tagged_selection, "recipe: revision or tag chosen other than by exact tag resolution"),
        ("literal untagged SHA", reword(docs, "guide", resolve, "REVIEWED_REVISION='" + "0" * 40 + "'"),
         test_tagged_selection, "recipe: moving branch or directly chosen SHA"),
        ("moving branch resolution", reword(
            docs, "guide", resolve, 'REVIEWED_REVISION="$(git rev-parse --verify origin/main)"'),
         test_tagged_selection, "recipe: moving branch or directly chosen SHA"),
        ("moving branch checkout", reword(docs, "guide", RECIPE_ORDER[4][1], "git checkout main"),
         test_tagged_selection, "recipe: unexpected checkout or branch update"),
        ("arbitrary starting point", reword(docs, "guide", RECIPE_ORDER[0][1], "RELEASE_TAG=\"$(git describe)\""),
         test_tagged_selection, "recipe: must start from an operator-selected published RELEASE_TAG"),
        ("publication checked after checkout", reword(
            reword(docs, "guide", RECIPE_ORDER[1][1], ""), "guide",
            RECIPE_ORDER[4][1], RECIPE_ORDER[4][1] + "\n" + RECIPE_ORDER[1][1]),
         test_tagged_selection, "recipe: steps out of order"),
        ("helper given another revision", reword(
            docs, "guide", RECIPE_ORDER[-1][1], 'python3 scripts/stage_mail_candidate.py --revision "$(git rev-parse HEAD)" '
            '--output "$MAIL_CANDIDATE_DIR"'),
         test_tagged_selection, "recipe: missing unchanged helper"),
        ("configured target platform", reword(
            docs, "release", DOWNLOAD_LINES[0][1], 'HOST_PLATFORM="$(go env GOOS)-$(go env GOARCH)"'),
         test_manifest_and_published_bytes, "download recipe: configured target used as host platform"),
        ("download directory removed", reword(docs, "release", DOWNLOAD_LINES[2][1], ""),
         test_manifest_and_published_bytes, "download recipe: missing download directory"),
        ("download directory after download", reword(
            reword(docs, "release", DOWNLOAD_LINES[2][1], ""), "release",
            DOWNLOAD_LINES[-1][1], DOWNLOAD_LINES[-1][1] + "\n" + DOWNLOAD_LINES[2][1]),
         test_manifest_and_published_bytes, "download recipe: steps out of order"),
        ("download directory reassigned", reword(
            docs, "release", DOWNLOAD_LINES[3][1], "MAIL_RELEASE_DIR=/tmp/other\n" + DOWNLOAD_LINES[3][1]),
         test_manifest_and_published_bytes, "download recipe: download directory assigned other than once"),
        ("download pattern without value", reword(
            docs, "release", DOWNLOAD_LINES[-1][1], 'gh release download "$RELEASE_TAG" --repo wevial/croton-mcp --pattern'),
         test_manifest_and_published_bytes, "download recipe: missing two-asset download"),
        ("manifest asset not downloaded", reword(
            docs, "release", DOWNLOAD_LINES[-1][1], DOWNLOAD_LINES[-1][1].rsplit(" --pattern", 1)[0]),
         test_manifest_and_published_bytes, "download recipe: missing two-asset download"),
        ("missing download marker", reword(docs, "release", "<!-- mail-release-download-recipe -->", ""),
         test_manifest_and_published_bytes, "download recipe: expected one marked recipe"),
        ("source copy over published candidate", reword(docs, "guide", PUBLISHED_COPY, SOURCE_COPY),
         test_integrity_limitations, "first install: expected one separate"),
        ("published entry also copies source build", reword(
            docs, "guide", PUBLISHED_COPY, PUBLISHED_COPY + "\n" + SOURCE_COPY),
         test_integrity_limitations, "first install: expected one separate"),
        ("missing recipe marker", reword(docs, "guide", "<!-- mail-release-tagged-recipe -->", ""),
         test_tagged_selection, "recipe: expected one marked tagged shell recipe"),
        ("retained rollback guard removed", reword(
            docs, "guide", "rename the staged executable and every staged config, helper and trust file",
            "rename only the staged executable"),
         test_update_boundaries, "installer: Update and rollback: missing contract phrase"),
        ("retained loopback guard removed", reword(docs, "guide", '"host": "127.0.0.1"', '"host": "192.0.2.1"'),
         test_update_boundaries, "IMAP must be loopback"),
        ("missing README link", reword(docs, "readme", "(docs/RELEASE.md)", "(docs/RELEASE.txt)"),
         test_links_and_status, "README.md: link to docs/RELEASE.md missing"),
        ("missing link target", {**docs, "files": docs["files"] - {"docs/RELEASE.md"}},
         test_links_and_status, "README.md: link to docs/RELEASE.md does not resolve"),
        ("fabricated published state", reword(
            docs, "guide", "This guide does not claim that any release has been published",
            "The first Mail release has been published. This guide does not claim that any release has "
            "been published"),
         test_links_and_status, "fabricated release, signing or automation claim"),
        ("fabricated latest download", reword(
            docs, "readme", "See [the Mail release guide]", "Download the latest release. See [the Mail release guide]"),
         test_links_and_status, "fabricated release, signing or automation claim"),
        ("fabricated signing", reword(
            docs, "release", "Checksums provide integrity, not signatures or provenance.",
            "Checksums provide integrity, not signatures or provenance. Release binaries are signed."),
         test_publication_boundary, "fabricated release, signing or automation claim"),
        ("fabricated reproducibility", reword(
            docs, "release", "Builds are not guaranteed reproducible", "Builds are guaranteed reproducible"),
         test_publication_boundary, "fabricated release, signing or automation claim"),
    ]

    executed = 0
    failed = []
    for name, candidate, test, expected in cases:
        errors = verify(candidate) if test is None else test(candidate)
        executed += 1
        if (expected is None and errors) or (expected is not None and not any(
                expected in error for error in errors)):
            failed.append(f"self-test {name}: unexpected result {errors}")

    print(f"mail-release self-test: {executed} cases executed")
    if executed == 0:
        failed.append("self-test executed zero cases")
    return failed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    args = parser.parse_args()

    try:
        docs = {key: (ROOT / path).read_text(encoding="utf-8") for key, path in PATHS.items()}
        docs["files"] = frozenset(str(Path(base, target)) for _, target, base in LINKS
                                  if (ROOT / base / target).is_file())
        failures = verify(docs)
        if args.self_test:
            failures.extend(self_test(docs))
    except (OSError, ValueError) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        return 1

    if failures:
        for failure in failures:
            print(f"FAIL: {failure}", file=sys.stderr)
        return 1

    print("mail-release docs OK (offline synthetic checks only)")
    return 0


if __name__ == "__main__":
    sys.exit(main())

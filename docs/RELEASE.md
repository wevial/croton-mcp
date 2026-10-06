# Mail release guide

This guide defines the operator-owned release path for the `croton-mcp` Mail
executable. It is a manual checklist. It creates no tag, publishes no release,
uploads no asset, installs nothing, enables no mutation and reads no mail.
Each of those is a separate action that the operator authorizes and performs.
All tags, paths and hashes below are synthetic placeholders.

## Publication boundary

A Mail release is a reviewed git tag plus a matching published non-draft GitHub
release for that exact tag. A moving branch, an untagged commit SHA, a draft
release, or a tag with no published release is not a release. If no matching
published non-draft GitHub release exists for the selected tag, STOP: do not
build, stage, accept or install anything from that tag as a release.

Publication status: completing this documentation does not create or claim an
existing release. Check the repository's GitHub releases page for what has
actually been published. Creating and pushing a tag, creating a GitHub release,
uploading assets, installing, enabling mutations and running a live triage
pilot each remain separate authorized operator actions.

## Maintainer release checklist

1. Select a full 40-hex commit SHA that a maintainer reviewed and that passed
   the required checks. Record it as the reviewed SHA.
2. Create a new immutable tag at that SHA, recorded as `RELEASE_TAG`, for
   example `v0.1.0`. Never reuse, move, retarget or delete a published tag.
   Resolve the tag to its commit with
   `git rev-parse --verify "refs/tags/$RELEASE_TAG^{commit}"` and confirm the
   result equals the reviewed SHA before pushing it.
3. For each native platform you choose to publish, build on a Linux or macOS
   host of that platform. In a fresh clone, check out the tag-resolved SHA as a
   detached HEAD, confirm HEAD equals it, run `go build`, `go vet` and
   `go test -race` with Go 1.26.6, and pass that full SHA to the unchanged
   `scripts/stage_mail_candidate.py`, as in Source build in the
   [user-owned installation guide](USER-INSTALL.md). That recipe's publication
   check applies only once step 7 is complete. Cross-compiled artifacts are not
   published. Publishing every platform at once is not required.
4. Review the staged `manifest.json`: `revision` equals the tag-resolved SHA,
   `toolchain` is `go1.26.6`, `GOOS` and `GOARCH` name the native build
   platform, `binary` is `croton-mcp`, and `sha256` is the staged binary's
   SHA-256. Independently hash the staged binary with the platform's local hash
   utility and compare it with the manifest `sha256`. On any mismatch, STOP;
   never edit the manifest or an expected checksum to make it match.
5. Copy the unchanged staged bytes to platform-identified asset names. The
   binary asset is `croton-mcp-<RELEASE_TAG>-<GOOS>-<GOARCH>`, a byte-identical
   copy of the staged `croton-mcp` named by the manifest `binary` field. The
   manifest asset is `croton-mcp-<RELEASE_TAG>-<GOOS>-<GOARCH>.manifest.json`,
   a byte-identical copy of the staged `manifest.json`. This filename mapping
   is the only change; the manifest content and schema are not rewritten.
   Independently hash both copies and confirm the binary asset hash equals the
   manifest `sha256`.
6. Create the GitHub release for the exact tag. Its release notes must state,
   for each published platform, the manifest's full `revision`, `toolchain`,
   `GOOS` and `GOARCH`, the exact published binary asset filename, its staged
   manifest `binary` name, and the binary SHA-256. Upload each platform's binary
   and manifest assets together as a platform-identified pair, then publish the
   release as non-draft. No secrets, private paths, account metadata or mail
   content belong in release notes or assets.
7. After publication, confirm with `gh release view` that `tagName` exactly
   equals the tag and `isDraft` is false. Resolve the tag to its commit
   independently; `targetCommitish` alone is not proof of the immutable tag's
   resolved SHA. Download the published assets into a new absent directory,
   independently hash the published bytes, and compare them with the release
   notes and the published manifest `sha256`. On any mismatch, STOP. Do not
   replace accepted assets in place or retarget the tag; a correction needs a
   new tag and a new release.

A synthetic release-notes shape, one block per published platform:

```text
Croton Mail v0.1.0
revision: 0000000000000000000000000000000000000000
toolchain: go1.26.6
GOOS: linux
GOARCH: amd64
binary asset: croton-mcp-v0.1.0-linux-amd64 (staged manifest binary: croton-mcp)
manifest asset: croton-mcp-v0.1.0-linux-amd64.manifest.json
sha256: 0000000000000000000000000000000000000000000000000000000000000000
Checksums provide integrity, not signatures or provenance.
```

## Accepting published bytes

Pilot acceptance installs the published staged binary for the operator's native
platform, not a local rebuild. Select a published `RELEASE_TAG`, confirm its
GitHub release `tagName` and `isDraft` as in step 7, and resolve the tag to its
full commit SHA in a fresh clone. Run `go env GOOS GOARCH` or an equivalent
local check to identify the native platform. Select the binary and manifest
assets whose names carry that exact `GOOS` and `GOARCH`.

Download both assets into a new absent operator-owned directory, for example
with `gh release download "$RELEASE_TAG" --repo wevial/croton-mcp --pattern`
for each exact asset name. Independently hash the binary asset with
`sha256sum` on Linux or `shasum -a 256` on macOS. Accept it only if that hash
equals the release-note SHA-256 and the published manifest `sha256`, and the
manifest `revision`, `toolchain`, `GOOS` and `GOARCH` equal the tag-resolved
SHA, `go1.26.6` and the native platform. On any mismatch, STOP; do not edit
expected checksums. Then stage the accepted bytes with mode `0700` as
`croton-mcp.candidate` and continue with the user-owned installation guide.

## Integrity limitations

- Checksums provide integrity, not signatures or provenance.
- Locally rebuilt bytes are not assumed equal to a release asset. Builds are not
  guaranteed reproducible, and a source build from the same tag can hash
  differently. Never compare only the commit identity.
- Pilot acceptance requires the selected published staged bytes whose SHA-256
  matches the release notes and the published manifest.
- If the platform is unsupported or no native asset is published for it, STOP
  the binary path; a local source build is not pilot acceptance of published bytes.
- No signing is required while Ko is the only installer. The person who
  publishes also installs, so the release-note checksum detects corruption or
  substitution of assets after publication; it does not prove who built them.
  Signing must be reconsidered before anyone else installs from a release.

## Updates and authority

Updates select another published release tag and repeat the selection,
acceptance and Update and rollback procedure in the user-owned installation
guide, including its backup, launch blocking and complete matched-set rollback.
Never update from a moving branch or an untagged SHA. Publication grants no
authority to install, enable mutations or make a live `tools/call`; each needs
separate explicit operator approval. Keep the prior accepted release's bytes,
manifest and backups until the update is accepted.

## Offline verification

```sh
python3 scripts/verify_docs_mail_release.py --self-test
python3 scripts/verify_docs_mail_release.py
```

These checks inspect repository text and synthetic fixtures only. They never
call git, gh, the network, a build or an installer, and they are not evidence
that any release exists.

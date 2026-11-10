# Mail release checklist

Pushing a `vX.Y.Z` tag runs `.github/workflows/release.yml`, which builds
`croton-mcp` with `CGO_ENABLED=0 -trimpath` for linux and darwin on amd64 and
arm64, stamps the tag into `serverInfo.version`, writes `SHA256SUMS`, and
publishes a GitHub release. A tag containing a hyphen is a prerelease.

1. Pick a commit on `main` that a maintainer reviewed and that passed CI.
2. Get explicit maintainer approval to publish; tagging publishes.
3. Tag and push: `git tag -a vX.Y.Z <sha> -m vX.Y.Z && git push origin vX.Y.Z`.
4. Watch the Release workflow; on failure, fix forward with a new tag.
5. Never move, reuse or delete a published tag or replace its assets.
6. Check: `gh release view vX.Y.Z` lists four `croton-mcp-vX.Y.Z-<os>-<arch>`
   binaries plus `SHA256SUMS`.

To install published bytes, download the native binary and `SHA256SUMS` with
the GitHub CLI (`gh`). Go is not needed; set `ASSET` to your OS (`linux` or
`darwin`) and CPU (`amd64`, or `arm64` for ARM and Apple silicon):

```sh
RELEASE_TAG=vX.Y.Z
ASSET="croton-mcp-$RELEASE_TAG-linux-amd64"
gh release download "$RELEASE_TAG" --repo wevial/croton-mcp --pattern "$ASSET" --pattern SHA256SUMS
grep " $ASSET\$" SHA256SUMS | sha256sum -c -   # macOS: shasum -a 256 -c -
```

On mismatch, STOP. Then install the binary as described in
[INSTALL.md](INSTALL.md). Checksums provide integrity, not signatures
or provenance. Publishing grants no authority to install, enable writes, or
make a live `tools/call`.

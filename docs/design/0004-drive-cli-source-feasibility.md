# Pinned Drive CLI bounded-source feasibility

## Pin

| Field | Value |
| --- | --- |
| version | 0.8.0 |
| banner | Proton Drive CLI cli-drive@0.8.0 |
| handshake | version |

## Verdict

blocked

## Evidence

| Requirement | Kind | Immutable upstream reference | Source/manual location | Exact command/flags | Finding |
| --- | --- | --- | --- | --- | --- |
| source command/flags | real-cli | https://github.com/ProtonDriveApps/sdk/blob/5491f2eea473acaaa86b5969774b84610a37bd46/cli/src/commands/fileSystem/commandFileSystemDownload.ts | cli/src/commands/fileSystem/commandFileSystemDownload.ts:22-75, 107-120; transferSummary.ts:58-69 in the same directory and commit | proton-drive filesystem download <remotePath...> <localFolder> --file-conflict-strategy skip --folder-conflict-strategy skip --json | The registered command takes a local directory and conflict options, with no stdout/descriptor payload option. It resolves and creates the directory before processing the queue. JSON is a transfer summary, not file bytes. |
| pre-copy writes | real-cli | https://github.com/ProtonDriveApps/sdk/blob/5491f2eea473acaaa86b5969774b84610a37bd46/cli/src/commands/fileSystem/downloadOperations.ts | cli/src/commands/fileSystem/downloadOperations.ts:74-170, downloadRemoteFile and downloadToPath | proton-drive filesystem download <remotePath...> <localFolder> --file-conflict-strategy skip --folder-conflict-strategy skip --json | Bun.file(localPath).writer() receives downloadToStream output before completion; no Croton byte budget intervenes. Downloading to staging then copying would allow all these pathname writes before bounded copying. The SDK stream API is internal to this CLI path, not an exposed CLI source interface. |
| interruption | real-cli | https://github.com/ProtonDriveApps/sdk/blob/5491f2eea473acaaa86b5969774b84610a37bd46/cli/src/commands/fileSystem/downloadOperations.ts | cli/src/commands/fileSystem/downloadOperations.ts:126-168, downloadToPath | proton-drive filesystem download <remotePath...> <localFolder> --file-conflict-strategy skip --folder-conflict-strategy skip --json | The wrapper awaits controller.completion(); its abort callback ends the writer and attempts unlink, and its catch also attempts unlink. This does not establish external cancellation, a deadline, interruption of blocked reads, or cleanup after forced process termination. No supported cancellation contract is established. |
| reaping | missing | unavailable | internal/drivecli/client.go:invoke; internal/drivecli/process_linux.go:configureProcess; internal/drivecli/process_unix.go:configureProcess | No supported bounded-source command/flags established | Croton configures a process group, SIGKILL on context cancellation and Run with WaitDelay. This is local adapter source evidence only. No real-CLI download lifecycle evidence establishes termination and reaping of the child and handling of descendants on every exit path. |

## Applicability

| Platform | Status | Assessment |
| --- | --- | --- |
| Linux | blocked | The inspected upstream download path uses pathname output without a platform-specific descriptor source. Croton's Linux process-group cancellation code cannot cap upstream staging writes. No real CLI execution or source-adapter lifecycle proof was performed. |
| macOS | blocked | The same upstream pathname output path applies; Croton's darwin process-group implementation supplies no stream source or staging cap. No macOS execution or real-CLI cancellation/reaping proof was performed. |

## Contract

None; no supported integration is claimed.

## Blockers

| Requirement | Unmet requirement or unavailable artifact |
| --- | --- |
| source command/flags | The inspected pinned download command exposes a directory destination, not a bounded stdout/inherited-descriptor source usable by the transactional writer. No exact alternative command with that contract was established. |
| pre-copy writes | CLI-owned pathname writes occur before a later copy; neither Croton's destination cap nor the CLI JSON output budget constrains them. Passing a validated destination to this command would reopen it by pathname. |
| interruption | No established source contract interrupts a blocked read within the configured deadline and bounds transfer writes while stopping. Error-path unlink code is not proof of cleanup on SIGKILL. |
| reaping | Missing real-CLI lifecycle evidence for termination, child reaping, descendant handling and descriptor cleanup on cancellation, timeout, overflow, failure and success on both target platforms. |

## Evidence boundaries

Downloads remain disabled. draft09 is not enabled by this ticket.

Synthetic evidence does not prove real-CLI behavior.

Post-download size checks and size polling are not proof of an enforced transfer cap.

The verifier checks structure and pin agreement; human review evaluates cited evidence.

## Next step

Keep integration blocked. A separate source-interface proposal must first identify
an immutable upstream artifact exposing a bounded, interruptible stream without
unbounded pathname staging, then specify byte/time bounds, termination/reaping
and cleanup on Linux and macOS. It must feed transactional descriptors without
reopening a validated destination. Do not change the pin or enable draft09 to
work around this blocker.

## Investigation

Offline inspection on 2026-09-18 UTC, limited to 30 minutes. KO-483 is already in
this checkout's history at 69e5e1a, satisfying the operator's sequencing gate.
No login, account/service access, dependency installation, CLI execution or
synthetic process experiment was used for this investigation.

The repository version authority is
[client.go](../../internal/drivecli/client.go), constants pinnedCLIVersion and
versionBannerPrefix plus Handshake/parseVersionBanner. This is a banner contract,
not binary provenance verification. The disabled argv is in
[commands.go](../../internal/drivecli/commands.go), Download; its allowlist rejects
that command. The [Drive documentation](../DRIVE-MCP.md) and
[resolution boundary](0003-download-boundary.md) supply the existing policy.
[BoundedTransactionalCopy](../../internal/drivefs/bounded_copy.go) explicitly
requires a cooperative source; it cannot interrupt an arbitrary blocked Read or
bound the CLI's staging writes. The local limitedBuffer discards excess command
output and reports overflow after Run; that is not a download transfer cap.

An existing public-source research note at
`/srv/dev/hermes-scratch/croton-drive-cli-contract-v0.8.0.md` led to the local
checkout `/srv/dev/hermes-scratch/croton-drive-recon/sdk`. Offline `git rev-parse
HEAD cli/v0.8.0` returned `5491f2eea473acaaa86b5969774b84610a37bd46` for both;
`git status --short` was empty. The exact-tag download command was also read with
`git show cli/v0.8.0:cli/src/commands/fileSystem/commandFileSystemDownload.ts`.
The evidence table cites immutable source locations so review does not depend on
retaining scratch files. No upstream manual was needed or independently checked.
The tag maps the source to release 0.8.0; `cli/package.json` says 0.0.1 and is not
the release version authority. At the same commit,
[build-cli.mjs](https://github.com/ProtonDriveApps/sdk/blob/5491f2eea473acaaa86b5969774b84610a37bd46/cli/scripts/build-cli.mjs)
lines 50 and 69-105 derive the app version from CLI_VERSION or cli tags.
The research note's external manifest claims were not revalidated and are not
used as evidence of a bounded source or of any installed binary's identity.

Documented evidence above is source inspection, not observed real-CLI execution.
Existing fake CLI fixtures and this verifier's invented `.test` supported record
are synthetic only. Unverified assumptions include treating a special pathname
such as `/dev/fd/...` as an inherited-descriptor interface, assuming SDK streams
are exposed by CLI flags, and assuming process-group killing proves complete
cleanup or descendant reaping. None is accepted as a supported contract.

Run `python3 scripts/verify_docs_drive_source.py --self-test` and
`python3 scripts/verify_docs_drive_source.py` before review and again before merge.
The standard-library verifier reads only this record and the local version
contract; it does not access scratch artifacts or fetch citation URLs.

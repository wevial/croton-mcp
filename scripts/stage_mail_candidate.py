#!/usr/bin/env python3
"""Stage a local Mail source candidate; never install or publish a release."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tarfile
import tempfile


class StageError(Exception):
    pass


def command(argv, cwd, env, stdout=subprocess.PIPE):
    result = subprocess.run(argv, cwd=cwd, env=env, stdin=subprocess.DEVNULL,
                            stdout=stdout, stderr=subprocess.PIPE, check=False)
    if result.returncode:
        # Do not relay potentially private command diagnostics.
        raise StageError(f"{argv[0]} {argv[1]} failed (exit {result.returncode})")

    return result.stdout.decode("utf-8").strip() if stdout == subprocess.PIPE else None


def stage(revision, output):
    if sys.platform not in ("linux", "darwin"):
        raise StageError("only native Linux and macOS builds are supported")
    if not re.fullmatch(r"(?:[0-9a-f]{40}|[0-9a-f]{64})", revision):
        raise StageError("revision must be an explicit full lowercase commit SHA")

    env = os.environ.copy()
    env.update(GIT_NO_REPLACE_OBJECTS="1", GOTOOLCHAIN="go1.26.6",
               GOENV="off", GOWORK="off", GOFLAGS="")
    root = Path(command(["git", "rev-parse", "--show-toplevel"], Path.cwd(), env)).resolve()
    head = command(["git", "rev-parse", "HEAD"], root, env)
    if revision != head:
        raise StageError("reviewed revision must equal checkout HEAD")
    if command(["git", "status", "--porcelain", "--untracked-files=no"], root, env):
        raise StageError("tracked working tree and index must be clean")
    if not output.is_absolute():
        raise StageError("output must be an absolute path")
    if os.path.lexists(output):
        raise StageError("output must be absent")

    output = output.resolve()
    if output == root or root in output.parents:
        raise StageError("output must be outside the checkout")
    if not output.parent.is_dir():
        raise StageError("output parent must already exist")

    with tempfile.TemporaryDirectory(prefix=".mail-candidate-", dir=output.parent) as temporary:
        work = Path(temporary)
        source = work / "source"
        source.mkdir()
        archive = work / "source.tar"
        with archive.open("wb") as stream:
            command(["git", "archive", "--format=tar", revision], root, env, stdout=stream)

        with tarfile.open(archive) as tracked:
            # data filtering rejects traversal and links outside the export.
            tracked.extractall(source, filter="data")

        info = json.loads(command(["go", "env", "-json", "GOVERSION", "GOOS", "GOARCH",
                                   "GOHOSTOS", "GOHOSTARCH"], source, env))
        native_os = {"linux": "linux", "darwin": "darwin"}[sys.platform]
        if (info["GOOS"] != native_os or info["GOHOSTOS"] != native_os
                or info["GOARCH"] != info["GOHOSTARCH"]):
            raise StageError("target must match the native Linux or macOS platform")
        if info["GOVERSION"] != "go1.26.6":
            raise StageError("Go 1.26.6 is required")

        candidate = work / "candidate"
        candidate.mkdir(mode=0o700)
        binary = candidate / "croton-mcp"
        command(["go", "build", "-trimpath", "-o", str(binary), "./cmd/croton-mcp"], source, env)
        binary.chmod(0o700)
        manifest = {
            "revision": revision,
            "toolchain": info["GOVERSION"],
            "GOOS": info["GOOS"],
            "GOARCH": info["GOARCH"],
            "binary": binary.name,
            "sha256": hashlib.sha256(binary.read_bytes()).hexdigest(),
        }
        (candidate / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n", encoding="utf-8")

        # Exclusive mkdir reserves the name even if another process created it
        # during the build. Never rename a directory over an existing output.
        output.mkdir(mode=0o700)
        published = []
        try:
            for name in ("croton-mcp", "manifest.json"):
                os.link(candidate / name, output / name)
                published.append(output / name)
        except OSError:
            for path in published:
                path.unlink()
            output.rmdir()
            raise

    return manifest


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--revision", required=True, help="full maintainer-reviewed SHA equal to HEAD")
    parser.add_argument("--output", required=True, type=Path, help="absent absolute directory outside checkout")
    args = parser.parse_args(argv)

    try:
        stage(args.revision, args.output)
    except StageError as error:
        print(f"Mail candidate staging failed: {error}", file=sys.stderr)
        return 1
    except (OSError, ValueError, KeyError, tarfile.TarError):
        print("Mail candidate staging failed; check revision, clean tree, output, native target and toolchain.",
              file=sys.stderr)
        return 1

    print("Local Mail candidate staged: croton-mcp and manifest.json. Nothing installed or published.")
    return 0


if __name__ == "__main__":
    sys.exit(main())

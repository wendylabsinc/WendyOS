#!/usr/bin/env python3
"""Copy checksum-pinned local Coke bundles without downloading training assets."""
from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path, PurePosixPath
import shutil
import sys
import tempfile


ROOT = Path(__file__).resolve().parent


def safe_path(root, relative):
    path = PurePosixPath(relative)
    if path.is_absolute() or not path.parts or any(part in (".", "..") for part in path.parts):
        raise ValueError(f"Invalid asset path: {relative}")
    candidate = root.joinpath(*path.parts)
    if not candidate.resolve().is_relative_to(root.resolve()):
        raise ValueError(f"Asset path escapes its directory: {relative}")
    return candidate


def verify_file(path, spec):
    if not path.is_file():
        raise ValueError(f"Missing asset: {path}")
    if path.stat().st_size != spec["bytes"]:
        raise ValueError(f"Asset size mismatch: {path}")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    if digest.hexdigest() != spec["sha256"]:
        raise ValueError(f"Asset SHA-256 mismatch: {path}")


def prepare(root, lock, *, sources=None, check=False):
    """Validate every input before publishing; preserve any existing valid file."""
    if lock.get("schema") != "wendy.g1.coke.example-assets.v1":
        raise ValueError("Unsupported asset lock schema")
    pending = []
    for relative, spec in lock["files"].items():
        destination = safe_path(root, relative)
        if check or destination.exists():
            verify_file(destination, spec)
            continue
        source_root = (sources or {}).get(spec["source"])
        if source_root is None:
            raise ValueError(f"Pass --{spec['source']} to supply {relative}")
        source = safe_path(source_root, spec["path"])
        verify_file(source, spec)
        pending.append((source, destination, spec))
    if check:
        return 0
    for source, destination, spec in pending:
        destination.parent.mkdir(parents=True, exist_ok=True)
        # Copy through a unique temporary file so an interrupted copy never
        # looks like a prepared asset on the next invocation.
        with tempfile.NamedTemporaryFile(dir=destination.parent, prefix=".prepare-", delete=False) as stream:
            temporary = Path(stream.name)
        try:
            shutil.copyfile(source, temporary)
            verify_file(temporary, spec)
            if destination.exists():
                verify_file(destination, spec)
            else:
                # A hard link publishes without replacing a concurrent writer.
                try:
                    os.link(temporary, destination)
                except FileExistsError:
                    verify_file(destination, spec)
        finally:
            temporary.unlink(missing_ok=True)
    return len(pending)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--expert", type=Path, help="g1-expert-sim-replay-20260915 directory")
    parser.add_argument("--runtime", type=Path, help="g1-reference-residual-runtime directory containing bundle/")
    parser.add_argument("--check", action="store_true", help="Verify prepared assets without copying")
    args = parser.parse_args()
    try:
        lock = json.loads((ROOT / "assets.lock.json").read_text())
        copied = prepare(ROOT, lock, sources={name: value.expanduser().resolve() for name in ("expert", "runtime")
                                              if (value := getattr(args, name)) is not None}, check=args.check)
    except (OSError, ValueError, KeyError) as error:
        print(f"Coke assets: {error}", file=sys.stderr)
        return 1
    total = sum(spec["bytes"] for spec in lock["files"].values())
    print(f"Verified {len(lock['files'])} files ({total / 1024**2:.1f} MiB); copied {copied} files.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())

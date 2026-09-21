#!/usr/bin/env python3
"""Fetch pinned Unitree ROS interfaces and SDK source without executing them.

The standard-library-only fetcher verifies archive and individual file hashes.
Run --check for offline validation. Output paths are relative to the G1
project directory, including ros_ws/src/unitree/, assets/, and licenses/.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import sys
import tarfile
import tempfile

from fetch_assets import destination, download, valid


ROOT = Path(__file__).resolve().parents[1]


def check_manifest(manifest: dict) -> None:
    if manifest.get("schema_version") != 1:
        raise ValueError("Unsupported Unitree lock schema")
    archives = manifest["archives"]
    archive_ids = {entry["id"] for entry in archives}
    if len(archives) != len(archive_ids):
        raise ValueError("Duplicate archive ID in Unitree lock")
    paths = [entry["path"] for entry in archives + manifest["files"]]
    if len(paths) != len(set(paths)):
        raise ValueError("Duplicate destination in Unitree lock")
    for entry in manifest["files"]:
        if entry["archive"] not in archive_ids:
            raise ValueError(f"Unknown archive: {entry['archive']}")
        member = PurePosixPath(entry["member"])
        if member.is_absolute() or ".." in member.parts or not member.parts:
            raise ValueError(f"Invalid archive member: {entry['member']!r}")
    for entry in archives + manifest["files"]:
        if not isinstance(entry["size"], int) or entry["size"] < 0:
            raise ValueError(f"Invalid pinned size: {entry['path']}")
        digest = entry["sha256"]
        if len(digest) != 64 or any(c not in "0123456789abcdef" for c in digest):
            raise ValueError(f"Invalid SHA-256: {entry['path']}")


def extract_file(archive: tarfile.TarFile, path: Path, entry: dict) -> None:
    """Write only a manifest-listed regular member, replacing after verification."""
    member = archive.getmember(entry["member"])
    if not member.isfile() or member.size != entry["size"]:
        raise ValueError(f"Archive member type or size mismatch: {entry['member']}")
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = None
    try:
        source = archive.extractfile(member)
        if source is None:
            raise ValueError(f"Unreadable archive member: {entry['member']}")
        with source, tempfile.NamedTemporaryFile(
            mode="wb", prefix=f".{path.name}.", suffix=".partial", dir=path.parent, delete=False
        ) as target:
            temporary = Path(target.name)
            received = 0
            digest = hashlib.sha256()
            for chunk in iter(lambda: source.read(1024 * 1024), b""):
                received += len(chunk)
                if received > entry["size"]:
                    raise ValueError(f"Archive member exceeds pinned size: {entry['member']}")
                digest.update(chunk)
                target.write(chunk)
        if received != entry["size"] or digest.hexdigest() != entry["sha256"]:
            raise ValueError(f"Archive member SHA-256 mismatch: {entry['member']}")
        temporary.chmod(0o644)
        temporary.replace(path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def acquire(manifest: dict, root: Path, *, check: bool) -> int:
    check_manifest(manifest)
    # Check all paths before writing, including pre-existing symlink parents.
    for entry in manifest["archives"] + manifest["files"]:
        destination(root, entry["path"])
    failures = []
    for pinned_archive in manifest["archives"]:
        archive_path = destination(root, pinned_archive["path"])
        if not valid(archive_path, pinned_archive):
            if check:
                failures.append(pinned_archive["path"])
            else:
                download(archive_path, pinned_archive)
                print(f"fetched {pinned_archive['path']}", flush=True)
        entries = [entry for entry in manifest["files"] if entry["archive"] == pinned_archive["id"]]
        pending = [entry for entry in entries if not valid(destination(root, entry["path"]), entry)]
        if check:
            failures.extend(entry["path"] for entry in pending)
        elif pending:
            with tarfile.open(archive_path, mode="r:gz") as archive:
                names = [member.name for member in archive.getmembers()]
                if len(names) != len(set(names)):
                    raise ValueError(f"Duplicate member in archive: {pinned_archive['id']}")
                for entry in pending:
                    extract_file(archive, destination(root, entry["path"]), entry)
            print(f"extracted {len(pending)} verified files from {pinned_archive['id']}", flush=True)
    if failures:
        for path in failures:
            print(f"missing or invalid: {path}", file=sys.stderr)
        return 1
    print(f"Verified {len(manifest['archives'])} archives and {len(manifest['files'])} pinned files in {root.resolve()}")
    return 0


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=ROOT, help="G1 project root for all manifest destinations")
    parser.add_argument("--lock", type=Path, default=ROOT / "unitree.lock.json")
    parser.add_argument("--check", action="store_true", help="Verify existing archives and files without network access")
    args = parser.parse_args()
    return acquire(json.loads(args.lock.read_text()), args.output_dir, check=args.check)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError, tarfile.TarError) as error:
        print(f"Unitree source acquisition failed: {error}", file=sys.stderr)
        sys.exit(1)

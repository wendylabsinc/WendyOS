#!/usr/bin/env python3
"""Fetch the Go2 bundle from immutable URLs and verify every file's SHA-256.

Uses only the Python standard library. Run --check for offline validation.
Existing valid files are retained; downloads replace a file atomically only
after both the expected size and SHA-256 have been verified.
"""

from __future__ import annotations

import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
import sys
import tempfile
import time
from urllib.error import URLError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen


ROOT = Path(__file__).resolve().parents[1]


def sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def valid(path: Path, entry: dict) -> bool:
    return (
        path.is_file()
        and path.stat().st_size == entry["size"]
        and sha256(path) == entry["sha256"]
    )


def destination(root: Path, relative: str) -> Path:
    path = PurePosixPath(relative)
    if path.is_absolute() or not path.parts or any(p in ("..", ".") for p in path.parts):
        raise ValueError(f"Invalid asset path: {relative!r}")
    result = root.joinpath(*path.parts)
    if not result.resolve().is_relative_to(root.resolve()):
        raise ValueError(f"Asset path escapes output directory: {relative!r}")
    return result


def download(path: Path, entry: dict) -> None:
    url = entry["url"]
    if urlsplit(url).scheme != "https":
        raise ValueError(f"Asset URL must use HTTPS: {url}")
    path.parent.mkdir(parents=True, exist_ok=True)
    for attempt in range(3):
        temporary = None
        try:
            request = Request(url, headers={"User-Agent": "wendy-go2-assets/1"})
            with urlopen(request, timeout=60) as source, tempfile.NamedTemporaryFile(
                mode="wb", prefix=f".{path.name}.", suffix=".partial", dir=path.parent, delete=False
            ) as target:
                temporary = Path(target.name)
                received = 0
                for chunk in iter(lambda: source.read(1024 * 1024), b""):
                    received += len(chunk)
                    if received > entry["size"]:
                        raise ValueError(f"Download exceeds pinned size: {entry['path']}")
                    target.write(chunk)
            if not valid(temporary, entry):
                raise ValueError(f"Size or SHA-256 mismatch: {entry['path']}")
            temporary.chmod(0o644)
            temporary.replace(path)
            return
        except (OSError, URLError) as error:
            if attempt == 2:
                raise RuntimeError(f"Cannot download {entry['path']}: {error}") from error
            time.sleep(attempt + 1)
        finally:
            if temporary is not None:
                temporary.unlink(missing_ok=True)


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output-dir", type=Path, default=ROOT / "assets")
    parser.add_argument("--lock", type=Path, default=ROOT / "assets.lock.json")
    parser.add_argument("--check", action="store_true", help="Verify existing files without network access")
    args = parser.parse_args()
    manifest = json.loads(args.lock.read_text())
    if manifest.get("schema_version") != 1:
        raise ValueError("Unsupported asset lock schema")
    entries = manifest["files"]
    paths = [entry["path"] for entry in entries]
    if len(paths) != len(set(paths)):
        raise ValueError("Duplicate destination in asset lock")
    failures = []
    for entry in entries:
        path = destination(args.output_dir, entry["path"])
        if valid(path, entry):
            print(f"verified {entry['path']}")
        elif args.check:
            failures.append(entry["path"])
            print(f"missing or invalid: {entry['path']}", file=sys.stderr)
        else:
            download(path, entry)
            print(f"fetched {entry['path']}", flush=True)
    if failures:
        return 1
    print(f"Verified {len(entries)} pinned assets in {args.output_dir.resolve()}")
    return 0


if __name__ == "__main__":
    try:
        sys.exit(main())
    except (OSError, ValueError, KeyError, RuntimeError) as error:
        print(f"Asset acquisition failed: {error}", file=sys.stderr)
        sys.exit(1)

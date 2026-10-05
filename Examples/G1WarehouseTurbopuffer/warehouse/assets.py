"""Fetch the third-party files listed in assets.lock.json and check their SHA-256.

The Unitree G1 model, the GR00T WBC policies and the three.js viewer modules are not
stored in this repository. Each file is downloaded from an immutable commit URL,
checked against its pinned hash, and written atomically. Files that are already
present and match are left alone.

python -m warehouse.assets          fetch what is missing
python -m warehouse.assets --check  verify only, never download
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
import sys
import tempfile
import time
import urllib.request
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
LOCK = ROOT / "assets.lock.json"


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for block in iter(lambda: handle.read(1 << 20), b""):
            digest.update(block)
    return digest.hexdigest()


def _entries() -> list[dict]:
    files = json.loads(LOCK.read_text())["files"]
    for entry in files:
        target = (ROOT / entry["path"]).resolve()
        if ROOT not in target.parents:
            raise ValueError(f"lock entry escapes the example directory: {entry['path']}")
        # raw files, or Git LFS objects (meshes, policies) served for the same pinned commit
        if not entry["url"].startswith(("https://raw.githubusercontent.com/", "https://media.githubusercontent.com/media/")):
            raise ValueError(f"lock entry is not a pinned GitHub URL: {entry['url']}")
    return files


def _download(url: str, attempts: int = 4) -> bytes:
    for attempt in range(attempts):
        try:
            with urllib.request.urlopen(url, timeout=60) as response:
                return response.read()
        except OSError:
            if attempt == attempts - 1:
                raise
            time.sleep(1.5 * (attempt + 1))
    raise AssertionError("unreachable")


def ensure(check_only: bool = False) -> list[str]:
    """Make every locked file present and verified. Returns the paths fetched."""
    fetched = []
    for entry in _entries():
        target = ROOT / entry["path"]
        if target.is_file() and _sha256(target) == entry["sha256"]:
            continue
        if check_only:
            raise SystemExit(f"missing or modified: {entry['path']} (run python -m warehouse.assets)")
        data = _download(entry["url"])
        if data.startswith(b"version https://git-lfs"):
            raise SystemExit(f"got a Git LFS pointer instead of the file for {entry['path']}")
        actual = hashlib.sha256(data).hexdigest()
        if actual != entry["sha256"]:
            raise SystemExit(f"hash mismatch for {entry['path']}: expected {entry['sha256']}, got {actual}")
        target.parent.mkdir(parents=True, exist_ok=True)
        with tempfile.NamedTemporaryFile(dir=target.parent, delete=False) as handle:
            handle.write(data)
        os.chmod(handle.name, 0o644)
        os.replace(handle.name, target)
        fetched.append(entry["path"])
    return fetched


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    parser.add_argument("--check", action="store_true", help="verify only; fail instead of downloading")
    args = parser.parse_args()
    fetched = ensure(check_only=args.check)
    total = len(_entries())
    print(f"assets: {total} files verified" + (f", {len(fetched)} downloaded" if fetched else ""), file=sys.stderr)


if __name__ == "__main__":
    main()

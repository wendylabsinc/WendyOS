"""Verify the SDK used by the acceptance image without importing its code."""

import hashlib
import json
from pathlib import Path


def main():
    root = Path("/opt/wendy-go2")
    lock = json.loads(Path("/app/unitree.lock.json").read_text())
    entries = [entry for entry in lock["files"] if entry["path"].startswith("assets/sdk2_python/")]
    entries += [entry for entry in lock["archives"] if entry["id"] == "unitree_sdk2_python"]
    if len(entries) != 267:
        raise ValueError("unexpected SDK source manifest")
    for entry in entries:
        data = (root / entry["path"]).read_bytes()
        if len(data) != entry["size"] or hashlib.sha256(data).hexdigest() != entry["sha256"]:
            raise ValueError(f"SDK source differs from pin: {entry['path']}")
    print("Verified 266 SDK source files and pinned SDK archive")


if __name__ == "__main__":
    main()

"""Fetch one pinned, permissively licensed ONNX model. No mutable latest URL."""
import argparse
import hashlib
import json
from pathlib import Path
import tempfile
import urllib.request


METADATA = json.loads(Path(__file__).with_name("model.json").read_text())


def verify(data):
    if len(data) != METADATA["bytes"] or hashlib.sha256(data).hexdigest() != METADATA["sha256"]:
        raise ValueError("model bytes do not match the pinned conversion")


def fetch(destination):
    destination = Path(destination)
    if destination.exists():
        verify(destination.read_bytes())
        return destination
    destination.parent.mkdir(parents=True, exist_ok=True)
    with urllib.request.urlopen(METADATA["url"], timeout=60) as response:
        data = response.read(METADATA["bytes"] + 1)
    verify(data)
    with tempfile.NamedTemporaryFile(dir=destination.parent, delete=False) as tmp:
        tmp.write(data)
        pending = Path(tmp.name)
    try:
        # The command owns this explicit destination; existing mismatches above refuse.
        pending.replace(destination)
    finally:
        pending.unlink(missing_ok=True)
    return destination


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()
    path = fetch(args.output)
    print(json.dumps({"path": str(path), "sha256": METADATA["sha256"], "bytes": METADATA["bytes"]}))

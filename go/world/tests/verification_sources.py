"""Source inventory for World verification within the WendyOS root module."""
import hashlib
from pathlib import Path

WORLD_ROOT = Path(__file__).resolve().parents[1]
MODULE_ROOT = WORLD_ROOT.parents[1]


def source_hashes(extra=()):
    paths = set(extra)
    for directory in (WORLD_ROOT / "production", WORLD_ROOT / "internal",
                      MODULE_ROOT / "go/proto/gen/sensorlinkpb"):
        paths.update(directory.rglob("*.go"))
    paths.update(MODULE_ROOT / name for name in ("go.mod", "go.sum", "go/babel/go.mod"))
    paths.add(Path(__file__).resolve())
    return {p.relative_to(MODULE_ROOT).as_posix(): hashlib.sha256(p.read_bytes()).hexdigest()
            for p in sorted(paths)}

#!/usr/bin/env python3
"""Extract unitree_go and unitree_api from the pinned unitree_ros2 archive and
apply the Go2 simulator's compatibility patches, so this bridge publishes the
exact message types the simulator does. Keep PATCHES in sync with
go/simulator/go2/tools/prepare_unitree.py."""

import hashlib
import sys
import tarfile
from pathlib import Path

COMMIT = "668d1ec5a05d1c38d3306bdca7d59f2ba3581a88"
SHA256 = "3e96da843c611878e28148ce113d978e5464dec922fc06dcd8c340f8232a6883"
PREFIX = f"unitree_ros2-{COMMIT}/cyclonedds_ws/src/unitree/"
PACKAGES = ("unitree_go/", "unitree_api/")
PATCHES = {
    "unitree_go/msg/SportModeState.msg": (
        b"float32[12] foot_speed_body\n",
        b"float32[12] foot_speed_body\n# Wendy SDK compatibility: trajectory feedback is unsupported; all points are zero.\nPathPoint[10] path_point\n",
    ),
    "unitree_api/msg/Response.msg": (b"int8[] binary", b"uint8[] binary"),
}


def main(archive: Path, output: Path) -> None:
    if hashlib.sha256(archive.read_bytes()).hexdigest() != SHA256:
        sys.exit("unitree_ros2 archive hash mismatch")
    with tarfile.open(archive) as tar:
        for member in tar.getmembers():
            if not member.isfile() or not member.name.startswith(PREFIX):
                continue
            relative = member.name[len(PREFIX):]
            if not relative.startswith(PACKAGES) or ".." in relative:
                continue
            data = tar.extractfile(member).read()
            if relative in PATCHES:
                old, new = PATCHES[relative]
                if data.count(old) != 1:
                    sys.exit(f"patch does not match exactly once: {relative}")
                data = data.replace(old, new)
            target = output / relative
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
    for name in PATCHES:
        if not (output / name).is_file():
            sys.exit(f"missing {name}")


if __name__ == "__main__":
    main(Path(sys.argv[1]), Path(sys.argv[2]))

#!/usr/bin/env python3
"""Export only native ROS interfaces from a locally built managed image.

The temporary container is never started. Its prepared files form an ordinary
standalone Docker context, so remote buildx builders need no local image lookup.
"""

import argparse
import json
from pathlib import Path
import shutil
import subprocess
import tempfile


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="wendy-go2-managed:dev")
    args = parser.parse_args()
    root = Path(__file__).resolve().parent
    metadata = json.loads(subprocess.check_output(["docker", "image", "inspect", args.image]))[0]
    image_id = metadata["Id"]
    target = root / "build" / "overlay"
    if target.exists():
        marker = target / "source.json"
        if not marker.is_file() or json.loads(marker.read_text()).get("kind") != "wendy-go2-soak-interfaces":
            raise RuntimeError(f"refusing to replace an unrecognized directory: {target}")
    target.parent.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix=".prepare-", dir=target.parent) as directory:
        prepared = Path(directory) / "overlay"
        prepared.mkdir()
        container = subprocess.check_output([
            "docker", "create", "--network", "none", "--entrypoint", "/bin/true", image_id], text=True).strip()
        try:
            for source, destination in (
                ("ros_ws/install", "install"), ("licenses/unitree_ros2.LICENSE", "UNITREE_ROS_LICENSE"),
                ("wendy-compatibility.json", "wendy-compatibility.json"), ("cyclonedds.xml", "cyclonedds.xml"),
            ):
                subprocess.run(["docker", "cp", f"{container}:/opt/wendy-go2/{source}",
                                str(prepared / destination)], check=True)
        finally:
            subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)
        manifest = {"kind": "wendy-go2-soak-interfaces", "image": args.image,
                    "image_id": image_id, "architecture": metadata["Architecture"]}
        (prepared / "source.json").write_text(json.dumps(manifest, indent=2) + "\n")
        if target.exists():
            shutil.rmtree(target)
        prepared.rename(target)
    print(json.dumps({"prepared": str(target), **manifest}))


if __name__ == "__main__":
    main()

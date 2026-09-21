"""Export SDK/stock-ROS dependencies for a buildx-compatible VM test image.

The temporary source container is never started. No VM or DDS endpoint is
contacted. Exported dependencies remain outside source control.
"""

import argparse
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import tempfile


EXPORTS = (
    "/opt/ros/humble",  # Exact ROS/Cyclone shared libraries used to compile the SDK bindings.
    "/opt/cyclonedds-sdk",
    "/app/stock_ros/install",
    "/app/unitree.lock.json",
    "/opt/wendy-go2/assets/sdk2_python",
    "/opt/wendy-go2/assets/archives",
    "/usr/local/lib/python3.10/dist-packages/cyclonedds",
    "/usr/local/lib/python3.10/dist-packages/cyclonedds-0.10.2.dist-info",
    "/usr/local/lib/python3.10/dist-packages/typing_extensions.py",
)


def run(*args):
    return subprocess.check_output(args, text=True).strip()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="wendy-go2-native-test:dev")
    parser.add_argument("--output", type=Path, default=Path(__file__).parent / ".vm-deps")
    args = parser.parse_args()
    output = args.output.resolve()
    if output.exists():
        raise ValueError(f"dependency output already exists; preserve or remove it explicitly before re-exporting: {output}")
    details = json.loads(run("docker", "image", "inspect", args.image))[0]
    if details["Os"] != "linux" or details["Architecture"] != "arm64":
        raise ValueError("the current VM acceptance dependency bundle requires a Linux ARM64 native-test image")
    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = Path(tempfile.mkdtemp(prefix=".vm-deps.", suffix=".tmp", dir=output.parent))
    container = None
    try:
        container = run("docker", "create", "--network", "none", "--entrypoint", "/bin/true", details["Id"])
        root = temporary / "root"
        for path in EXPORTS:
            target = root / path.lstrip("/")
            target.parent.mkdir(parents=True, exist_ok=True)
            subprocess.run(["docker", "cp", container + ":" + path, str(target)], check=True)
        files = {}
        for path in sorted(root.rglob("*")):
            relative = str(path.relative_to(root))
            if path.is_symlink():
                files[relative] = {"symlink": str(path.readlink())}
            elif path.is_file():
                files[relative] = {"size": path.stat().st_size, "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}
        manifest = {"source_image": details["Id"], "architecture": "arm64", "files": files,
                    "simulator_runtime_exported": False, "derived_ros_overlay_exported": False}
        (temporary / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        temporary.rename(output)
        print(json.dumps({"prepared": str(output), "source_image": details["Id"],
                          "files": len(files), "bytes": sum(item.get("size", 0) for item in files.values())}))
    finally:
        if container is not None:
            subprocess.run(["docker", "rm", container], check=True, stdout=subprocess.DEVNULL)
        if temporary.exists():
            shutil.rmtree(temporary)


if __name__ == "__main__":
    main()

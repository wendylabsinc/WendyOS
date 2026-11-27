"""Verify the Swift client against a live site, reader loss and reader recovery."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile

from native_client_site import NativeSite
from virtual_site import ROOT, command


def run(node_image, scene_image):
    command(["swift", "build", "--package-path", "../macos/WorldClient", "--product", "WorldClientProbe"])
    with tempfile.TemporaryDirectory(prefix="wendy-native-probe-") as parent:
        site = NativeSite(Path(parent) / "site", node_image, scene_image)
        try:
            info = site.setup()
            environment = os.environ.copy()
            environment["WORLD_PROFILE_PATH"] = info["profiles"]["apple"]
            environment["WORLD_CACHE_DIR"] = str(site.folder / "swift-cache")

            def probe(mode):
                environment["WORLD_PROBE_MODE"] = mode
                result = subprocess.run([str(ROOT / "../macos/WorldClient/.build/debug/WorldClientProbe")],
                    cwd=ROOT, env=environment, capture_output=True, timeout=40)
                if result.returncode:
                    raise RuntimeError(result.stderr.decode()[-1000:])
                return json.loads(result.stdout)

            online = site.until(lambda: probe("online"), lambda value: value["sensorLineageMatched"])
            reader = f"native-node-3-{site.suffix}"
            port_before = command(["docker", "port", reader, "8080/tcp"])
            command(["docker", "stop", reader])
            offline = probe("offline")
            assert offline["lastKnown"] and offline["networkRequests"] == 0
            command(["docker", "start", reader])
            assert command(["docker", "port", reader, "8080/tcp"]) == port_before
            recovered = site.until(lambda: probe("online"), lambda value: value["sensorLineageMatched"])
            return {"mode": "native Swift client against three Linux World nodes and live MuJoCo",
                "run_id": site.run_id, "node_image": info["node_image"], "scene_image": info["scene_image"],
                "online": online, "offline": offline, "recovered": recovered,
                "reader_endpoint_preserved_after_restart": True,
                "limits": ["synthetic simulation HTTP", "SDK integration; app runtime is recorded separately",
                           "no physical robot or performance acceptance"]}
        finally:
            site.cleanup()


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--scene-image", default="wendy-world-live:local")
    args = parser.parse_args()
    print(json.dumps(run(args.node_image, args.scene_image), indent=2))

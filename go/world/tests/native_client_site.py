"""Serve a bounded, private virtual site for real native-client verification."""
import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import time
import uuid

from virtual_site import command


class NativeSite:
    def __init__(self, folder, node_image, scene_image):
        self.folder = folder.resolve()
        self.node_image, self.scene_image = node_image, scene_image
        self.suffix = uuid.uuid4().hex[:10]
        self.network = "native-world-" + self.suffix
        self.created, self.made_network = [], False
        self.user = f"{os.getuid()}:{os.getgid()}"
        self.run_id = "native-client-" + self.suffix

    def write(self, name, value):
        path = self.folder / name
        path.write_text(json.dumps(value, indent=2) + "\n")
        path.chmod(0o600)
        return path

    def until(self, fn, predicate, timeout=35):
        end, last = time.monotonic() + timeout, None
        while time.monotonic() < end:
            try:
                last = fn()
                if predicate(last):
                    return last
            except (RuntimeError, OSError, KeyError) as error:
                last = str(error)
            time.sleep(.15)
        raise AssertionError(str(last)[:1500])

    def start(self, role, binary, args, alias=None, publish=False):
        name = f"native-{role}-{self.suffix}"
        options = ["docker", "run", "-d", "--name", name, "--network", self.network,
                   "--user", self.user, "--label", "wendy.world.native-test=" + self.suffix]
        if alias:
            options += ["--network-alias", alias]
        if publish:
            # Pin the chosen port into HostConfig. Docker's empty host port can
            # change after stop/start, which would invalidate imported profiles.
            with socket.socket() as listener:
                listener.bind(("127.0.0.1", 0))
                port = listener.getsockname()[1]
            options += ["-p", f"127.0.0.1:{port}:8080"]
        options += ["--mount", f"type=bind,src={self.folder},dst=/lab"]
        if binary:
            options += ["--entrypoint", "/" + binary]
        image = self.scene_image if role == "scene" else self.node_image
        self.created.append(name)
        command([*options, image, *args])
        return name

    def cli(self, binary, *args):
        return json.loads(command(["docker", "run", "--rm", "--network", self.network,
            "--user", self.user, "--entrypoint", "/" + binary,
            "--mount", f"type=bind,src={self.folder},dst=/lab,readonly", self.node_image, *args]))

    def setup(self):
        self.folder.mkdir(mode=0o700, parents=True, exist_ok=False)
        command(["go", "run", "./cmd/world-lab", "--out", str(self.folder), "--runtime-dir", "/lab",
                 "--containers", "--missions", "--sensors", "--task-relay", "--run-id", self.run_id])
        # A host-reachable bridge lets existing native simulators connect to the
        # loopback-only published ports. Partition isolation has separate tests.
        self.made_network = True
        command(["docker", "network", "create", "--label", "wendy.world.native-test=" + self.suffix, self.network])
        nodes, ports = [], []
        for i in range(1, 4):
            node = self.start(f"node-{i}", "worldd", ["--config", f"/lab/node-{i}.json"], f"world-node-{i}", True)
            nodes.append(node)
            mapped = command(["docker", "port", node, "8080/tcp"]).decode().strip()
            assert mapped.startswith("127.0.0.1:"), mapped
            ports.append(int(mapped.split(":")[-1]))
        self.until(lambda: self.cli("world-query", "--credentials", "/lab/client-3.json"), lambda v: not v["components"])
        self.start("scene", None, ["examples/live-office/world.json", "--token-file", "/lab/adapter.token",
            "--sensor-token-file", "/lab/sensor.token", "--run-id", self.run_id, "--host", "0.0.0.0",
            "--allow-network-simulation", "--allow-test-faults"], "scene")
        for sensor in ("camera-a", "camera-b", "gas-1", "audio-1"):
            self.start("sensor-" + sensor, "world-live-sense", ["--credentials", f"/lab/sensor-{sensor}.json",
                "--adapter", "http://scene:8091", "--sensor-token-file", "/lab/sensor.token", "--sensor", sensor,
                "--run-id", self.run_id, "--ttl", "5s"])
        self.start("fusion", "world-fuse", ["--config", "/lab/fusion.json", "--run-id", self.run_id])
        snapshot = self.until(lambda: self.cli("world-query", "--credentials", "/lab/client-3.json"),
            lambda v: any(c["key"]["type"] == "wendy.hazard.v1" for c in v["components"]))
        # The executor verifies a startup stop; start it only after the live
        # scene is serving samples, so a startup connection race cannot exit it.
        self.start("executor", "world-executor", ["--credentials", "/lab/executor-1.json", "--adapter", "http://scene:8091",
            "--adapter-token-file", "/lab/adapter.token", "--run-id", self.run_id])
        sample = next(c["payload"] for c in snapshot["components"] if c["key"]["type"] == "wendy.sensor.v1")
        document = json.loads(Path("examples/geography/office.json").read_text())
        document["package_sha256"] = sample["package_sha256"]
        start, end = datetime.now(timezone.utc) - timedelta(minutes=1), datetime.now(timezone.utc) + timedelta(hours=2)
        for value in [document, *document["frames"]]:
            value["valid_from"], value["valid_until"] = start.isoformat(), end.isoformat()
        self.write("site.json", document)
        self.cli("world", "site", "publish", "--credentials", "/lab/site-admin-1.json", "--site", "/lab/site.json",
                 "--run-id", self.run_id, "--ttl", "1h", "--json")
        spec = {"id": "native-inspection", "type": "wendy.nav.inspect.v1", "site": "synthetic-office",
            "assignee": "sim-robot/navigation", "domain": "simulation", "class": "MOVE", "mode": "autonomous",
            "frame": {"id": "office-floor", "revision": "office-frame-v1"},
            "not_after_unix_ms": int(time.time()*1000)+120000,
            "area": {"min": [-2.1, .1, -.1], "max": [2.1, .3, .1]}, "required_sensors": ["gas", "local-obstacles"],
            "minimum_energy": .2, "fallback": "hold", "target": [-.6, .2, 0], "speed_mps": .4,
            "tolerance_m": .03, "stop_speed_mps": .03}
        self.write("mission.json", spec)
        self.cli("world", "task", "send", "--credentials", "/lab/operator-3.json", "--destination", "node-1",
                 "--spec", "/lab/mission.json", "--json")
        tasks = self.until(lambda: self.cli("world", "task", "list", "--credentials", "/lab/operator-3.json", "--relayed", "--json"),
            lambda v: any(e.get("status", {}).get("record", {}).get("status", {}).get("Phase") == "succeeded" for e in v["entries"]))
        outcome = next(e["status"]["record"] for e in tasks["entries"] if e.get("status", {}).get("task_id") == "native-inspection")
        profiles = {}
        contributor_profiles = {}
        for name, host in [("apple", "127.0.0.1"), ("android", "10.0.2.2")]:
            endpoints = []
            for i in range(1, 4):
                credential = json.loads((self.folder / ("viewer-2.json" if i == 2 else "viewer-1.json")).read_text())
                credential["url"], credential["audience"] = f"http://{host}:{ports[i-1]}", f"node-{i}"
                endpoints.append(credential)
            profiles[name] = str(self.write(name + "-profile.json", {"credentials": endpoints[2], "evidence_sources": endpoints[:2]}))
            # A publisher profile belongs to one installation. Sharing its key
            # across platforms would let epoch allocation fence pending notes.
            platforms = ("apple", "macos", "ios", "visionos") if name == "apple" else ("android",)
            for platform in platforms:
                contributor = json.loads((self.folder / ("contributor-" + platform + ".json")).read_text())
                contributor["url"], contributor["audience"] = f"http://{host}:{ports[2]}", "node-3"
                contributor_profiles[platform] = str(self.write(platform + "-contributor-profile.json", {
                    "credentials": contributor, "evidence_sources": endpoints[:2],
                    "annotation_context": {"run_id": self.run_id,
                        "frame": {"id": "office-floor", "revision": "office-frame-v1"}}}))
        self.write("expected-result.json", outcome)
        expiry = min(json.loads((self.folder / name).read_text())["grant"]["grant"]["not_after_unix_ms"]
                     for name in ("viewer-1.json", "viewer-2.json", "client-3.json"))
        self.available_until = expiry / 1000 - 30
        info = {"schema": "wendy.native-test.site.v1", "mode": "live synthetic MuJoCo site on existing Docker Linux VM",
            "run_id": self.run_id, "profiles": profiles, "contributor_profiles": contributor_profiles,
            "node_ports": ports, "world_nodes": 3,
            "inspection_samples": len(outcome["result"]["samples"]), "mission_id": "native-inspection",
            "reader_credentials_expire_unix_ms": expiry,
            "maximum_remaining_seconds": max(0, int(self.available_until - time.time())),
            "node_image": command(["docker", "image", "inspect", self.node_image, "--format", "{{.Id}}"]).decode().strip(),
            "scene_image": command(["docker", "image", "inspect", self.scene_image, "--format", "{{.Id}}"]).decode().strip(),
            "limits": ["synthetic cameras, gas, audio and illustrative geographic placement", "reader and annotation-only profiles; no motion authorization",
                       "HTTP simulation transport; no independent physical trust or safety claim"]}
        self.write("ready.json", info)
        return info

    def cleanup(self):
        for name in reversed(self.created):
            label = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "wendy.world.native-test"}}', name], capture_output=True, text=True)
            if label.returncode == 0 and label.stdout.strip() == self.suffix:
                subprocess.run(["docker", "rm", "-f", name], capture_output=True)
        if self.made_network:
            label = subprocess.run(["docker", "network", "inspect", "--format", '{{index .Labels "wendy.world.native-test"}}', self.network], capture_output=True, text=True)
            if label.returncode == 0 and label.stdout.strip() == self.suffix:
                subprocess.run(["docker", "network", "rm", self.network], capture_output=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--duration", type=int, default=1800)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--scene-image", default="wendy-world-live:local")
    args = parser.parse_args()
    if not 30 <= args.duration <= 3600:
        parser.error("duration must be 30..3600 seconds")
    site = NativeSite(args.out, args.node_image, args.scene_image)
    def terminate(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, terminate)
    try:
        print(json.dumps(site.setup()), flush=True)
        deadline = time.monotonic() + min(args.duration, max(0, site.available_until - time.time()))
        while time.monotonic() < deadline and not (site.folder / "stop").exists():
            time.sleep(.5)
    except KeyboardInterrupt:
        pass
    finally:
        site.cleanup()

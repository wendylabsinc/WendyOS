"""Serve a private MuJoCo site for explicit mission actions in native clients.

Uses the existing Docker Linux VM. Each invocation owns two labeled networks;
partitioning the peer network leaves the robot gateway, executor, local sensors
and native control connection together. No mission is submitted by setup.
"""
import argparse
from datetime import datetime, timedelta, timezone
import json
import os
from pathlib import Path
import signal
import subprocess
import time
import urllib.request

from native_client_site import NativeSite
from virtual_site import ROOT, command


def selected_package(package=None):
    """Choose an existing example manifest shared by host commissioning and image."""
    path = Path(package) if package is not None else ROOT / "examples/live-office/world.json"
    if not path.is_absolute():
        path = ROOT / path
    path = path.resolve()
    if not path.is_relative_to(ROOT / "examples") or not path.is_file() or path.suffix != ".json":
        raise ValueError("select an existing JSON world package inside repository examples")
    return path, path.relative_to(ROOT).as_posix()


class NativeMissionSite(NativeSite):
    def __init__(self, folder, node_image, scene_image, package=None):
        self.package_path, self.scene_package = selected_package(package)
        # Resolve mutable tags once, before creating any runtime resource.
        # A concurrent image rebuild must not mix replicas or mislabel evidence.
        node_id = command(["docker", "image", "inspect", node_image, "--format", "{{.Id}}"]).decode().strip()
        scene_id = command(["docker", "image", "inspect", scene_image, "--format", "{{.Id}}"]).decode().strip()
        super().__init__(folder, node_id, scene_id)
        self.run_id = "native-missions-" + self.suffix
        self.control_network = "native-control-" + self.suffix
        self.made_control = False

    def start(self, role, binary, args, alias=None, publish=False):
        peers = self.network
        local = role in ("node-1", "scene", "executor", "sensor-camera-a", "sensor-gas-1", "sensor-audio-1")
        if local:
            self.network = self.control_network
        try:
            return super().start(role, binary, args, alias, publish)
        finally:
            self.network = peers

    def setup(self):
        self.folder.mkdir(mode=0o700, parents=True, exist_ok=False)
        command(["go", "run", "./cmd/world-lab", "--out", str(self.folder), "--runtime-dir", "/lab",
                 "--containers", "--missions", "--sensors", "--task-relay", "--proposals",
                 "--native-mission-package", str(self.package_path), "--run-id", self.run_id])
        self.made_network = True
        command(["docker", "network", "create", "--label", "wendy.world.native-test=" + self.suffix, self.network])
        self.made_control = True
        command(["docker", "network", "create", "--label", "wendy.world.native-test=" + self.suffix, self.control_network])
        nodes, ports = [], []
        for i in range(1, 4):
            node = self.start(f"node-{i}", "worldd", ["--config", f"/lab/node-{i}.json"], f"world-node-{i}", True)
            if i == 1:
                command(["docker", "network", "connect", "--alias", "world-node-1", self.network, node])
            nodes.append(node)
            mapped = command(["docker", "port", node, "8080/tcp"]).decode().strip()
            assert mapped.startswith("127.0.0.1:"), mapped
            ports.append(int(mapped.split(":")[-1]))
        for i in range(1,4):
            self.until(lambda: self.cli("world-query", "--credentials", f"/lab/client-{i}.json"), lambda v: not v["components"])
        scene = self.start("scene", None, [self.scene_package, "--token-file", "/lab/adapter.token",
            "--sensor-token-file", "/lab/sensor.token", "--run-id", self.run_id, "--host", "0.0.0.0", "--port", "8080",
            "--allow-network-simulation", "--allow-test-faults"], "scene", True)
        command(["docker", "network", "connect", "--alias", "scene", self.network, scene])
        scene_port = int(command(["docker", "port", scene, "8080/tcp"]).decode().strip().split(":")[-1])
        def scene_samples():
            request = urllib.request.Request(f"http://127.0.0.1:{scene_port}/v1/samples",
                headers={"Authorization": "Bearer " + (self.folder / "sensor.token").read_text().strip()})
            with urllib.request.urlopen(request, timeout=2) as response:
                return json.load(response)
        self.until(scene_samples, lambda value: len(value["observations"]) == 4)
        for sensor in ("camera-a", "camera-b", "gas-1", "audio-1"):
            self.start("sensor-" + sensor, "world-live-sense", ["--credentials", f"/lab/sensor-{sensor}.json",
                "--adapter", "http://scene:8080", "--sensor-token-file", "/lab/sensor.token", "--sensor", sensor,
                "--run-id", self.run_id, "--ttl", "5s"])
        self.until(lambda: self.cli("world-query", "--credentials", "/lab/client-3.json"),
            lambda v: len([c for c in v["components"] if c["key"]["type"] == "wendy.sensor.v1"]) == 4)
        self.start("fusion", "world-fuse", ["--config", "/lab/fusion.json", "--run-id", self.run_id])
        snapshot = self.until(lambda: self.cli("world-query", "--credentials", "/lab/client-3.json"),
            lambda v: any(c["key"]["type"] == "wendy.hazard.v1" for c in v["components"]))
        self.start("executor", "world-executor", ["--credentials", "/lab/executor-1.json", "--adapter", "http://scene:8080",
            "--adapter-token-file", "/lab/adapter.token", "--run-id", self.run_id])
        sample = next(c["payload"] for c in snapshot["components"] if c["key"]["type"] == "wendy.sensor.v1")
        context = json.loads((self.folder / "mission-context.json").read_text())
        assert sample["package_sha256"] == context["context"]["package_sha256"]
        document = json.loads(Path("examples/geography/office.json").read_text())
        document["package_sha256"] = sample["package_sha256"]
        start, end = datetime.now(timezone.utc) - timedelta(minutes=1), datetime.now(timezone.utc) + timedelta(hours=2)
        for value in [document, *document["frames"]]:
            value["valid_from"], value["valid_until"] = start.isoformat(), end.isoformat()
        self.write("site.json", document)
        self.cli("world", "site", "publish", "--credentials", "/lab/site-admin-1.json", "--site", "/lab/site.json",
                 "--run-id", self.run_id, "--ttl", "1h", "--json")
        profiles, expired_profiles, proposals = {}, {}, {}
        pins = json.loads((self.folder / "mission-clock-trust.json").read_text())
        for platform in ("macos", "ios", "visionos", "android"):
            host = "10.0.2.2" if platform == "android" else "127.0.0.1"
            sources = []
            for i in range(2):
                credentials = json.loads((self.folder / f"viewer-{i+1}.json").read_text())
                credentials["url"], credentials["audience"] = f"http://{host}:{ports[i]}", f"node-{i+1}"
                sources.append(credentials)
            reader = json.loads((self.folder / ("contributor-" + platform + ".json")).read_text())
            reader["url"] = f"http://{host}:{ports[2]}"
            for expired in (False, True):
                prefix = "mission-expired-" if expired else "mission-"
                credentials = json.loads((self.folder / (prefix + platform + ".json")).read_text())
                credentials["url"] = f"http://{host}:{ports[0]}"
                profile = {"credentials": reader, "evidence_sources": sources,
                    "annotation_context": {"run_id": self.run_id, "frame": context["context"]["frame"]},
                    "mission_credentials": credentials, "mission_context": context, "mission_clock_trust": pins}
                path = self.write(platform + ("-expired" if expired else "") + "-mission-profile.json", profile)
                (expired_profiles if expired else profiles)[platform] = str(path)
            proposal = {"version": 1, "id": "proposal-" + platform, "tenant": "lab", "site": "synthetic-office",
                "run_id": self.run_id, "assignee": "sim-robot/navigation", "type": "wendy.nav.inspect.v1",
                "parameters": {"target": [1.8, .2, 0]}, "summary": "Inspect the simulated office target. This is an unapproved agent suggestion.",
                "not_after_unix_ms": context["context"]["not_after_unix_ms"] - 30000}
            self.write("proposal-" + platform + ".json", proposal)
            result = self.cli("world", "task", "propose", "--credentials", "/lab/agent-1.json",
                "--proposal", "/lab/proposal-" + platform + ".json", "--json")
            self.write("proposal-" + platform + "-receipt.json", result)
            proposals[platform] = {"id": proposal["id"], "body_sha256": result["record"]["body_sha256"]}
        # No task has been sent or approved by fixture setup.
        tasks = self.cli("world", "task", "list", "--credentials", "/lab/operator-1.json", "--json")
        assert tasks.get("tasks", tasks.get("records", [])) == [], tasks
        expiry = context["context"]["not_after_unix_ms"]
        self.available_until = expiry / 1000 - 30
        info = {"schema": "wendy.native-test.site.v1", "mission_schema": "wendy.native-mission.site.v1",
            "mode": "live synthetic MuJoCo 4 site on the existing Docker Linux VM", "run_id": self.run_id,
            "native_mission_profiles": profiles, "native_mission_expired_profiles": expired_profiles,
            "proposals": proposals, "node_ports": ports, "scene_port": scene_port, "world_nodes": 3,
            "package_sha256": sample["package_sha256"], "world_package": self.scene_package,
            "reader_credentials_expire_unix_ms": expiry,
            "maximum_remaining_seconds": max(0, int(self.available_until-time.time())),
            "network": self.network, "control_network": self.control_network, "node_names": nodes, "scene_name": scene,
            "ownership_label": self.suffix,
            "node_image": command(["docker", "image", "inspect", self.node_image, "--format", "{{.Id}}"]).decode().strip(),
            "scene_image": command(["docker", "image", "inspect", self.scene_image, "--format", "{{.Id}}"]).decode().strip(),
            "limits": ["synthetic one-axis robot, cameras, gas and audio; illustrative geographic placement",
                "native explicit operator actions required; setup submits proposals only",
                "HTTP simulation authority; no production enrollment, mobile supervision or physical safety claim"]}
        self.write("ready.json", info)
        return info

    def cleanup(self):
        # Preserve bounded diagnostics before deleting only our owned containers.
        for name in self.created:
            label = subprocess.run(["docker", "inspect", "--format", '{{index .Config.Labels "wendy.world.native-test"}}', name], capture_output=True, text=True)
            if label.returncode == 0 and label.stdout.strip() == self.suffix:
                log = subprocess.run(["docker", "logs", "--tail", "100", name], capture_output=True, timeout=10)
                path = self.folder / (name + ".log")
                path.write_bytes((log.stdout + log.stderr)[-262144:]); path.chmod(0o600)
        super().cleanup()
        if self.made_control:
            label = subprocess.run(["docker", "network", "inspect", "--format", '{{index .Labels "wendy.world.native-test"}}', self.control_network], capture_output=True, text=True)
            if label.returncode == 0 and label.stdout.strip() == self.suffix:
                subprocess.run(["docker", "network", "rm", self.control_network], capture_output=True)


def fixture_action(path, action):
    ready = json.loads(path.read_text())
    if ready.get("mission_schema") != "wendy.native-mission.site.v1":
        raise ValueError("explicit native mission fixture required")
    network, node = ready["network"], ready["node_names"][0]
    for kind, name, template in (("network", network, '{{index .Labels "wendy.world.native-test"}}'),
            ("container", node, '{{index .Config.Labels "wendy.world.native-test"}}'),
            ("container", ready["scene_name"], '{{index .Config.Labels "wendy.world.native-test"}}')):
        label = command(["docker", kind, "inspect", "--format", template, name]).decode().strip()
        if label != ready["ownership_label"]:
            raise ValueError("fixture ownership mismatch")
    scene = json.loads(command(["docker", "inspect", ready["scene_name"]]))[0]
    mapped = scene["NetworkSettings"]["Ports"].get("8080/tcp")
    if (mapped != [{"HostIp":"127.0.0.1", "HostPort":str(ready["scene_port"])}]
            or scene["Image"] != ready["scene_image"] or not scene["State"]["Running"]):
        raise ValueError("scene endpoint or image no longer belongs to the selected fixture")
    connected = network in json.loads(command(["docker", "inspect", node]))[0]["NetworkSettings"]["Networks"]
    if action == "partition" and connected:
        command(["docker", "network", "disconnect", network, node])
    elif action == "rejoin" and not connected:
        command(["docker", "network", "connect", "--alias", "world-node-1", network, node])
    request = urllib.request.Request(f'http://127.0.0.1:{ready["scene_port"]}/v1/state',
        headers={"Authorization": "Bearer " + (path.parent / "adapter.token").read_text().strip()})
    with urllib.request.urlopen(request, timeout=2) as response:
        state = json.load(response)
    if state["run_id"] != ready["run_id"] or state["provenance"]["package_sha256"] != ready["package_sha256"]:
        raise ValueError("scene differs from native fixture")
    return {"action": action, "run_id": ready["run_id"], "state": state, "observed_unix_ms": int(time.time()*1000)}


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path)
    parser.add_argument("--duration", type=int, default=1800)
    parser.add_argument("--node-image", default="wendy-world-node:native-missions")
    parser.add_argument("--scene-image", default="wendy-world-live:native-missions")
    parser.add_argument("--package", type=Path, help="example world manifest included at the same path in the scene image")
    parser.add_argument("--ready", type=Path)
    parser.add_argument("--action", choices=("partition", "rejoin", "state"))
    args = parser.parse_args()
    if args.action:
        if not args.ready: parser.error("--ready is required for fixture actions")
        print(json.dumps(fixture_action(args.ready, args.action)), flush=True)
    else:
        if not args.out or not 30 <= args.duration <= 3600:
            parser.error("--out and a duration of 30..3600 seconds are required")
        site = NativeMissionSite(args.out, args.node_image, args.scene_image, args.package)
        def terminate(signum, frame): raise KeyboardInterrupt
        signal.signal(signal.SIGTERM, terminate)
        try:
            print(json.dumps(site.setup()), flush=True)
            deadline = time.monotonic() + min(args.duration, max(0, site.available_until-time.time()))
            while time.monotonic() < deadline and not (site.folder / "stop").exists():
                time.sleep(.5)
        except KeyboardInterrupt:
            pass
        finally:
            site.cleanup()

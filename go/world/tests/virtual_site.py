"""Exercise signed MuJoCo observations across isolated durable Linux nodes.

Uses the existing Docker environment, with per-node private mounts. Creates only
temporary local networks/containers and removes them in finally. This verifies
the observation runtime, not yet fusion or actuator mission execution.
"""

import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]


def command(args, *, data=None):
    result = subprocess.run(args, input=data, stdout=subprocess.PIPE, stderr=subprocess.PIPE, cwd=ROOT, timeout=90)
    if result.returncode:
        raise RuntimeError(f"command failed: {' '.join(args[:5])}\n{result.stderr.decode()}")
    return result.stdout


def run(node_image, sim_image):
    suffix = uuid.uuid4().hex[:10]
    peer = f"world-peer-{suffix}"
    control = f"world-control-{suffix}"
    names = [f"world-node-{i}-{suffix}" for i in range(1, 4)]
    networks = []
    created = []
    user = f"{os.getuid()}:{os.getgid()}"
    with tempfile.TemporaryDirectory(prefix="wendy-world-site-") as temporary:
        folder = Path(temporary)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state", "--containers", "--run-id", "network-test"])
            for network in (peer, control):
                command(["docker", "network", "create", "--internal", network])
                networks.append(network)
            for i, name in enumerate(names, 1):
                state = folder / f"state-{i}"
                state.mkdir(mode=0o700)
                network = control if i == 1 else peer
                command(["docker", "run", "-d", "--name", name, "--network", network,
                         "--network-alias", f"world-node-{i}", "--user", user,
                         "--mount", f"type=bind,src={folder / f'node-{i}.json'},dst=/etc/world/config.json,readonly",
                         "--mount", f"type=bind,src={state},dst=/state",
                         node_image, "--config", "/etc/world/config.json"])
                created.append(name)
            command(["docker", "network", "connect", "--alias", "world-node-1", peer, names[0]])

            def query(i):
                return json.loads(command(["docker", "run", "--rm", "--network", control if i == 1 else peer,
                                          "--user", user, "--entrypoint", "/world-query",
                                          "--mount", f"type=bind,src={folder / f'client-{i}.json'},dst=/client.json,readonly",
                                          node_image, "--credentials", "/client.json"]))

            def wait_for(i, predicate, timeout=15):
                deadline = time.monotonic() + timeout
                last = None
                while time.monotonic() < deadline:
                    try:
                        last = query(i)
                        if predicate(last):
                            return last
                    except RuntimeError as error:
                        last = str(error)
                    time.sleep(0.2)
                raise AssertionError(f"node {i} did not reach expected state: {last}")

            def ingest(steps):
                samples = command(["docker", "run", "--rm", "--network", "none", sim_image,
                                   "examples/office/world.json", "--steps", str(steps), "--run-id", "network-test"])
                result = command(["docker", "run", "--rm", "-i", "--network", control, "--user", user,
                                  "--entrypoint", "/world-ingest", "--mount",
                                  f"type=bind,src={folder / 'client-1.json'},dst=/client.json,readonly",
                                  node_image, "--credentials", "/client.json", "--run-id", "network-test", "--ttl", "30s"], data=samples)
                return json.loads(result)

            def epoch(snapshot, value):
                records = snapshot["components"]
                return len(records) == 2 and all(c["version"]["epoch"] == value for c in records)

            for i in range(1, 4):
                wait_for(i, lambda snapshot: not snapshot["components"])
            first = ingest(100)
            assert first["published"] == 20 and first["epoch"] == 1, first
            for i in range(1, 4):
                wait_for(i, lambda snapshot: epoch(snapshot, 1))

            command(["docker", "network", "disconnect", peer, names[0]])
            second = ingest(200)
            assert second["published"] == 40 and second["epoch"] == 2, second
            assert epoch(query(1), 2)
            assert epoch(query(2), 1), "partition did not isolate the publisher"
            assert epoch(query(3), 1), "partition did not isolate the publisher"

            command(["docker", "kill", "--signal", "KILL", names[0]])
            command(["docker", "start", names[0]])
            wait_for(1, lambda snapshot: epoch(snapshot, 2))
            assert epoch(query(2), 1)
            started = time.monotonic()
            command(["docker", "network", "connect", "--alias", "world-node-1", peer, names[0]])
            for i in (2, 3):
                snapshot = wait_for(i, lambda value: epoch(value, 2))
                for component in snapshot["components"]:
                    assert component["provenance"]["origin"] == "simulated"
                    assert component["key"]["source"]["device"] == "sim-publisher"
                    assert component["payload"]["provenance"]["package_sha256"]
            return {"mode": "Linux containers on existing Docker environment", "nodes": 3,
                    "mujoco_observations_published": first["published"] + second["published"],
                    "partition_verified": True, "sigkill_recovery_verified": True,
                    "reconciliation_seconds_including_client_startup": round(time.monotonic() - started, 3),
                    "limits": ["mission execution is verified in the separate mission_site test", "no multimodal fusion yet", "no physical hardware"]}
        except Exception:
            for name in created:
                result = subprocess.run(["docker", "logs", "--tail", "15", name], capture_output=True)
                print(f"{name}: {result.stderr.decode()}", file=__import__("sys").stderr)
            raise
        finally:
            for name in reversed(created):
                subprocess.run(["docker", "rm", "-f", name], capture_output=True)
            for network in reversed(networks):
                subprocess.run(["docker", "network", "rm", network], capture_output=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--sim-image", default="wendy-world-sim:local")
    args = parser.parse_args()
    print(json.dumps(run(args.node_image, args.sim_image), indent=2))

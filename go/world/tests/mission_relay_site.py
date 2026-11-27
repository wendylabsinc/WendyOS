"""Verify durable mission delivery and assignee status across two real hops."""
import argparse
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid

from virtual_site import command


def run(node_image, scene_image):
    suffix = uuid.uuid4().hex[:10]
    west, east, control = [f"relay-{part}-{suffix}" for part in ("west", "east", "control")]
    nodes = [f"relay-node-{i}-{suffix}" for i in range(1, 4)]
    scene, executor = [f"relay-{part}-{suffix}" for part in ("scene", "executor")]
    created, networks = [], []
    user = f"{os.getuid()}:{os.getgid()}"
    with tempfile.TemporaryDirectory(prefix="wendy-mission-relay-") as temporary:
        folder = Path(temporary)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state",
                     "--containers", "--missions", "--sensors", "--task-relay", "--run-id", "relay-test"])
            # The ingress and assignee share neither a Docker network nor a peer
            # configuration. Node 2 is the only connection between them.
            for i in range(1, 4):
                path = folder / f"node-{i}.json"
                config = json.loads(path.read_text())
                neighbors = {1: {"node-2"}, 2: {"node-1", "node-3"}, 3: {"node-2"}}[i]
                config["peers"] = [peer for peer in config["peers"] if peer["audience"] in neighbors]
                path.write_text(json.dumps(config))
            for network in (west, east, control):
                command(["docker", "network", "create", "--internal", network])
                networks.append(network)
            for i, name in enumerate(nodes, 1):
                state = folder / f"state-{i}"
                state.mkdir(mode=0o700)
                command(["docker", "run", "-d", "--name", name, "--network", control if i == 1 else west,
                         "--network-alias", f"world-node-{i}", "--user", user,
                         "--mount", f"type=bind,src={folder / f'node-{i}.json'},dst=/config.json,readonly",
                         "--mount", f"type=bind,src={state},dst=/state", node_image, "--config", "/config.json"])
                created.append(name)
            command(["docker", "network", "connect", "--alias", "world-node-1", east, nodes[0]])
            command(["docker", "network", "connect", "--alias", "world-node-2", east, nodes[1]])
            command(["docker", "run", "-d", "--name", scene, "--network", control, "--network-alias", "scene",
                     "--user", user, "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly",
                     "--mount", f"type=bind,src={folder / 'sensor.token'},dst=/sensor-token,readonly", scene_image,
                     "examples/live-office/world.json", "--token-file", "/token", "--sensor-token-file", "/sensor-token",
                     "--run-id", "relay-test", "--host", "0.0.0.0", "--allow-network-simulation", "--allow-test-faults"])
            created.append(scene)

            def until(fn, predicate, timeout=25):
                end, last = time.monotonic() + timeout, None
                while time.monotonic() < end:
                    try:
                        last = fn()
                        if predicate(last):
                            return last
                    except (RuntimeError, OSError, KeyError) as error:
                        last = str(error)
                    time.sleep(.1)
                raise AssertionError(f"condition not met: {last}")

            def physics():
                script = """import urllib.request
request=urllib.request.Request('http://127.0.0.1:8091/v1/state',headers={'Authorization':'Bearer '+open('/token').read()})
with urllib.request.urlopen(request,timeout=2) as response: print(response.read().decode())
"""
                return json.loads(command(["docker", "exec", scene, "python", "-c", script]))

            def cli(index, verb, *args, spec=None):
                mounts = ["--mount", f"type=bind,src={folder / f'operator-{index}.json'},dst=/operator.json,readonly"]
                if spec is not None:
                    mounts += ["--mount", f"type=bind,src={spec},dst=/spec.json,readonly"]
                    args += ("--spec", "/spec.json")
                return json.loads(command(["docker", "run", "--rm", "--network", control if index == 1 else west,
                    "--user", user, "--entrypoint", "/world", *mounts, node_image,
                    "task", verb, "--credentials", "/operator.json", "--json", *args]))

            def relay(index):
                result = cli(index, "list", "--relayed")
                assert result["last_known"] is True
                return result["entries"]

            def entry(index, task_id):
                return next((item for item in relay(index) if item["task_id"] == task_id), None)

            def status(index, task_id):
                item = entry(index, task_id)
                return None if item is None else item.get("status")

            def phase(signed):
                return signed["record"]["status"]["Phase"] if signed else None

            def local_state():
                return json.loads((folder / "state-1" / "node-1.tasks").read_text())["state"]

            def local_task(task_id):
                state = local_state()
                saved = state["records"].get(task_id)
                return saved["record"] if saved else state["stops"].get(task_id)

            def stopped(state):
                return abs(state["velocity_mps"][0]) < .001 and abs(state["commanded_velocity_mps"]) < .001

            def hold_position():
                before = until(physics, stopped)
                time.sleep(.45)
                after = physics()
                assert stopped(after) and abs(after["position_m"][0] - before["position_m"][0]) < .005
                return after

            def submit(task_id, target, inspect=False, lifetime_ms=120000):
                spec = {"id": task_id, "type": "wendy.nav.inspect.v1" if inspect else "wendy.nav.go_to.v1",
                        "site": "synthetic-office", "assignee": "sim-robot/navigation", "domain": "simulation",
                        "class": "MOVE", "mode": "autonomous", "frame": {"id": "office-floor", "revision": "office-frame-v1"},
                        "not_after_unix_ms": int(time.time() * 1000) + lifetime_ms,
                        "area": {"min": [-2.1, .1, -.1], "max": [2.1, .3, .1]},
                        "required_sensors": ["gas", "local-obstacles"], "minimum_energy": .2, "fallback": "hold",
                        "target": [target, .2, 0], "speed_mps": .4, "tolerance_m": .03, "stop_speed_mps": .03}
                path = folder / (task_id + ".json")
                path.write_text(json.dumps(spec))
                response = cli(3, "send", "--destination", "node-1", spec=path)
                assert response["state"] == "queued" and response["entry"]["audience"] == "node-1"
                return response["entry"]

            def stop(task_id):
                response = cli(3, "stop", "--destination", "node-1", "--id", task_id)
                assert response["state"] == "queued"
                return response["entry"]

            for i in range(1, 4):
                until(lambda i=i: relay(i), lambda entries: not entries)
            until(physics, lambda state: state["synthetic"])
            command(["docker", "run", "-d", "--name", executor, "--network", control, "--user", user,
                     "--entrypoint", "/world-executor", "--mount", f"type=bind,src={folder / 'executor-1.json'},dst=/client.json,readonly",
                     "--mount", f"type=bind,src={folder / 'adapter.token'},dst=/token,readonly", node_image,
                     "--credentials", "/client.json", "--adapter", "http://scene:8091", "--adapter-token-file", "/token", "--run-id", "relay-test"])
            created.append(executor)
            until(physics, lambda state: state["sequence"] > 0)

            command(["docker", "network", "disconnect", east, nodes[1]])
            initial = hold_position()["position_m"][0]
            queued = submit("relay-inspect", 1.2, inspect=True)
            at_relay = until(lambda: entry(2, "relay-inspect"), lambda item: item and item.get("mission"))
            assert at_relay["mission"] == queued["mission"] and not at_relay.get("status")
            assert local_task("relay-inspect") is None
            command(["docker", "kill", "--signal", "KILL", nodes[1]])
            command(["docker", "start", nodes[1]])
            recovered_queue = until(lambda: entry(2, "relay-inspect"), lambda item: item and item.get("mission"))
            assert recovered_queue["mission"] == queued["mission"]
            assert abs(hold_position()["position_m"][0] - initial) < .005

            command(["docker", "network", "connect", "--alias", "world-node-2", east, nodes[1]])
            until(lambda: status(3, "relay-inspect"), lambda signed: phase(signed) == "running")
            partition_pose = until(physics, lambda state: abs(state["velocity_mps"][0]) > .1)
            command(["docker", "network", "disconnect", east, nodes[1]])
            last_known = status(3, "relay-inspect")
            # Local disk and physics are test oracles. No operator connection to
            # the assignee remains during this autonomous completion interval.
            completed = until(lambda: local_task("relay-inspect"), lambda record: record and record["status"]["Phase"] == "succeeded")
            result = completed["result"]
            assert len(result["samples"]) == 4 and result["task_id"] == "relay-inspect"
            assert result["position_m"][0] - partition_pose["position_m"][0] > .3
            assert abs(result["position_m"][0] - 1.2) <= .031
            assert status(3, "relay-inspect") == last_known and phase(last_known) != "succeeded"
            hold_position()
            command(["docker", "network", "connect", "--alias", "world-node-2", east, nodes[1]])
            terminal = until(lambda: status(3, "relay-inspect"), lambda signed: phase(signed) == "succeeded")
            assert terminal["audience"] == "node-1" and terminal["assignee"] == "sim-robot/navigation"
            assert terminal["terminal"] is True and terminal["record"] == completed
            assert terminal["grant"]["grant"]["principal"] == "lab/node-1"

            # The terminal cache must be readable after reader restart with its
            # only relay disconnected. This does not claim fresh robot telemetry.
            command(["docker", "network", "disconnect", west, nodes[1]])
            command(["docker", "kill", "--signal", "KILL", nodes[2]])
            command(["docker", "start", nodes[2]])
            cached = until(lambda: status(3, "relay-inspect"), lambda signed: phase(signed) == "succeeded")
            assert cached == terminal
            command(["docker", "network", "connect", "--alias", "world-node-2", west, nodes[1]])
            references = [sample["evidence"] for sample in result["samples"] if "evidence" in sample]
            for ref in references:
                media = json.loads(command(["docker", "run", "--rm", "--network", control, "--user", user,
                    "--entrypoint", "/world-evidence", "--mount", f"type=bind,src={folder / 'viewer-1.json'},dst=/viewer.json,readonly",
                    node_image, "--credentials", "/viewer.json", "--sha256", ref["sha256"]]))
                assert media["verified"] and media["bytes"] == ref["size"]

            submit("relayed-stop", -1.2)
            until(physics, lambda state: state["velocity_mps"][0] < -.1)
            stop("relayed-stop")
            until(lambda: status(3, "relayed-stop"), lambda signed: phase(signed) == "canceled")
            hold_position()

            command(["docker", "network", "disconnect", east, nodes[1]])
            submit("stopped-before-delivery", 1.2)
            stop("stopped-before-delivery")
            until(lambda: entry(2, "stopped-before-delivery"), lambda item: item and item.get("stop"))
            command(["docker", "network", "connect", "--alias", "world-node-2", east, nodes[1]])
            until(lambda: status(3, "stopped-before-delivery"), lambda signed: phase(signed) == "canceled")
            state = local_state()
            assert "stopped-before-delivery" not in state["records"]
            assert state["stops"]["stopped-before-delivery"]["status"]["Phase"] == "canceled"
            assert not any(audit["task_id"] == "stopped-before-delivery" and audit["event"] == "accepted" for audit in state["audit"])
            hold_position()

            command(["docker", "network", "disconnect", east, nodes[1]])
            expired = submit("expired-before-delivery", 1.2, lifetime_ms=10000)
            until(lambda: entry(2, "expired-before-delivery"), lambda item: item and item.get("mission"))
            expiry = expired["mission"]["expires_at_unix_ms"]
            while time.time() * 1000 <= expiry + 200:
                time.sleep(.1)
            command(["docker", "network", "connect", "--alias", "world-node-2", east, nodes[1]])
            # A fresh stop must reach the same destination, proving the restored
            # route was exercised before checking the expired task stayed absent.
            stop("route-after-expiry")
            until(lambda: status(3, "route-after-expiry"), lambda signed: phase(signed) == "canceled")
            assert local_task("expired-before-delivery") is None
            hold_position()

            return {"mode": "two-hop mission relay on existing Docker Linux VM", "world_nodes": 3,
                    "ingress": "node-3", "relay": "node-2", "assignee": "node-1", "ingress_has_no_direct_assignee_network": True,
                    "queued_instruction_survived_relay_sigkill": True, "original_operator_proof_preserved": True,
                    "autonomous_inspection_completed_during_partition": True,
                    "movement_after_partition_m": round(result["position_m"][0] - partition_pose["position_m"][0], 4),
                    "remote_status_stayed_last_known_during_partition": True, "terminal_result_repaired_across_two_hops": True,
                    "terminal_result_survived_isolated_reader_sigkill": True, "inspection_samples": len(result["samples"]),
                    "capture_node_media_verified": len(references), "relayed_stop_canceled_motion": True,
                    "queued_stop_prevented_admission": True, "expired_queued_mission_never_started": True,
                    "node_image": command(["docker", "image", "inspect", node_image, "--format", "{{.Id}}"]).decode().strip(),
                    "scene_image": command(["docker", "image", "inspect", scene_image, "--format", "{{.Id}}"]).decode().strip(),
                    "limits": ["synthetic one-axis MuJoCo robot; no physical hardware", "signed simulation HTTP repair; no production Wendy mesh transport",
                               "cached status is last-known, not live telemetry", "media verified directly at capture node; no relayed blob lookup",
                               "single functional run; no p95 or physical safety acceptance"]}
        except Exception:
            for name in created:
                logs = subprocess.run(["docker", "logs", "--tail", "10", name], capture_output=True)
                bounded = lambda data: "\n".join(line[:1200] for line in data.decode().splitlines())
                print(name, bounded(logs.stdout), bounded(logs.stderr), file=__import__("sys").stderr)
            raise
        finally:
            for name in reversed(created):
                subprocess.run(["docker", "rm", "-f", name], capture_output=True)
            for network in reversed(networks):
                subprocess.run(["docker", "network", "rm", network], capture_output=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--scene-image", default="wendy-world-live:local")
    args = parser.parse_args()
    print(json.dumps(run(args.node_image, args.scene_image), indent=2))

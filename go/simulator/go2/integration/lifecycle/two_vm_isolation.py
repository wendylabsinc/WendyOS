"""Bounded two-VM DDS/world isolation acceptance using normal CLI and HTTP.

Run only with two explicitly named, already running Wendy Go2 VMs. Every motion
command uses the peer VM's normal ROS CLI; HTTP only grants, resets or disarms.
"""

import argparse
from datetime import datetime, timezone
import hashlib
import json
import math
from pathlib import Path
import re
import subprocess
import tempfile
import time
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener


def require(condition, message):
    if not condition:
        raise AssertionError(message)


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeError("simulator endpoint redirected")


class API:
    def __init__(self, name, url, digest):
        parsed = urlsplit(url)
        require(re.fullmatch(r"[a-zA-Z0-9][a-zA-Z0-9_-]*", name), "invalid VM name")
        require(parsed.scheme == "http" and parsed.hostname == "127.0.0.1"
                and parsed.port and parsed.path in {"", "/"}
                and not parsed.username and not parsed.password
                and not parsed.query and not parsed.fragment, "expected explicit loopback HTTP port")
        self.name, self.url, self.digest = name, url.rstrip("/"), digest
        self.http = build_opener(ProxyHandler({}), NoRedirects())

    def request(self, path, body=None):
        request = Request(self.url + path, data=None if body is None else json.dumps(body).encode(),
                          headers={} if body is None else {"Content-Type": "application/json"})
        with self.http.open(request, timeout=3) as response:
            return json.load(response)

    def status(self, *, require_ready=True):
        value = self.request("/api/status")
        require(value.get("simulation") is True and value.get("robot") == "go2"
                and value.get("robot_kind") == "go2" and type(value.get("profile_version")) is int
                and value["profile_version"] == 1 and value.get("vm_name") == self.name
                and value.get("source_digest") == self.digest
                and value.get("dds_isolation") == "udp-rtps-loopback"
                and value.get("clock_mode") == "device", "managed VM identity mismatch")
        require(value.get("healthy") is True and value.get("error") is None,
                "virtual robot is not healthy")
        if require_ready:
            require(value.get("ready") is True, "virtual robot is not ready")
        return value

    def post(self, path, body=None):
        require(path in {"/api/arm_ros", "/api/reset", "/api/disarm_ros"}, "invalid lifecycle operation")
        self.status()
        return self.request(path, body or {})


def compact(status):
    return {key: status[key] for key in (
        "time", "epoch", "mode", "armed", "position", "command", "applied_command",
        "ros_commands", "healthy", "ready", "source_digest", "vm_name",
    )} | {"physics_steps": status["metrics"]["physics_steps"]}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--cli", required=True, type=Path)
    parser.add_argument("--primary", required=True)
    parser.add_argument("--primary-url", required=True)
    parser.add_argument("--peer", required=True)
    parser.add_argument("--peer-url", required=True)
    parser.add_argument("--source-digest", required=True)
    parser.add_argument("--output", required=True, type=Path)
    args = parser.parse_args()
    require(re.fullmatch(r"sha256:[0-9a-f]{64}", args.source_digest), "invalid source digest")
    require(args.primary != args.peer and args.primary_url != args.peer_url, "two distinct VMs required")
    primary = API(args.primary, args.primary_url, args.source_digest)
    peer = API(args.peer, args.peer_url, args.source_digest)
    started = time.monotonic()
    result = {"passed": False, "started_at": datetime.now(timezone.utc).isoformat(),
              "source_digest": args.source_digest,
              "harness_sha256": hashlib.sha256(Path(__file__).read_bytes()).hexdigest(),
              "cli_sha256": hashlib.sha256(args.cli.read_bytes()).hexdigest(),
              "endpoints": {primary.name: primary.url, peer.name: peer.url},
              "commands": [], "checks": [], "samples": [],
              "limits": {"velocity_mps": 0.3, "armed_seconds": 3.0,
                         "primary_maximum_drift_m": 0.03, "peer_minimum_displacement_m": 0.2},
              "limitations": ["Bounded two-VM test, not a sustained throughput benchmark.",
                              "Checks normal local DDS discovery and command ingress; no packet escape probe.",
                              "Primary position drift is measured over the command/reset observation window."]}
    publisher = None
    publisher_log = tempfile.TemporaryFile(mode="w+")
    authorized = []

    def check(name, **details):
        result["checks"].append({"check": name, "passed": True, **details})
        print(json.dumps(result["checks"][-1]), flush=True)

    def cli(vm, *arguments):
        return [str(args.cli), "--device", "vm:" + vm, "device", "ros2", "exec", "--", *arguments]

    def graph(vm, phase):
        command = cli(vm, "topic", "info", "/lowstate", "--verbose")
        process = subprocess.run(command, stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                                 text=True, timeout=20)
        result["commands"].append({"argv": command, "phase": phase,
                                   "returncode": process.returncode, "output": process.stdout})
        require(process.returncode == 0, f"{vm}: native topic inspection failed")
        counts = re.findall(r"^Publisher count: (\d+)\s*$", process.stdout, re.MULTILINE)
        gids = re.findall(r"^GID: ([0-9a-f.]+)\s*$", process.stdout, re.MULTILINE)
        require(counts == ["1"] and len(gids) == 1
                and "Type: unitree_go/msg/LowState" in process.stdout,
                f"{vm}: expected exactly one native LowState publisher")
        return gids[0]

    def pair(phase):
        value = {"elapsed_seconds": time.monotonic() - started, "phase": phase,
                 "primary": compact(primary.status()),
                 "peer": compact(peer.status(require_ready=phase != "after_peer_reset"))}
        result["samples"].append(value)
        return value

    def primary_unchanged(value, baseline, gid):
        state = value["primary"]
        require(state["epoch"] == baseline["epoch"], "peer action reset primary epoch")
        require(state["ros_commands"]["owner"] == baseline["ros_commands"]["owner"]
                and state["armed"] == baseline["armed"], "peer action changed primary ownership")
        require(state["ros_commands"]["accepted"] == baseline["ros_commands"]["accepted"],
                "primary accepted a command during the peer-only test")
        require(all(source["publisher_gid"] != gid for source in state["ros_commands"]["sources"]),
                "peer publisher crossed into primary command ingress")
        require(state["command"] == baseline["command"], "peer action changed primary command")

    try:
        initial = pair("initial")
        for api, key in ((primary, "primary"), (peer, "peer")):
            require(initial[key]["armed"] is False and initial[key]["ros_commands"]["owner"] is None,
                    f"{api.name}: test requires an initially disarmed robot")
            authorized.append(api)
        before_gids = {api.name: graph(api.name, "before") for api in (primary, peer)}
        require(len(set(before_gids.values())) == 2, "VMs share the native DDS publisher identity")
        check("independent_native_graphs_before", publishers=before_gids)
        known = {source["publisher_gid"] for source in peer.status()["ros_commands"]["sources"]}
        command = cli(peer.name, "topic", "pub", "/cmd_vel", "geometry_msgs/msg/Twist",
                      "{linear: {x: 0.3}, angular: {z: 0.0}}", "--rate", "20", "--times", "120",
                      "--print", "20", "--qos-durability", "volatile",
                      "--node-name", "wendy_go2_two_vm_isolation")
        result["commands"].append({"argv": command, "phase": "peer_motion"})
        publisher = subprocess.Popen(command, stdout=publisher_log, stderr=subprocess.STDOUT, text=True)
        deadline = time.monotonic() + 15
        gid = None
        while time.monotonic() < deadline:
            sources = peer.status()["ros_commands"]["sources"]
            candidates = [source["publisher_gid"] for source in sources
                          if source["publisher_gid"] not in known and source["kind"] == "twist"
                          and source["age_ms"] < 300 and source["requires_restart"] is False]
            require(len(candidates) <= 1, "multiple uncoordinated new peer publishers")
            if candidates:
                gid = candidates[0]
                break
            require(publisher.poll() is None, "peer publisher exited before discovery")
            time.sleep(0.05)
        require(gid is not None, "fresh peer Twist GID was not discovered")
        result["peer_publisher_gid"] = gid
        baseline = pair("before_grant")
        primary_unchanged(baseline, initial["primary"], gid)
        result["grant"] = peer.post("/api/arm_ros", {"publisher_gid": gid})
        arm_started = time.monotonic()
        while time.monotonic() - arm_started < 3.0:
            value = pair("peer_motion")
            primary_unchanged(value, baseline["primary"], gid)
            require(value["peer"]["epoch"] == baseline["peer"]["epoch"]
                    and value["peer"]["ros_commands"]["owner"] == gid,
                    "peer lost the explicit ROS grant")
            time.sleep(min(0.1, max(0, 3.0 - (time.monotonic() - arm_started))))
        moved = pair("before_peer_reset")
        result["armed_seconds"] = time.monotonic() - arm_started
        result["reset"] = peer.post("/api/reset")
        reset_until = time.monotonic() + 1.0
        while time.monotonic() < reset_until:
            value = pair("after_peer_reset")
            primary_unchanged(value, baseline["primary"], gid)
            require(value["peer"]["epoch"] == baseline["peer"]["epoch"] + 1
                    and value["peer"]["armed"] is False
                    and value["peer"]["ros_commands"]["owner"] is None,
                    "peer reset did not independently revoke ownership")
            time.sleep(0.1)
        end = pair("control_window_end")
        window = result["samples"][result["samples"].index(baseline):]
        maximum_drift = max(math.dist(value["primary"]["position"], baseline["primary"]["position"])
                            for value in window)
        require(maximum_drift < 0.03, "primary drift exceeded 3 cm during peer command/reset")
        primary_unchanged(end, baseline["primary"], gid)
        displacement = math.dist(moved["peer"]["position"][:2], baseline["peer"]["position"][:2])
        accepted = moved["peer"]["ros_commands"]["accepted"] - baseline["peer"]["ros_commands"]["accepted"]
        require(displacement >= 0.2 and accepted >= 40, "peer did not move using sustained ROS commands")
        require(moved["peer"]["time"] - baseline["peer"]["time"] >= 2.5,
                "peer physics did not advance during motion")
        require(end["primary"]["time"] - baseline["primary"]["time"] >= 3.0
                and end["primary"]["physics_steps"] > baseline["primary"]["physics_steps"],
                "primary physics did not continue")
        reset_samples = [value["peer"] for value in window if value["phase"] == "after_peer_reset"]
        require(len({state["ros_commands"]["accepted"] for state in reset_samples}) == 1
                and any(source["publisher_gid"] == gid and source["requires_restart"]
                        for source in end["peer"]["ros_commands"]["sources"]),
                "reset allowed the old peer publisher to resume")
        check("peer_ros_motion", displacement_m=displacement, accepted_commands=accepted,
              armed_seconds=result["armed_seconds"], publisher_gid=gid)
        check("primary_unaffected_by_peer_motion_and_reset", maximum_position_drift_m=maximum_drift,
              physics_advance_seconds=end["primary"]["time"] - baseline["primary"]["time"],
              epoch=baseline["primary"]["epoch"], owner=baseline["primary"]["ros_commands"]["owner"],
              samples=len(window), peer_gid_absent=True)
        check("peer_reset_isolated_and_revokes_old_publisher", epoch_before=baseline["peer"]["epoch"],
              epoch_after=end["peer"]["epoch"])
        require(publisher.wait(timeout=15) == 0, "bounded peer publisher failed")
        after_gids = {api.name: graph(api.name, "after") for api in (primary, peer)}
        require(after_gids == before_gids, "native publisher identities changed or crossed VMs")
        final = pair("after_graph_checks")
        primary_unchanged(final, baseline["primary"], gid)
        check("independent_native_graphs_after", publishers=after_gids)
        result["passed"] = True
    except Exception as error:
        result["error"] = f"{type(error).__name__}: {error}"
    finally:
        cleanup = {}
        for api in authorized:
            try:
                api.post("/api/disarm_ros")
                status = api.status()
                require(status["armed"] is False and status["ros_commands"]["owner"] is None,
                        "cleanup did not disarm")
                cleanup[api.name] = {"disarmed": True, "status": compact(status)}
            except Exception as error:
                result["passed"] = False
                cleanup[api.name] = {"error": f"{type(error).__name__}: {error}"}
        if publisher is not None:
            try:
                publisher.wait(timeout=15)
            except subprocess.TimeoutExpired:
                publisher.terminate()
                publisher.wait(timeout=5)
            publisher_log.seek(0)
            result["publisher_output"] = publisher_log.read()
            result["publisher_returncode"] = publisher.returncode
        publisher_log.close()
        result["cleanup"] = cleanup
        result["wall_seconds"] = time.monotonic() - started
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(json.dumps(result, indent=2, allow_nan=False) + "\n")
    print(json.dumps({"passed": result["passed"], "output": str(args.output),
                      "error": result.get("error"), "wall_seconds": result["wall_seconds"]}), flush=True)
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

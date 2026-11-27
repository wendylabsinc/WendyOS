"""Verify rolling live evidence and retained references on the existing Docker VM.

Owns only uniquely labeled synthetic containers/networks. The shared Go2 is not
used. Disk manifests are test oracles; media availability is checked separately
through signed reader requests. This is one functional run, not a safety test.
"""
import argparse
from datetime import datetime, timedelta, timezone
import hashlib
import json
from pathlib import Path
import shutil
import signal
import subprocess
import time
import urllib.request

from native_mission_site import NativeMissionSite
from virtual_site import ROOT, command


class EvidenceRetentionSite(NativeMissionSite):
    def __init__(self, folder, node_image, scene_image, max_seconds=300):
        super().__init__(folder, node_image, scene_image)
        self.run_id = "evidence-retention-" + self.suffix
        self.cap, self.cycles = 64, 4
        self.deadline = time.monotonic() + max_seconds
        self.nodes, self.sensors = {}, {}
        self.observed = {1: {}, 2: {}}
        self.max_entries = {1: 0, 2: 0}
        self.timeline = []
        self.last_monitor = 0
        self.report = {"schema": "wendy.evidence-retention.verification.v1", "passed": False,
            "run_id": self.run_id, "max_entries": self.cap, "required_churn_cycles": self.cycles,
            "mode": "synthetic MuJoCo 4 live capture on existing Docker Linux VM",
            "node_image": self.node_image, "scene_image": self.scene_image,
            "limits": ["single functional run; no physical hardware or safety certification",
                "generated audio and analytic gas; camera pixels rendered by MuJoCo",
                "manifest occupancy is sampled; unit tests establish lock-level quota bounds",
                "allocation counters observe complete persisted manifests, not HTTP acknowledgements",
                "reference pins do not extend immutable original blob expiry"]}

    def guard(self):
        if time.monotonic() >= self.deadline:
            raise AssertionError("bounded evidence-retention fixture deadline reached")

    def wait(self, fn, predicate, timeout=40):
        end, last = min(self.deadline, time.monotonic() + timeout), None
        while time.monotonic() < end:
            self.monitor()
            try:
                last = fn()
                if predicate(last):
                    return last
            except (RuntimeError, OSError, KeyError) as error:
                last = str(error)
            time.sleep(.2)
        raise AssertionError(f"condition not met: {str(last)[:1500]}")

    def identity(self, name):
        row = json.loads(command(["docker", "inspect", name]))[0]
        if row["Config"]["Labels"].get("wendy.world.native-test") != self.suffix:
            raise AssertionError("fixture ownership changed")
        state = row["State"]
        return {"container_id": row["Id"], "name": name, "image": row["Image"],
            "pid": state["Pid"], "started_at": state["StartedAt"], "running": state["Running"],
            "exit_code": state["ExitCode"], "restart_count": row["RestartCount"]}

    def assert_running(self, name):
        identity = self.identity(name)
        if not identity["running"]:
            raise AssertionError(f"process exited: {identity}")
        return identity

    def manifests(self, index):
        result = {}
        for path in (self.folder / f"node-{index}.evidence").glob("*.json"):
            if len(path.stem) != 64:
                continue
            try:
                value = json.loads(path.read_text())
            except FileNotFoundError:
                continue  # Eviction can win after directory enumeration.
            result[value["sha256"]] = value
        return result

    def monitor(self, force=False):
        now = time.monotonic()
        if not force and now - self.last_monitor < .2:
            return
        self.last_monitor = now
        counts = {}
        for index in (1, 2):
            entries = self.manifests(index)
            # A listing concurrent with replacement may include an old and a
            # new generation. Confirm any apparent overflow from a fresh scan.
            if len(entries) > self.cap:
                entries = self.manifests(index)
            assert len(entries) <= self.cap, (index, len(entries))
            self.max_entries[index] = max(self.max_entries[index], len(entries))
            for digest, entry in entries.items():
                if entry.get("complete") and entry.get("retention_class") == "rolling":
                    allocation = str(entry["allocation"])
                    record = {"sha256": digest, "size": entry["size"], "content_type": entry["content_type"],
                        "owner": entry["owner"], "retain_until_unix_ms": entry["retain_until_unix_ms"]}
                    previous = self.observed[index].setdefault(allocation, record)
                    assert previous == record, "allocation metadata changed"
            counts[str(index)] = {"current_entries": len(entries),
                "observed_completed_rolling_allocations": len(self.observed[index]),
                "max_allocation": max((e.get("allocation", 0) for e in entries.values()), default=0)}
        if not self.timeline or force or now - self.timeline[-1]["monotonic_seconds"] >= 1:
            row = {"unix_ms": int(time.time()*1000), "monotonic_seconds": now, "nodes": counts}
            self.timeline.append(row)
            with (self.folder / "occupancy.jsonl").open("a") as stream:
                stream.write(json.dumps(row) + "\n")

    def physics(self, endpoint="state"):
        token = "sensor.token" if endpoint == "samples" else "adapter.token"
        request = urllib.request.Request(f"http://127.0.0.1:{self.scene_port}/v1/{endpoint}",
            headers={"Authorization": "Bearer " + (self.folder / token).read_text().strip()})
        with urllib.request.urlopen(request, timeout=3) as response:
            value = json.load(response)
        assert value.get("run_id") == self.run_id
        return value

    def query(self, index=3):
        return self.cli("world-query", "--credentials", f"/lab/client-{index}.json")

    @staticmethod
    def hazards(snapshot):
        return [c for c in snapshot["components"] if c["key"]["type"] == "wendy.hazard.v1"]

    def task(self, task_id):
        records = json.loads((self.folder / "node-1.tasks").read_text())["state"]["records"]
        return records.get(task_id, {}).get("record", {})

    def submit(self, task_id, target, inspect=False):
        self.guard()
        spec = {"id": task_id, "type": "wendy.nav.inspect.v1" if inspect else "wendy.nav.go_to.v1",
            "site": "synthetic-office", "assignee": "sim-robot/navigation", "domain": "simulation",
            "class": "MOVE", "mode": "autonomous", "frame": {"id": "office-floor", "revision": "office-frame-v1"},
            "not_after_unix_ms": int(time.time()*1000)+90000,
            "area": {"min": [-2.1, .1, -.1], "max": [2.1, .3, .1]},
            "required_sensors": ["gas", "local-obstacles"], "minimum_energy": .2,
            "fallback": "hold", "target": [target, .2, 0], "speed_mps": .4,
            "tolerance_m": .03, "stop_speed_mps": .03}
        self.write(task_id + ".json", spec)
        return self.cli("world", "task", "send", "--credentials", "/lab/operator-1.json",
            "--spec", f"/lab/{task_id}.json", "--json")

    def completed(self, task_id):
        record = self.wait(lambda: self.task(task_id),
            lambda t: t.get("status", {}).get("Phase") in ("succeeded", "failed", "held", "stopped"), 30)
        assert record["status"]["Phase"] == "succeeded", record
        return record

    def verify_media(self, refs, credential="viewer-1", save_phase=None):
        receipts = []
        for ref in refs:
            args = ["--credentials", f"/lab/{credential}.json", "--sha256", ref["sha256"]]
            if save_phase:
                destination = self.folder / "verified-media"
                destination.mkdir(mode=0o700, exist_ok=True)
                extension = {"image/png": "png", "audio/wav": "wav"}.get(ref["content_type"], "bin")
                filename = f"{save_phase}-{ref['sha256']}.{extension}"
                response = json.loads(command(["docker", "run", "--rm", "--network", self.network,
                    "--user", self.user, "--entrypoint", "/world-evidence",
                    "--mount", f"type=bind,src={self.folder},dst=/lab,readonly",
                    "--mount", f"type=bind,src={destination},dst=/verified-media", self.node_image,
                    *args, "--out", "/verified-media/" + filename]))
                raw = (destination / filename).read_bytes()
                assert hashlib.sha256(raw).hexdigest() == ref["sha256"] and len(raw) == ref["size"]
                self.report.setdefault("saved_media", []).append({"path": "verified-media/" + filename, **ref})
            else:
                response = self.cli("world-evidence", *args)
            assert response["verified"] and response["bytes"] == ref["size"], response
            assert response["metadata"]["content_type"] == ref["content_type"], response
            receipts.append(response)
        return receipts

    def setup(self):
        self.folder.mkdir(mode=0o700, parents=True, exist_ok=False)
        self.write("report.json", self.report)
        command(["go", "run", "./cmd/world-lab", "--out", str(self.folder), "--runtime-dir", "/lab",
            "--containers", "--missions", "--sensors", "--task-relay", "--run-id", self.run_id])
        for index in range(1, 4):
            cfg = json.loads((self.folder / f"node-{index}.json").read_text())
            cfg["evidence"]["max_entries"] = self.cap
            self.write(f"node-{index}.json", cfg)
        for network in (self.network, self.control_network):
            command(["docker", "network", "create", "--label", "wendy.world.native-test=" + self.suffix, network])
            if network == self.network:
                self.made_network = True
            else:
                self.made_control = True
        for index in range(1, 4):
            name = self.start(f"node-{index}", "worldd", ["--config", f"/lab/node-{index}.json"], f"world-node-{index}")
            self.nodes[index] = name
            if index == 1:
                command(["docker", "network", "connect", "--alias", "world-node-1", self.network, name])
        self.wait(lambda: self.query(), lambda value: not value["components"])
        self.scene = self.start("scene", None, ["examples/live-office/world.json", "--token-file", "/lab/adapter.token",
            "--sensor-token-file", "/lab/sensor.token", "--run-id", self.run_id, "--host", "0.0.0.0", "--port", "8080",
            "--allow-network-simulation", "--allow-test-faults"], "scene", True)
        command(["docker", "network", "connect", "--alias", "scene", self.network, self.scene])
        self.scene_port = int(command(["docker", "port", self.scene, "8080/tcp"]).decode().strip().split(":")[-1])
        first = self.wait(lambda: self.physics("samples"), lambda value: len(value["observations"]) == 4)
        version = command(["docker", "exec", self.scene, "python", "-c", "import mujoco; print(mujoco.__version__)"]).decode().strip()
        assert version == "4.0.0", version
        self.report.update({"mujoco_version": version, "scene_boot_id": first["boot_id"], "initial_sample_sequence": first["sequence"]})
        self.report["sensor_models"] = {s["sensor_id"]: s["sensor_model"] for s in first["observations"]}
        assert all(self.report["sensor_models"][sensor] == "mujoco-4.0.0-live-rgb-pinhole" for sensor in ("camera-a", "camera-b"))
        fixture = self.folder / "package"
        shutil.copytree(ROOT / "examples/live-office", fixture)
        root = (fixture / "world.json").read_bytes()
        package = json.loads(root)
        self.package_refs = [{"sha256": hashlib.sha256(root).hexdigest(), "size": len(root), "content_type": "application/octet-stream"}]
        model = (fixture / package["mujoco"]["path"]).read_bytes()
        self.package_refs.append({"sha256": hashlib.sha256(model).hexdigest(), "size": len(model), "content_type": "application/octet-stream"})
        document = json.loads((ROOT / "examples/geography/office.json").read_text())
        document["package_sha256"] = self.package_refs[0]["sha256"]
        begin, end = datetime.now(timezone.utc)-timedelta(minutes=1), datetime.now(timezone.utc)+timedelta(hours=1)
        for item in [document, *document["frames"]]:
            item["valid_from"], item["valid_until"] = begin.isoformat(), end.isoformat()
        self.write("site.json", document)
        published = self.cli("world", "package", "publish", "--credentials", "/lab/site-admin-1.json",
            "--site", "/lab/site.json", "--package", "/lab/package/world.json", "--run-id", self.run_id, "--ttl", "30m", "--json")
        assert published["published"] and published["package_sha256"] == self.package_refs[0]["sha256"]
        self.report["package_publication"] = published
        self.report["package_initial_receipts"] = self.verify_media(self.package_refs, save_phase="package-initial")
        for sensor in ("camera-a", "camera-b", "gas-1", "audio-1"):
            self.sensors[sensor] = self.start("sensor-" + sensor, "world-live-sense",
                ["--credentials", f"/lab/sensor-{sensor}.json", "--adapter", "http://scene:8080",
                    "--sensor-token-file", "/lab/sensor.token", "--sensor", sensor, "--run-id", self.run_id, "--ttl", "5s"])
        self.fuser = self.start("fusion", "world-fuse", ["--config", "/lab/fusion.json", "--run-id", self.run_id])
        self.wait(self.query, lambda snapshot: bool(self.hazards(snapshot)))
        self.executor = self.start("executor", "world-executor", ["--credentials", "/lab/executor-1.json",
            "--adapter", "http://scene:8080", "--adapter-token-file", "/lab/adapter.token", "--run-id", self.run_id])
        self.wait(lambda: self.physics(), lambda value: value["sequence"] > 0)
        self.initial_processes = {name: self.assert_running(name) for name in [*self.nodes.values(), self.scene, *self.sensors.values(), self.fuser, self.executor]}
        self.report["initial_processes"] = self.initial_processes
        self.submit("retained-inspection", -.6, True)
        self.inspection = self.completed("retained-inspection")
        self.result = self.inspection["result"]
        self.result_refs = [sample["evidence"] for sample in self.result["samples"] if "evidence" in sample]
        assert len(self.result_refs) == 3 and {r["content_type"] for r in self.result_refs} == {"image/png", "audio/wav"}
        self.report["inspection_result"] = self.result
        self.report["result_initial_receipts"] = self.verify_media(self.result_refs, save_phase="result-initial")
        manifests = self.manifests(1)
        assert all(manifests[ref["sha256"]]["retention_class"] == "rolling" for ref in self.result_refs)
        self.report["result_blobs_initially_rolling"] = True
        ledger = json.loads((self.folder / "node-1.evidence/.retention.json").read_text())
        holds = [entry for entry in ledger["records"].values() if entry["namespace"] == "task" and entry["id"] == "retained-inspection"]
        assert len(holds) == 1 and holds[0]["state"] == "active", holds
        assert set(holds[0]["sha256"]) == {r["sha256"] for r in self.result_refs}
        self.report["task_hold"] = holds[0]
        protected = {r["sha256"] for r in self.result_refs + self.package_refs}
        self.old_candidates = [entry for digest, entry in self.manifests(1).items()
            if digest not in protected and entry.get("retention_class") == "rolling" and entry.get("complete")]
        self.monitor(True)
        self.churn_baseline = {i: len(self.observed[i]) for i in (1, 2)}
        self.report["churn_baseline"] = self.churn_baseline

    def churn(self):
        # Moving this fixture's puck changes both rendered cameras. Generated
        # audio continues independently; no arbitrary padding blobs are uploaded.
        for number in range(24):
            self.guard()
            task_id = "capture-sweep-" + str(number)
            self.submit(task_id, 1.5 if number % 2 == 0 else -1.5)
            self.completed(task_id)
            self.monitor(True)
            for name in self.sensors.values():
                assert self.assert_running(name) == self.initial_processes[name], "capture publisher restarted"
            if all(len(self.observed[i])-self.churn_baseline[i] >= self.cap*self.cycles for i in (1, 2)):
                self.report["completed_sweeps"] = number + 1
                break
        else:
            raise AssertionError("changing camera evidence did not complete the required churn")
        self.report["result_after_churn_receipts"] = self.verify_media(self.result_refs)
        self.report["package_after_churn_receipts"] = self.verify_media(self.package_refs)
        assert self.report["result_after_churn_receipts"] == self.report["result_initial_receipts"]
        assert self.report["package_after_churn_receipts"] == self.report["package_initial_receipts"]
        current = self.manifests(1)
        evicted = [e for e in self.old_candidates if e["sha256"] not in current]
        assert evicted, "no observed unpinned rolling object was evicted"
        try:
            self.cli("world-evidence", "--credentials", "/lab/viewer-1.json", "--sha256", evicted[0]["sha256"])
        except RuntimeError as error:
            assert "404" in str(error), str(error)
        else:
            raise AssertionError("evicted evidence remained readable")
        self.report["evicted_rolling_reference"] = evicted[0]
        self.submit("capture-settle", -.6)
        self.completed("capture-settle")

    def recover_fuser(self):
        before = self.wait(self.query, lambda value: bool(self.hazards(value)))
        hazard = self.hazards(before)[0]
        identity = self.assert_running(self.fuser)
        started = time.monotonic()
        command(["docker", "network", "disconnect", self.network, self.fuser])
        self.wait(self.query, lambda value: not self.hazards(value), 20)
        while time.monotonic() - started < 12:
            self.guard()
            self.monitor()
            time.sleep(.2)
        assert self.assert_running(self.fuser) == identity
        disconnected_seconds = time.monotonic() - started
        command(["docker", "network", "connect", self.network, self.fuser])
        restored = time.monotonic()
        after = self.wait(self.query, lambda value: any(c["version"]["epoch"] == hazard["version"]["epoch"]
            and c["version"]["sequence"] > hazard["version"]["sequence"] for c in self.hazards(value)), 50)
        assert self.assert_running(self.fuser) == identity
        self.report["fuser_recovery"] = {"identity": identity, "disconnected_seconds": disconnected_seconds,
            "old_observation_ttl_seconds": 5, "expired_hazard_observed": True,
            "recovery_seconds": time.monotonic()-restored, "before_version": hazard["version"],
            "after_version": self.hazards(after)[0]["version"], "same_process_and_epoch": True}

    def restart_capture(self):
        before = self.assert_running(self.nodes[1])
        command(["docker", "kill", "--signal", "KILL", self.nodes[1]])
        command(["docker", "start", self.nodes[1]])
        self.wait(lambda: self.query(1), lambda value: bool(value["components"]))
        after = self.assert_running(self.nodes[1])
        assert before["container_id"] == after["container_id"] and before["started_at"] != after["started_at"]
        assert self.task("retained-inspection")["result"] == self.result
        self.report["result_after_restart_receipts"] = self.verify_media(self.result_refs, save_phase="result-after-restart")
        self.report["package_after_restart_receipts"] = self.verify_media(self.package_refs, save_phase="package-after-restart")
        assert self.report["result_after_restart_receipts"] == self.report["result_initial_receipts"]
        assert self.report["package_after_restart_receipts"] == self.report["package_initial_receipts"]
        manifests = self.manifests(1)
        assert all(manifests[ref["sha256"]]["retention_class"] == "protected" for ref in self.package_refs)
        self.wait(self.query, lambda value: bool(self.hazards(value)), 50)
        self.report["capture_restart"] = {"before": before, "after": after, "retained_result_unchanged": True}

    def finish_report(self):
        self.monitor(True)
        self.report["counters"] = {str(i): {"observed_completed_rolling_allocations": len(self.observed[i]),
            "after_result_allocations": len(self.observed[i])-self.churn_baseline[i],
            "post_result_capacity_cycles": (len(self.observed[i])-self.churn_baseline[i])/self.cap,
            "max_sampled_entries": self.max_entries[i], "current_entries": len(self.manifests(i)),
            "distinct_observed_media_digests": len({e["sha256"] for e in self.observed[i].values()})} for i in (1, 2)}
        for index in (1, 2):
            rows = list(self.observed[index].values())
            self.report["counters"][str(index)]["completed_persisted_allocations_by_content_type"] = {
                kind: sum(e["content_type"] == kind for e in rows) for kind in sorted({e["content_type"] for e in rows})}
        self.write("observed-rolling-allocations.json", self.observed)
        self.report["final_processes"] = {name: self.assert_running(name) for name in self.initial_processes}
        for name, initial in self.initial_processes.items():
            if name != self.nodes[1]:
                assert self.report["final_processes"][name] == initial, f"unexpected restart: {name}"
        batch = self.physics("samples")
        assert batch["boot_id"] == self.report["scene_boot_id"]
        self.report["final_sample_sequence"] = batch["sequence"]
        self.report["passed"] = True
        self.write("report.json", self.report)
        return self.report

    def cleanup(self):
        owned = list(self.created)
        networks = [name for name, created in ((self.network, self.made_network),
            (self.control_network, self.made_control)) if created]
        diagnostics = {}
        for name in owned:
            try:
                diagnostics[name] = self.identity(name)
            except (RuntimeError, OSError) as error:
                diagnostics[name] = {"unavailable": str(error)[:1500]}
        if self.folder.exists():
            self.write("processes-before-cleanup.json", diagnostics)
            self.report["diagnostics"] = {"processes": "processes-before-cleanup.json",
                "logs": [name + ".log" for name in owned], "allocation_oracle": "observed-rolling-allocations.json",
                "occupancy_timeline": "occupancy.jsonl"}
        # NativeMissionSite archives bounded owned-container logs before removal.
        super().cleanup()
        remaining = []
        for kind, names in (("container", owned), ("network", networks)):
            for name in names:
                result = subprocess.run(["docker", kind, "inspect", name], capture_output=True, timeout=10)
                if result.returncode == 0:
                    remaining.append({"kind": kind, "name": name})
                elif not any(term in result.stderr.decode().lower() for term in ("no such", "not found")):
                    remaining.append({"kind": kind, "name": name, "cleanup_unknown": result.stderr.decode()[:1000]})
        self.report["cleanup"] = {"containers": owned, "networks": networks,
            "remaining_owned_resources": remaining, "complete": not remaining}
        if self.folder.exists():
            if remaining:
                self.report["passed"] = False
            self.write("report.json", self.report)
        if remaining:
            raise AssertionError(f"fixture cleanup incomplete: {remaining}")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--node-image", required=True)
    parser.add_argument("--scene-image", required=True)
    parser.add_argument("--max-seconds", type=int, default=300)
    args = parser.parse_args()
    if not 120 <= args.max_seconds <= 600:
        parser.error("max-seconds must be 120..600")
    site = EvidenceRetentionSite(args.out, args.node_image, args.scene_image, args.max_seconds)
    def terminate(signum, frame):
        raise KeyboardInterrupt
    signal.signal(signal.SIGTERM, terminate)
    try:
        for phase, action in (("setup", site.setup), ("churn", site.churn),
                ("fuser-recovery", site.recover_fuser), ("capture-restart", site.restart_capture)):
            print(json.dumps({"phase": phase, "state": "starting", "run_id": site.run_id}), flush=True)
            action()
            site.report["last_completed_phase"] = phase
            site.write("report.json", site.report)
            print(json.dumps({"phase": phase, "state": "completed", "run_id": site.run_id}), flush=True)
        site.finish_report()
    except BaseException as error:
        if site.folder.exists():
            site.report["failure"] = {"type": type(error).__name__, "message": str(error)[:4000]}
            site.write("report.json", site.report)
            site.write("observed-rolling-allocations.json", site.observed)
        raise
    finally:
        site.cleanup()
    print(json.dumps(site.report), flush=True)


if __name__ == "__main__":
    main()

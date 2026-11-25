"""Fetch signed model-only packages, disconnect the replicas, then run MuJoCo 4."""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from sim.package_launch import LaunchError, launch, local_image
from sim.package_runner import RunnerError, canonical_bytes
from virtual_site import command


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def cleanup_resources(container_names, network, label):
    """Inspect the unique run label, attempt every cleanup, then report errors."""
    failures = []
    for kind, names in (("container", list(reversed(container_names))), ("network", [network])):
        for name in names:
            try:
                # Listing by both name and run label also handles a timed-out
                # create whose object never appeared, without removing others.
                identifiers = command(["docker", kind, "ls", *( ["--all"] if kind == "container" else []),
                    "--filter", "name="+name, "--filter", "label=wendy.world.generic-package-run="+label,
                    "--format", "{{.ID}}"])
                for identity in identifiers.decode().splitlines():
                    records = json.loads(command(["docker", kind, "inspect", identity]))
                    if len(records) != 1:
                        raise RuntimeError("ambiguous cleanup target")
                    record = records[0]
                    actual_name = record["Name"].removeprefix("/")
                    labels = record.get("Labels", {}) if kind == "network" else record.get("Config", {}).get("Labels", {})
                    if actual_name != name or labels.get("wendy.world.generic-package-run") != label:
                        raise RuntimeError("cleanup identity mismatch")
                    command(["docker", kind, "rm", *( ["--force"] if kind == "container" else []), identity])
            except Exception:
                failures.append(kind+":"+name)
    if failures:
        raise RuntimeError("cleanup unconfirmed for "+", ".join(failures))


def expected_refusal(label, error):
    """An unrelated transport failure must not count as an input refusal."""
    if label == "wrong-digest":
        return isinstance(error, LaunchError) and str(error) == "package digest differs from requested run"
    if label == "excess-steps":
        return isinstance(error, RunnerError) and error.code == "invalid_request" and str(error) == "invalid step or sampling bounds"
    prefix = "container command failed (1): "
    if not isinstance(error, LaunchError) or not str(error).startswith(prefix):
        return False
    try:
        value = json.loads(str(error)[len(prefix):])
        return value["completed"] is False and value["error"]["code"] == "missing_binding" and value["error"]["stage"] == "compilation"
    except (ValueError, KeyError, TypeError):
        return False


def run_fixture(image, sim_image, fixture, archive):
    suffix = uuid.uuid4().hex[:10]
    network = "generic-package-" + suffix
    nodes = [f"generic-package-{i}-{suffix}" for i in range(1, 4)]
    attempted = []
    user = f"{os.getuid()}:{os.getgid()}"
    package = json.loads((fixture/"world.json").read_bytes())
    package_hash = sha(fixture/"world.json")
    files = ["world.json", package["mujoco"]["path"]] + [a["path"] for a in package.get("assets", [])]
    before = {name: sha(fixture/name) for name in files}
    with tempfile.TemporaryDirectory(prefix="wendy-generic-site-") as tmp:
        folder = Path(tmp)
        downloads = folder/"downloads"
        downloads.mkdir(mode=0o700)
        try:
            command(["go", "run", "./cmd/world-lab", "--out", str(folder), "--runtime-dir", "/state",
                     "--containers", "--sensors", "--run-id", "generic-package-transfer"])
            command(["docker", "network", "create", "--internal", "--label", "wendy.world.generic-package-run="+suffix, network])
            for i, name in enumerate(nodes, 1):
                state = folder/f"state-{i}"
                state.mkdir(mode=0o700)
                attempted.append(name)
                command(["docker", "run", "-d", "--name", name, "--label", "wendy.world.generic-package-run="+suffix, "--network", network,
                         "--network-alias", f"world-node-{i}", "--user", user,
                         "--mount", f"type=bind,src={folder/f'node-{i}.json'},dst=/config.json,readonly",
                         "--mount", f"type=bind,src={state},dst=/state", image, "--config", "/config.json"])

            def cli(index, group, verb, *args):
                cli_name = "generic-cli-"+uuid.uuid4().hex
                attempted.append(cli_name)
                return json.loads(command(["docker", "run", "--rm", "--name", cli_name,
                    "--label", "wendy.world.generic-package-run="+suffix, "--network", network,
                    "--user", user, "--entrypoint", "/world", "--mount", f"type=bind,src={folder},dst=/lab,readonly",
                    "--mount", f"type=bind,src={fixture},dst=/fixture,readonly",
                    "--mount", f"type=bind,src={downloads},dst=/output", image, group, verb,
                    "--credentials", f"/lab/site-admin-{index}.json", *args, "--json"]))

            def until(fn, predicate):
                deadline = time.monotonic() + 20
                last = None
                while time.monotonic() < deadline:
                    try:
                        last = fn()
                        if predicate(last):
                            return last
                    except RuntimeError as error:
                        last = str(error)
                    time.sleep(.2)
                raise AssertionError(last)

            for index in (1, 3):
                until(lambda: cli(index, "site", "list", "--trust", "/lab/site-trust.json"), lambda r: r["verified"])
            site = json.loads((ROOT/"examples/geography/office.json").read_bytes())
            site["package_sha256"] = package_hash
            (folder/"site.json").write_bytes(canonical_bytes(site))
            published = cli(1, "package", "publish", "--site", "/lab/site.json", "--package", "/fixture/world.json",
                            "--run-id", "generic-package-transfer")
            digest = published["publication"]["sha256"]
            assert published["published"] is True and published["package_sha256"] == package_hash
            until(lambda: cli(3, "site", "fetch", "--trust", "/lab/site-trust.json", "--sha256", digest), lambda r: r["verified"])
            try:
                cli(3, "package", "fetch", "--trust", "/lab/site-trust.json", "--sha256", digest, "--out", "/output/missing")
            except RuntimeError as error:
                if "node HTTP 404:" not in str(error):
                    raise AssertionError("missing-blob case failed outside the evidence lookup") from error
            else:
                raise AssertionError("replica claimed missing bulk assets")
            assert not (downloads/"missing").exists()
            installed = cli(3, "package", "fetch", "--trust", "/lab/site-trust.json", "--sha256", digest,
                            "--blob-credentials", "/lab/site-admin-1.json", "--out", "/output/installed")
            assert installed["verified"] and installed["installed"] and installed["package_sha256"] == package_hash
            assert {name: sha(downloads/"installed"/name) for name in files} == before
            for name in nodes:
                command(["docker", "network", "disconnect", network, name])
            disconnected = json.loads(command(["docker", "inspect", *nodes]))
            assert len(disconnected) == len(nodes)
            assert {record["Name"].removeprefix("/") for record in disconnected} == set(nodes)
            assert all(not record["NetworkSettings"]["Networks"] for record in disconnected)

            request_path = fixture/"run.json"
            result_path = archive/(fixture.name+"-execution.json")
            execution = launch(downloads/"installed/world.json", request_path, sim_image, result_path)
            report = execution["result"]
            assert report["runtime"]["mujoco_version"] == "4.0.0"
            assert report["summary"]["completed_steps"] == 100
            if fixture.name == "generic-office":
                assert report["model"]["nu"] == report["model"]["nv"] == 0
                assert all(math.isclose(actual, expected, rel_tol=0, abs_tol=1e-12) for actual, expected in
                           zip(report["samples"][-1]["sensors"][0]["values"], [.6, .4, .55]))
            else:
                assert report["model"]["nu"] == report["model"]["nv"] == 2
                assert report["samples"][-1]["qpos"] != report["initial_state"]["qpos"]
                assert report["samples"][-1]["ctrl"] == [.3, -.2]

            failures = []
            for label, mutate in (
                ("wrong-digest", lambda r: r.update(package_sha256="0"*64)),
                ("missing-body", lambda r: r.update(body_names=["absent-body"])),
                ("missing-sensor", lambda r: r.update(sensor_names=["absent-sensor"])),
                ("missing-actuator", lambda r: r.update(control={"mode": "fixed", "values": [{"actuator": "absent", "value": 0}]})),
                ("excess-steps", lambda r: r.update(steps=10001)),
            ):
                request = json.loads(request_path.read_bytes())
                mutate(request)
                bad_request = folder/(label+".json")
                bad_request.write_bytes(canonical_bytes(request))
                bad_output = archive/(fixture.name+"-"+label+"-unexpected.json")
                try:
                    launch(downloads/"installed/world.json", bad_request, sim_image, bad_output)
                except ValueError as error:
                    if not expected_refusal(label, error):
                        raise AssertionError("run failed for an unrelated reason: "+label) from error
                    failures.append({"case": label, "error": str(error)[:512], "output_absent": not bad_output.exists()})
                else:
                    raise AssertionError("invalid run succeeded: "+label)
                assert not bad_output.exists()
            prior = sha(result_path)
            try:
                launch(downloads/"installed/world.json", request_path, sim_image, result_path)
            except ValueError as error:
                assert str(error) == "output must be a new file in an existing directory"
            else:
                raise AssertionError("prior execution overwritten")
            assert sha(result_path) == prior
            assert {name: sha(downloads/"installed"/name) for name in files} == before
            corrupted = folder/"corrupted"
            shutil.copytree(downloads/"installed", corrupted)
            (corrupted/package["mujoco"]["path"]).write_bytes(b"tampered")
            corrupt_output = archive/(fixture.name+"-corrupt-unexpected.json")
            try:
                launch(corrupted/"world.json", request_path, sim_image, corrupt_output)
            except ValueError as error:
                assert "model or asset digest mismatch" in str(error)
            else:
                raise AssertionError("corrupt model accepted")
            assert not corrupt_output.exists()
            return {"fixture": fixture.name, "package_sha256": package_hash, "site_publication_sha256": digest,
                "files_verified": before, "replicas_disconnected_before_execution": True,
                "metadata_repaired_to_replica": True, "bulk_fetched_from_explicit_origin": True,
                "missing_blob_fetch_left_no_installation": True, "installed_bytes_preserved": True,
                "prior_execution_preserved": True, "tampered_model_refused": True, "refusals": failures,
                "fixture_resources_removed": True,
                "execution_file": result_path.name, "execution_sha256": prior}
        finally:
            cleanup_resources(attempted, network, suffix)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", default="wendy-world-node:mujoco4")
    parser.add_argument("--sim-image", default="wendy-world-package:mujoco4")
    parser.add_argument("--out", required=True, type=Path)
    args = parser.parse_args()
    args.out = args.out.absolute()
    args.out.mkdir(mode=0o700, exist_ok=False)
    report = {"schema_version": "wendy.world.generic-package-verification.v1", "complete": False,
        "node_image": None, "simulation_image": None,
        "mode": "isolated Linux processes in existing Docker VM", "fixtures": [],
        "limitations": ["synthetic assets, no measured office or physical activation", "explicit blob origin, not automatic discovery",
                        "fixed controls, not a general robot controller or mission adapter", "resource limits are not a native-code security proof"]}
    try:
        # Resolve local immutable images before launching any networked fixture.
        report["node_image"] = local_image(args.image)
        report["simulation_image"] = local_image(args.sim_image)
        for name in ("generic-office", "articulated-twin"):
            report["fixtures"].append(run_fixture(report["node_image"], report["simulation_image"], ROOT/"examples"/name, args.out))
        report["complete"] = True
    except Exception as error:
        report["error"] = {"type": type(error).__name__, "message": str(error)[:512]}
        raise
    finally:
        (args.out/"report.json").write_bytes(canonical_bytes(report))
    print(json.dumps({"complete": True, "fixtures": len(report["fixtures"]), "report": str(args.out/"report.json")}))


if __name__ == "__main__":
    main()

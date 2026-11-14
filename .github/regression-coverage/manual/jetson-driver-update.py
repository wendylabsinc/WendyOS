#!/usr/bin/env python3
"""Prepare a physical NVIDIA driver-file rename/restart rehearsal for audit 2828.

No device is contacted. Inventory JSON requires cli_binary, agent_binary,
agent_source_commit, os_artifact (local update artifact), artifact_url (immutable
HTTPS URL for those exact bytes), target_os_version, recovery_record (local owner-
approved recovery instructions), old_driver_path and new_driver_path. board has
selector, serial, owner and device_info (saved device-info JSON path).
Use a reserved WendyOS AGX Orin or Thor, a candidate Agent installed before the
first deployment, and an OS update known to rename the selected NVIDIA library.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
from urllib.parse import urlsplit
import uuid

spec = importlib.util.spec_from_file_location("gpu_rehearsal_helpers", Path(__file__).with_name("jetson-gpu-rehearsal.py"))
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
ROOT = helpers.ROOT


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    artifacts = {key: helpers.artifact(raw[key]) for key in ("cli_binary", "agent_binary", "os_artifact", "recovery_record")}
    cli, agent = artifacts["cli_binary"]["path"], artifacts["agent_binary"]["path"]
    if not os.access(cli, os.X_OK):
        raise ValueError("cli_binary is not executable")
    source = helpers.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    url = helpers.line(raw["artifact_url"], "artifact_url")
    parsed = urlsplit(url)
    if parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.fragment or "latest" in parsed.path.lower():
        raise ValueError("use an immutable HTTPS artifact URL without userinfo or fragment")
    target = helpers.line(raw["target_os_version"], "target_os_version")
    board = {k: helpers.line(raw["board"][k], k) for k in ("selector", "serial", "owner")}
    info_file = helpers.artifact(raw["board"]["device_info"])
    info = json.loads(Path(info_file["path"]).read_text())
    kind = info.get("deviceType")
    expected_arch = {"jetson-agx-orin": "sm_87", "jetson-agx-thor": "sm_110"}
    if (kind not in expected_arch or info.get("gpuArch") != expected_arch[kind]
            or info.get("gpuVendor") != "nvidia" or info.get("os") != "wendyos"
            or info.get("cpuArchitecture") not in ("arm64", "aarch64") or board["selector"].startswith("-")):
        raise ValueError("requires an explicit WendyOS AGX Orin or Thor target")
    for key in ("version", "osVersion", "jetpackVersion", "cudaVersion"):
        helpers.line(info.get(key), key)
    if target == info["osVersion"]:
        raise ValueError("target OS version must differ from the starting version")
    paths = [helpers.line(raw[k], k) for k in ("old_driver_path", "new_driver_path")]
    if paths[0] == paths[1] or any(not p.startswith("/") or ".so" not in p or ".." in Path(p).parts for p in paths):
        raise ValueError("old/new NVIDIA library paths must be distinct absolute paths")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve(); out.mkdir(mode=0o700, parents=True, exist_ok=False)
    run_id = uuid.uuid4().hex[:8]
    cases, commands = [], {}
    all_artifacts = list(artifacts.values()) + [info_file]
    selector = board["selector"]
    for fixture in ("python-gpu", "python-onnx-gpu"):
        project = out / "projects" / fixture
        shutil.copytree(ROOT / ".github/ci-tests" / fixture, project)
        cfg = json.loads((project / "wendy.json").read_text())
        app = f"sh.wendy.driver-update.{run_id}.{fixture}"
        cfg["appId"] = app
        (project / "wendy.json").write_text(json.dumps(cfg, indent=2) + "\n")
        all_artifacts.extend(helpers.artifact(str(p)) for p in sorted(project.iterdir()) if p.is_file())
        dockerfile = "Dockerfile.cuda13" if kind == "jetson-agx-thor" else "Dockerfile"
        commands[fixture] = {
            "deploy": [cli, "--json", "run", "--device", selector, "--prefix", str(project), "--dockerfile", dockerfile, "--detach", "--no-restart", "--yes"],
            # No --detach: that flag would replace the no-restart policy.
            "restart": [cli, "--json", "device", "apps", "start", app, "--device", selector],
            "state": [cli, "--json", "device", "apps", "list", "--device", selector],
            "logs": [cli, "--json", "device", "logs", "--app", app, "--tail", "100", "--no-follow", "--device", selector],
            "remove": [cli, "device", "apps", "remove", app, "--force", "--device", selector],
        }
        for phase in ("before-update", "after-update", "second-restart"):
            cases.append({"id": fixture + "-" + phase, "command_group": fixture, "app_id": app,
                          "phase": phase, "status": "not_run", "container_exit_code": None,
                          "stdout_log": None, "state_log": None, "container_metadata": None,
                          "image_digest": None, "start_utc": None, "end_utc": None, "reviewer": None})
    commands["board"] = {
        "info": [cli, "--json", "device", "info", "--device", selector],
        "os-update": [cli, "--json", "device", "update", "--device", selector, "--binary", agent, "--artifact-url", url, "--yes"],
    }
    for phase, driver in (("before", paths[0]), ("after", paths[1])):
        console = ["#!/bin/sh", "set -eu", "date -u", "cat /etc/os-release",
                   "cdi_path=/var/run/cdi/nvidia.yaml",
                   'if [ ! -f "$cdi_path" ]; then cdi_path=/etc/cdi/nvidia.yaml; fi',
                   'sha256sum "$cdi_path"', 'cat "$cdi_path"',
                   shlex.join(["test", "-f", driver]), shlex.join(["sha256sum", driver])]
        if phase == "after":
            console.append(shlex.join(["test", "!", "-e", paths[0]]))
        for app in sorted({case["app_id"] for case in cases}):
            console.append(shlex.join(["ctr", "--namespace", "default", "containers", "info", app]))
        path = out / f"console-{phase}.sh"
        helpers.write(path, "\n".join(console) + "\n")
        all_artifacts.append(helpers.artifact(str(path)))
    evidence = {"audit_entries": ["2828"], "source_head": head, "agent_source_commit": source,
                "dirty_preview": dirty, "hardware_execution": "not_run", "board": board, "before_info": info,
                "target_os_version": target, "old_driver_path": paths[0], "new_driver_path": paths[1],
                "artifact_url": url, "artifacts": all_artifacts, "cases": cases,
                "update_result": None, "before_cdi": None, "after_cdi": None, "cleanup": None}
    for name, data in (("evidence.json", evidence), ("commands.json", commands)):
        helpers.write(out / name, json.dumps(data, indent=2) + "\n")
    timeout_line = 'limit = 1200 if stage == "deploy" else 30'
    if helpers.RUNNER.count(timeout_line) != 1:
        raise ValueError("shared runner timeout contract changed")
    runner = helpers.RUNNER.replace(timeout_line,
                                   'limit = {"deploy": 1200, "os-update": 1800, "restart": 120}.get(stage, 30)')
    helpers.write(out / "run-command.py", runner)
    run = shlex.join(["python3", str(out / "run-command.py")])
    helpers.write(out / "procedure.txt", f'''NVIDIA driver rename across WendyOS update, audit 2828
Source {head}; Agent {source}; dirty preview {dirty}
Physical serial {board['serial']}; selector {selector}; owner {board['owner']}
Starting OS {info['osVersion']}; target OS {target}
Old library {paths[0]}; new library {paths[1]}
OS artifact SHA-256 {artifacts['os_artifact']['sha256']}
Recovery record {artifacts['recovery_record']['path']}
All six compute observations and the update are NOT RUN.

Before any device operation
1. Regenerate from a clean committed checkout. Reserve this nonproduction board,
   physically confirm its serial and selector, and review the hashed recovery
   record with its owner. Require local maintenance-console access, stable power,
   enough disk for the update, saved configuration and a maintainer-approved
   rollback route. An unavailable recovery route blocks this rehearsal.
2. Verify the installed candidate Agent matches the binary/source SHA recorded
   here, before deployment. Confirm the update artifact is for this exact board
   and storage layout. Independently compare the bytes at artifact_url with the
   local artifact SHA-256; retain the download/build record. A filename or version
   string alone does not prove the artifact. Recheck immediately before update.
3. Use the physical maintenance console to capture current NVIDIA CDI JSON/YAML,
   driver package versions, library hashes and the local container metadata.
   Record the chosen old library as an actual CDI mount source in each deployed
   app. The source must exist before updating. This is not a fake filename rename.
   Keep console commands/output in the evidence bundle, including UTC and operator.
   The generated console-before.sh and console-after.sh contain read-only capture
   commands for the Agent's CDI search order, library hashes and both container
   records. Transfer them through the owner's approved maintenance process and
   run with sudo sh at the matching stage; save stdout/stderr on the operator host.
   Inspect them first. They assume the default containerd socket and namespace;
   if the installation differs, record the exact adjusted read-only command.

Before-update compute, for both python-gpu and python-onnx-gpu
4. Replace FIXTURE below with each exact command group. Deploy once only:
     {run} board info before
     {run} FIXTURE deploy before
     {run} FIXTURE state before-01
     {run} FIXTURE logs before
   Poll state every five seconds for at most 90 seconds using unique labels.
   Require the exact app_id STOPPED with exit 0 and a fresh fixture PASS line.
   PyTorch must synchronize and verify all 65536 elements equal 256. ONNX must
   report actual Mul events exclusively on CUDA and a correct doubled result.
   Save image digests, creation timestamps, snapshot keys and stored OCI specs
   via `sudo ctr --namespace default containers info APP_ID` at the local console.
   Verify the chosen old CDI source is in both stored specs. Record the real
   containerd address if the installation uses a nondefault socket/namespace.
   Keep both containers; do not remove, redeploy or edit their stored specs.
   Run console-before.sh now, after both apps exist and before the update.

Explicit OS-update boundary
5. Obtain the owner's explicit approval for this serial, target OS and artifact
   SHA. The next command updates the OS and may reboot; it also reapplies the
   pinned candidate Agent binary afterward. Execute only after that approval:
     {run} board os-update update
   Preserve all output and update status. The runner allows 30 minutes. A timeout
   does not mean the device stopped updating: monitor the local console and do
   not retry, disconnect power, flash storage or issue another update. Follow the
   recovery record with the owner if it does not recover within the approved window.
6. Reconnect to this exact board and capture:
     {run} board info after
   Require the target OS version, the same physical serial and the candidate Agent
   binary hash. Capture the regenerated host CDI spec and driver package versions.
   Verify the old path is absent, the new path exists and hashes match the target
   build, and the new source appears in the current CDI list. If names did not
   change, the regression was not exercised: record not_run, not pass.
   Run console-after.sh before restarting, then again after step 8. Preserve both
   captures so the stale stored spec and its subsequent repair can be compared.

Restart existing containers, twice each
7. Do not run deploy again. Start the existing app, keeping its no-restart policy:
     {run} FIXTURE restart after-update
     {run} FIXTURE state after-update
     {run} FIXTURE logs after-update
   Repeat with label second-restart. Each start must terminate within 120 seconds,
   with exit 0 and fresh completed CUDA computation matching step 4. Preserve
   timestamps so old successful logs cannot substitute for this run.
8. Capture container metadata again at the console. For each app require unchanged
   container ID, creation timestamp, snapshot key and image digest. The stored spec
   must omit the old driver source and contain the new one, without duplicate CDI
   mounts. A redeployed app, edited spec, manually restored old library, CPU fallback
   or merely successful start command cannot pass. Have a second reviewer compare
   before/after metadata, host CDI, hashes and all six compute observations.

Cleanup and evidence
9. Remove only these two disposable apps and confirm absence:
     {run} FIXTURE remove cleanup
     {run} FIXTURE state after-cleanup
   Do not delete shared images, volumes or builders. Record the final OS/Agent
   versions and return the board in the state agreed with its owner. Any rollback
   is a separate owner-approved action; never automate an erase or downgrade.
10. Fill each case's evidence fields plus update_result, before_cdi, after_cdi and
    cleanup. Keep failed attempts; a retry uses a new bundle. Set hardware_execution
    to passed only after the real rename, unchanged container identities, all six
    CUDA results and cleanup have been reviewed. This does not prove NPU behavior.
''')
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print(f"Prepared six unexecuted compute observations and one OS update in {args.output}")

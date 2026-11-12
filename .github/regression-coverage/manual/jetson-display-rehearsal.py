#!/usr/bin/env python3
"""Prepare physical display-only EGL checks for audit entry 1666; contact no device.

Run: python3 jetson-display-rehearsal.py --inventory inventory.json --output NEW_DIR
Inventory JSON has cli_binary (executable path), agent_source_commit (40 hex), and
board: {selector, serial, owner, device_info}. device_info names a saved JSON file
from `wendy --json device info --device SELECTOR`. Confirm the serial physically.
The generated procedure requires a reserved Jetson and host NVIDIA EGL/GLES CDI
provisioning. Missing graphics driver provisioning is blocked, never a pass.
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
import uuid

HELPER = Path(__file__).with_name("jetson-gpu-rehearsal.py")
spec = importlib.util.spec_from_file_location("gpu_rehearsal_helpers", HELPER)
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
ROOT = helpers.ROOT


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    cli = helpers.artifact(raw["cli_binary"])
    if not os.access(cli["path"], os.X_OK):
        raise ValueError("cli_binary is not executable")
    source = helpers.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    board = {key: helpers.line(raw["board"][key], key) for key in ("selector", "serial", "owner")}
    info_file = helpers.artifact(raw["board"]["device_info"])
    info = json.loads(Path(info_file["path"]).read_text())
    if (info.get("deviceType") not in ("jetson-orin-nx", "jetson-orin-nano", "jetson-agx-orin", "jetson-agx-thor")
            or info.get("gpuVendor") != "nvidia" or info.get("cpuArchitecture") not in ("arm64", "aarch64")
            or info.get("os") not in ("linux", "wendyos") or board["selector"].startswith("-")):
        raise ValueError("requires an explicit supported NVIDIA Jetson target")
    for key in ("version", "osVersion", "jetpackVersion", "gpuArch"):
        helpers.line(info.get(key), key)
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    run_id = uuid.uuid4().hex[:8]
    artifacts, cases, commands = [cli, info_file], [], {}
    for i, case in enumerate(("display-1", "display-2", "display-3", "no-display-control"), 1):
        project = out / "projects" / case
        shutil.copytree(Path(__file__).parent / "fixtures/jetson-display", project)
        cfg = json.loads((project / "wendy.json").read_text())
        app = f"sh.wendy.egl-rehearsal.{run_id}.{i}"
        cfg["appId"] = app
        if i == 4:
            cfg["entitlements"] = []
        (project / "wendy.json").write_text(json.dumps(cfg, indent=2) + "\n")
        artifacts.extend(helpers.artifact(str(p)) for p in sorted(project.iterdir()) if p.is_file())
        selector = board["selector"]
        commands[case] = {
            "info": [cli["path"], "--json", "device", "info", "--device", selector],
            "deploy": [cli["path"], "--json", "run", "--device", selector, "--prefix", str(project), "--dockerfile", "Dockerfile", "--detach", "--no-restart", "--yes"],
            "state": [cli["path"], "--json", "device", "apps", "list", "--device", selector],
            "logs": [cli["path"], "--json", "device", "logs", "--app", app, "--tail", "100", "--no-follow", "--device", selector],
            "remove": [cli["path"], "device", "apps", "remove", app, "--force", "--device", selector],
        }
        cases.append({"id": case, "app_id": app, "expect": "denied" if i == 4 else "nvidia_egl_readback",
                      "status": "not_run", "image_digest": None, "container_exit_code": None,
                      "start_utc": None, "end_utc": None, "stdout_log": None, "state_log": None,
                      "cleanup_log": None, "reviewer": None, "notes": None})
    evidence = {"audit_entries": ["1666"], "source_head": head, "agent_source_commit": source,
                "dirty_preview": dirty, "hardware_execution": "not_run", "board": board, "device_info": info,
                "artifacts": artifacts, "cases": cases, "driver_provisioning_record": None}
    for name, data in (("evidence.json", evidence), ("commands.json", commands)):
        helpers.write(out / name, json.dumps(data, indent=2) + "\n")
    helpers.write(out / "run-command.py", helpers.RUNNER)
    run = shlex.join(["python3", str(out / "run-command.py")])
    helpers.write(out / "procedure.txt", f'''Display-only Jetson EGL, audit 1666
Source {head}; candidate Agent {source}; dirty preview {dirty}
Target {board['selector']}; physical serial {board['serial']}; owner {board['owner']}
This bundle contains four NOT RUN cases. A prepared script is not a hardware pass.

Prerequisites and trigger
Run on changes to display entitlements, NVIDIA CDI injection or graphics driver
provisioning. Reserve this nonproduction board, physically verify its serial and
selector, and obtain its owner's agreement to deploy four disposable apps. Do
not operate robot actuators or change another app. Save fresh device info and
match Agent/OS/JetPack/GPU versions to evidence.json. Keep the installed Agent's
build record proving the candidate SHA. Regenerate from a clean checkout; a
changed binary, fixture, board or OS requires a new bundle.

The image supplies GLVND dispatch only. The host must supply NVIDIA EGL/GLES
libraries and its GLVND vendor JSON through CDI. The original fix changed the
Agent injection gate; an older compute-only OS CDI list is insufficient. Have
the board maintainer capture the current nvidia-ctk/CDI specification and graphics
package versions from the local maintenance console, with source/destination
paths and file hashes. Link this in driver_provisioning_record. Check that the
NVIDIA EGL/GLES implementations and vendor JSON are present. If absent, record
blocked with that evidence; request the appropriate OS image from its maintainer.
Do not add a gpu entitlement, mount host libraries manually, install a container
NVIDIA driver, or accept Mesa/software rendering to make this test pass.

Run each case named in commands.json, in order
1. Use these exact generated commands, substituting a case id for CASE. The
   runner pins the CLI and project hashes, captures stdout/stderr/return code,
   and times out deployment after 20 minutes and other commands after 30 seconds.
     {run} CASE info before
     {run} CASE deploy deploy
   Save the resolved base and built image digests from the build record. Record
   UTC and operator. A deployment success alone never establishes a result.
2. Poll state with a new label every five seconds, for at most 90 seconds:
     {run} CASE state state-01
   Match the exact case app_id, require STOPPED and an explicit exit code, then:
     {run} CASE logs final
   Missing/still-running/crash-looping containers and missing exit codes cannot
   pass. Confirm timestamps belong to this run. The image runs one bounded probe;
   --no-restart prevents automatic retries from hiding a failed first attempt.
3. Each display case must have ONLY the display entitlement, exit 0, NVIDIA EGL
   and GL vendors, a nonempty NVIDIA renderer, and the exact line:
     PASS: display-only NVIDIA EGL rendered and verified 512 pixels
   All red and green framebuffer pixels must match and EGL cleanup must succeed.
   A vendor string, successful context creation or file mount alone is not enough.
   Three fresh app identities must all pass. No physical monitor/window is used;
   this tests graphics driver execution, not Wayland presentation or UI behavior.
4. The no-display-control has no entitlements and must exit nonzero after printing
   Starting display-only NVIDIA EGL probe. It must fail to obtain an NVIDIA EGL
   display/context, with no PASS line. Build failure, missing dispatch libraries,
   loader failure before main, network failure or timeout is not access denial.
   Compare image digests with the positives; only app identity/entitlements differ.
5. Remove only the generated app and verify its absence:
     {run} CASE remove cleanup
     {run} CASE state after-cleanup
   Preserve the cleanup result. Do not remove shared images, volumes or builders.
6. Fill every case's evidence fields, link captured files and have a second
   reviewer inspect target identity, installed build, image digest, NVIDIA pixel
   output, negative control and cleanup. Keep failures; retries use a new bundle.
   Set hardware_execution to passed only when all four cases meet these criteria.

Recovery
On disconnect or timeout, stop deployment, reconnect to this exact serial/selector,
capture app state and remove only remaining bundle app IDs. Hand unresolved IDs
to the owner. Do not flash, reboot or change host permissions as automatic recovery.
Restore the owner's agreed state before releasing the board.
''')
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print(f"Prepared four unexecuted display cases in {args.output}")

#!/usr/bin/env python3
"""Prepare vendor device-renumbering checks without contacting hardware.

Inventory requires cli_binary, agent_binary, agent_source_commit, old_os_artifact,
new_os_artifact, artifact_url (immutable HTTPS for new bytes), recovery_record,
transition_record and board {selector, serial, owner, device_info}.
transition_record is a local JSON qualification from a previously observed,
owner-approved WendyOS update on this exact board, using the schema below. No
known repeatable transition means the hardware case remains pending; this
program will not invent module unload/rebind commands or a renumbering result.

transition_record schema (all fields required):
  schema: wendy.accelerator-transition.v1; method: wendy-os-update
  serial, device_type: match the reserved board
  approved_by, observed_utc: owner approval and previous qualification time
  from_os_version, to_os_version, from_os_sha256, to_os_sha256
  from_driver_version, to_driver_version, from_driver_sha256, to_driver_sha256
  host_device_path, container_device_path, driver_library: exact absolute paths
  before_device, after_device: different [major, minor] decimal integer pairs
  observation_log: local file with the prior update/device observations
  observation_sha256: SHA-256 of that exact log
OS artifact hashes must match the supplied local bytes. Driver hashes describe
that same host library before/after the qualified update and may be unchanged;
actual device numbers must differ. No record is supplied or vouched for here.

Qualcomm additionally requires qualcomm_checkout, endpoint and two images as in
qualcomm-npu-rehearsal.py. The approved QCS9075 Qwen project is read from its pinned
Git objects. NVIDIA uses the repository's actual CUDA-profiled ONNX model.
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
import sys
from urllib.parse import urlsplit
import uuid


def module(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


helpers = module("renumber_helpers", "jetson-gpu-rehearsal.py")
npu = module("renumber_npu", "qualcomm-npu-rehearsal.py")
ROOT = helpers.ROOT
MESH_EVIDENCE = "https://github.com/wendylabsinc/WendyOS/actions/runs/43294052121/job/131447498192"


def absolute_path(value, name):
    value = helpers.line(value, name)
    if not value.startswith("/") or ".." in Path(value).parts or str(Path(value)) != value:
        raise ValueError(name + " must be a normalized absolute path")
    return value


def qualification(raw, board, info, artifacts):
    record = json.loads(Path(artifacts["transition_record"]["path"]).read_text())
    if (record.get("schema") != "wendy.accelerator-transition.v1"
            or record.get("method") != "wendy-os-update"
            or record.get("serial") != board["serial"]
            or record.get("device_type") != info["deviceType"]):
        raise ValueError("requires a qualified WendyOS transition for this exact board")
    for field in ("approved_by", "observed_utc", "from_driver_version", "to_driver_version", "to_os_version"):
        helpers.line(record.get(field), field)
    if record.get("from_os_version") != info["osVersion"] or record["to_os_version"] == info["osVersion"]:
        raise ValueError("qualified transition must start on the recorded OS and change its version")
    for phase, artifact in (("from", "old_os_artifact"), ("to", "new_os_artifact")):
        if record.get(phase + "_os_sha256") != artifacts[artifact]["sha256"]:
            raise ValueError("qualified OS artifact hash does not match local bytes")
        digest = record.get(phase + "_driver_sha256", "")
        if not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise ValueError("qualified host driver library SHA-256 is required")
    if record["from_os_sha256"] == record["to_os_sha256"]:
        raise ValueError("old and new OS artifacts must differ")
    for phase in ("before", "after"):
        pair = record.get(phase + "_device")
        if (not isinstance(pair, list) or len(pair) != 2
                or any(type(n) is not int or n < 0 or n > 2**32-1 for n in pair)):
            raise ValueError("device numbers must be two nonnegative decimal integers")
    if record["before_device"] == record["after_device"]:
        raise ValueError("unchanged device numbers do not qualify a renumbering transition")
    for key in ("host_device_path", "container_device_path", "driver_library"):
        record[key] = absolute_path(record.get(key), key)
    if not record["container_device_path"].startswith("/dev/"):
        raise ValueError("container device path must remain under /dev")
    if not re.fullmatch(r"/(?:usr/lib(?:64)?|run/wendyos/npu/lib)/.+\.so(?:\.[A-Za-z0-9._-]+)?", record["driver_library"]):
        raise ValueError("driver_library must name the qualified host shared library")
    host = record["host_device_path"]
    if info["deviceType"] == "dragonwing-iq-9075":
        if not re.fullmatch(r"/dev/fastrpc-[A-Za-z0-9_-]+", host) or host.endswith("-secure"):
            raise ValueError("requires an entitled non-secure FastRPC device")
    elif not re.fullmatch(r"/dev/(nvidia[A-Za-z0-9_-]*|nvhost-[A-Za-z0-9_-]+|nvgpu/[A-Za-z0-9_-]+)", host):
        raise ValueError("requires the actual NVIDIA vendor node from the saved GPU spec")
    # A narrative alone cannot establish which previous transition was observed.
    observed = helpers.artifact(record["observation_log"])
    if observed["sha256"] != record.get("observation_sha256"):
        raise ValueError("transition observation log hash does not match")
    artifacts["transition_observation"] = observed
    return record


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    keys = ("cli_binary", "agent_binary", "old_os_artifact", "new_os_artifact", "recovery_record", "transition_record")
    artifacts = {k: helpers.artifact(raw[k]) for k in keys}
    cli = artifacts["cli_binary"]["path"]
    if not os.access(cli, os.X_OK):
        raise ValueError("cli_binary is not executable")
    source = helpers.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    board = {k: helpers.line(raw["board"][k], k) for k in ("selector", "serial", "owner")}
    if board["selector"].startswith("-"):
        raise ValueError("board selector cannot be an option")
    artifacts["device_info"] = helpers.artifact(raw["board"]["device_info"])
    info = json.loads(Path(artifacts["device_info"]["path"]).read_text())
    kind = info.get("deviceType")
    gpu = {"jetson-agx-orin": "sm_87", "jetson-agx-thor": "sm_110"}
    if (kind not in (*gpu, "dragonwing-iq-9075") or info.get("os") != "wendyos"
            or info.get("cpuArchitecture") not in ("arm64", "aarch64")):
        raise ValueError("requires WendyOS IQ-9075, AGX Orin or AGX Thor")
    for key in ("version", "osVersion"):
        helpers.line(info.get(key), key)
    is_npu = kind == "dragonwing-iq-9075"
    if not is_npu:
        if info.get("gpuVendor") != "nvidia" or info.get("gpuArch") != gpu[kind]:
            raise ValueError("NVIDIA GPU architecture does not match the board")
        for key in ("jetpackVersion", "cudaVersion"):
            helpers.line(info.get(key), key)
    transition = qualification(raw, board, info, artifacts)
    url = helpers.line(raw["artifact_url"], "artifact_url")
    parsed = urlsplit(url)
    if (parsed.scheme != "https" or not parsed.hostname or parsed.username or parsed.password
            or parsed.query or parsed.fragment or "latest" in parsed.path.lower()):
        raise ValueError("use an immutable HTTPS artifact URL without credentials/query/fragment")
    files, images, endpoint = None, [], None
    if is_npu:
        files = npu.pinned_project(Path(raw["qualcomm_checkout"]).resolve(strict=True))
        endpoint = helpers.line(raw["endpoint"], "endpoint").rstrip("/")
        target = urlsplit(endpoint)
        if (target.scheme != "http" or not target.hostname or target.port != 9110 or target.path
                or target.username or target.password or target.query or target.fragment):
            raise ValueError("requires the exact reserved board's direct HTTP endpoint on port 9110")
        for item in raw["images"]:
            image = helpers.artifact(item["path"])
            if Path(image["path"]).stat().st_size > 8 * 1024 * 1024:
                raise ValueError("image exceeds 8 MiB")
            images.append(dict(image, expected=helpers.line(item["expected"], "visual facts")))
        if len(images) != 2 or images[0]["sha256"] == images[1]["sha256"]:
            raise ValueError("requires two distinct visual fixtures")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve(); out.mkdir(mode=0o700, parents=True, exist_ok=False)
    app = "sh.wendy.renumber." + uuid.uuid4().hex[:8]
    project = out / "project"
    if is_npu:
        project.mkdir(mode=0o700)
        for path, data in files.items():
            destination = project / path
            destination.parent.mkdir(parents=True, exist_ok=True)
            destination.write_bytes(data); destination.chmod(0o600)
    else:
        shutil.copytree(ROOT / ".github/ci-tests/python-onnx-gpu", project)
    config = json.loads((project / "wendy.json").read_text())
    config["appId"] = app
    volume = None
    if is_npu:
        persist = [e for e in config["entitlements"] if e["type"] == "persist"]
        if len(persist) != 1 or persist[0]["path"] != "/data":
            raise ValueError("pinned Qwen persistence contract changed")
        volume = "renumber-models-" + app.rsplit(".", 1)[1]
        persist[0]["name"] = volume
    (project / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
    selector = board["selector"]
    cli_command = lambda *args: [cli, "--json", *args, "--device", selector]
    deploy = cli_command("run", "--prefix", str(project), "--detach", "--no-restart", "--yes")
    if not is_npu:
        deploy += ["--dockerfile", "Dockerfile.cuda13" if kind == "jetson-agx-thor" else "Dockerfile"]
    start = cli_command("device", "apps", "start", app)
    if is_npu: start.append("--detach")
    commands = {"app": {"deploy": deploy, "start": start,
                "stop": cli_command("device", "apps", "stop", app),
                "state": cli_command("device", "apps", "list"),
                "logs": cli_command("device", "logs", "--app", app, "--tail", "200", "--no-follow"),
                "remove": [cli, "device", "apps", "remove", app, "--force", "--device", selector]},
                "board": {"info": cli_command("device", "info"),
                # Artifact updates explicitly reject JSON mode, which skips OS
                # updates. The shared runner still records bounded text output.
                "update": [cli, "device", "update", "--binary", artifacts["agent_binary"]["path"],
                           "--artifact-url", url, "--yes", "--device", selector]}}
    if is_npu:
        helpers.write(out / "probe.py", npu.PROBE)
        for stage in ("status", "report", "log"):
            commands["app"][stage] = [sys.executable, str(out / "probe.py"), endpoint, stage]
        for i, image in enumerate(images, 1):
            commands["app"][f"infer-{i}"] = [sys.executable, str(out / "probe.py"), endpoint, "infer", image["path"]]
    console = ["#!/bin/sh", "set -eu", "date -u", "cat /proc/sys/kernel/random/boot_id", "cat /etc/os-release", "uname -a",
               shlex.join(["test", "-c", transition["host_device_path"]]),
               shlex.join(["stat", "-Lc", "%n %F %t:%T %a", transition["host_device_path"]]),
               shlex.join(["test", "-f", transition["driver_library"]]),
               shlex.join(["sha256sum", transition["driver_library"]]),
               shlex.join(["ctr", "--namespace", "default", "containers", "info", app]),
               'for cdi in /var/run/cdi/nvidia.yaml /etc/cdi/nvidia.yaml; do',
               '  if [ -f "$cdi" ]; then sha256sum "$cdi"; cat "$cdi"; fi', "done"]
    helpers.write(out / "console-capture.sh", "\n".join(console) + "\n")
    evidence = {"audit_entries": ["2540", "2554"] if is_npu else ["1888"],
                "source_head": head, "dirty_preview": dirty, "agent_source_commit": source,
                "hardware_execution": "not_run", "board": board, "device_info": info,
                "transition_execution": {"status": "not_run", "update_log": None, "before_boot_id": None, "after_boot_id": None},
                "qualification": transition, "qualification_review": "pending_human_review",
                "app_id": app, "model_volume": volume, "endpoint": endpoint, "images": images,
                "companion_commit": npu.COMPANION_COMMIT if is_npu else None,
                "mesh_2554": {"status": "verified_independently", "evidence": MESH_EVIDENCE,
                              "scope": "live duplicate admission only; no vendor device execution"},
                "cases": [{"id": phase, "status": "not_run", "host_device": None, "stored_spec": None,
                           "container_identity": None, "inference_evidence": None, "reviewer": None}
                          for phase in ("before-transition", "renumbered-restart", "second-restart")],
                "missing_device_observation": None, "cleanup": None,
                "artifacts": list(artifacts.values()) + images}
    evidence["artifacts"] += [helpers.artifact(str(p)) for p in sorted(project.rglob("*")) if p.is_file()]
    evidence["artifacts"].append(helpers.artifact(str(out / "console-capture.sh")))
    if is_npu: evidence["artifacts"].append(helpers.artifact(str(out / "probe.py")))
    helpers.write(out / "commands.json", json.dumps(commands, indent=2) + "\n")
    evidence["artifacts"].append(helpers.artifact(str(out / "commands.json")))
    helpers.write(out / "evidence.json", json.dumps(evidence, indent=2) + "\n")
    needle = 'limit = 1200 if stage == "deploy" else 30'
    if helpers.RUNNER.count(needle) != 1: raise ValueError("shared runner contract changed")
    helpers.write(out / "run-command.py", helpers.RUNNER.replace(needle,
                  'limit = 1200 if stage == "deploy" else (1800 if stage == "update" else (180 if stage.startswith("infer-") else 120))'))
    run = shlex.join([sys.executable, str(out / "run-command.py")])
    helpers.write(out / "procedure.txt", procedure(evidence, artifacts, run, is_npu))
    return evidence


def procedure(e, a, run, is_npu):
    q, board = e["qualification"], e["board"]
    compute = (f'''Reserve exclusive NPU access and TCP 9110; confirm {e['endpoint']} reaches this serial.
   The pinned QCS9075 Qwen model is {npu.MODEL_URL}; record downloaded model hashes,
   built image digest, image QAIRT/Genie/QNN hashes and framework versions. Require
   ready status, two fresh answers matching {json.dumps([i['expected'] for i in e['images']])},
   and local-console evidence of the current Genie process's QNN HTP mappings and
   FastRPC descriptor. Record PID/start time, /proc/PID/maps and /proc/PID/fd; hash
   mapped libraries through /proc/PID/root. No CPU fallback or old answer can pass.
     {run} app status PHASE
     {run} app infer-1 PHASE
     {run} app infer-2 PHASE
     {run} app report PHASE
   Retain the report and model provenance on every phase. Start --detach is used
   for this HTTP service and enables unless-stopped; explicitly stop before the
   OS transition, between restart observations and during cleanup.''' if is_npu else '''The exact generated ONNX model multiplies ten ones by two. Require exit 0,
   fresh "PASS: ONNX GPU inference verified", output twos, and profiled Mul events
   exclusively on CUDAExecutionProvider. Available-provider listings alone cannot
   pass. Save framework versions and resolved image digest; no CPU fallback.
   Start is attached so the existing no-restart policy is preserved.''')
    return f'''Vendor accelerator device-number repair; hardware execution NOT RUN
Source {e['source_head']}; candidate Agent {e['agent_source_commit']}
Exact board {board['serial']}; selector {board['selector']}; owner {board['owner']}
Disposable app {e['app_id']}; model volume {e['model_volume']}
Qualified transition {q['from_os_version']} -> {q['to_os_version']}
Host path {q['host_device_path']}; container path {q['container_device_path']}
Decimal device pair {q['before_device']} -> {q['after_device']}
Host driver library {q['driver_library']}
Old/new driver versions {q['from_driver_version']} -> {q['to_driver_version']}
Old/new driver hashes {q['from_driver_sha256']} -> {q['to_driver_sha256']}
Old/new OS hashes {q['from_os_sha256']} -> {q['to_os_sha256']}
Qualification log {a['transition_observation']['path']}; recovery {a['recovery_record']['path']}

1. Preparation: regenerate from a clean committed checkout. Reserve the exact
   nonproduction board, stable power and local maintenance-console access. Have
   its owner review the hashed qualification log: this exact supported WendyOS
   update must previously have changed this path's real device numbers. A generic
   reboot or guessed module reload is not a qualified transition. Missing prior
   evidence leaves the case pending; do not make up expected numbers. Confirm
   rollback artifacts, storage layout, saved settings and the owner's recovery
   route. The generator did not approve or execute an update.
   Verify physical serial, saved device info and installed candidate Agent bytes
   against this bundle. Verify both OS artifact hashes and independently compare
   the bytes at artifact_url against the new local artifact hash. Keep that record.
2. Deploy this disposable app ONCE, then observe actual compute:
     {run} board info before
     {run} app deploy before
     {run} app state before
     {run} app logs before
   {compute}
   PHASE is a fresh filename-safe label such as before, after or second; repeated
   polls need unique labels. Bound readiness/state polling to five minutes.
3. Transfer console-capture.sh through the approved maintenance channel and run
   `sudo sh console-capture.sh` after deployment. Save its stdout/stderr locally;
   it is read-only and assumes the default containerd address/namespace. Record
   any exact installation-specific address adjustment. stat prints hexadecimal
   major:minor; convert to decimal before comparing with the pairs above.
   Require the selected real character device in the stored vendor-injected OCI
   bindings: sh.wendy/pinned-devices records path/hostPath/type/major/minor, and
   linux.resources.devices has the exact corresponding allow rule. Also save the
   actual GPU linux.devices entry or NPU bind mount. Record all injected driver
   mount sources, their hashes, CDI provenance where present, container ID,
   CreatedAt, SnapshotKey, Image digest and the unique model volume. A legacy
   cgroup-only spec with no source provenance is ineligible; do not patch it.
4. Stop only this app and verify no task/owner remains before updating:
     {run} app stop before-update
     {run} app state before-update
   At the console, `sudo ctr --namespace default tasks list` must show no running
   task for this app; preserve its stopped container record. Require persisted
   label sh.wendy/stopped-by-user=true (and record sh.wendy/restart.policy), so
   Agent boot cannot start the app and repair its spec before observation.
   Obtain the owner's explicit approval for this serial and exact new OS hash:
     {run} board update transition
   This may reboot and reapplies the pinned Agent. A command timeout does not mean
   the update stopped: observe the maintenance console, do not repeat the update
   or cut power, and follow the reviewed recovery route with the owner.
5. After recovery, verify the same physical board, target OS/driver versions,
   candidate Agent hash and new boot ID. BEFORE starting the app, capture info and
   console-capture.sh again:
     {run} board info after-update
   Require the host pair changed exactly as qualified, matching new driver hash,
   while the SAME stopped container still has the OLD injected pair/grant. Its ID,
   creation time, snapshot, image and volume must be unchanged. Preserve stale
   OCI annotations, node entries/cgroup rules, and injected driver mounts as
   evidence. If numbers did not change, or the spec was already recreated or
   repaired, record not_run: a successful ordinary reboot proves no renumbering.
   Do not edit OCI specs/CDI, mknod, chmod devices, swap symlinks, unbind drivers or
   rename libraries to manufacture the transition. Library-path rename is the
   separate audit 2828 procedure; it cannot substitute for these changed numbers.
6. Start the retained app; never deploy again:
     {run} app start after
     {run} app state after
     {run} app logs after
   Repeat the actual compute observations in step 2 with fresh after labels.
   Capture console-capture.sh again. Require persisted pins, applicable device
   entries and exact cgroup grants now match the host; the stale grant must be
   removed unless another independently recorded device still legitimately uses
   that pair. No wildcard permission expansion. Preserve bind destination/source,
   image, ID, CreatedAt, snapshot and volume. Save the Agent's timestamped
   "Re-resolved stale host device numbers before start" log when emitted.
   If the qualified node is genuinely absent, start must refuse before model
   execution and name the missing host path; retain that failure, unchanged spec
   and absent task evidence. Do not delete a node to force this case. Restore via
   the approved recovery route; an absence test is separately recorded and cannot
   satisfy the successful renumbering case.
7. Stop the disposable app, then start it a second time without redeployment:
     {run} app stop before-second
     {run} app start second
     {run} app state second
     {run} app logs second
   Repeat compute and console capture with second labels. Require the already
   repaired bindings to remain correct and the same retained identity/snapshot.
   Successful CLI acceptance or an old PASS log is insufficient.
8. Stop/remove only this bundle's app and verify absence:
     {run} app stop cleanup
     {run} app remove cleanup
     {run} app state after-cleanup
   Do not delete shared images, production models, volumes, builders or another
   app. Keep this unique model volume unless its owner separately approves its
   deletion. Return OS/Agent state through the agreed recovery process; no
   automatic downgrade or erase. Record cleanup and reviewer evidence for all
   three cases. Hardware stays not_run until the actual changed device pair,
   naturally stale saved spec, repaired same-container grants and new inference
   outputs have been independently reviewed. A failed attempt needs a new bundle.

Audit 2554's live mesh duplicate-admission half is already verified separately:
{MESH_EVIDENCE}
That result does not execute a vendor NPU/GPU. This procedure neither repeats nor
changes its mesh verdict, and no offline generator test constitutes hardware evidence.
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print("Prepared three NOT RUN device observations; qualification still requires human review")

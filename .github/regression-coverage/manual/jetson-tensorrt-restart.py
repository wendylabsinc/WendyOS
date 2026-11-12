#!/usr/bin/env python3
"""Prepare audit 2549's physical TensorRT cache/reboot rehearsal without device I/O.

Inventory: cli_binary, agent_binary, agent_source_commit, model_onnx,
recovery_record, inference_root, camera_source, and board with selector, serial,
owner and device_info (saved Wendy device-info JSON). Use a reserved Orin Nano
8GB with JetPack 7.2, CUDA 13 and TensorRT 10.16.2. Model bytes must be the pinned
webnn/yolo11n revision below. Console maintenance and WAN isolation are manual.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import uuid

spec = importlib.util.spec_from_file_location("gpu_helpers", Path(__file__).with_name("jetson-gpu-rehearsal.py"))
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
ROOT = helpers.ROOT
MODEL_REVISION = "9c5acfdd74aaff2d0f47c51b878506361039a51f"


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    artifacts = [helpers.artifact(raw[k]) for k in ("cli_binary", "agent_binary", "model_onnx", "recovery_record")]
    cli = artifacts[0]["path"]
    if not os.access(cli, os.X_OK):
        raise ValueError("cli_binary must be executable")
    source = helpers.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    board = {k: helpers.line(raw["board"][k], k) for k in ("selector", "serial", "owner")}
    info_artifact = helpers.artifact(raw["board"]["device_info"])
    info = json.loads(Path(info_artifact["path"]).read_text())
    if (info.get("deviceType") != "jetson-orin-nano" or info.get("gpuArch") != "sm_87"
            or info.get("gpuVendor") != "nvidia" or info.get("cpuArchitecture") not in ("arm64", "aarch64")
            or info.get("os") != "wendyos" or not str(info.get("jetpackVersion", "")).startswith("7.2")
            or not str(info.get("cudaVersion", "")).startswith("13.") or board["selector"].startswith("-")):
        raise ValueError("requires a WendyOS Orin Nano with JetPack 7.2 and CUDA 13")
    for key in ("version", "osVersion"):
        helpers.line(info.get(key), key)
    camera = helpers.line(raw["camera_source"], "camera_source")
    if camera == "*" or camera.startswith("-"):
        raise ValueError("camera_source must be one discovered stable camera ID")
    inference_root = helpers.line(raw["inference_root"], "inference_root")
    if not inference_root.startswith("/") or ".." in Path(inference_root).parts or inference_root == "/":
        raise ValueError("inference_root must be the actual absolute Agent inference directory")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    name = "trt-restart-" + uuid.uuid4().hex[:8]
    campaign = out / "campaign.yaml"
    helpers.write(campaign, f'''version: 1
name: {name}
sources:
  - camera: {json.dumps(camera)}
capture:
  buffer: 2s
  drain: 1s
  after_trigger: 3s
  triggers:
    - event: regression_person
upload:
  when: manual
export:
  annotation: cvat
inference:
  backend: yolo_onnx
  accelerator: auto
  model: webnn/yolo11n
  revision: {MODEL_REVISION}
  model_file: onnx/yolo11n.onnx
  labels: [person]
  threshold: 0.5
  rate: 2
  event: regression_person
  clear_after: 2s
  cooldown: 10s
  enabled: true
''')
    selector = board["selector"]
    command = lambda *args: [cli, "--json", *args, "--device", selector]
    commands = {"board": {
        "info": command("device", "info"),
        "sources": command("data", "sources"),
        "campaigns": command("data", "campaign", "list"),
        "episodes": command("data", "episodes"),
    }, "campaign": {
        "deploy": command("data", "campaign", "deploy", str(campaign), "--skip-cloud-registration"),
        "inspect": command("data", "campaign", "inspect", name),
        "disarm": command("data", "campaign", "disarm", name),
        "remove": command("data", "campaign", "remove", name),
    }}
    capture = out / "capture-cache.py"
    helpers.write(capture, CACHE_CAPTURE)
    console = ["#!/bin/sh", "set -eu", "date -u", "cat /proc/sys/kernel/random/boot_id",
               "cat /etc/os-release", "nvpmodel -q", "free -m",
               "systemctl show wendy-agent --property=MainPID --property=ExecMainStartTimestamp --property=ExecStart",
               "dpkg-query -W 'libnvinfer*' 'libcudart*'",
               "ps -eo pid,ppid,lstart,args | grep -E '[w]endy-agent|[i]nference/.*/worker.py|[a]ccelerator.py'",
               "journalctl -u wendy-agent -b --no-pager -o short-iso"]
    helpers.write(out / "console-capture.sh", "\n".join(console) + "\n")
    artifacts.extend([info_artifact, helpers.artifact(str(campaign)), helpers.artifact(str(capture)),
                      helpers.artifact(str(out / "console-capture.sh"))])
    cases = [{"id": phase, "status": "not_run", "inspect_samples": [], "cache_capture": None,
              "console_capture": None, "boot_id": None, "agent_pid": None, "worker_pid": None,
              "episode_id": None, "episode_evidence": None, "reviewer": None}
             for phase in ("baseline", "agent-restart", "online-reboot", "offline-reboot")]
    evidence = {"audit_entries": ["2549"], "source_head": head, "dirty_preview": dirty,
                "agent_source_commit": source, "board": board, "device_info": info,
                "campaign": name, "camera_source": camera, "inference_root": inference_root,
                "model_revision": MODEL_REVISION, "model_sha256": artifacts[2]["sha256"],
                "artifacts": artifacts, "cases": cases, "hardware_execution": "not_run", "cleanup": None}
    helpers.write(out / "evidence.json", json.dumps(evidence, indent=2) + "\n")
    helpers.write(out / "commands.json", json.dumps(commands, indent=2) + "\n")
    helpers.write(out / "run-command.py", helpers.RUNNER)
    run = shlex.join(["python3", str(out / "run-command.py")])
    capture_command = shlex.join(["sudo", "python3", "capture-cache.py", "--inference-root", inference_root])
    helpers.write(out / "procedure.txt", f'''TensorRT cached-engine restart rehearsal, audit 2549
Source {head}; candidate Agent {source}; dirty preview {dirty}
Reserved serial {board['serial']}; selector {selector}; owner {board['owner']}
Campaign {name}; camera {camera}
All four observations are NOT RUN. Preparation does not contact a device.

Prerequisites and identity
1. Regenerate from a clean committed checkout. Reserve an Orin Nano 8GB with
   JetPack 7.2, CUDA 13, libnvinfer 10.16.2 and a healthy USB/CSI camera. Confirm
   the board's physical serial, RAM, device selector and installed candidate
   Agent binary SHA against the hashed local binary. A version string alone is
   insufficient. No other campaigns or workloads may be disrupted by restart.
   Review {artifacts[3]['path']} with the owner, including console recovery and
   the exact WAN isolation/reconnection method. Keep LAN and local console access.
   No flashing, package upgrades or cache deletion are part of this procedure.
2. Confirm camera_source is the exact stable ID in current discovery, then check
   that no existing campaign has the generated name. These are read-only commands:
     {run} board info before
     {run} board sources before
     {run} board campaigns before
   Use a non-sensitive scene with a person who agreed to be recorded. This plan
   captures short episodes locally and does not enable upload or notifications.
3. Confirm the actual inference directory from the Agent's startup log/config;
   it may remain at the legacy path after a data-store migration. It must equal
   {inference_root}. Record the managed Python/runtime and binding paths. Confirm
   the local model artifact is webnn/yolo11n at {MODEL_REVISION}, onnx/yolo11n.onnx.
   Compare its SHA-256 with the cached model used by the worker and the reported
   accelerator.model_sha256. Do not substitute another model or a moving revision.

Establish a working cached engine
4. Deploy once while WAN is available:
     {run} campaign deploy baseline
     {run} campaign inspect baseline-000
   Inspect every ten seconds with a unique label for at most 90 minutes. First
   install/build may need a 20-minute jitter, 30-minute CPU wait and 30-minute
   build. Require at least 3 GiB MemAvailable before building. If it still cannot
   build, preserve the waiting/error state and stop; do not remove the memory gate.
   Keep a person in view for parity. Require inference_status.state running,
   accelerator.active tensorrt, precision fp16, parity.passed true with frames > 0,
   engine.state ready, and no degraded/error status. Observe at least six inspect
   samples ten seconds apart with sources detecting and frames_scored increasing.
   A successful deploy, ready flag or advertised provider alone is insufficient.
5. Transfer capture-cache.py and console-capture.sh to a private evidence directory
   at the local maintenance console through the owner's approved process. Inspect
   them first; they only read device state. Run `sudo sh console-capture.sh` and:
     {capture_command} --key-prefix ENGINE_KEY_FROM_INSPECT
   Use the exact engine.key prefix from inspect. Preserve stdout/stderr with UTC
   stage labels. The cache helper requires exactly one matching engine directory,
   verifies the stored plan SHA, and records both file hashes and nanosecond mtimes.
   Capture the actual loaded TensorRT binding files too with `sha256sum` and
   `stat -c '%n %s %Y'`, using the verified image or cache path from step 3.
   Match the stored record's model hash, GPU, CUDA and TensorRT fields to this board.
6. Demonstrate healthy output with a new person appearance after an empty view
   lasting at least clear_after and cooldown. Record the resulting episode ID for
   this exact campaign/revision using `{run} board episodes baseline`.
   With the pinned CLI and explicit --device {shlex.quote(selector)}, use `data inspect ID`
   after the episode seals, then `data download ID --output NEW_DIRECTORY`.
   Check checksums, actual camera identity, visible person, fresh prediction
   timestamps, person detections and accelerator=tensorrt/precision=fp16 in
   events.jsonl. Retain the evidence outside Git; never commit captured images.

Agent restart, online reboot, offline reboot
7. Obtain the owner's approval for the reserved board's Agent restart. At its
   local console run `sudo systemctl restart wendy-agent`. Do not redeploy the
   campaign, change the model, remove cache files or alter the stored plan.
   Repeat only the inspect, cache/console and episode observations from steps 4-6,
   with labels agent-restart. Do not repeat deployment. Allow at most five minutes to
   return to TensorRT, then observe healthy output for a full minute. Require a
   new Agent/worker PID or start time but the SAME boot ID, campaign revision,
   engine key, plan/record hashes and mtimes. Require engine.cached true and no
   scheduled/building state. Review the journal for the entire restart interval;
   any worker exit/retry loop or rebuild fails the cached-restart case.
8. Obtain approval for a reboot with the WAN still connected, then run
   `sudo systemctl reboot` at the console. Wait up to five minutes for this exact
   serial/selector to return. Repeat only the inspect, cache/console and episode
   observations from steps 4-6, without deployment, using online-reboot labels. Check
   a NEW boot ID, new Agent/worker start times, unchanged campaign revision,
   engine key, plan/record hashes/mtimes, cached true, successful parity and fresh
   GPU predictions. Inspect every ten seconds for five minutes after reconnect.
   Native warnings such as 'Using an engine plan file across different models'
   may appear in engine.warnings/journal; they must not corrupt protocol JSON or
   cause the worker to die. Preserve warnings and nvpmodel output for comparison.
9. With the owner's approval, isolate only this reserved board's WAN using the
   agreed physical uplink method. Keep its LAN and console available. Document
   the uplink state and verify external model/package destinations are unreachable
   before reboot. Do not infer offline operation from a missing Wi-Fi icon or a
   failed unrelated endpoint. Reboot once more, then repeat step 8 as offline-reboot.
   Require unchanged cached model/binding/engine hashes and mtimes and no download
   attempt needed for successful startup. Verify the board cannot reach the WAN
   throughout the five-minute observation, with fresh GPU predictions still arriving.
   Readiness on the CPU or merely reporting cached=true cannot pass this case.

Failure handling and cleanup
10. If a restart takes longer than five minutes, a worker crash-loops, or the board
    is unreachable, save console/journal output and stop the sequence. Do not retry
    reboot, force-kill the worker, delete its cache, downgrade or flash the board.
    Follow the owner's recovery record. Retain all failed attempts.
11. Restore the WAN by the approved method and verify the original connectivity.
    Disarm only {name}:
      {run} campaign disarm cleanup
      {run} campaign inspect cleanup
    Poll up to 60 seconds until inference_status.state is disarmed, not stopping,
    and wait for this campaign's short episode to seal. Then remove only its plan:
      {run} campaign remove cleanup
      {run} board campaigns after-cleanup
    Confirm the name is absent. Removing a campaign keeps episodes; retain the
    test episode IDs and hand them to the owner. Do not delete unrelated data,
    engines, runtimes, images or volumes. Record final board and network state.
12. Link all observations in evidence.json and have a second reviewer compare the
    physical identity, restart/boot evidence, cache files, logs and four episodes.
    Set hardware_execution to passed only after all four actual cases and cleanup
    meet the criteria. The local subprocess regression fakes GPU dependencies and
    does not replace this physical result. This procedure does not claim testing
    a different Jetson/TensorRT version or the separate engine-invalidation cases.
''')
    return evidence


CACHE_CAPTURE = '''#!/usr/bin/env python3
"""Read one cached TensorRT plan and its record; never changes the cache."""
import argparse, hashlib, json, re
from pathlib import Path
p = argparse.ArgumentParser()
p.add_argument("--inference-root", type=Path, required=True)
p.add_argument("--key-prefix", required=True)
a = p.parse_args()
if not re.fullmatch(r"[0-9a-f]{16,64}", a.key_prefix):
    p.error("use the hexadecimal engine key reported by campaign inspect")
paths = [d for d in (a.inference_root / "engines").iterdir()
         if d.is_dir() and re.fullmatch(r"[0-9a-f]{32}", d.name)
         and d.name.startswith(a.key_prefix[:32])]
if len(paths) != 1:
    p.error("the key must identify exactly one persisted engine")
root = paths[0]
record = json.loads((root / "engine.json").read_text())
if not record["key"].startswith(a.key_prefix) or root.name != record["key"][:32]:
    p.error("engine directory and record key differ")
key = hashlib.sha256(json.dumps(record["fields"], sort_keys=True, separators=(",", ":")).encode()).hexdigest()
if key != record["key"]:
    p.error("record fields do not match their key")
files = {}
for name in ("engine.plan", "engine.json"):
    file = root / name
    if file.is_symlink() or not file.is_file():
        p.error("engine evidence must be regular files")
    digest = hashlib.sha256()
    with file.open("rb") as src:
        for block in iter(lambda: src.read(1024 * 1024), b""):
            digest.update(block)
    files[name] = {"sha256": digest.hexdigest(), "bytes": file.stat().st_size,
                   "mtime_ns": file.stat().st_mtime_ns}
if files["engine.plan"]["sha256"] != record["plan_sha256"] or files["engine.plan"]["bytes"] != record["plan_bytes"]:
    p.error("engine bytes do not match the stored record")
print(json.dumps({"path": str(root), "record": record, "files": files}, indent=2))
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print(f"Prepared four unexecuted TensorRT restart observations in {args.output}")

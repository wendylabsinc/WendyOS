#!/usr/bin/env python3
"""Prepare IQ-9075 model/runtime and owner-drain checks, without contacting a board.

Inventory requires cli_binary, agent_binary, agent_source_commit, recovery_record,
qualcomm_checkout (a local clone containing COMPANION_COMMIT), endpoint (the
reserved board's direct Qwen HTTP URL on port 9110), two images (path and expected
visual facts), and board (selector, serial, owner, saved device_info JSON path).
The pinned app is copied from Git objects, never from an uncommitted working tree.
Only the copied app ID and persistence volume name change. No production app is
replaced. The human must reserve port 9110 and exclusive NPU access first.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import sys
from urllib.parse import urlsplit
import uuid

spec = importlib.util.spec_from_file_location("gpu_helpers", Path(__file__).with_name("jetson-gpu-rehearsal.py"))
helpers = importlib.util.module_from_spec(spec)
spec.loader.exec_module(helpers)
ROOT = helpers.ROOT
COMPANION_COMMIT = "157a2dec629287aa5d1257f7a837f99700961f59"
MODEL_URL = ("https://qaihub-public-assets.s3.us-west-2.amazonaws.com/qai-hub-models/models/"
             "qwen3_vl_4b_instruct/releases/v0.64.0/"
             "qwen3_vl_4b_instruct-genie-w4a16-qualcomm_qcs9075.zip")


def pinned_project(repo):
    """Read only regular files beneath iq9-qwen at the reviewed companion commit."""
    head = subprocess.check_output(["git", "rev-parse", COMPANION_COMMIT + "^{commit}"], cwd=repo, text=True).strip()
    if head != COMPANION_COMMIT:
        raise ValueError("companion commit did not resolve exactly")
    tree = subprocess.check_output(["git", "ls-tree", "-rz", head, "--", "iq9-qwen/"], cwd=repo)
    files = {}
    for record in tree.split(b"\0"):
        if not record:
            continue
        metadata, path = record.split(b"\t", 1)
        mode, kind, blob = metadata.decode().split()
        relative = Path(path.decode()).relative_to("iq9-qwen")
        if kind != "blob" or mode not in ("100644", "100755") or ".." in relative.parts:
            raise ValueError("companion project contains a non-regular file")
        files[relative] = subprocess.check_output(["git", "cat-file", "blob", blob], cwd=repo)
    for name in ("Dockerfile", "wendy.json", "server.py", "genie.py", "worker.py", "model.py", "preprocess.py", "index.html"):
        if Path(name) not in files:
            raise ValueError("missing companion source: " + name)
    return files


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    artifacts = [helpers.artifact(raw[k]) for k in ("cli_binary", "agent_binary", "recovery_record")]
    cli = artifacts[0]["path"]
    if not os.access(cli, os.X_OK):
        raise ValueError("cli_binary is not executable")
    source = helpers.line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    board = {key: helpers.line(raw["board"][key], key) for key in ("selector", "serial", "owner")}
    if board["selector"].startswith("-"):
        raise ValueError("board selector cannot be an option")
    info_artifact = helpers.artifact(raw["board"]["device_info"])
    info = json.loads(Path(info_artifact["path"]).read_text())
    if (info.get("deviceType") != "dragonwing-iq-9075"
            or info.get("cpuArchitecture") not in ("arm64", "aarch64")
            or info.get("os") not in ("wendyos", "linux")):
        raise ValueError("requires an ARM64 Dragonwing IQ-9075 with the matching QCS9075 model")
    for key in ("version", "osVersion"):
        helpers.line(info.get(key), key)
    artifacts.append(info_artifact)
    endpoint = helpers.line(raw["endpoint"], "endpoint").rstrip("/")
    url = urlsplit(endpoint)
    if (url.scheme not in ("http", "https") or not url.hostname or url.port != 9110
            or url.username or url.password or url.query or url.fragment or url.path):
        raise ValueError("use the reserved board's direct HTTP(S) endpoint on port 9110, without credentials or a path")
    images = []
    for item in raw["images"]:
        image = helpers.artifact(item["path"])
        if Path(image["path"]).stat().st_size > 8 * 1024 * 1024:
            raise ValueError("image exceeds this rehearsal's 8 MiB limit")
        images.append(dict(image, expected=helpers.line(item["expected"], "expected visual facts")))
        artifacts.append(image)
    if len(images) != 2 or images[0]["sha256"] == images[1]["sha256"]:
        raise ValueError("provide two distinct non-sensitive JPEG/PNG fixtures and expected visual facts")
    files = pinned_project(Path(raw["qualcomm_checkout"]).resolve(strict=True))
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    project = out / "project"
    project.mkdir(mode=0o700)
    for path, data in files.items():
        destination = project / path
        destination.parent.mkdir(parents=True, exist_ok=True)
        destination.write_bytes(data)
        destination.chmod(0o600)
    run_id = uuid.uuid4().hex[:8]
    app = "sh.wendy.npu-rehearsal." + run_id
    volume = "npu-rehearsal-models-" + run_id
    config = json.loads((project / "wendy.json").read_text())
    config["appId"] = app
    persist = [e for e in config["entitlements"] if e["type"] == "persist"]
    if len(persist) != 1 or persist[0]["path"] != "/data":
        raise ValueError("companion persistence contract changed")
    persist[0]["name"] = volume
    (project / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
    probe = out / "probe.py"
    helpers.write(probe, PROBE)
    capture = out / "console-capture.sh"
    helpers.write(capture, CONSOLE.replace("APP_ID", shlex.quote(app)))
    artifacts.extend(helpers.artifact(str(p)) for p in sorted(project.rglob("*")) if p.is_file())
    artifacts.extend((helpers.artifact(str(probe)), helpers.artifact(str(capture))))
    selector = board["selector"]
    commands = {"qwen": {
        "deploy": [cli, "--json", "run", "--device", selector, "--prefix", str(project), "--detach", "--no-restart", "--yes"],
        # Explicit --detach is intentional for this long-running HTTP service.
        # Stop before the reboot phase; there is no automatic reboot in this bundle.
        "start": [cli, "--json", "device", "apps", "start", app, "--device", selector, "--detach"],
        "stop": [cli, "--json", "device", "apps", "stop", app, "--device", selector],
        "state": [cli, "--json", "device", "apps", "list", "--device", selector],
        "logs": [cli, "--json", "device", "logs", "--app", app, "--tail", "200", "--no-follow", "--device", selector],
        "remove": [cli, "device", "apps", "remove", app, "--force", "--device", selector],
        "info": [cli, "--json", "device", "info", "--device", selector],
    }}
    for stage in ("status", "report", "log"):
        commands["qwen"][stage] = [sys.executable, str(probe), endpoint, stage]
    for i, image in enumerate(images, 1):
        commands["qwen"][f"infer-{i}"] = [sys.executable, str(probe), endpoint, "infer", image["path"]]
    phases = ("initial", "replacement-1", "replacement-2", "replacement-3", "stopped-restart", "blocked-owner-replacement", "resumed-owner")
    cases = [{"id": phase, "status": "not_run", "app_id": app, "inference_evidence": None,
              "owner_trace": None, "container_metadata": None, "host_boot_id": None,
              "reviewer": None, "notes": None} for phase in phases]
    evidence = {"audit_entries": ["1988", "2547"], "source_head": head, "dirty_preview": dirty,
                "companion_commit": COMPANION_COMMIT, "agent_source_commit": source,
                "hardware_execution": "not_run", "board": board, "device_info": info,
                "endpoint": endpoint, "images": images, "artifacts": artifacts, "cases": cases,
                "app_id": app, "volume": volume, "cleanup": None,
                "model_url": MODEL_URL, "model_file_hashes": None, "image_digest": None,
                "image_framework_hashes": None, "runtime_provenance": None}
    for name, data in (("evidence.json", evidence), ("commands.json", commands)):
        helpers.write(out / name, json.dumps(data, indent=2) + "\n")
    timeout_line = 'limit = 1200 if stage == "deploy" else 30'
    if helpers.RUNNER.count(timeout_line) != 1:
        raise ValueError("shared runner timeout contract changed")
    runner = helpers.RUNNER.replace(timeout_line, 'limit = 1200 if stage == "deploy" else (180 if stage.startswith("infer-") else 60)')
    helpers.write(out / "run-command.py", runner)
    command = shlex.join([sys.executable, str(out / "run-command.py"), "qwen"])
    helpers.write(out / "procedure.txt", PROCEDURE.format(
        head=head, source=source, companion=COMPANION_COMMIT, serial=board["serial"], owner=board["owner"],
        selector=selector, endpoint=endpoint, app=app, volume=volume, command=command,
        recovery=artifacts[2]["path"], project=project, model=MODEL_URL,
        image1=images[0]["expected"], image2=images[1]["expected"]))
    return evidence


PROBE = '''#!/usr/bin/env python3
"""One bounded HTTP observation, never a hardware pass verdict."""
import json, sys, urllib.request, urllib.parse

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        raise RuntimeError("refusing to send fixture data to a redirected endpoint")

def main(argv):
    base, stage = argv[:2]
    data = None
    if stage == "infer":
        with open(argv[2], "rb") as stream:
            data = stream.read(8 * 1024 * 1024 + 1)
        if not data or len(data) > 8 * 1024 * 1024:
            raise ValueError("invalid fixture image size")
        path = "/ask?" + urllib.parse.urlencode({"q": "Describe the visible objects and their colors."})
    else:
        path = {"status": "/status", "report": "/report.txt", "log": "/log.txt"}[stage]
    request = urllib.request.Request(base + path, data=data)
    with urllib.request.build_opener(NoRedirect()).open(request, timeout=150 if data else 10) as response:
        body = response.read(2 * 1024 * 1024 + 1)
    if len(body) > 2 * 1024 * 1024:
        raise ValueError("response exceeded observation limit")
    text = body.decode("utf-8")
    print(text, flush=True)
    if stage in ("status", "infer"):
        result = json.loads(text)
        if stage == "status" and result.get("state") != "ready":
            raise RuntimeError("model is not ready")
        if stage == "infer" and (result.get("error") or not isinstance(result.get("answer"), str) or not result["answer"].strip()):
            raise RuntimeError("model did not produce an answer")
    # A nonempty answer is only evidence for human comparison, never a pass.
    return 0

if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
'''

CONSOLE = '''#!/bin/sh
# Read-only capture on the reserved board. Pass current server and Genie HOST PIDs.
set -eu
umask 077
[ "$#" -ge 2 ] || { echo 'usage: sh console-capture.sh SERVER_HOST_PID GENIE_HOST_PID' >&2; exit 2; }
for pid in "$@"; do
    case "$pid" in ''|*[!0-9]*) echo 'numeric PIDs required' >&2; exit 2;; esac
done
date -u
cat /proc/sys/kernel/random/boot_id
cat /etc/os-release
ctr --namespace default containers info APP_ID
ctr --namespace default tasks ps APP_ID
for node in /dev/fastrpc-*; do stat -Lc '%n %t:%T %a' "$node"; done
for lib in /run/wendyos/npu/lib/*; do sha256sum "$lib"; done
for pid in "$@"; do
    printf 'HOST PID %s\\n' "$pid"
    cat "/proc/$pid/stat"
    readlink "/proc/$pid/exe"
    cat "/proc/$pid/maps"
    cat "/proc/$pid/mountinfo"
    ls -l "/proc/$pid/fd"
    tr '\\000' '\\n' < "/proc/$pid/environ" | sed -n '/^LD_LIBRARY_PATH=/p; /^ADSP_LIBRARY_PATH=/p'
    for lib in libGenie.so libQnnHtp.so libQnnSystem.so; do
        sha256sum "/proc/$pid/root/opt/app/qairt/lib/$lib"
    done
done
'''

PROCEDURE = '''Qualcomm IQ-9075 NPU runtime and orderly owner replacement, audits 1988 / 2547
WendyOS source {head}; candidate Agent {source}; companion {companion}
Board serial {serial}; owner {owner}; selector {selector}; HTTP {endpoint}
Disposable app {app}; unique model volume {volume}
Prepared project {project}; recovery record {recovery}
All seven cases are NOT RUN. The generator has not contacted any board.

Preparation
1. Regenerate from a clean committed WendyOS checkout. Reserve this nonproduction
   IQ-9075 and local maintenance-console access. Physically verify serial/selector
   and that endpoint reaches this same board. Reserve TCP 9110 and exclusive NPU
   access. Do not replace PROD-qwen-vlm or any unrelated workload. If that port or
   the NPU is occupied, have its owner arrange an isolated test window first.
   The copied app uses host networking and its own model volume. A successful
   deployment acknowledgement or open HTTP port is not model readiness.
2. Verify the installed candidate Agent binary against the hashed artifact and
   source/build record. The companion app is pinned to the merged orderly-shutdown
   fix; do not substitute an older app that ignores TERM. Review its Dockerfile,
   QAIRT 2.47.0.260601 SDK SHA-256, QCS9075 v0.64.0 model URL and SDK terms. Confirm
   outbound model/SDK downloads are approved, with at least 10 GB free for model
   download/extraction plus image storage. Model URL: {model}
   Save final image digest, build output, model file SHA-256 inventory and SDK
   library hashes. URL/version alone is not an integrity record. If the pinned
   upstream objects are unavailable or incompatible, stop and record not_run.
3. Prepare two non-sensitive JPEG/PNG images with distinct known visual content.
   Image 1 expected facts: {image1}
   Image 2 expected facts: {image2}
   The images will be sent only to the explicit endpoint; redirects are refused.
   Preserve HTTP and CLI output in the private bundle. Application logs can include
   the question and answer. No camera, live scene or browser UI is required.
4. At the board's maintenance console confirm ctr, strace, timeout and sha256sum
   are available. Do not install packages or change security policy as an implicit
   part of this procedure. Review console-capture.sh before running it. It assumes
   containerd's default socket/namespace; record adjusted read-only commands if
   this installation differs. Start bounded host observation in separate console
   sessions, saving output to the operator's evidence folder:
     sudo timeout 3600 ctr --namespace default events
     sudo timeout 3600 dmesg --follow --ctime
   Keep the host boot ID from console captures. Reboot, reset, driver unbind or a
   forced kill is not recovery for a passing case. Review the recovery record with
   the owner before loading the model. Stop deployments on disconnect/timeouts.

Initial model and driver-locked runtime, audit 1988
5. Run each command once, retaining its unique label:
     {command} info initial
     {command} deploy initial
     {command} state initial
   Poll status every 10 seconds with labels initial-001, initial-002, etc., for at
   most 20 minutes. The model's first download is about 3 GB. Do not deploy again
   while waiting. Require state ready and the exact app ID running:
     {command} status initial-001
     {command} infer-1 initial
     {command} infer-2 initial
     {command} report initial
     {command} log initial
     {command} logs initial
   A human must compare both answers to the stated visual facts. Empty, incorrect,
   repeated canned answers or a provider listing without successful computation
   fail. Model loading/download/SDK errors are failures, not entitlement evidence.
6. Get current HOST PIDs with `sudo ctr --namespace default tasks ps {app}`.
   Identify the Python server and its genie-app worker by executable and parent
   relation. Capture `sudo sh console-capture.sh SERVER_PID GENIE_PID`, saving the
   output. Record /proc/PID/stat start times to distinguish PID reuse. Require the
   running Genie worker to map image-owned /opt/app/qairt/lib/libGenie.so and
   libQnnHtp.so, use the HTP backend and hold an actual /dev/fastrpc-* descriptor.
   Retain the generated model config and log proving the Hexagon backend loaded.
   Read those config files via /proc/GENIE_PID/root/data at the console; hash all
   model files there. No CPU backend, stub inference or framework-name listing
   substitutes for this evidence.
7. Compare the in-container framework hashes with the same files extracted from
   the recorded built image, before Wendy host injection. For example, on the
   build host create a stopped disposable Docker container from IMAGE_DIGEST,
   `docker cp` its /opt/app/qairt/lib directory to the evidence folder, hash it,
   and remove only that stopped inspection container. Record the exact commands,
   image digest and inspection container ID. Do not use an unpinned image tag.
   The OCI mounts must inject only the driver-locked transport at
   /opt/wendyos/npu/lib plus the board's conf.d and DSP shell/runtime files.
   Their hashes must match the host staging files. No host /usr/lib, /usr/bin or
   /opt/app/qairt tree may shadow the image's framework. Check mount destinations,
   LD_LIBRARY_PATH, actual mapped libraries and hashes together. The inference
   results from step 5 plus this provenance are the pass criteria for audit 1988.

Three successive real replacements, audit 2547
8. While the current model is healthy, attach a bounded read-only process trace
   at the maintenance console. Replace PID arguments with the verified current
   server/Genie host PIDs from step 6, not a name-based match:
     sudo timeout 3600 strace -f -ttt -s 256 -e trace=process,signal,write,close -p SERVER_PID -p GENIE_PID
   Save stderr, including timestamps. Start fresh bounded containerd/kernel
   observers before each cycle if the earlier 3600-second observers have expired;
   a missing interval in owner/task evidence cannot pass.
   In another terminal use the same generated deploy command, label replacement-1.
   This replaces the disposable app, not a production app:
     {command} deploy replacement-1
   Observe state, status, infer-1, infer-2, report, log and logs with replacement-1
   labels, followed by console/provenance capture from steps 6-7. Do not repeat
   step 5's deploy command. Repeat this single-deploy cycle for replacement-2 and
   replacement-3, attaching to the NEW verified PIDs each time. Keep complete
   traces from TERM through the new owner's FastRPC acquisition. If an observer
   expires before that interval completes, record incomplete evidence rather than
   joining disconnected traces into a pass. Reuse the exact image/model/framework;
   retain each image digest.
9. For each replacement require this ordering on the same host clock: Agent TERM
   delivered to the old owner, Genie node/pipeline-free commands written, FIFO
   closed, Genie worker exited and was reaped, old server exited, then replacement
   task started and acquired FastRPC. Correlate trace, task create/start/exit events,
   PID start times, app ID and fresh inference timestamps. Do not infer release
   just because the old UI or HTTP connection disappeared. Require no SIGKILL,
   no surviving old owner/descendants and successful fresh answers for both images
   from the replacement. Host boot ID must remain unchanged throughout. Kernel
   warning text alone is not a pass/fail diagnosis; retain it with the observations.

Stop and restart the same stored container
10. Trace the current owner again, then:
      {command} stop stopped-restart
      {command} state after-stop
    Require STOPPED, graceful owner/worker exit and no remaining task. Save the
    container ID, creation time, image digest, snapshot and stored OCI spec.
      {command} start stopped-restart
    This long-running service deliberately uses --detach, which sets its restart
    policy to unless-stopped. Observe readiness and both fresh answers again.
    Require unchanged stored container/snapshot identity. Stop it explicitly during
    final cleanup. This case does not by itself prove device renumbering or mesh
    duplicate admission; those portions of audits 2540/2554/1888 remain separate.

Refuse replacement while an NPU owner cannot drain
11. First stop the service from step 10 and deploy it afresh with no-restart:
      {command} stop fault-setup
      {command} deploy fault-setup
    Wait for readiness and both fresh inference answers, as in step 5. This avoids
    automatic restart hiding the later refusal. Reserve this fault-injection
    window with the board owner. Confirm recovery
    access and the current Genie PID/executable/start time. Start process tracing
    as in step 8. At the console pause ONLY that disposable app's Genie process:
      sudo kill -STOP -- GENIE_PID
    Verify its process state is stopped. Do not pause PID 1, the Agent, the host,
    or any unrelated app. Attempt exactly one replacement:
      {command} deploy blocked-owner-replacement
    The Agent's bounded drain must fail without SIGKILL, task deletion or a new
    NPU owner. Inspect the host trace, task list, events and old PID identity after
    the error; waiting for a replacement to become ready would hide this failure.
    The CLI runner permits build time, but the Agent drain is bounded separately.
    A CLI timeout alone is not proof of correct refusal. Keep hardware_execution
    not_run if you cannot distinguish Agent refusal from transport/build failure.
12. Verify the paused process is still the same PID/start time/executable before:
      sudo kill -CONT -- GENIE_PID
    Let queued SDK cleanup finish, with no force kill. Require worker and server
    exit. If it does not drain within 60 seconds, stop issuing commands and use
    the agreed owner-led recovery process; preserve failure evidence. Only after
    confirmed exit may you deploy once with label resumed-owner and repeat the
    readiness, two inference and runtime observations. A reset/reboot needed to
    restore NPU access fails this case. Never turn an unhealthy recovery into pass.

Cleanup and review
13. Gracefully stop and remove only {app}, after recording its final observations:
      {command} stop cleanup
      {command} state stopped-cleanup
      {command} remove cleanup
      {command} state removed-cleanup
    Confirm the app and its processes are absent. End only this rehearsal's trace
    observers. Keep the uniquely named volume {volume} with its model hash record;
    record its retained size and owner handoff. Do not delete shared images,
    volumes or builders. Volume deletion, if desired, is a separate owner action.
14. Fill all seven cases and provenance/cleanup fields in evidence.json, retaining
    failed attempts. A second reviewer must inspect actual inference, framework
    origin, host transport, process-exit ordering and blocked-owner refusal before
    setting hardware_execution to passed. Generated commands, offline tests and
    successful deploys cannot set that verdict. This procedure covers only
    IQ-9075 with this pinned Qwen/QAIRT combination, not all Qualcomm boards.
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print(f"Prepared seven unexecuted NPU observations in {args.output}")

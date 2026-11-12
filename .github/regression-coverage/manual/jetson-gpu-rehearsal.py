#!/usr/bin/env python3
"""Prepare physical CUDA checks for audit entries 2347, 1699 and 1693.

This generator only reads local inventory/artifacts and creates a NEW directory.
It never contacts a device. Run the generated commands manually on reserved boards.

Inventory JSON:
  cli_binary: path to the exact Wendy CLI executable
  agent_source_commit: full source SHA of the installed candidate Agent
  boards: three objects, one each for Orin NX, AGX Orin and AGX Thor, containing
    selector: explicit Wendy device selector
    serial: physical serial/asset identifier, confirmed on the board
    device_info: path to saved `wendy --json device info --device SELECTOR` output
    owner: person who reserved this board for the test

Capture device info with the same CLI after installing the candidate Agent by the
normal approved process. A supplied SHA alone is not proof of the installed build.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[3]
BOARDS = {"jetson-orin-nx": ("sm_87", "Dockerfile"),
          "jetson-agx-orin": ("sm_87", "Dockerfile"),
          "jetson-agx-thor": ("sm_110", "Dockerfile.cuda13")}
FIXTURES = ("python-gpu", "python-onnx-gpu")


def line(value, name):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value) or '<' in value or '>' in value:
        raise ValueError(f"{name} must be a nonempty single line without placeholders")
    return value


def artifact(value):
    path = Path(line(value, "artifact")).expanduser().resolve(strict=True)
    if not path.is_file() or not path.stat().st_size:
        raise ValueError("artifact must be a nonempty regular file")
    return {"path": str(path), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}


def write(path, text):
    with path.open("x", encoding="utf-8") as stream:
        os.chmod(path, 0o600)
        stream.write(text)


def generate(inventory, output):
    raw = json.loads(inventory.read_text())
    cli = artifact(raw["cli_binary"])
    if not os.access(cli["path"], os.X_OK):
        raise ValueError("cli_binary is not executable")
    source = line(raw["agent_source_commit"], "agent_source_commit")
    if not re.fullmatch(r"[0-9a-f]{40}", source):
        raise ValueError("agent_source_commit must be a full SHA")
    boards, kinds, serials, selectors = [], set(), set(), set()
    for board in raw["boards"]:
        info_file = artifact(board["device_info"])
        info = json.loads(Path(info_file["path"]).read_text())
        kind = info.get("deviceType")
        if kind not in BOARDS or kind in kinds:
            raise ValueError("need one distinct supported board per deviceType")
        if info.get("gpuVendor") != "nvidia" or info.get("gpuArch") != BOARDS[kind][0] or info.get("cpuArchitecture") not in ("arm64", "aarch64") or info.get("os") not in ("wendyos", "linux"):
            raise ValueError(f"incompatible hardware metadata for {kind}")
        for key in ("version", "osVersion", "jetpackVersion", "cudaVersion"):
            line(info.get(key), key)
        if kind == "jetson-orin-nx" and not info["jetpackVersion"].startswith("6."):
            raise ValueError("Orin NX must cover the JetPack 6 render/nvhost regression")
        if kind == "jetson-agx-orin" and (not info["jetpackVersion"].startswith("7.") or not info["cudaVersion"].startswith("13.")):
            raise ValueError("AGX Orin must cover CUDA 12 images on a JetPack 7 / CUDA 13 host")
        if kind == "jetson-agx-thor" and not info["cudaVersion"].startswith("13."):
            raise ValueError("Thor must exercise the CUDA 13 runtime")
        item = {key: line(board[key], key) for key in ("selector", "serial", "owner")}
        if item["selector"].startswith("-") or item["selector"] in selectors or item["serial"] in serials:
            raise ValueError("board selectors and physical serials must be distinct")
        kinds.add(kind); selectors.add(item["selector"]); serials.add(item["serial"])
        boards.append(dict(item, device_type=kind, device_info=info, info_artifact=info_file))
    if kinds != set(BOARDS):
        raise ValueError("inventory must include Orin NX, AGX Orin and AGX Thor")
    head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    out = output.resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    run_id = uuid.uuid4().hex[:8]
    cases, commands, artifacts = [], {}, [cli]
    for board in boards:
        selector = board["selector"]
        kind = board["device_type"]
        for fixture in FIXTURES:
            for attempt in ("positive-1", "positive-2", "positive-3", "no-gpu-control"):
                case = f"{kind}-{fixture}-{attempt}"
                project = out / "projects" / case
                shutil.copytree(ROOT / ".github/ci-tests" / fixture, project)
                config = json.loads((project / "wendy.json").read_text())
                app_id = f"sh.wendy.gpu-rehearsal.{run_id}.{len(cases)+1}"
                config["appId"] = app_id
                if attempt == "no-gpu-control":
                    config["entitlements"] = []
                (project / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
                for path in sorted(project.iterdir()):
                    if path.is_file(): artifacts.append(artifact(str(path)))
                dockerfile = BOARDS[kind][1]
                commands[case] = {
                    "info": [cli["path"], "--json", "device", "info", "--device", selector],
                    "deploy": [cli["path"], "--json", "run", "--device", selector, "--prefix", str(project), "--dockerfile", dockerfile, "--detach", "--no-restart", "--yes"],
                    "state": [cli["path"], "--json", "device", "apps", "list", "--device", selector],
                    "logs": [cli["path"], "--json", "device", "logs", "--app", app_id, "--tail", "100", "--no-follow", "--device", selector],
                    "remove": [cli["path"], "device", "apps", "remove", app_id, "--force", "--device", selector],
                }
                cases.append({"id": case, "selector": selector, "serial": board["serial"], "app_id": app_id,
                              "dockerfile": dockerfile, "expect": "denied" if attempt == "no-gpu-control" else "cuda_compute",
                              "status": "not_run", "start_utc": None, "end_utc": None, "image_digest": None,
                              "container_exit_code": None, "observed_cuda_version": None, "observed_gpu_name": None,
                              "stdout_log": None, "state_log": None, "cleanup_log": None, "reviewer": None, "notes": None})
    evidence = {"audit_entries": ["2347", "1699", "1693"], "source_head": head, "dirty_preview": dirty,
                "agent_source_commit": source, "hardware_execution": "not_run", "boards": boards,
                "artifacts": artifacts, "cases": cases}
    write(out / "evidence.json", json.dumps(evidence, indent=2) + "\n")
    write(out / "commands.json", json.dumps(commands, indent=2) + "\n")
    write(out / "run-command.py", RUNNER)
    command = shlex.join(["python3", str(out / "run-command.py")])
    procedure = f'''Jetson CUDA hardware rehearsal, audit 2347 / 1699 / 1693
Procedure source: {head}; dirty preview: {dirty}
Candidate Agent source: {source}
CLI SHA-256: {cli['sha256']}

This is a prepared procedure, not a hardware result. All 24 cases start not_run.
Regenerate from a clean committed checkout before execution. A VM, CPU-only
runner, fixture unit test, successful deployment, or advertised CUDA provider
cannot replace these board results.

When required
Before accepting changes to GPU entitlements, CDI/vendor runtime injection,
device discovery, GPU fixture Dockerfiles/main.py, or test-ci GPU selection,
run this matrix on Orin NX with JetPack 6, AGX Orin with JetPack 7 / CUDA 13,
and AGX Thor with CUDA 13. Orin NX reproduces the render/nvhost permission path.
AGX Orin reproduces CUDA 12 fixture libraries with a CUDA 13 host driver. Record
the PR and source SHA.
If a required board is unavailable, record blocked/not_run; never relabel it pass.
AGX Orin covers the CUDA-12 library compatibility fix 2347. Orin NX covers
the JetPack 6 entitlement regression 1699; all boards require actual GPU access. Thor covers CUDA-13 execution 1693.

Preparation
1. Reserve all three boards with the inventory owners. Physically match each
   serial/asset label and confirm its network selector. Check the authenticated
   Wendy target against the reservation. Names, model and GPU vendor alone do
   not establish identity. Stop on an ambiguous or changed target.
2. Verify the installed Agent build's provenance against the candidate source
   SHA and retain that build record. Save fresh device-info JSON and GPU hardware
   inventory using `wendy --json device hardware list --category gpu --device`
   followed by the explicit selector. Compare deviceType, CPU, GPU architecture,
   Agent, OS, JetPack and CUDA versions with evidence.json. A different version
   requires a new inventory and new output directory, not editing old results.
3. Use dedicated test boards with no production apps. Record free disk, available
   memory, power mode and thermal state; CUDA images need several GiB. Obtain the
   owner's approval to deploy these disposable apps. Do not update/reflash the OS,
   remove host drivers, change permissions or stop unrelated apps for this test.
4. Use this pinned CLI and generated project copies. Dockerfiles are selected
   explicitly: Orin uses Dockerfile, Thor uses Dockerfile.cuda13. Save resolved
   base-image and built-image digests from the build log, and framework versions
   from the app log. The project files and CLI hashes must still match the bundle.
   Floating image tags are not provenance; retain the digest actually executed.

Run every evidence.json case in order on its named board
1. Record UTC and the operator. Invoke the bounded command runner as follows,
   replacing CASE with an exact id from commands.json and LABEL with a fresh
   filename-safe observation label. Outputs are exclusive files in this bundle:
     {command} CASE info before
     {command} CASE deploy deploy
   The runner verifies fixture/CLI hashes, allows 20 minutes for the image build
   and deployment, and 30 seconds for other commands. Timeouts are failures;
   preserve logs and inspect the selected app before another attempt.
2. Deployment acceptance is not the app verdict. Poll state at most every five
   seconds for up to 90 seconds, using a fresh label for each observation:
     {command} CASE state state-01
   Match the exact app_id in evidence.json. Require the app to have exited and a
   recorded exit code. RUNNING, CRASH_LOOPING, a missing app/exit code or timeout
   cannot pass. --no-restart prevents a restart from hiding the first failure.
3. Save the app's actual output after it exits:
     {command} CASE logs final
   Use the unique per-case app ID and timestamps; never borrow an earlier log.
   Retain the framework/GPU/driver information, full output and state observation.
4. For each positive case require exit 0 and the fixture's PASS line, without
   traceback, driver errors or FAIL. python-gpu must show CUDA available and the
   expected physical GPU, completed synchronization, and all 65536 matrix result
   elements equal 256. python-onnx-gpu must show a correct doubled result and
   Executed Mul providers containing only CUDAExecutionProvider. Merely listing
   CUDA as available/preferred does not pass. No CPU fallback is accepted.
5. For no-gpu-control require successful build/deployment of the same framework
   and driver-compatible fixture, followed by a nonzero app exit and CUDA access
   refusal. Import/build/network failure is not an entitlement denial. Require
   framework version output and no PASS/completed CUDA result. The two cases
   differ only in app identity and declared entitlements; compare image digests.
   If compute succeeds without the entitlement, fail the control and investigate.
6. Remove only the exact generated disposable app and capture state afterward:
     {command} CASE remove cleanup
     {command} CASE state after-cleanup
   Require that app ID to be absent. Do not delete volumes, shared images, Docker
   builders, or unrelated apps. If cleanup fails, record the remaining ID and
   owner handoff; do not claim completion. Reuse build caches for later cases.
7. Fill the case's evidence fields, link output files, and have a second reviewer
   inspect the identity, image digest, GPU execution proof and cleanup. Keep
   failing attempts unchanged; a retry gets a new bundle. Hardware_execution may
   become passed only after all 24 cases pass on the exact recorded candidates.

Recovery
On timeout or disconnect, stop issuing deployments. Reconnect to the exact board,
capture device info and app state, and remove only this bundle's remaining app
IDs. Ask the board owner to recover the reserved device if unavailable. Never
flash storage or restart a shared device as automatic cleanup. Keep failure logs.
Restore the reserved board to its owner's agreed state before releasing it.

Scope limits
These cases do not verify driver-file renaming, node renumbering, display-only
CDI, TensorRT cache reuse, or NPU ownership. Those audit entries stay open.
'''
    write(out / "procedure.txt", procedure)
    return evidence


RUNNER = '''#!/usr/bin/env python3
"""Execute one explicitly selected rehearsal command; never records a pass."""
import hashlib, json, os, re, signal, subprocess, sys, time
from pathlib import Path
root = Path(__file__).resolve().parent
if len(sys.argv) != 4:
    sys.exit("usage: run-command.py CASE STAGE UNIQUE_LABEL")
case, stage, label = sys.argv[1:]
if not re.fullmatch(r"[A-Za-z0-9_-]+", label): sys.exit("invalid label")
evidence = json.loads((root / "evidence.json").read_text())
if evidence["dirty_preview"]: sys.exit("regenerate from a clean committed checkout")
for item in evidence["artifacts"]:
    if hashlib.sha256(Path(item["path"]).read_bytes()).hexdigest() != item["sha256"]:
        sys.exit("artifact changed: " + item["path"])
argv = json.loads((root / "commands.json").read_text())[case][stage]
limit = 1200 if stage == "deploy" else 30
prefix = root / (case + "-" + stage + "-" + label)
record = {"argv": argv, "timeout_seconds": limit, "start_utc": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())}
with prefix.with_suffix(".result.json").open("x") as meta:
    os.chmod(meta.name, 0o600)
    with prefix.with_suffix(".stdout").open("xb") as out, prefix.with_suffix(".stderr").open("xb") as err:
        os.chmod(out.name, 0o600); os.chmod(err.name, 0o600)
        process = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=out, stderr=err, start_new_session=True)
        try:
            code = process.wait(timeout=limit)
        except (subprocess.TimeoutExpired, KeyboardInterrupt):
            os.killpg(process.pid, signal.SIGTERM)
            try: process.wait(timeout=5)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL); process.wait()
            code = 124
        record.update(exit_code=code, end_utc=time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
        json.dump(record, meta, indent=2)
print(str(prefix), "command exit", code, "(not a hardware verdict)")
sys.exit(code)
'''


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)
    print(f"Prepared 24 unexecuted hardware cases in {args.output}")


if __name__ == "__main__":
    main()

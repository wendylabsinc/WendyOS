#!/usr/bin/env python3
"""Build real ESP-IDF firmware as root; require runner-owned generated files.

Run as the ordinary Linux CI user with access to a rootful Docker daemon.
No device, privileged container, new builder, or global cleanup is used.
"""
import argparse
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import signal
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location("owned_idf_process", Path(__file__).with_name("run-owned-media-test.py"))
GUARDIAN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GUARDIAN)
IMAGE = "espressif/idf@sha256:30b42a38415e918b1b14825ccf0e4c40e7910c5132618dcd12e3c45361be0943"
PACKAGE = "./go/internal/cli/espidftoolchain"


def captured(*command, timeout=30):
    return subprocess.run(command, text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout)


def checked(*command, timeout=30):
    result = captured(*command, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f"command failed: {command!r}\n{result.stdout[-4000:]}{result.stderr[-4000:]}")
    return result.stdout


def belongs_to(path, root):
    return Path(path).resolve().is_relative_to(root)


def cleanup(work, image_available, report):
    """Catch abandoned production containers only when their mount proves ownership."""
    try:
        names = checked("docker", "ps", "-a", "--filter", "name=wendy-idf-", "--format", "{{.Names}}").splitlines()
        for name in names:
            result = captured("docker", "inspect", name)
            if result.returncode and re.search(r"no such (?:object|container)", result.stderr, re.I):
                continue
            if result.returncode:
                raise RuntimeError(f"cannot inspect potential fixture container {name}: {result.stderr}")
            records = json.loads(result.stdout)
            mounts = records[0].get("Mounts", [])
            if not mounts or not any(belongs_to(item["Source"], work) for item in mounts):
                continue
            if len(mounts) != 1 or records[0]["Config"]["Image"] != IMAGE:
                raise RuntimeError(f"unexpected container sharing fixture workspace: {name}")
            checked("docker", "rm", "--force", name)
        if image_available:
            name = "wendy-idf-cleanup-" + uuid.uuid4().hex
            try:
                checked("docker", "run", "--rm", "--name", name, "--network=none", "-v", str(work) + ":/fixture",
                        "--entrypoint", "find", IMAGE, "-P", "/fixture", "-xdev", "-exec", "chown", "-h", "--",
                        f"{os.getuid()}:{os.getgid()}", "{}", "+", timeout=40)
            finally:
                result = captured("docker", "inspect", name)
                if result.returncode == 0:
                    # This exact random name was created by this cleanup call.
                    checked("docker", "rm", "--force", name)
                elif not re.search(r"no such (?:object|container)", result.stderr, re.I):
                    raise RuntimeError(f"cannot verify cleanup container removal: {result.stderr}")
        shutil.rmtree(work)
    except Exception as error:
        report["cleanup_errors"].append(str(error))
        report["retained_workspace"] = str(work)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != "linux" or os.getuid() <= 0:
        raise RuntimeError("requires non-root native Linux caller and rootful Docker")
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix="wendy-idf-ownership-", dir=os.environ.get("RUNNER_TEMP"))).resolve()
    (work / "tmp").mkdir()
    env = {**os.environ, "TMPDIR": str(work / "tmp"), "WENDY_TEST_IDF_DOCKER_OWNERSHIP": "1"}
    report = {"status": "failed", "image": IMAGE, "caller": f"{os.getuid()}:{os.getgid()}", "cleanup_errors": [], "commands": [],
              "scope": "ESP32-C6 firmware compilation and rootful Linux ownership; no flash, hardware, or Mosaico app claim"}
    image_available = False

    def interrupted(signum, _frame):
        raise InterruptedError(f"ESP-IDF fixture interrupted by signal {signum}")

    previous_handlers = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}

    def run(label, command, timeout):
        log = output / (label + ".log")
        started = time.monotonic()
        with log.open("w") as stream:
            code = GUARDIAN.run(command, timeout, cwd=ROOT, env=env, stdout=stream)
        report["commands"].append({"name": label, "exit_code": code, "seconds": round(time.monotonic() - started, 3)})
        if code:
            print(log.read_text(errors="replace")[-10000:], file=sys.stderr)
        return code

    try:
        if run("pull", ["docker", "pull", IMAGE], 600):
            raise RuntimeError("pinned ESP-IDF image pull failed")
        image_available = True
        report["image_inspect"] = json.loads(checked("docker", "image", "inspect", IMAGE))
        if run("compile", ["go", "test", "-c", "-o", str(work / "normal.test"), PACKAGE], 180):
            raise RuntimeError("ESP-IDF fixture compilation failed")
        source = ROOT / "go/internal/cli/espidftoolchain/project.go"
        text = source.read_text()
        needle = "p.restoreOutputOwnership(ctx)"
        if text.count(needle) != 1:
            raise RuntimeError("ownership mutation source changed; inspect before updating")
        mutant = work / "project.go"
        mutant.write_text(text.replace(needle, "nil"))
        overlay = work / "overlay.json"
        overlay.write_text(json.dumps({"Replace": {str(source): str(mutant)}}))
        if run("compile-control", ["go", "test", "-overlay=" + str(overlay), "-c", "-o", str(work / "control.test"), PACKAGE], 180):
            raise RuntimeError("ownership negative-control compilation failed")
        if run("normal", [str(work / "normal.test"), "-test.v", "-test.run=^TestProjectToolchainDockerOwnership$", "-test.timeout=11m"], 750):
            raise RuntimeError("real ESP-IDF ownership journey failed")
        normal = (output / "normal.log").read_text()
        for outcome in ("success", "failure", "cancelled"):
            if f"--- PASS: TestProjectToolchainDockerOwnership/{outcome} " not in normal:
                raise RuntimeError(f"missing actual {outcome} build execution")
        code = run("negative-control", [str(work / "control.test"), "-test.v", "-test.run=^TestProjectToolchainDockerOwnership$/^success$", "-test.timeout=4m"], 270)
        control = (output / "negative-control.log").read_text()
        if code != 1 or "ESP-IDF output owner mismatch:" not in control or "restore fixture workspace for removal:" in control:
            raise RuntimeError("negative control did not fail specifically on generated ownership")
        report["negative_control_detected"] = True
        report["status"] = "passed"
    finally:
        for sig in previous_handlers:
            signal.signal(sig, signal.SIG_IGN)
        try:
            cleanup(work, image_available, report)
            if report["cleanup_errors"]:
                report["status"] = "failed"
            (output / "evidence.json").write_text(json.dumps(report, indent=2) + "\n")
            print(json.dumps(report, indent=2))
        finally:
            for sig, handler in previous_handlers.items():
                signal.signal(sig, handler)
    if report["status"] != "passed":
        raise RuntimeError("ESP-IDF fixture cleanup failed")


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Run production authority test binaries inside an existing Linux Docker VM.

No VM creation, image pull, publishing, enrollment, exposed ports, or hardware.
Network-none containers can use loopback for local signed-time and TLS fixtures.
The public-time/live-service integration test is explicitly disabled.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import time

ROOT = Path(__file__).resolve().parents[1]
PRODUCTION = ROOT / "production"
LIVE_ENV = "WENDY_WORLD_LIVE_READ_CONFIG"


def run(args, *, cwd=ROOT, env=None, timeout=120):
    result = subprocess.run(args, cwd=cwd, env=env, text=True,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
                            timeout=timeout, check=False)
    if result.returncode:
        raise RuntimeError(f"{args[0]} failed with exit {result.returncode}:\n{result.stdout[-6000:]}")
    return result.stdout


def json_objects(raw):
    """Decode the concatenated JSON objects emitted by go list -json."""
    decoder = json.JSONDecoder()
    remaining = raw.lstrip()
    while remaining:
        value, end = decoder.raw_decode(remaining)
        yield value
        remaining = remaining[end:].lstrip()


def discover_packages(env, timeout):
    raw = run(["go", "list", "-json", "./..."], cwd=PRODUCTION,
              env=env, timeout=timeout)
    packages = []
    for value in json_objects(raw):
        if not value.get("TestGoFiles") and not value.get("XTestGoFiles"):
            continue
        directory = Path(value["Dir"]).resolve()
        relative = directory.relative_to(PRODUCTION.resolve()).as_posix()
        if relative == "." or not re.fullmatch(r"[a-zA-Z0-9_/-]+", relative):
            raise RuntimeError("unsupported production test package path")
        packages.append(relative)
    if not packages or len(packages) != len(set(packages)):
        raise RuntimeError("production test discovery was empty or ambiguous")
    return sorted(packages)


def source_hashes():
    from verification_sources import source_hashes as inventory
    return inventory([Path(__file__).resolve()])


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True, help="existing Linux arm64 image; never pulled")
    parser.add_argument("--run-id", required=True)
    parser.add_argument("--max-runtime-seconds", type=int, default=600,
                        help="overall build-and-test budget, 60–1800 seconds (default 600)")
    args = parser.parse_args()
    if not re.fullmatch(r"[a-z0-9][a-z0-9-]{0,63}", args.run_id):
        parser.error("run-id must be a bounded lowercase identifier")
    if not 60 <= args.max_runtime_seconds <= 1800:
        parser.error("max-runtime-seconds must be between 60 and 1800")
    started = time.monotonic()
    deadline = started + args.max_runtime_seconds

    def budget(maximum):
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise RuntimeError("overall Linux authority verification budget exhausted")
        return min(maximum, remaining)

    inspected = json.loads(run(["docker", "image", "inspect", args.image], timeout=budget(30)))[0]
    if inspected["Os"] != "linux" or inspected["Architecture"] != "arm64":
        parser.error("this runner requires an existing Linux arm64 image")
    image_id = inspected["Id"]
    out = ROOT / ".local" / "production-authority-linux" / args.run_id
    out.mkdir(parents=True, exist_ok=False)
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH="arm64")
    env.pop(LIVE_ENV, None)
    toolchain = run(["go", "version"], cwd=PRODUCTION, env=env, timeout=budget(30)).strip()
    selected = discover_packages(env, budget(120))
    initial_sources = source_hashes()
    packages = []
    for index, package in enumerate(selected):
        filename = package.replace("/", "-") + ".test"
        binary = out / filename
        run(["go", "test", "-c", "-trimpath", "-buildvcs=false", "-o", str(binary), "./" + package],
            cwd=PRODUCTION, env=env, timeout=budget(120))
        container = f"world-authority-{args.run_id}-{index}"
        try:
            stdout = run([
                "docker", "run", "--rm", "--pull=never", "--network=none",
                "--name", container, "--read-only", "--user=65532:65532", "--cap-drop=ALL",
                "--security-opt=no-new-privileges", "--memory=512m", "--cpus=2",
                "--env", LIVE_ENV + "=",
                "--tmpfs=/tmp:rw,noexec,nosuid,size=64m,mode=1777",
                "--mount", f"type=bind,src={binary},dst=/authority.test,readonly",
                "--entrypoint=/authority.test", image_id,
                "-test.v", "-test.count=1", "-test.timeout=60s",
            ], timeout=budget(75))
        except (subprocess.TimeoutExpired, RuntimeError):
            # Killing the docker CLI alone can leave its container alive.
            # This unique name belongs to this invocation and no other app.
            subprocess.run(["docker", "rm", "--force", container], timeout=15,
                           stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
            raise
        (out / (filename + ".log")).write_text(stdout)
        passed = re.findall(r"^--- PASS: ([^ ]+)", stdout, re.MULTILINE)
        skipped = re.findall(r"^--- SKIP: ([^ ]+)", stdout, re.MULTILINE)
        if not passed or not stdout.rstrip().endswith("PASS"):
            raise RuntimeError(f"{package} did not report passing tests")
        if "TestLiveReadService" in passed:
            raise RuntimeError("live integration test unexpectedly ran in isolated verification")
        packages.append({"package": package, "test_groups": passed, "skipped_test_groups": skipped,
                         "binary_sha256": hashlib.sha256(binary.read_bytes()).hexdigest()})
        print(f"{package}: {len(passed)} test groups passed, {len(skipped)} skipped", flush=True)
    final_sources = source_hashes()
    if final_sources != initial_sources:
        raise RuntimeError("source files changed during verification; a stable final run is required")
    report = {
        "schema": "wendy.world.production-authority-linux.v1", "run_id": args.run_id,
        "result": "passed", "toolchain": toolchain, "target": "linux/arm64",
        "image_id": image_id, "existing_docker_vm": True, "network": "none",
        "uid": 65532, "read_only_root": True, "elapsed_seconds": round(time.monotonic() - started, 3),
        "runtime_budget_seconds": args.max_runtime_seconds,
        "package_selection": "go list ./... packages containing internal or external tests",
        "live_test_environment_cleared": LIVE_ENV,
        "packages": packages, "source_sha256": final_sources, "source_snapshot_unchanged": True,
        "limits": [
            "Generated test certificates only; no enrolled-operator interoperability.",
            "Authenticated-time tests use locally signed UDP fixtures and injected intervals; no public time server is queried.",
            "Read-handler tests use local TLS and a synthetic World dataset; public-time/live-service integration is skipped.",
            "No deployed World service, mesh route, device-origin signer or hardware execution.",
            "Linux binaries run without the race detector (CGO disabled); host race-test results are separate.",
        ],
    }
    (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
    print(f"Report: {out / 'report.json'}")


if __name__ == "__main__":
    main()

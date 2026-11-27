#!/usr/bin/env python3
"""Verify private publication code in the existing local Docker Linux VM.

Uses generated test identities only. It neither starts the Wendy agent nor
connects to devices, shared simulators, enrollment, or the Docker host socket.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import subprocess
import time

from verification_sources import source_hashes

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ["api/world/app/v1", "hostpath", "wendycert", "serviceorigin",
            "servicejournal", "appservice", "agentmodule"]


def sha(data):
    return hashlib.sha256(data).hexdigest()


def sources():
    return source_hashes([Path(__file__).resolve()])


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--out", required=True)
    parser.add_argument("--image", default="ubuntu:26.04")
    args = parser.parse_args()
    out = Path(args.out).resolve()
    out.mkdir(parents=True, exist_ok=False)
    before = sources()
    (out / "source-manifest.json").write_text(json.dumps(before, indent=2) + "\n")
    receipts = []
    env = dict(os.environ, GOWORK="off")

    def run(label, argv, *, linux=False, cwd=ROOT / "production"):
        started = time.time()
        result = subprocess.run(argv, cwd=cwd, env=dict(env, **({"GOOS": "linux", "GOARCH": "arm64", "CGO_ENABLED": "0"} if linux else {})), stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        (out / (label + ".stdout")).write_bytes(result.stdout)
        (out / (label + ".stderr")).write_bytes(result.stderr)
        receipts.append({"label": label, "command": argv, "exit_code": result.returncode,
                         "elapsed_seconds": round(time.time() - started, 3)})
        (out / "commands.json").write_text(json.dumps(receipts, indent=2) + "\n")
        print(label, "exit", result.returncode, flush=True)
        if result.returncode:
            raise RuntimeError("verification failed; retained output: " + label)
        return result.stdout

    run("host-platform", ["go", "version"])
    run("host-race", ["go", "test", "-race", "-json", "./..."])
    run("host-vet", ["go", "vet", "./..."])
    image = run("linux-image", ["docker", "image", "inspect", args.image, "--format", "{{.Id}}"], cwd=ROOT).decode().strip()
    run("docker-context", ["docker", "context", "show"], cwd=ROOT)
    base = ["docker", "run", "--rm", "--network", "none", "--read-only",
            # Foreign-owner refusal tests must exercise our ownership check
            # even when root can traverse the directory, then clean it up.
            "--cap-drop", "ALL", "--cap-add", "CHOWN", "--cap-add", "DAC_OVERRIDE",
            "--security-opt", "no-new-privileges", "--pids-limit", "128",
            "--memory", "512m", "--cpus", "2",
            "--tmpfs", "/tmp:rw,nosuid,nodev,size=128m",
            "--mount", "type=bind,src=" + str(out) + ",dst=/tests,readonly", image]
    run("linux-platform", base + ["uname", "-srm"], cwd=ROOT)
    for package in PACKAGES:
        name = package.replace("/", "-")
        binary = out / (name + ".test")
        run(name + "-compile", ["go", "test", "-c", "-o", str(binary), "./" + package], linux=True)
        run(name + "-linux", base + ["/tests/" + binary.name, "-test.v", "-test.timeout=90s"], cwd=ROOT)
    if sources() != before:
        raise RuntimeError("tested source changed during verification")
    summary = {"schema": "wendy.world.service-publication-verification.v1",
               "host": platform.platform(), "linux_image": image,
               "packages": PACKAGES, "source_files": len(before),
               "source_unchanged": True, "commands": len(receipts),
               "result": "passed", "scope": "Generated test identities; no device enrollment or agent deployment"}
    (out / "summary.json").write_text(json.dumps(summary, indent=2) + "\n")
    print(json.dumps(summary), flush=True)


if __name__ == "__main__":
    main()

#!/usr/bin/env python3
"""Opt-in actual read-service integration against a live virtual MuJoCo site.

Uses existing Docker images/VM only, generated UNTRUSTED test operator identities,
and a real Wendy authenticated-time quorum. No user credential config/keychain,
Cloud/device access, image pulls, trust installation or physical execution.
"""

import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import time

from native_client_site import NativeSite

ROOT = Path(__file__).resolve().parents[1]
ENVIRONMENT = "WENDY_WORLD_LIVE_READ_CONFIG"


def existing_image(image):
    result = subprocess.run(["docker", "image", "inspect", image], cwd=ROOT,
                            capture_output=True, text=True, timeout=15, check=False)
    if result.returncode:
        raise RuntimeError("an existing local image is required; this runner never pulls")
    details = json.loads(result.stdout)[0]
    if details["Os"] != "linux":
        raise RuntimeError("the existing image must target Linux")
    return details["Id"]


def private_json(path, value):
    # O_EXCL prevents replacing a prior run or following a final-component link.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as stream:
        json.dump(value, stream, indent=2)
        stream.write("\n")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True,
                        help="new private output directory; never overwritten")
    parser.add_argument("--node-image", default="wendy-world-node:local")
    parser.add_argument("--scene-image", default="wendy-world-live:local")
    args = parser.parse_args()
    node_image, scene_image = existing_image(args.node_image), existing_image(args.scene_image)
    out = args.out.expanduser().resolve()
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    os.chmod(out, 0o700)
    site = NativeSite(out / "site", node_image, scene_image)
    child = None
    started = time.monotonic()
    report = {"schema": "wendy.world.live-production-read-run.v1", "result": "failed",
              "identity": "generated untrusted fixture operator/root; no enrolled identity",
              "time": "real nonce-bound Wendy server quorum; no injected authority clock",
              "dataset": "live synthetic MuJoCo site on the existing Docker Linux VM",
              "images": {"node": node_image, "scene": scene_image}}
    previous = {}

    def interrupted(signum, frame):
        raise KeyboardInterrupt

    for sig in (signal.SIGTERM, signal.SIGINT):
        previous[sig] = signal.signal(sig, interrupted)
    previous_directory = Path.cwd()
    try:
        # NativeSite uses repository-relative package/model paths. Its resource
        # cleanup checks the unique run label before removing any container.
        os.chdir(ROOT)
        info = site.setup()
        if site.available_until - time.time() < 180:
            raise RuntimeError("simulation reader credentials have insufficient remaining validity")
        profile = json.loads((site.folder / "apple-profile.json").read_text())
        credential = profile["credentials"]
        grant = credential["grant"]["grant"]
        if grant["domain"] != "simulation" or set(grant["scopes"]) - {"read", "evidence-read"}:
            raise RuntimeError("fixture did not provide a read-only simulation credential")
        reader_path = out / "simulation-reader.json"
        private_json(reader_path, credential)
        dataset = {"id": "live-" + site.suffix, "domain": "simulation",
                   "source_tenant": grant["tenant"], "source_site": grant["site"],
                   "frame": {"id": "office-floor", "revision": "office-frame-v1"},
                   "run_id": site.run_id}
        test_report = out / "service-report.json"
        input_path = out / "test-input.json"
        private_json(input_path, {"version": 1, "simulation_reader": str(reader_path),
                                  "dataset": dataset, "report": str(test_report)})
        env = dict(os.environ)
        env[ENVIRONMENT] = str(input_path)
        log = out / "test.log"
        fd = os.open(log, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
        with os.fdopen(fd, "w") as stream:
            child = subprocess.Popen(["go", "test", "-race", "-count=1", "-v",
                                      "-timeout=180s", "-run", "^TestLiveReadService$",
                                      "./cmd/world-authd"], cwd=ROOT / "production", env=env,
                                     stdout=stream, stderr=subprocess.STDOUT,
                                     start_new_session=True)
            try:
                result = child.wait(timeout=200)
            except subprocess.TimeoutExpired:
                raise RuntimeError("live service integration exceeded its bounded test budget") from None
        if result:
            raise RuntimeError("live service test failed; inspect the private test.log")
        verified = json.loads(test_report.read_text())
        if verified.get("result") != "passed" or verified.get("dataset") != dataset:
            raise RuntimeError("live service report absent or mismatched")
        report.update(result="passed", run_id=site.run_id, dataset=dataset,
                      world_nodes=info["world_nodes"],
                      simulation_inspection_samples=info["inspection_samples"],
                      service_report_sha256=hashlib.sha256(test_report.read_bytes()).hexdigest(),
                      checks=verified["checks"],
                      source_sha256={str(path.relative_to(ROOT)): hashlib.sha256(path.read_bytes()).hexdigest()
                                     for path in sorted((ROOT / "production").rglob("*.go"))
                                     if not any(part.startswith(".") for part in path.relative_to(ROOT).parts)})
    except KeyboardInterrupt:
        report["error"] = "interrupted; cleanup requested"
        raise
    except Exception as error:
        # Do not echo credential files, subprocess arguments or configuration.
        report["error"] = str(error)
        raise
    finally:
        if child is not None and child.poll() is None:
            os.killpg(child.pid, signal.SIGTERM)
            try:
                child.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(child.pid, signal.SIGKILL)
                child.wait(timeout=5)
        site.cleanup()
        report["elapsed_seconds"] = round(time.monotonic() - started, 3)
        report["limits"] = ["Generated untrusted operator, not enrolled-operator interoperability.",
                            "All camera/gas/audio observations and robot execution are simulated.",
                            "Only loopback read service, no production mesh or physical actuation."]
        private_json(out / "report.json", report)
        os.chdir(previous_directory)
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    print(json.dumps({"result": report["result"], "report": str(out / "report.json"),
                      "service_report": str(out / "service-report.json")}), flush=True)


if __name__ == "__main__":
    main()

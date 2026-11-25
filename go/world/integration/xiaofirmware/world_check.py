"""Check live emulated firmware delivery through the actual World HTTP service.

The observer executable must connect to a running firmware emulator PTY. This
script provides no images or device responses. All evidence stays outside Git.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import urllib.request
import uuid


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    for name in ("world-camera", "observer", "prepared", "bridge-receipt", "out"):
        parser.add_argument("--" + name, type=Path, required=True)
    parser.add_argument("--serial-pty", required=True)
    parser.add_argument("--native-recorder", type=Path)
    args = parser.parse_args()
    out = args.out.resolve()
    if out.is_relative_to(Path(__file__).resolve().parents[2]):
        parser.error("runtime artifacts must be outside the checkout")
    out.mkdir(mode=0o700, parents=True, exist_ok=False)
    bridge = json.loads(args.bridge_receipt.read_text())
    if not bridge.get("emulated") or bridge.get("status") != "running" or bridge.get("serial_port") != args.serial_pty:
        raise ValueError("bridge receipt must identify the running emulator and selected PTY")
    (out / "bridge-snapshot.json").write_text(json.dumps(bridge, indent=2) + "\n")
    state = out / "state"
    state.mkdir(mode=0o700)
    run = "xiao-firmware-" + uuid.uuid4().hex
    # Each reconnect invokes the same production client against the live PTY.
    # exec avoids an intermediate process surviving World's cancellation.
    source = out / "capture-source"
    source.write_text("#!" + sys.executable + "\n" +
        "import os, uuid\n" +
        "binary = " + repr(str(args.observer.resolve())) + "\n" +
        "out = " + repr(str(out)) + " + '/capture-' + uuid.uuid4().hex\n" +
        "os.execv(binary, [binary, '--serial-pty', " + repr(args.serial_pty) +
        ", '--out', out, '--duration', '120s', '--stdout-jpeg'])\n")
    source.chmod(0o700)
    command = [str(args.world_camera.resolve()), "--device", "wendy-lite:emulated-xiao",
               "--camera-id", "0", "--state-dir", str(state), "--simulation-run", run,
               "--emulated-source", str(source), "--duration", "5m"]
    report = {"schema": "wendy.xiao-firmware-world-check.v1", "origin": "simulated",
              "run_id": run, "checks": {}, "command": command, "passed": False}
    report["firmware_sha256"] = bridge["firmware_sha256"]
    report["management_transport"] = bridge.get("management_transport", "USB Serial/JTAG")
    report["observer_sha256"] = hashlib.sha256(args.observer.read_bytes()).hexdigest()
    report["world_binary_sha256"] = hashlib.sha256(args.world_camera.read_bytes()).hexdigest()
    inputs = json.loads((args.prepared / "inputs.json").read_text())
    hashes = {item["sha256"] for item in inputs["stimuli"]}
    profile = None

    def request(path, body=None):
        req = urllib.request.Request(profile["url"] + path,
            data=None if body is None else json.dumps(body).encode(),
            headers={"Authorization": "Bearer " + profile["token"], "Content-Type": "application/json"})
        with urllib.request.urlopen(req, timeout=5) as response:
            return response.read()

    def wait_state(predicate, seconds=60):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            if process.poll() is not None:
                raise RuntimeError("World service exited before acceptance")
            current = json.loads(request("/v1/state"))
            if predicate(current):
                return current
            time.sleep(0.2)
        raise RuntimeError("live firmware did not reach required World state")

    with (out / "world.log").open("wb") as log:
        process = subprocess.Popen(command, stdout=log, stderr=subprocess.STDOUT)
        try:
            deadline = time.monotonic() + 15
            while not (state / "viewer-profile.json").exists():
                if process.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError("World service did not start")
                time.sleep(0.1)
            profile = json.loads((state / "viewer-profile.json").read_text())
            first = wait_state(lambda s: s["fresh"] and s.get("latest") is not None)
            receipt = first["latest"]
            report["checks"]["simulated_run_preserved"] = receipt["origin"] == "simulated" and receipt["simulation_run"] == run
            original = request("/v1/evidence/" + receipt["sha256"])
            report["checks"]["exact_sensor_input_received"] = hashlib.sha256(original).hexdigest() == receipt["sha256"] and receipt["sha256"] in hashes
            report["checks"]["full_resolution"] = (receipt["width"], receipt["height"]) == (2048, 1536)
            (out / "first-state.json").write_text(json.dumps(first, indent=2) + "\n")
            request("/v1/control", {"action": "pause"})
            paused = wait_state(lambda s: s["connection"] == "stopped" and not s["fresh"], 5)
            report["checks"]["pause_stops_freshness"] = paused["latest"]["id"] == receipt["id"]
            report["checks"]["original_retained_while_paused"] = request("/v1/evidence/" + receipt["sha256"]) == original
            request("/v1/control", {"action": "resume"})
            resumed = wait_state(lambda s: s["fresh"] and s["latest"]["source_session"] != receipt["source_session"] and s["latest"]["sha256"] != receipt["sha256"])
            report["checks"]["new_session_and_changed_frame_after_resume"] = resumed["latest"]["sha256"] in hashes
            report["checks"]["run_survives_reconnect"] = resumed["latest"]["origin"] == "simulated" and resumed["latest"]["simulation_run"] == run
            (out / "resumed-state.json").write_text(json.dumps(resumed, indent=2) + "\n")
            if args.native_recorder:
                subprocess.run([str(args.native_recorder.resolve()), "--record-emulated",
                                str(state / "viewer-profile.json"), str(out / "native-recording")],
                               stdout=log, stderr=subprocess.STDOUT, check=True, timeout=100)
                report["checks"]["native_recording"] = True
            report["passed"] = all(report["checks"].values())
        except Exception as error:
            report["error"] = str(error)
        finally:
            if process.poll() is None:
                process.send_signal(signal.SIGTERM)
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait(timeout=5)
            report["service_exit_code"] = process.returncode
            report["passed"] = report["passed"] and process.returncode == 0
            (out / "receipt.json").write_text(json.dumps(report, indent=2) + "\n")
    print(json.dumps({"passed": report["passed"], "receipt": str(out / "receipt.json")}))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

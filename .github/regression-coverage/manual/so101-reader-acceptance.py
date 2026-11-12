#!/usr/bin/env python3
"""Prepare audit 2247 physical adapter acceptance; generation performs no device I/O.

Inventory JSON requires platform (macos/linux), hostname, os_version,
architecture, python_version (3.12.x), lsof_version, captured_at, inventory_capture
(saved native USB/tty inventory path), owner, adapter with port/vendor_id/
product_id/serial_number/usb_location/physical_label, and six motors with id/name/
expected_model_number. A missing adapter serial is null, never inferred.
Output must be a new directory outside the checkout. All cases start not_run.
"""

import argparse
from datetime import datetime
import hashlib
import json
from pathlib import Path
import re
import shlex
import subprocess

ROOT = Path(__file__).resolve().parents[3]
READER = ROOT / "Examples/SO101Reader"
CASES = ("native-baseline", "stop-and-reopen", "usb-unplug-replug", "explicit-path-rebind", "busy-visible-owner", "native-permissions")


def line(value, name):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError(f"{name} must be a nonempty single-line value")
    return value


def artifact(path):
    path = Path(path).resolve(strict=True)
    if not path.is_file():
        raise ValueError(f"not a file: {path}")
    return {"path": str(path), "sha256": hashlib.sha256(path.read_bytes()).hexdigest()}


def generate(inventory_path, output):
    raw = json.loads(inventory_path.read_text())
    platform = raw["platform"]
    if platform not in ("macos", "linux"):
        raise ValueError("platform must select native macos or linux")
    for key in ("hostname", "os_version", "architecture", "python_version", "lsof_version", "captured_at", "owner"):
        line(raw[key], key)
    if not re.fullmatch(r"3\.12\.\d+", raw["python_version"]):
        raise ValueError("record the installed Python 3.12.x interpreter used for this acceptance")
    if datetime.fromisoformat(raw["captured_at"].replace("Z", "+00:00")).tzinfo is None:
        raise ValueError("captured_at must include its timezone")
    adapter = raw["adapter"]
    port = line(adapter["port"], "adapter.port")
    pattern = r"/dev/cu\.[A-Za-z0-9_.-]+" if platform == "macos" else r"/dev/(?:serial/by-id/[A-Za-z0-9_.:+-]+|tty(?:USB|ACM)[0-9]+)"
    if not re.fullmatch(pattern, port):
        raise ValueError("adapter.port must be one exact native callout/USB tty path, without globs or traversal")
    for key in ("vendor_id", "product_id"):
        if not re.fullmatch(r"[0-9a-f]{4}", adapter[key]):
            raise ValueError(f"adapter.{key} must be four lowercase hexadecimal digits")
    for key in ("usb_location", "physical_label"):
        line(adapter[key], key)
    if adapter["serial_number"] is not None:
        line(adapter["serial_number"], "serial_number")
    motors = raw["motors"]
    if (not isinstance(motors, list) or len(motors) != 6
            or any(type(m["id"]) is not int or not 1 <= m["id"] <= 253 for m in motors)
            or len({m["id"] for m in motors}) != 6 or len({m["name"] for m in motors}) != 6):
        raise ValueError("inventory must identify six distinct unicast motors and joint names")
    for motor in motors:
        line(motor["name"], "joint name")
        if motor["expected_model_number"] not in (777, 2825, 11272):
            raise ValueError("expected motor models must be supported STS/SMS register values")
    http_port = raw.get("http_port", 18876)
    if type(http_port) is not int or not 1024 <= http_port <= 65535:
        raise ValueError("http_port must be an explicit unprivileged TCP port")
    inventory_capture = artifact(raw["inventory_capture"])
    source_head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    source = [artifact(READER / name) for name in ("reader.py", "transport.py", "protocol.py", "identity.py", "run.sh")]
    out = output.resolve()
    if out == ROOT or ROOT in out.parents:
        raise ValueError("keep generated acceptance artifacts outside the checkout")
    out.mkdir(parents=True, mode=0o700, exist_ok=False)
    config = {"mode": "physical", "port": port, "baud": 1_000_000, "ids": [m["id"] for m in motors],
              "joints": [m["name"] for m in motors], "host": "127.0.0.1", "http_port": http_port,
              "interval": 0.5, "timeout": 0.1, "stale_after": 5, "reconnect_delay": 1,
              "binding": {"source": "operator_supplied_acceptance_inventory", "physical_label": adapter["physical_label"],
                          "inventory_sha256": inventory_capture["sha256"], "identity_verified": False}}
    evidence = {"audit_entries": ["2247"], "source_head": source_head, "source_root": str(ROOT), "dirty_preview": dirty,
                "inventory": raw, "inventory_artifact": artifact(inventory_path), "inventory_capture": inventory_capture,
                "reader_source": source, "historical_validation": artifact(READER / "VALIDATION.md"),
                "hardware_execution": "not_run", "robot_identity_proven": False,
                "cases": [{"id": name, "status": "not_run", "observations": [], "reviewer": None} for name in CASES],
                "cleanup": {"status": "not_run", "reader_exited": None, "serial_owner_after": None}}
    (out / "reader-config.json").write_text(json.dumps(config, indent=2) + "\n")
    evidence["reader_config"] = artifact(out / "reader-config.json")
    (out / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    (out / "capture-reader.py").write_text(CAPTURE)
    commands = {name: ["python3.12", str(out / "capture-reader.py"), "--case", name, "--selected-port", port, "--seconds", "30"] for name in CASES}
    (out / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
    inventory_command = ("/usr/sbin/ioreg -a -l -r -c IOUSBHostDevice" if platform == "macos"
                         else shlex.join(["udevadm", "info", "--query=property", "--name", port]))
    baseline = shlex.join(commands["native-baseline"])
    (out / "procedure.txt").write_text(f'''SO-101 native read-only adapter acceptance, audit 2247
Prepared for {raw['hostname']} ({platform}, {raw['os_version']}, {raw['architecture']})
Owner: {raw['owner']}; exact port: {port}
Adapter VID:PID {adapter['vendor_id']}:{adapter['product_id']}; serial {adapter['serial_number']!r}
USB location {adapter['usb_location']}; physical label {adapter['physical_label']}
Source {source_head}; dirty preview {dirty}
ALL CASES ARE NOT RUN. Generation has not queried or opened any serial device.

1. Provenance and reservation
   Regenerate from a clean committed checkout on the recorded native host.
   Reserve only the adapter identified above and the six inventoried motors.
   Save fresh output of `{inventory_command}`, `uname -a`, `python3.12 --version`
   and `lsof -v`, with timestamps. Compare the selected tty, VID/PID, serial and
   USB location to the saved inventory and physically labelled adapter before
   every run. If serial is absent, require the recorded physical label/location
   and an operator witness; never invent a serial or claim robot identity.
   Do not use a VM/container, automatic port discovery, sudo, a baud sweep,
   permission changes, motor SDK, calibration/homing, torque or goal commands.
   Keep the arm supported and clear of people. This reader does not change its
   existing torque state; physical handling must follow the owner's procedure.

2. Native baseline
   Confirm no motor-control app or reader owns this bus. The capture helper
   records bounded lsof output; ownership_unknown/busy is a refusal, not a pass.
   Run {baseline}
   The helper runs the unmodified reader for 30 seconds, snapshots health,
   readiness and observations on loopback, then sends SIGTERM to only its child.
   Each HTTP request is bounded to 2 seconds; shutdown allows 8 seconds, then
   records forced termination as a failed cleanup. No automatic pass is written.
   Require at least three advancing completed scan sequences, schema_version=1,
   source=physical_serial, synthetic=false, read_only=true, hardware_ready=true,
   /readyz HTTP 200, six inventoried IDs/names/models and fresh, error-free state.
   Compare adapter metadata source to macos_ioreg/linux_sysfs and the saved USB
   identity. Raw words are authoritative; every joint's position.calibrated,
   connection.identity_verified and connection.adapter.robot_identity_proven
   must remain false. USB presence alone is not proof.
   Do not equate PTY success with macOS IOSSIOSPEED or Linux USB-driver acceptance.

3. Stop and reopen
   Inspect cleanup.json, reader.log and lsof-after.txt. Require ordinary reader
   exit with cleanup.json returncode=0, forced_kill=false, no remaining descriptor
   owned by that reader PID, and no change to unrelated processes.
   Run the stop-and-reopen command from
   commands.json and require fresh physical sequences again, then clean shutdown.
   This checks real descriptor release and native baud configuration twice.
   The transport restores saved termios best-effort; an unplugged tty may have
   disappeared. Do not claim unmeasured physical baud or motor configuration.

4. USB unplug/replug
   With owner approval for physical handling, run the usb-unplug-replug command.
   Disconnect only the selected USB data link after healthy samples. Require
   hardware_ready=false, /readyz 503, disconnected/no_response/stale evidence and
   invalidated old readings. Reconnect the same adapter within the observation
   window. Recheck its inventory. If the exact configured path returns, require
   new fresh sequences and the same metadata, without stale readings labelled
   fresh. If the path changed, record that fact instead of silently selecting it.

5. Explicit rebind after a changed path
   The reader loads its binding at startup and never follows another tty.
   After stopping it, save the new native inventory, verify the same physical
   adapter, and generate a NEW plan naming the new exact port. Retain both plans
   and compare serial/location evidence. Run that plan's explicit-path-rebind
   case. A path change or supplied binding alone never proves robot identity.

6. Ownership and permissions
   For busy-visible-owner, use an already identified read-only owner with the
   owner's agreement. Do not launch a competing motor-control process. Record
   its PID and lsof evidence; require reader state busy, readiness 503, and no
   change to that owner's lifetime. The helper stops only its own reader.
   For native-permissions, prepare a separate plan as an existing ordinary host
   account whose access is refused. Require permission_denied with health still available. Do not
   chmod/chown the device or change udev rules to manufacture this case.
   If these conditions are unavailable, leave their cases not_run with a reason.

7. Evidence and cleanup
   Review each capture's observations.jsonl, reader.log, cleanup.json and owner
   inventory. Attach hashes and reviewer identity to evidence.json. Record a
   case passed only after its physical criteria were observed; failed/unknown
   evidence is not a pass. Every case also requires successful child exit and
   no forced kill. JSON from an occupied port cannot establish acceptance if
   this reader failed to start. Stop only this reader; leave other owners/services
   intact. Do not send any motor commands during cleanup. Restore only USB
   cabling changed for this reserved test according to the owner's procedure.
   Existing VALIDATION.md contains historical physical macOS acceptance. This
   new plan neither replaces it nor reuses it as a fresh run. It does not prove
   Darwin Agent deployment, container ownership visibility or calibrated angles.
''')
    return evidence


CAPTURE = r'''#!/usr/bin/env python3
"""Human-invoked, bounded native observations. Never writes motor registers."""
import argparse
from contextlib import contextmanager
from datetime import datetime, timezone
import hashlib
import json
import os
from pathlib import Path
import platform
import signal
import socket
import subprocess
import sys
import time
import urllib.error
import urllib.request
import uuid

root = Path(__file__).resolve().parent
plan = json.loads((root / "evidence.json").read_text())
parser = argparse.ArgumentParser(description=__doc__)
parser.add_argument("--case", choices=[case["id"] for case in plan["cases"]], required=True)
parser.add_argument("--selected-port", required=True)
parser.add_argument("--seconds", type=int, default=30)
args = parser.parse_args()
expected = plan["inventory"]
if args.selected_port != expected["adapter"]["port"] or not 10 <= args.seconds <= 60:
    parser.error("name the exact inventoried port and a 10..60 second observation")
if sys.version_info[:2] != (3, 12) or platform.system() != {"macos": "Darwin", "linux": "Linux"}[expected["platform"]]:
    parser.error("use Python 3.12 on the inventoried native platform")
if platform.node() != expected["hostname"]:
    parser.error("this is not the inventoried host")
source = Path(plan["source_root"])
head = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=source, text=True).strip()
dirty = subprocess.check_output(["git", "status", "--porcelain"], cwd=source, text=True).strip()
if plan["dirty_preview"] or dirty or head != plan["source_head"]:
    parser.error("regenerate against the exact clean committed source")
for item in [*plan["reader_source"], plan["reader_config"], plan["inventory_artifact"], plan["inventory_capture"]]:
    if hashlib.sha256(Path(item["path"]).read_bytes()).hexdigest() != item["sha256"]:
        parser.error("source, config or inventory differs from the prepared plan")
if json.loads(Path(plan["inventory_artifact"]["path"]).read_text()) != expected:
    parser.error("evidence inventory differs from the prepared inventory artifact")
config = json.loads((root / "reader-config.json").read_text())
if config["mode"] != "physical" or config["port"] != args.selected_port or config["host"] != "127.0.0.1":
    parser.error("reader config no longer matches the prepared physical target")
try:
    with socket.socket() as probe:
        probe.bind(("127.0.0.1", config["http_port"]))
except OSError:
    parser.error("the planned HTTP port is occupied; do not consume another reader's output")

class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, message, headers, new_url):
        return None

opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())

@contextmanager
def request_deadline():
    # A socket timeout alone permits an indefinitely slow-dripping response.
    def expired(signum, frame):
        raise TimeoutError("reader request exceeded 2 second wall deadline")

    previous_handler = signal.signal(signal.SIGALRM, expired)
    previous_timer = signal.setitimer(signal.ITIMER_REAL, 2)
    started = time.monotonic()
    try:
        yield
    finally:
        signal.setitimer(signal.ITIMER_REAL, 0)
        signal.signal(signal.SIGALRM, previous_handler)
        if previous_timer[0]:
            signal.setitimer(signal.ITIMER_REAL, max(0.000001, previous_timer[0] - (time.monotonic() - started)), previous_timer[1])

@contextmanager
def graceful_stop():
    state = {"requested": False}

    def requested(signum, frame):
        state["requested"] = True

    previous = signal.signal(signal.SIGTERM, requested)
    try:
        yield state
    finally:
        signal.signal(signal.SIGTERM, previous)

out = root / "captures" / (args.case + "-" + uuid.uuid4().hex[:8])
out.mkdir(parents=True, exist_ok=False)
ports = [args.selected_port]
if expected["platform"] == "macos":
    ports.append("/dev/tty." + args.selected_port.removeprefix("/dev/cu."))

def owners(name):
    try:
        result = subprocess.run(["lsof", "-nP", "--", *ports], capture_output=True, text=True, timeout=5)
        (out / name).write_text(f"returncode={result.returncode}\n{result.stdout}\n{result.stderr}")
    except (OSError, subprocess.TimeoutExpired) as error:
        (out / name).write_text("ownership_unknown: " + str(error))

owners("lsof-before.txt")
env = {key: value for key, value in os.environ.items() if not key.startswith("SO101_")}
env.update(SO101_CONFIG=str(root / "reader-config.json"), SO101_PYTHON=sys.executable)
cleanup = {"case": args.case, "status": "needs_human_review", "forced_kill": False,
           "python": sys.version, "port": args.selected_port, "source_head": head}
with (out / "reader.log").open("w") as log, graceful_stop() as stop:
    process = subprocess.Popen(["/bin/sh", str(source / "Examples/SO101Reader/run.sh")], env=env, stdout=log, stderr=subprocess.STDOUT)
    cleanup["reader_pid"] = process.pid
    try:
        deadline = time.monotonic() + args.seconds
        with (out / "observations.jsonl").open("w") as observations:
            while not stop["requested"] and time.monotonic() < deadline and process.poll() is None:
                for path in ("/healthz", "/readyz", "/v1/observations"):
                    sample = {"at": datetime.now(timezone.utc).isoformat(), "path": path}
                    try:
                        with request_deadline():
                            try:
                                response = opener.open(f"http://127.0.0.1:{config['http_port']}{path}", timeout=2)
                            except urllib.error.HTTPError as error:
                                response = error
                            with response:
                                body = response.read(1_048_577)
                                if len(body) > 1_048_576:
                                    raise ValueError("reader response exceeded 1 MiB")
                                sample.update(status=response.status, body=json.loads(body))
                    except (OSError, ValueError) as error:
                        sample["error"] = str(error)
                    observations.write(json.dumps(sample) + "\n")
                    observations.flush()
                time.sleep(0.5)
    finally:
        if process.poll() is None:
            process.send_signal(signal.SIGTERM)
        try:
            cleanup["returncode"] = process.wait(timeout=8)
        except subprocess.TimeoutExpired:
            cleanup["forced_kill"] = True
            process.kill()
            cleanup["returncode"] = process.wait(timeout=2)
        owners("lsof-after.txt")
        (out / "cleanup.json").write_text(json.dumps(cleanup, indent=2) + "\n")
print(out)
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("--inventory", type=Path, required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    generate(args.inventory, args.output)

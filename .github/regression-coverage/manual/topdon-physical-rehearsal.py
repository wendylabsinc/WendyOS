#!/usr/bin/env python3
"""Generate 2770's physical TOPDON plan offline; never access hardware.

Inventory: cli_binary, agent_binary, agent_source_commit, threshold_celsius; board {selector, serial,
owner, architecture}; camera {stable_id, serial, usb_port, bcd_device}; records
{device_info, camera_list, usb_descriptor, installed_binary, reference_calibration,
reference_setup, recovery}. All records are paths to existing files. The exact
supported device is USB 2bdf:0102, bcdDevice 0409, with observed 96x200 HIK mode.
Reference setup must predeclare emissivity, environment, instrument, uncertainty,
target placement/temperature sequence and threshold. Values are never invented.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import re
import shlex
import shutil
import uuid

ROOT = Path(__file__).resolve().parents[3]


def artifact(path):
    path = Path(path).resolve(strict=True)
    if not path.is_file() or not path.stat().st_size:
        raise ValueError("record must be a nonempty regular file")
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(chunk)
    return {"path": str(path), "sha256": digest.hexdigest()}


def generate(inventory, destination):
    data = json.loads(Path(inventory).read_text())
    cli, agent = artifact(data["cli_binary"]), artifact(data["agent_binary"])
    if not os.access(cli["path"], os.X_OK) or not re.fullmatch(r"[0-9a-f]{40}", data["agent_source_commit"]):
        raise ValueError("pin executable CLI and full Agent source SHA")
    board, camera = data["board"], data["camera"]
    for group, keys in ((board, ("selector", "serial", "owner")), (camera, ("stable_id", "serial", "usb_port"))):
        for key in keys:
            if not isinstance(group[key], str) or not group[key].strip() or any(ord(c) < 32 for c in group[key]):
                raise ValueError("identity fields require nonempty single lines")
    if board["selector"].startswith("-") or board["architecture"] not in ("arm64", "amd64") or camera["bcd_device"] != "0409":
        raise ValueError("requires explicit Linux target architecture and supported bcdDevice 0409")
    if not camera["stable_id"].startswith(("by-id:", "by-path:")):
        raise ValueError("use the observed physical camera stable ID")
    threshold = data["threshold_celsius"]
    if isinstance(threshold, bool) or not isinstance(threshold, (int, float)) or not math.isfinite(threshold):
        raise ValueError("predeclare a finite threshold in the reference setup")
    records = {key: artifact(data["records"][key]) for key in ("device_info", "camera_list", "usb_descriptor",
        "installed_binary", "reference_calibration", "reference_setup", "recovery")}
    rows = json.loads(Path(records["camera_list"]["path"]).read_text())
    matches = [row for row in rows if row.get("stable_id") == camera["stable_id"]]
    if len(matches) != 1 or matches[0].get("thermal") != {"adapter": "topdon-tc001-hik-v1", "capture_width": 96, "capture_height": 200}:
        raise ValueError("actual camera listing must uniquely declare the 96x200 HIK mode")
    descriptor = json.loads(Path(records["usb_descriptor"]["path"]).read_text())
    if any(descriptor.get(key) != value for key, value in {"idVendor": "2bdf", "idProduct": "0102", "bcdDevice": "0409", "serial": camera["serial"], "port": camera["usb_port"]}.items()):
        raise ValueError("USB descriptor transcription must match the exact supported physical target")
    output = Path(destination).resolve()
    output.mkdir(mode=0o700, parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex[:12]
    app_id = "sh.wendy.regression.topdon" + nonce
    app = output / "app"
    app.mkdir(mode=0o700)
    example = ROOT / "Examples/ThermalNotify"
    for filename in ("Package.swift", "Package.resolved", "build.stagefile.yaml", "build.stagefile.lock.yaml"):
        shutil.copyfile(example / filename, app / filename)
    shutil.copytree(example / "Sources", app / "Sources")
    shutil.copytree(example / "Tests", app / "Tests")
    config = json.loads((example / "wendy.json").read_text())
    config["appId"] = app_id
    config["entitlements"] = [{"type": "admin"}]
    # Keep the generated project safe even if someone omits the CLI --env flags.
    config["env"] = {"THERMAL_NOTIFY_DELIVERY": "monitor-only", "THERMAL_CAMERA_STABLE_ID": camera["stable_id"],
                     "THERMAL_NOTIFY_THRESHOLD_CELSIUS": str(threshold)}
    (app / "wendy.json").write_text(json.dumps(config, indent=2) + "\n")
    commands = {
        "camera_listing": [cli["path"], "--device", board["selector"], "--json", "device", "camera", "list"],
        "monitor_only_attached_from_app_directory": ["python3", str(ROOT / ".github/scripts/run-owned-media-test.py"), "--timeout", "300", "--", cli["path"], "run", "--device", board["selector"], "--yes", "--env", "THERMAL_NOTIFY_DELIVERY=monitor-only", "--env", "THERMAL_CAMERA_STABLE_ID=" + camera["stable_id"], "--env", "THERMAL_NOTIFY_THRESHOLD_CELSIUS=" + str(threshold)],
        "stop_owned_app": [cli["path"], "--device", board["selector"], "device", "apps", "stop", app_id],
        "apps": [cli["path"], "--device", board["selector"], "--json", "device", "apps", "list"],
    }
    (output / "inspect-frame.py").write_text("""#!/usr/bin/env python3
import base64,json,struct,sys
frame=json.load(open(sys.argv[1]))
raw=base64.b64decode(frame['data'], validate=True)
if len(raw)!=38400: raise ValueError('expected exact 96x200 frame')
print('geometry header:', struct.unpack_from('<8H',raw,97*192))
print('summary:', struct.unpack_from('<10H',raw,196*192))
""")
    (output / "commands.json").write_text(json.dumps(commands, indent=2) + "\n")
    build = f"CGO_ENABLED=0 GOOS=linux GOARCH={board['architecture']} go build -o " + shlex.quote(str(output / "topdon-capture")) + " ./.github/scripts/fixtures/topdon-physical-capture"
    capture = shlex.join(["./topdon-capture", "--stable-id", camera["stable_id"], "--duration", "60s", "--frames", "1600"])
    guide = Path(__file__).with_suffix(".txt").read_text().replace("@OUTPUT@", shlex.quote(str(output))).replace("@APP_ID@", app_id).replace("@BUILD@", build).replace("@CAPTURE@", capture)
    guide += "\nPinned operator commands:\n" + "\n".join(key + ":\n" + shlex.join(argv) for key, argv in commands.items()) + "\n"
    (output / "procedure.txt").write_text(guide)
    shutil.copyfile(Path(__file__).with_name("topdon-recorded-replay.swift"), output / "RecordedTopdonTests.swift")
    (output / "reference.template.json").write_text(json.dumps({"observer": None, "reference_serial": None,
        "calibration_record_sha256": records["reference_calibration"]["sha256"], "samples": []}, indent=2) + "\n")
    sources = [Path(__file__), Path(__file__).with_suffix(".txt"), Path(__file__).with_name("topdon-recorded-replay.swift"),
        ROOT / ".github/scripts/fixtures/topdon-physical-capture/main.go", example / "Sources/ThermalNotify/ThermalReading.swift",
        example / "Sources/ThermalNotify/Notifications.swift", ROOT / "go/internal/agent/services/video_thermal.go"]
    sources += list((example / "Sources").rglob("*.swift"))
    sources += [example / name for name in ("Package.swift", "Package.resolved", "wendy.json", "build.stagefile.yaml", "build.stagefile.lock.yaml")]
    evidence = {"audit_entries": ["2770"], "inventory": data, "cli": cli, "agent": agent, "records": records,
        "source_files": [artifact(path) for path in sorted(set(sources))], "generated_app_config": artifact(app / "wendy.json"), "app_id": app_id,
        "hardware_execution": "not_run", "cases": [{"id": key, "status": "not_run", "evidence": [], "reviewer": None}
            for key in ("identity", "physical_capture", "independent_celsius", "geometry", "freshness_replay", "threshold_monitor", "cleanup")]}
    (output / "evidence.json").write_text(json.dumps(evidence, indent=2) + "\n")
    return evidence


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("inventory", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    generate(args.inventory, args.output)

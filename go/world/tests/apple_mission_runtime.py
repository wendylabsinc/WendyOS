"""Exercise explicit mission actions in an actual DEBUG Apple app.

Requires a built app and an existing simulator for iOS/visionOS. Imports the
profile privately, binds every report to an invocation nonce and the fixture
run, and never creates a simulator or sends a mission implicitly.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import secrets
import shutil
import sys
import subprocess
import time

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from integration import nativeapp


def command(*args):
    result = subprocess.run(args, capture_output=True, text=True, timeout=60)
    if result.returncode:
        raise RuntimeError(result.stderr[-1500:] or result.stdout[-1500:])
    return result.stdout.strip()


def run(args):
    ready = json.loads(args.ready.read_text())
    if ready.get("mission_schema") != "wendy.native-mission.site.v1":
        raise ValueError("explicit native mission fixture required")
    if time.time()*1000 + 120000 >= ready["reader_credentials_expire_unix_ms"]:
        raise ValueError("fixture has insufficient validity remaining")
    source = args.profile or Path(ready["native_mission_profiles"][args.platform])
    raw = source.read_bytes()
    if len(raw) > 128 << 10:
        raise ValueError("profile exceeds bound")
    profile = json.loads(raw)
    if (profile["mission_context"]["context"]["run_id"] != ready["run_id"]
            or profile["mission_credentials"]["url"] != f'http://127.0.0.1:{ready["node_ports"][0]}'
            or profile["mission_context"]["context"]["package_sha256"] != ready["package_sha256"]):
        raise ValueError("profile differs from selected fixture")
    del raw, profile
    app = args.app.resolve()
    bundle = "dev.wendy.world.operator." + args.platform
    if args.platform == "macos":
        if args.install: raise ValueError("macOS runs the explicitly built bundle")
        data, imported = Path.home(), source.resolve()
    else:
        if not args.device: raise ValueError("an explicit existing simulator UUID is required")
        if args.install: command("xcrun", "simctl", "install", args.device, str(app))
        data = Path(command("xcrun", "simctl", "get_app_container", args.device, bundle, "data"))
        imported = data / "Documents/native-mission-test-profile.json"
        imported.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, imported)
        imported.chmod(0o600)
    report = data / "Library/Application Support/WendyWorld/MissionTests" / (args.stage + ".json")
    report.unlink(missing_ok=True)
    nonce = secrets.token_hex(32)
    invocation = ["--profile", str(imported), "--mission-test-stage", args.stage,
        "--mission-test-nonce", nonce, "--mission-test-id", args.task_id,
        "--mission-test-target-x", str(args.target_x), "--mission-test-duration-ms", str(args.duration_ms)]
    if args.proposal: invocation += ["--mission-test-proposal", args.proposal]
    started = time.monotonic()
    process = None
    try:
        if args.platform == "macos":
            process = subprocess.Popen([nativeapp.executable(app), *invocation], start_new_session=True)
        else: command("xcrun", "simctl", "launch", "--terminate-running-process", args.device, bundle, *invocation)
        deadline = time.monotonic() + 65
        while not report.exists() and time.monotonic() < deadline:
            time.sleep(.2)
        if not report.exists(): raise RuntimeError("app did not produce its bounded mission report")
        raw = report.read_bytes()
        if len(raw) > 4 << 20: raise RuntimeError("report exceeds bound")
        result = json.loads(raw)
        if (result.get("schema") != "wendy.apple.mission-model-test.v1" or result.get("invocation_nonce") != nonce
                or result.get("run_id") != ready["run_id"] or result.get("platform") != args.platform
                or result.get("stage") != args.stage or result.get("task_id") != args.task_id):
            raise RuntimeError("report does not identify this invocation")
        args.out.mkdir(mode=0o700, parents=True, exist_ok=True)
        label = args.label or args.platform + "-" + args.stage
        if not label or any(c not in "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_" for c in label):
            raise ValueError("invalid evidence label")
        saved = args.out / (label + ".json")
        saved.write_bytes(raw); saved.chmod(0o600)
        hashes = {saved.name: hashlib.sha256(raw).hexdigest()}
        if args.platform != "macos" and not args.no_screenshot:
            time.sleep(2)
            screenshot = args.out / (label + ".png")
            command("xcrun", "simctl", "io", args.device, "screenshot", str(screenshot))
            hashes[screenshot.name] = hashlib.sha256(screenshot.read_bytes()).hexdigest()
        summary = {"schema": "wendy.apple.mission-stage.v1", "stage": args.stage, "task_id": args.task_id,
            "run_id": ready["run_id"], "invocation_nonce": nonce, "platform": args.platform,
            "device": args.device, "node_image": ready["node_image"], "scene_image": ready["scene_image"],
            "elapsed_seconds": round(time.monotonic()-started, 3), "result": "failed" if result.get("error") else "passed",
            "artifacts_sha256": hashes}
        (args.out / (label + "-summary.json")).write_text(json.dumps(summary, indent=2)+"\n")
        if result.get("error"): raise RuntimeError(str(result["error"])[:1500])
        print(json.dumps(summary), flush=True)
    finally:
        if process is not None:
            nativeapp.stop_owned(process)
        if args.platform != "macos": imported.unlink(missing_ok=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--ready", type=Path, required=True)
    parser.add_argument("--profile", type=Path)
    parser.add_argument("--platform", choices=("macos", "ios", "visionos"), required=True)
    parser.add_argument("--app", type=Path, required=True)
    parser.add_argument("--device")
    parser.add_argument("--install", action="store_true")
    parser.add_argument("--no-screenshot", action="store_true", help="omit screenshot collection for timing-sensitive send/stop stages")
    parser.add_argument("--stage", choices=("draft", "proposal", "reopen", "send", "reconcile", "stop"), required=True)
    parser.add_argument("--task-id", required=True)
    parser.add_argument("--target-x", type=float, default=1.8)
    parser.add_argument("--duration-ms", type=int, default=120000)
    parser.add_argument("--proposal")
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--label")
    run(parser.parse_args())

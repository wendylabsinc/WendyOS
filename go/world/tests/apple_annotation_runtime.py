"""Run one DEBUG annotation stage in an explicitly selected existing Apple simulator."""
import argparse
import json
import os
from pathlib import Path
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


def run(device, platform, site, stage, out, install, app_path=None):
    bundle = "dev.wendy.world.operator." + platform
    if platform != "macos" and app_path is None:
        raise ValueError("Mobile QA requires an explicitly built legacy app host via --app")
    app = app_path.resolve() if app_path else None
    process = None
    if platform == "macos":
        if install:
            raise ValueError("macOS runs the local test bundle; --install is for simulators")
        data = Path.home()
        profile = (site / "macos-contributor-profile.json").resolve()
    else:
        if not device:
            raise ValueError("--device is required for an existing simulator")
        if install:
            command("xcrun", "simctl", "install", device, str(app))
        data = Path(command("xcrun", "simctl", "get_app_container", device, bundle, "data"))
        profile = data / "Documents" / "annotation-test-profile.json"
        profile.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(site / (platform + "-contributor-profile.json"), profile)
        profile.chmod(0o600)
    report = data / "Library/Application Support/WendyWorld/AnnotationTests" / (stage + ".json")
    report.unlink(missing_ok=True)
    args = ["--profile", str(profile), "--annotation-test-stage", stage,
            "--annotation-test-text", platform + "-offline-annotation"]
    try:
        if platform == "macos":
            process = subprocess.Popen([nativeapp.executable(app), *args], start_new_session=True)
        else:
            command("xcrun", "simctl", "launch", "--terminate-running-process", device, bundle, *args)
        end = time.monotonic() + 65
        while not report.exists() and time.monotonic() < end:
            time.sleep(.2)
        if not report.exists():
            raise RuntimeError(f"{platform} did not produce its bounded {stage} report")
        result = json.loads(report.read_text())
        if result.get("error") or not result.get("canAnnotate"):
            raise RuntimeError(json.dumps(result)[:1500])
        out.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(report, out / (platform + "-" + stage + ".json"))
        if platform != "macos":
            # Reports precede the simulator's window transition. This bounded delay
            # allows the rendered window to replace the launch placeholder.
            time.sleep(2)
            command("xcrun", "simctl", "io", device, "screenshot", str(out / (platform + "-" + stage + ".png")))
            profile.unlink(missing_ok=True)
        print(json.dumps({"platform": platform, "stage": stage, "device": device, "report": result}))
    finally:
        if process is not None:
            nativeapp.stop_owned(process)



if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--device", help="existing booted simulator UUID; required except for macOS; creates no VM")
    parser.add_argument("--platform", choices=("ios", "visionos", "macos"), required=True)
    parser.add_argument("--site", type=Path, required=True)
    parser.add_argument("--stage", choices=("prepare", "enqueue", "reopen", "sync"), required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--install", action="store_true")
    parser.add_argument("--app", type=Path, help="explicit locally built app bundle")
    args = parser.parse_args()
    run(args.device, args.platform, args.site, args.stage, args.out, args.install, args.app)

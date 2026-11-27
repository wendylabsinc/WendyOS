"""Run one explicit mission UI stage on a named existing Android emulator.

This never creates an AVD, controls hardware, or changes guest time. The caller
owns the simulation fixture and coordinates partitions and human-action stages.
"""
import argparse
import hashlib
import json
from pathlib import Path
import re
import secrets
import shlex
import subprocess
import time

APP = "dev.wendy.world"
RUNNER = "dev.wendy.world.test/dev.wendy.world.SmokeRunner"


def run(args):
    def adb(*argv, raw=None, timeout=30, silent=False):
        result = subprocess.run([str(args.adb), "-s", args.serial, *argv], input=raw,
            stdout=subprocess.DEVNULL if silent else subprocess.PIPE,
            stderr=subprocess.PIPE, timeout=timeout)
        if result.returncode:
            raise RuntimeError("bounded adb command failed: " + argv[0])
        return result.stdout or b""

    def shell(*argv, **kwargs):
        return adb("shell", "-T", shlex.join(argv), **kwargs)

    if not re.fullmatch(r"emulator-[0-9]+", args.serial):
        raise ValueError("an explicit emulator serial is required")
    avd = adb("emu", "avd", "name").decode().splitlines()
    if not avd or avd[0] != args.avd:
        raise ValueError("selected emulator is not the named existing AVD")
    ready = json.loads(args.ready.read_text())
    if ready.get("mission_schema") != "wendy.native-mission.site.v1":
        raise ValueError("unsupported mission fixture")
    args.output.mkdir(mode=0o700, parents=True, exist_ok=True)
    label = args.label or args.stage
    if not re.fullmatch(r"[a-zA-Z0-9-]+", label):
        raise ValueError("invalid evidence label")
    nonce = secrets.token_hex(32)

    def save(name, data):
        path = args.output / name
        path.write_bytes(data)
        path.chmod(0o600)
        return hashlib.sha256(data).hexdigest()

    shell("am", "force-stop", "--user", "0", APP)
    if args.stage != "reopen":
        profile_path = args.profile or Path(ready["native_mission_profiles"]["android"])
        raw = profile_path.read_bytes()
        if not 0 < len(raw) <= 65536:
            raise ValueError("profile exceeds bound")
        profile = json.loads(raw)
        credential = profile["mission_credentials"]
        if (credential["url"] != "http://10.0.2.2:" + str(ready["node_ports"][0])
                or credential["audience"] != "node-1"
                or profile["mission_clock_trust"]["run_id"] != ready["run_id"]
                or profile["mission_context"]["context"]["run_id"] != ready["run_id"]
                or profile["mission_context"]["context"]["package_sha256"] != ready["package_sha256"]):
            raise ValueError("mission profile does not match the selected fixture")
        shell("run-as", APP, "mkdir", "-p", "files")
        shell("run-as", APP, "tee", "files/instrumentation-profile.json", raw=raw, silent=True)
        shell("run-as", APP, "chmod", "600", "files/instrumentation-profile.json")
        del raw, profile, credential
    name = "android-missions-" + args.stage
    shell("run-as", APP, "rm", "-f", "files/" + name + ".json")
    options = ["-e", "missions_stage", args.stage, "-e", "invocation_nonce", nonce,
        "-e", "run_id", ready["run_id"], "-e", "target_x", str(args.target_x),
        "-e", "duration_ms", str(args.duration_ms)]
    if args.task_id:
        options += ["-e", "task_id", args.task_id]
    if args.proposal_id:
        options += ["-e", "proposal_id", args.proposal_id]
    started = time.monotonic()
    output = shell("am", "instrument", "--user", "0", "-w", *options, RUNNER, timeout=120)
    artifacts = {label + "-instrumentation.txt": save(label + "-instrumentation.txt", output)}
    if (b"Android mission stage passed." not in output
            or (b"INSTRUMENTATION_CODE:" in output and b"INSTRUMENTATION_CODE: -1" not in output)):
        try:
            failure = adb("exec-out", "run-as", APP, "cat", "files/" + name + ".json")
        except RuntimeError:
            failure = b""
        if len(failure) <= 256 << 10:
            try:
                details = json.loads(failure)
                if details.get("invocation_nonce") == nonce and details.get("run_id") == ready["run_id"]:
                    save(label + "-failure.json", failure)
            except (ValueError, TypeError):
                pass
        raise RuntimeError("mission instrumentation failed; output retained")
    proof = adb("exec-out", "run-as", APP, "cat", "files/" + name + ".json")
    if len(proof) > 256 << 10:
        raise RuntimeError("mission report exceeds bound")
    report = json.loads(proof)
    if report.get("stage") != args.stage or report.get("invocation_nonce") != nonce or report.get("run_id") != ready["run_id"]:
        raise RuntimeError("report does not bind this invocation and run")
    artifacts[label + ".json"] = save(label + ".json", proof)
    screenshot = adb("exec-out", "run-as", APP, "cat", "files/" + name + ".png")
    if not screenshot.startswith(b"\x89PNG\r\n\x1a\n") or len(screenshot) > 16 << 20:
        raise RuntimeError("invalid mission screenshot")
    artifacts[label + ".png"] = save(label + ".png", screenshot)
    if args.stage in ("authorize", "denied", "review"):
        review = adb("exec-out", "run-as", APP, "cat", "files/android-mission-review.png")
        if not review.startswith(b"\x89PNG\r\n\x1a\n") or len(review) > 16 << 20:
            raise RuntimeError("invalid review screenshot")
        artifacts[label + "-review.png"] = save(label + "-review.png", review)
    if "instrumentation-profile.json" in shell("run-as", APP, "ls", "files").decode().splitlines():
        raise RuntimeError("runner left its private import profile behind")

    def installed_apk_digest(package):
        paths = shell("pm", "path", package).decode().splitlines()
        if len(paths) != 1 or not paths[0].startswith("package:/data/app/") or not paths[0].endswith(".apk"):
            raise RuntimeError("unexpected installed APK layout")
        digest = shell("sha256sum", paths[0][len("package:"):]).decode().split()[0]
        if not re.fullmatch(r"[0-9a-f]{64}", digest):
            raise RuntimeError("invalid installed APK digest")
        return digest

    summary = {"schema": "wendy.android.mission-runtime.v1", "stage": args.stage,
        "run_id": ready["run_id"], "invocation_nonce": nonce, "serial": args.serial, "avd": args.avd,
        "task_id": report["task_id"], "instruction_sha256": report["entry"].get("instruction_sha256"),
        "signed_instruction_kind": "stop" if args.stage == "stop-external" else "mission",
        "node_image": ready["node_image"], "scene_image": ready["scene_image"],
        "elapsed_seconds": round(time.monotonic() - started, 3), "result": "passed",
        "profile_removed": True, "installed_app_apk_sha256": installed_apk_digest(APP),
        "installed_test_apk_sha256": installed_apk_digest(APP + ".test"), "artifacts_sha256": artifacts}
    save(label + "-summary.json", (json.dumps(summary, indent=2) + "\n").encode())
    print(json.dumps(summary), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adb", type=Path, required=True)
    parser.add_argument("--serial", required=True)
    parser.add_argument("--avd", required=True)
    parser.add_argument("--ready", type=Path, required=True)
    parser.add_argument("--profile", type=Path)
    parser.add_argument("--stage", choices=("draft", "review", "reopen", "authorize", "retry", "reconcile", "stop", "stop-external", "denied", "completed"), required=True)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--label")
    parser.add_argument("--task-id")
    parser.add_argument("--proposal-id")
    parser.add_argument("--target-x", type=float, default=1.8)
    parser.add_argument("--duration-ms", type=int, default=120000)
    run(parser.parse_args())

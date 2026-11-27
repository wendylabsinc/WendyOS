"""Run one annotation stage on an explicitly selected existing Android AVD.

Profiles enter only app-private storage. Evidence freshness uses a unique
invocation nonce and fixture run, never a comparison with the guest wall clock.
This helper neither creates an AVD nor changes a device clock.
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
        raise ValueError("selected emulator is not the expected existing AVD")
    ready = json.loads((args.site / "ready.json").read_text())
    if ready.get("schema") != "wendy.native-test.site.v1":
        raise ValueError("unsupported native fixture")
    if time.time() * 1000 + 120000 >= ready["reader_credentials_expire_unix_ms"]:
        raise ValueError("fixture credentials have insufficient remaining validity")
    args.out.mkdir(mode=0o700, parents=True, exist_ok=True)
    label = args.label or args.stage
    if not re.fullmatch(r"[a-zA-Z0-9-]+", label):
        raise ValueError("invalid evidence label")
    nonce = secrets.token_hex(32)

    def save(name, data):
        path = args.out / name
        path.write_bytes(data)
        path.chmod(0o600)
        return hashlib.sha256(data).hexdigest()

    shell("am", "force-stop", "--user", "0", APP)
    if args.stage in ("prepare", "sync"):
        raw = (args.site / "android-contributor-profile.json").read_bytes()
        if not 0 < len(raw) <= 65536:
            raise ValueError("profile exceeds bound")
        profile = json.loads(raw)
        credential = profile["credentials"]
        if (credential["url"] != "http://10.0.2.2:" + str(ready["node_ports"][2])
                or credential["audience"] != "node-3"
                or credential["clock_trust"]["run_id"] != ready["run_id"]
                or profile["annotation_context"]["run_id"] != ready["run_id"]):
            raise ValueError("contributor profile does not match the selected fixture")
        shell("run-as", APP, "mkdir", "-p", "files")
        shell("run-as", APP, "tee", "files/instrumentation-profile.json", raw=raw, silent=True)
        shell("run-as", APP, "chmod", "600", "files/instrumentation-profile.json")
        del raw, profile, credential

    name = "android-annotations-" + args.stage
    shell("run-as", APP, "rm", "-f", "files/" + name + ".json")
    options = ["-e", "annotations_stage", args.stage, "-e", "invocation_nonce", nonce]
    if args.expect_clock_unknown:
        if args.stage != "reopen":
            raise ValueError("unknown-clock assertion is only supported by reopen")
        options += ["-e", "expect_clock_unknown", "true"]
    if args.stage == "enqueue":
        options += ["-e", "annotation_text", args.text]
    if args.stage == "sync" and args.expected_other_text:
        options += ["-e", "expected_other_text", args.expected_other_text]
    started = time.monotonic()
    output = shell("am", "instrument", "--user", "0", "-w", *options, RUNNER, timeout=180)
    files = {label + "-instrumentation.txt": save(label + "-instrumentation.txt", output)}
    if (b"Android annotation stage passed." not in output
            or (b"INSTRUMENTATION_CODE:" in output and b"INSTRUMENTATION_CODE: -1" not in output)):
        raise RuntimeError("instrumentation did not report the expected success; output retained")
    proof = adb("exec-out", "run-as", APP, "cat", "files/" + name + ".json")
    if len(proof) > 256 << 10:
        raise RuntimeError("instrumentation report exceeds bound")
    report = json.loads(proof)
    if report.get("stage") != args.stage or report.get("invocation_nonce") != nonce or report.get("run_id") != ready["run_id"]:
        raise RuntimeError("instrumentation report does not bind this invocation and run")
    files[label + ".json"] = save(label + ".json", proof)
    screenshot = adb("exec-out", "run-as", APP, "cat", "files/" + name + ".png")
    if not screenshot.startswith(b"\x89PNG\r\n\x1a\n") or len(screenshot) > 16 << 20:
        raise RuntimeError("invalid bounded screenshot")
    files[label + ".png"] = save(label + ".png", screenshot)
    leftovers = shell("run-as", APP, "ls", "files").decode().splitlines()
    if "instrumentation-profile.json" in leftovers:
        raise RuntimeError("runner left its private import profile behind")
    summary = {"schema": "wendy.android.annotation-stage.v2", "stage": args.stage,
        "run_id": ready["run_id"], "invocation_nonce": nonce, "serial": args.serial,
        "avd": args.avd, "node_image": ready["node_image"], "scene_image": ready["scene_image"],
        "elapsed_seconds": round(time.monotonic() - started, 3), "result": "passed",
        "profile_removed": True, "artifacts_sha256": files}
    save(label + "-summary.json", (json.dumps(summary, indent=2) + "\n").encode())
    print(json.dumps(summary), flush=True)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--adb", type=Path, required=True)
    parser.add_argument("--serial", required=True)
    parser.add_argument("--avd", required=True)
    parser.add_argument("--site", type=Path, required=True)
    parser.add_argument("--stage", choices=("prepare", "enqueue", "reopen", "sync"), required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--label")
    parser.add_argument("--text", default="Android verified offline annotation")
    parser.add_argument("--expected-other-text")
    parser.add_argument("--expect-clock-unknown", action="store_true")
    run(parser.parse_args())

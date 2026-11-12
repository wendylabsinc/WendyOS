#!/usr/bin/env python3
"""Prepare physical Mac acceptance for 1924/2011, without running native tools.

--inventory JSON --output NEW_DIRECTORY writes a private plan outside the repo.
Required inventory: candidate_source_commit, bundle (.app directory), cli_binary,
decoder_binary (ffmpeg), captured_at (ISO timestamp with timezone), owner,
mac {serial, hardware_uuid, hostname, os_version, architecture}, camera
{unique_id, name, physical_label, id}, endpoint (literal IPv4:port), and records
{native_inventory, camera_mapping, permissions, signing, build, listener,
auth_profiles}. Records are saved files, never commands. Profiles maps operator,
anonymous, foreign_operator and same_tenant_device to separately provisioned
absolute WENDY_CONFIG_DIR directories. Only their config.json hashes are retained.
actor_checks is true only when the saved build record proves enabled runtime
actor checks in this exact signed bundle. It does not enable checks itself.

--run PLAN --command NAME --output NEW_DIRECTORY explicitly executes one bounded
headless CLI observation on the inventoried Mac. Generation never does this.
"""

import argparse
from datetime import datetime
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import platform
import re
import resource
import shlex
import signal
import stat
import subprocess
import time

ROOT = Path(__file__).resolve().parents[3]
PROFILES = ("operator", "anonymous", "foreign_operator", "same_tenant_device")
RECORDS = ("native_inventory", "camera_mapping", "permissions", "signing", "build", "listener", "auth_profiles")
CASES = ("packaged-listener", "physical-decode", "anonymous-refused", "foreign-operator-refused",
         "device-principal-refused", "two-stream-limit", "cancel-sibling-reuse", "last-cancel-release",
         "repeated-start-stop-2011", "actor-runtime-checks-2011")


def line(value, name):
    if not isinstance(value, str) or not value.strip() or any(ord(c) < 32 for c in value):
        raise ValueError(f"{name} must be a nonempty single line")
    return value


def artifact(value):
    path = Path(value).resolve(strict=True)
    if not path.is_file() or path.stat().st_size == 0:
        raise ValueError(f"not a nonempty regular file: {path}")
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for block in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(block)
    return {"path": str(path), "sha256": digest.hexdigest()}


def bundle_inventory(value):
    root = Path(value).resolve(strict=True)
    if not root.is_dir() or root.suffix != ".app":
        raise ValueError("bundle must be an existing .app directory")
    entries = []
    # os.walk does not follow directory symlinks. Framework symlinks are allowed
    # only within the signed bundle; their link text is also part of the digest.
    for directory, dirs, files in os.walk(root, followlinks=False):
        for name in sorted(dirs + files):
            path = Path(directory) / name
            metadata = path.lstat()
            item = {"path": str(path.relative_to(root)), "mode": stat.S_IMODE(metadata.st_mode)}
            if path.is_symlink():
                target = path.resolve(strict=True)
                if target != root and root not in target.parents:
                    raise ValueError("bundle symlink escapes the candidate")
                item["symlink"] = os.readlink(path)
            elif path.is_file():
                item["sha256"] = artifact(path)["sha256"] if metadata.st_size else hashlib.sha256(b"").hexdigest()
            elif not path.is_dir():
                raise ValueError("bundle contains a nonregular entry")
            entries.append(item)
    if not (root / "Contents/Info.plist").is_file() or not (root / "Contents/_CodeSignature/CodeResources").is_file():
        raise ValueError("candidate must contain bundle metadata and a code signature resource seal")
    entries.sort(key=lambda item: item["path"])
    return {"path": str(root), "sha256": hashlib.sha256(json.dumps(entries, sort_keys=True).encode()).hexdigest(),
            "entries": entries}


def write(path, value):
    with path.open("x") as stream:
        os.chmod(path, 0o600)
        stream.write(value)


def new_output(value):
    path = Path(value).resolve()
    if path == ROOT or ROOT in path.parents:
        raise ValueError("output must be outside the checkout")
    path.mkdir(parents=True, mode=0o700, exist_ok=False)
    return path


def generate(inventory, output):
    inventory = Path(inventory).resolve(strict=True)
    raw = json.loads(inventory.read_text())
    if not re.fullmatch(r"[0-9a-f]{40}", raw["candidate_source_commit"]):
        raise ValueError("candidate_source_commit must be a full SHA")
    if datetime.fromisoformat(raw["captured_at"].replace("Z", "+00:00")).tzinfo is None:
        raise ValueError("captured_at requires a timezone")
    line(raw["owner"], "owner")
    for key in ("serial", "hardware_uuid", "hostname", "os_version", "architecture"):
        line(raw["mac"][key], key)
    if raw["mac"]["architecture"] not in ("arm64", "x86_64"):
        raise ValueError("select a native Mac architecture")
    for key in ("unique_id", "name", "physical_label"):
        line(raw["camera"][key], key)
    camera_id = raw["camera"]["id"]
    if type(camera_id) is not int or not 0 <= camera_id <= 2**32 - 1:
        raise ValueError("camera id must be the observed uint32 RPC id")
    host, port = raw["endpoint"].rsplit(":", 1)
    ipaddress.IPv4Address(host)
    if not port.isdecimal() or not 1 <= int(port) <= 65535:
        raise ValueError("endpoint requires a literal IPv4 address and TCP port")
    if type(raw["actor_checks"]) is not bool:
        raise ValueError("actor_checks must be a boolean backed by the build record")
    cli, decoder = artifact(raw["cli_binary"]), artifact(raw["decoder_binary"])
    if not all(os.access(item["path"], os.X_OK) for item in (cli, decoder)):
        raise ValueError("CLI and ffmpeg must be executable")
    bundle = bundle_inventory(raw["bundle"])
    records = {key: artifact(raw["records"][key]) for key in RECORDS}
    profiles = {}
    for name in PROFILES:
        if not Path(raw["profiles"][name]).is_absolute():
            raise ValueError("profile directories must be absolute")
        path = Path(raw["profiles"][name]).resolve(strict=True)
        config = artifact(path / "config.json")
        # File mode does not prevent legacy keychain references from resolving.
        if "keychain:" in Path(config["path"]).read_text():
            raise ValueError("profiles must have inline disposable credentials, without Keychain references")
        profiles[name] = {"directory": str(path), "config": config}
    if len({profile["directory"] for profile in profiles.values()}) != len(PROFILES):
        raise ValueError("each authentication role needs a distinct disposable profile")
    source = subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip()
    dirty = bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT, text=True).strip())
    source_files = [artifact(Path(__file__)), artifact(ROOT / "swift/WendyAgentCore/Sources/WendyAgent/Services/VideoService.swift"),
                    artifact(ROOT / "swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/CameraCapture.swift")]
    commands = {}
    for profile in PROFILES:
        commands[profile + "-list"] = {"profile": profile, "seconds": 10, "decode": False,
            "argv": [cli["path"], "--json", "device", "camera", "list", "--device", raw["endpoint"]]}
        for seconds in ([8, 20, 45] if profile == "operator" else [8]):
            commands[f"{profile}-capture-{seconds}"] = {"profile": profile, "seconds": seconds, "decode": True,
                "argv": [cli["path"], "device", "camera", "view", "--device", raw["endpoint"],
                         "--id", str(camera_id), "--stdout", "--non-interactive"]}
    evidence = {"audit_entries": ["1924", "2011"], "source_head": source, "dirty_preview": dirty,
        "inventory": raw, "inventory_artifact": artifact(inventory), "bundle": bundle,
        "cli": cli, "decoder": decoder, "records": records, "profiles": profiles, "source_files": source_files,
        "hardware_execution": "not_run", "cases": [{"id": case, "status": "not_run", "observations": [],
        "reviewer": None} for case in CASES], "cleanup": {"status": "not_run", "observations": []}}
    out = new_output(output)
    write(out / "commands.json", json.dumps(commands, indent=2) + "\n")
    evidence["commands_artifact"] = artifact(out / "commands.json")
    write(out / "evidence.json", json.dumps(evidence, indent=2) + "\n")
    run = shlex.join(["python3", str(Path(__file__).resolve()), "--run", str(out)])
    write(out / "procedure.txt", PROCEDURE.format(run=run, bundle=shlex.quote(bundle["path"]),
          endpoint=raw["endpoint"], port=int(port), hostname=raw["mac"]["hostname"], owner=raw["owner"],
          camera=raw["camera"]["physical_label"], candidate=raw["candidate_source_commit"], actor=raw["actor_checks"]))
    return evidence


def verify(plan):
    for item in [plan["inventory_artifact"], plan["commands_artifact"], plan["cli"], plan["decoder"],
                 *plan["records"].values(), *plan["source_files"],
                 *(profile["config"] for profile in plan["profiles"].values())]:
        if artifact(item["path"]) != item:
            raise ValueError(f"pinned artifact changed: {item['path']}")
    if bundle_inventory(plan["bundle"]["path"]) != plan["bundle"]:
        raise ValueError("signed bundle contents changed")


def bounded_process(argv, env, out, err, seconds, interrupt=False):
    """Only direct owned children; CLI stdout mode and ffmpeg spawn no players."""
    old_mask = signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM})
    def limits():
        signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
        resource.setrlimit(resource.RLIMIT_FSIZE, (64 * 1024 * 1024, 64 * 1024 * 1024))
        resource.setrlimit(resource.RLIMIT_CORE, (0, 0))
    start = time.monotonic()
    process = None
    record = {"start_utc": datetime.now().astimezone().isoformat(), "scheduled_cancel": False,
              "forced_termination": False, "timed_out": False}
    try:
        process = subprocess.Popen(argv, env=env, stdin=subprocess.DEVNULL, stdout=out, stderr=err,
                                   start_new_session=True, preexec_fn=limits)
        record["pid"] = process.pid
        # Enter the cleanup region before delivering a signal received at spawn.
        signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
        try:
            process.wait(timeout=seconds)
        except subprocess.TimeoutExpired:
            record["timed_out"] = True
            record["scheduled_cancel"] = interrupt
            record["cancel_utc"] = datetime.now().astimezone().isoformat()
            process.send_signal(signal.SIGINT if interrupt else signal.SIGTERM)
            try:
                process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                record["forced_termination"] = True
    finally:
        # Repeated signals must not interrupt the bounded owned-child drain.
        signal.pthread_sigmask(signal.SIG_BLOCK, {signal.SIGINT, signal.SIGTERM})
        if process is not None and process.poll() is None:
            record["forced_termination"] = True
            process.terminate()
            try:
                process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=2)
        record["returncode"] = process.returncode if process is not None else None
        record["elapsed_seconds"] = time.monotonic() - start
        record["end_utc"] = datetime.now().astimezone().isoformat()
        signal.pthread_sigmask(signal.SIG_SETMASK, old_mask)
    return record


def observe(directory, name, output):
    directory = Path(directory).resolve(strict=True)
    plan = json.loads((directory / "evidence.json").read_text())
    if plan["dirty_preview"]:
        raise ValueError("regenerate the plan from a clean committed checkout")
    verify(plan)
    if platform.system() != "Darwin" or platform.node() != plan["inventory"]["mac"]["hostname"]:
        raise ValueError("run only on the inventoried native Mac; also verify its serial/UUID manually")
    command = json.loads((directory / "commands.json").read_text())[name]
    out = new_output(output)
    # Dedicated acceptance account only. Do not inherit Cloud or credential env.
    env = {key: os.environ[key] for key in ("HOME", "TMPDIR", "USER", "LOGNAME") if key in os.environ}
    env.update(PATH="/usr/bin:/bin:/usr/sbin:/sbin", WENDY_SECRET_STORE="file",
               WENDY_CONFIG_DIR=plan["profiles"][command["profile"]]["directory"])
    old_term = signal.signal(signal.SIGTERM, lambda *_: (_ for _ in ()).throw(SystemExit(143)))
    try:
        with (out / "capture.bin").open("xb") as stdout, (out / "client.log").open("xb") as stderr:
            result = bounded_process(command["argv"], env, stdout, stderr, command["seconds"], interrupt=command["decode"])
        if command["decode"] and (out / "capture.bin").stat().st_size:
            argv = [plan["decoder"]["path"], "-nostdin", "-v", "error", "-protocol_whitelist", "file,pipe",
                    "-f", "h264", "-i", str(out / "capture.bin"), "-map", "0:v:0", "-f", "framemd5", "-"]
            with (out / "decoded.framemd5").open("xb") as stdout, (out / "decoder.log").open("xb") as stderr:
                result["decoder"] = bounded_process(argv, env, stdout, stderr, 15)
        result.update(command=name, status="needs_human_review", end_utc=datetime.now().astimezone().isoformat())
        write(out / "observation.json", json.dumps(result, indent=2) + "\n")
    finally:
        signal.signal(signal.SIGTERM, old_term)
        for path in out.iterdir():
            path.chmod(0o600)


PROCEDURE = '''Physical Mac camera acceptance, audits 1924 and 2011
Owner {owner}; host {hostname}; exact endpoint {endpoint}; camera {camera}.
Candidate source {candidate}; actor runtime checks claimed in build record: {actor}.
ALL CASES ARE NOT RUN. This generator does not inspect hardware or verify signatures.
Keep the plan, credentials and captured video private and outside the checkout.
Regenerate from a clean committed checkout before any physical observations.

1. Reserve and identify the candidate
Use a dedicated nonproduction Mac account and the owner's reserved camera. Point
it at a nonprivate moving test pattern. Stop if any other app owns the camera;
do not kill other apps, reset TCC, change permissions, enroll/unenroll a device,
or approve a permission dialog as part of this procedure. Required permissions
must already be granted to this exact signed packaged app by its owner. A denied
or unknown permission is blocked, not a camera regression pass.
Freshly save `system_profiler SPHardwareDataType SPCameraDataType`, `sw_vers`,
`uname -m`, and the owner's existing permission/ownership evidence. Compare Mac
serial, hardware UUID, OS, camera AVFoundation uniqueID/name/physical label and
the saved mapping to its current RPC numeric id. Numeric IDs may change across
launches. If any identity changes, regenerate the inventory and plan.
Run `codesign --verify --deep --strict --verbose=2 {bundle}` and
`codesign -d --verbose=4 {bundle}`. Retain status, TeamIdentifier, CDHash,
identifier, `codesign -d --entitlements :- {bundle}` output and the build record
linking this bundle to {candidate}.
The generator hashes all bundle entries including internal framework symlinks;
a resource seal's presence alone does not establish a valid signature.
Launch this exact packaged app by its normal owner-approved method. Do not use
swift run, an alternate daemon or a test handler as the physical acceptance.
Save timestamped `lsof -nP -iTCP:{port} -sTCP:LISTEN` and `ps -p PID -o pid=,lstart=,command=`
for its actual owned listener process. Resolve its executable and verify it is
inside the pinned bundle, including any signed bundled helper. PID is a manual
substitution from lsof, never a process-name search. Compare the enrollment,
device principal and tenant to saved listener/build records. Observe that same
PID/start time through every case; retain app logs and new crash reports.

2. Check auth profiles before requesting the camera
The four recorded profiles must be independently provisioned disposable test
identities. Preserve their CA/principal/tenant/certificate fingerprints and
expiry in the sanitized auth_profiles record, never private keys or tokens.
Operator is a valid same-tenant operator; foreign_operator chains to a foreign
CA; same_tenant_device is a device principal under the valid tenant; anonymous
has no client credential. Verify server identity using the CLI's normal pinned
trust policy. Never use an insecure override. WENDY_CONFIG_DIR alone does not
isolate legacy Keychain references; this generator refuses those references.
Use a dedicated test OS account to exclude ambient credentials and unrelated
TLS tickets. Do not log in, migrate, delete credentials or edit the profiles to
make a refusal pass. The anonymous profile needs preprovisioned public server
trust without client credentials. If the CLI cannot actually reach the intended
handshake/RPC with a role, record blocked and retain the client-side error.
Do not interpret a missing config, certificate load failure or network outage
as authorization enforcement. Correlate each request with the actual listener's
TLS/RPC evidence. Require foreign/no-certificate refusal at TLS and same-tenant
device permissionDenied at the service, before camera acquisition. If the
packaged logging cannot prove that boundary, leave the case pending.
Run each of these sequentially with a NEW private output directory:
  {run} --command anonymous-list --output /PRIVATE/anonymous-list
  {run} --command foreign_operator-list --output /PRIVATE/foreign-list
  {run} --command same_tenant_device-list --output /PRIVATE/device-list
Repeat each role's capture-8 command from commands.json. Require zero encoded
bytes, the correct refusal, no camera-active transition, and no stream-start
log. Confirm the valid operator still lists the intended camera afterward:
  {run} --command operator-list --output /PRIVATE/operator-list
This plan targets an enrolled listener. An unenrolled plaintext installation
requires a separately reserved Mac and its own inventory; never unenroll this
candidate to exercise it. Wire CI covers that listener mode independently.

3. Real camera and decode
  {run} --command operator-capture-8 --output /PRIVATE/baseline
The helper starts only the exact CLI, writes at most 64 MiB per output file,
and sends its child SIGINT at the selected duration. It allows 3 seconds, then
TERM/2 seconds and KILL/2 seconds. It decodes saved H.264 with the pinned ffmpeg
for at most 15 seconds. A signal to the helper also cleans up only its child.
It never marks a case passed. Review observation.json, client.log, decoder.log
and decoded.framemd5. Require scheduled_cancel=true, forced_termination=false,
valid CLI cancellation status, decoder returncode=0, at least 10 decoded frames,
positive dimensions and advancing frame timestamps. Move only the approved
pattern during capture and require multiple distinct decoded pixel hashes.
Repeat with a second distinguishable pattern and compare the decoded hashes.
The raw H.264 file omits RPC capture timestamps; ffmpeg's presentation timestamps
alone cannot prove sensor freshness. Choose the second pattern after capture
starts and retain the operator's timed observation linking it to this camera.
Truncated final H.264 data, decoder errors, no frames or unchanged stale frames
need investigation and a fresh run; a byte count alone is not acceptance.

4. Bound, cancel and reuse real capture
Use three terminals and distinct output directories. Start operator-capture-45
in terminal A. After actual frames and its stream-start log, start
operator-capture-20 in B. Confirm both are actively receiving decoded-pattern
data from the same selected physical camera before starting operator-capture-8
in C. C must promptly fail resourceExhausted with no video while A/B remain
healthy. If this camera cannot support two sessions, record that limitation;
do not count a hardware-busy error as the stream-limit check.
When B reaches its scheduled cancel, require its stream-stop log within 3
seconds while the agent stays alive. During A's remaining window run a new
operator-capture-8 in C; require decoded fresh pixels and no resourceExhausted.
A must continue delivering changing frames after B stops. Capture timestamps
in observation.json and frame hashes must establish overlap and sibling progress,
with the timed physical pattern observations, not merely
that all three files eventually exist. Repeat with the cancellation order
reversed. Resource usage must return to the measured idle baseline and each
accepted stream must have exactly one paired stop log.
After the last cancellation, observe the physical camera indicator/OS owner
state returning idle within 5 seconds without stopping the packaged agent.
Reopen and decode again. The stop log proves handler release, not by itself
AVFoundation teardown; retain independent owner/indicator evidence. A driver
without observable release remains a physical teardown gap. Do not interrupt
another app or unplug hardware to manufacture an idle-before-first-frame case;
that exact cancellation race remains covered by the real-wire CI fixture.

5. Startup ordering and actor checks, audit 2011
Repeat step 3 ten times, with a NEW output directory for each cycle, waiting
for physical release between cycles. Require fresh decoded frames on the first
attempt of every cycle, one stream start/stop pair, the same listener PID/start
time, and no new crash report, actor-isolation diagnostic or AVFoundation
startRunning/beginConfiguration/commitConfiguration exception. Do not retry a
failed cycle into a pass. A CLI reconnect/rejoin message is evidence to inspect,
not proof that the original capture survived. Repeat the two-stream overlap in
step 4 for three cycles and inspect all actor/capture logs.
For actor-runtime-checks-2011, the signed candidate's saved build record must
prove -enable-actor-data-race-checks and the intended Swift concurrency compiler
settings actually reached the camera target. Environment variables cannot add
missing instrumentation. If actor_checks is false or the build record is
incomplete, keep that case not_run. A release-build pass only covers the
observed AVFoundation startup ordering, not runtime actor instrumentation.

6. Cleanup and review
Every helper owns only its direct CLI/decoder child; it does not stop the agent.
Check every child has exited without forced termination, all accepted stream
start/stop pairs match, the camera returned idle, and the exact listener is
still healthy. Close only the packaged app instance started for this session
using its normal quit path, if the owner requested shutdown. Confirm its exact
listener closed; do not kill by name or port. Preserve existing permissions,
enrollment, user keychains and unrelated processes. Keep test credentials and
video private under the owner's retention policy; attach only redacted logs
and hashes for review. Do not commit media or credentials.
Record UTC start/end, operator, exact commands, observation files, listener PID,
camera identity, frame counts/hash changes and release evidence per case in
evidence.json. A reviewer must explicitly record pass/fail/blocked with reasons.
No generated case, synthetic fixture, wire test or saved historical acceptance
is proof that this exact signed bundle captured a physical camera successfully.
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--inventory", type=Path)
    mode.add_argument("--run", type=Path)
    parser.add_argument("--command")
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.inventory:
        generate(args.inventory, args.output)
    elif not args.command:
        parser.error("--run requires --command")
    else:
        observe(args.run, args.command, args.output)

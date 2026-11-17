#!/usr/bin/env python3
"""Companion acceptance for thermal 2315 and enrolled UVC 2222; generation is offline.

Use the mac-camera-rehearsal.py inventory schema, with its camera set to the
thermal target. Add thermal {format_record, provenance_record} and uvc {camera,
controls_record, camera_list_record, permission_record, store_record, control_name, permitted_value}.
Both cameras need their actual unique_id/name/physical_label/id. format_record
is JSON {camera_unique_id, dimensions}; provenance_record identifies who/how/when
captured that actual AVFoundation list. The historical source fixture is not a
fresh device capture. store_record is JSON {path, exists, sha256}; sha256 is null
when the exact packaged Agent account's controls.json did not exist.

--run-controls PLAN explicitly performs bounded nonpersistent UVC acceptance.
It requires --identity-confirmation JSON with captured_at, hostname, serial,
hardware_uuid, os_version, architecture, bundle_sha256, camera_unique_id, camera_id, listener_pid, listener_start,
observer and ownership_confirmed=true. A human freshly verifies the real USB
mapping: the current Mac camera list RPC does not expose its uniqueID.
Never invoke --run-controls as an offline test against real hardware.
"""
import argparse
from datetime import datetime, timezone
import importlib.util
import json
import os
from pathlib import Path
import re
import signal

BASE_PATH = Path(__file__).with_name("mac-camera-rehearsal.py")
SPEC = importlib.util.spec_from_file_location("mac_camera_base", BASE_PATH)
BASE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(BASE)
FIXTURE = Path(__file__).parents[1] / "fixtures/topdon-2315-formats.json"
SAFE_CONTROLS = {"brightness", "contrast", "saturation", "sharpness", "gamma"}
EXTRA_CASES = ("thermal-format-2315", "thermal-startup-actor-2315", "thermal-repeat-decode-2315",
               "uvc-auth-refusals-2222", "uvc-live-range-2222", "uvc-roundtrip-2222", "uvc-restore-2222")


def usb_identity(camera):
    uid = camera["unique_id"]
    if not isinstance(uid, str) or not re.fullmatch(r"0x[0-9a-fA-F]{1,16}", uid):
        raise ValueError("camera requires an exact AVFoundation USB uniqueID")
    bits = int(uid[2:], 16)
    if not bits >> 32 or not (bits >> 16) & 65535 or not bits & 65535:
        raise ValueError("USB uniqueID must encode location, vendor and product")
    for key in ("name", "physical_label"):
        BASE.line(camera[key], key)
    if type(camera["id"]) is not int or not 0 <= camera["id"] <= 2**32-1:
        raise ValueError("camera id must be an observed uint32")
    return {"location": f"{bits >> 32:08x}", "vendor_id": f"{bits >> 16 & 65535:04x}", "product_id": f"{bits & 65535:04x}"}


def control_record(data, name):
    if not isinstance(data, list):
        raise ValueError("controls record must be the real CLI JSON array")
    rows = [row for row in data if isinstance(row, dict) and row.get("name") == name]
    if len(rows) != 1:
        raise ValueError("selected control is missing or duplicated")
    row = rows[0]
    result = {key: row.get(key, 0) for key in ("value", "minimum", "maximum", "step", "default_value")}
    if any(type(v) is not int or not -(2**31) <= v < 2**31 for v in result.values()):
        raise ValueError("control metadata must contain int32 values")
    if row.get("mutable") is not True or result["step"] <= 0:
        raise ValueError("selected control must currently be mutable with a positive step")
    if not result["minimum"] <= result["value"] <= result["maximum"] or (result["value"] - result["minimum"]) % result["step"]:
        raise ValueError("original control value is outside its range/step")
    return dict(name=name, mutable=True, **result)


def camera_listing(data, camera):
    if not isinstance(data, list) or not data:
        raise ValueError("camera-list record must be a real CLI JSON array")
    ids = [row.get("id", 0) for row in data if isinstance(row, dict)]
    if len(ids) != len(data) or any(type(i) is not int for i in ids) or len(set(ids)) != len(ids):
        raise ValueError("camera listing has invalid or duplicate ids")
    targets = [row for row in data if row.get("name") == camera["name"]]
    if len(targets) != 1 or targets[0].get("id", 0) != camera["id"] or targets[0].get("transport") != 1 or targets[0].get("online") is not True:
        raise ValueError("camera listing must uniquely identify the reserved online USB camera")
    return data


def store_snapshot(path):
    path = Path(path)
    if path.is_symlink():
        raise ValueError("controls store must not be a symlink")
    if not path.exists():
        return {"path": str(path), "exists": False, "sha256": None}
    return {"path": str(path), "exists": True, "sha256": BASE.artifact(path)["sha256"]}


def generate(inventory, output):
    raw = json.loads(Path(inventory).read_text())
    thermal, uvc = raw["thermal"], raw["uvc"]
    thermal_usb, uvc_usb = usb_identity(raw["camera"]), usb_identity(uvc["camera"])
    reference = json.loads(FIXTURE.read_text())
    if any(thermal_usb[key] != reference[key] for key in ("vendor_id", "product_id")):
        raise ValueError("2315 requires the recorded TOPDON 2bdf:0102 target")
    formats = json.loads(Path(thermal["format_record"]).read_text())
    if formats.get("camera_unique_id") != raw["camera"]["unique_id"] or formats.get("dimensions") != reference["dimensions"]:
        raise ValueError("fresh exact-camera format record does not match the source-reported thermal fixture")
    if (raw["camera"]["id"] == uvc["camera"]["id"]) != (raw["camera"]["unique_id"] == uvc["camera"]["unique_id"]):
        raise ValueError("camera RPC ids and uniqueIDs describe an ambiguous mapping")
    build_record = Path(raw["records"]["build"]).read_text()
    if "-enable-actor-data-race-checks" not in build_record or raw["candidate_source_commit"] not in build_record:
        raise ValueError("build record must name candidate source and runtime actor-check flag")
    if raw["actor_checks"] is not True:
        raise ValueError("thermal startup requires runtime actor checks in the exact candidate build")
    if uvc["control_name"] not in SAFE_CONTROLS:
        raise ValueError("choose a permitted scalar image control, not exposure modes or mechanical controls")
    listing = camera_listing(json.loads(Path(uvc["camera_list_record"]).read_text()), uvc["camera"])
    control = control_record(json.loads(Path(uvc["controls_record"]).read_text()), uvc["control_name"])
    target = uvc["permitted_value"]
    if type(target) is not int or not control["minimum"] <= target <= control["maximum"] or abs(target-control["value"]) != control["step"]:
        raise ValueError("permitted value must be exactly one valid step from the original")
    saved = json.loads(Path(uvc["store_record"]).read_text())
    if not Path(saved["path"]).is_absolute() or not saved["path"].endswith("/Library/Application Support/sh.wendy.WendyAgentMac/camera-controls/controls.json"):
        raise ValueError("pin the exact packaged account camera-controls/controls.json path")
    if type(saved["exists"]) is not bool or (saved["exists"] and not re.fullmatch(r"[0-9a-f]{64}", saved.get("sha256", ""))) or (not saved["exists"] and saved.get("sha256") is not None):
        raise ValueError("store record requires existence and exact hash, or null when absent")
    records = {"thermal_formats": BASE.artifact(thermal["format_record"]),
               "thermal_provenance": BASE.artifact(thermal["provenance_record"]),
               "uvc_controls": BASE.artifact(uvc["controls_record"]),
               "uvc_camera_list": BASE.artifact(uvc["camera_list_record"]),
               "uvc_permission": BASE.artifact(uvc["permission_record"]),
               "uvc_store": BASE.artifact(uvc["store_record"])}
    plan = BASE.generate(inventory, output)
    plan["audit_entries"] += ["2315", "2222"]
    plan["records"].update(records)
    plan["source_files"] += [BASE.artifact(Path(__file__)), BASE.artifact(FIXTURE),
        BASE.artifact(BASE.ROOT / "go/internal/cli/commands/camera_controls.go"),
        BASE.artifact(BASE.ROOT / "swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/UVCControls.swift"),
        BASE.artifact(BASE.ROOT / "swift/WendyAgentCore/Sources/WendyAgent/Services/Platform/CameraControls.swift")]
    plan["hardware_extension"] = {"thermal_usb": thermal_usb, "uvc_usb": uvc_usb, "control": control,
                                  "permitted_value": target, "store": saved, "camera_listing": listing}
    plan["cases"] += [{"id": name, "status": "not_run", "observations": [], "reviewer": None} for name in EXTRA_CASES]
    out = Path(output).resolve()
    (out / "evidence.json").write_text(json.dumps(plan, indent=2)+"\n")
    run = BASE.shlex.join(["python3", str(Path(__file__).resolve()), "--run-controls", str(out)])
    BASE.write(out / "hardware-procedure.txt", PROCEDURE.format(run=run, original=control["value"],
        target=target, control=control["name"], camera=uvc["camera"]["physical_label"], uid=uvc["camera"]["unique_id"],
        camera_id=uvc["camera"]["id"], bundle_hash=plan["bundle"]["sha256"],
        endpoint=raw["endpoint"], cli=BASE.shlex.quote(plan["cli"]["path"]),
        profile=BASE.shlex.quote(plan["profiles"]["operator"]["directory"])))
    return plan


def confirm_identity(plan, confirmation):
    item = json.loads(Path(confirmation).read_text())
    camera = plan["inventory"]["uvc"]["camera"]
    when = datetime.fromisoformat(item["captured_at"].replace("Z", "+00:00"))
    if when.tzinfo is None or not -60 <= (datetime.now(timezone.utc)-when).total_seconds() <= 900:
        raise ValueError("identity confirmation must be fresh, within 15 minutes")
    for key, expected in {"hostname": plan["inventory"]["mac"]["hostname"], "bundle_sha256": plan["bundle"]["sha256"],
                         "camera_unique_id": camera["unique_id"], "camera_id": camera["id"], "ownership_confirmed": True,
                         **{key: plan["inventory"]["mac"][key] for key in ("serial", "hardware_uuid", "os_version", "architecture")}}.items():
        if type(item.get(key)) is not type(expected) or item[key] != expected:
            raise ValueError("fresh identity confirmation differs from the reserved camera/bundle")
    if type(item["listener_pid"]) is not int or item["listener_pid"] <= 0:
        raise ValueError("record the exact existing packaged listener PID")
    BASE.line(item["listener_start"], "listener_start")
    BASE.line(item["observer"], "observer")
    return BASE.artifact(confirmation)


def observe_controls(directory, confirmation, output):
    plan = json.loads((Path(directory)/"evidence.json").read_text())
    if plan["dirty_preview"]:
        raise ValueError("regenerate from a clean committed checkout")
    BASE.verify(plan)
    if BASE.platform.system() != "Darwin" or BASE.platform.node() != plan["inventory"]["mac"]["hostname"]:
        raise ValueError("run only on the inventoried native Mac")
    confirmation_artifact = confirm_identity(plan, confirmation)
    extension, camera = plan["hardware_extension"], plan["inventory"]["uvc"]["camera"]
    expected, original, target = extension["control"], extension["control"]["value"], extension["permitted_value"]
    if store_snapshot(extension["store"]["path"]) != extension["store"]:
        raise ValueError("persisted controls changed since inventory")
    out = BASE.new_output(output)
    observations = []
    result = {"status": "needs_human_review", "hardware_case_status": "not_run", "confirmation": confirmation_artifact,
              "restoration": "not_needed", "observations": observations}
    changed = False
    old_handlers = {sig: signal.signal(sig, lambda *_: (_ for _ in ()).throw(SystemExit(130))) for sig in (signal.SIGINT, signal.SIGTERM)}
    def invoke(label, role, value=None, listing=False):
        argv = [plan["cli"]["path"], "--json", "device", "camera"]
        argv += ["list"] if listing else ["controls" if value is None else "set-control", str(camera["id"])]
        if value is not None:
            argv += [expected["name"]+"="+str(value), "--no-persist"]
        argv += ["--device", plan["inventory"]["endpoint"]]
        env = {key: os.environ[key] for key in ("HOME", "TMPDIR", "USER", "LOGNAME") if key in os.environ}
        env.update(PATH="/usr/bin:/bin:/usr/sbin:/sbin", WENDY_SECRET_STORE="file", WENDY_CONFIG_DIR=plan["profiles"][role]["directory"])
        with (out/(label+".stdout")).open("xb") as stdout, (out/(label+".stderr")).open("xb") as stderr:
            observation = BASE.bounded_process(argv, env, stdout, stderr, 10)
        observation.update(label=label, role=role, argv=argv)
        observations.append(observation)
        return observation, (out/(label+".stdout")).read_text()
    def check_camera(label):
        observation, text = invoke(label+"-identity", "operator", listing=True)
        if observation["returncode"] != 0 or observation["timed_out"] or observation["forced_termination"]:
            raise ValueError("live camera listing unavailable before write/read")
        if camera_listing(json.loads(text), camera) != extension["camera_listing"]:
            raise ValueError("camera topology/listing changed; numeric id is no longer safe")
    def read(label, value):
        check_camera(label)
        observation, text = invoke(label, "operator")
        if observation["returncode"] != 0 or observation["timed_out"] or observation["forced_termination"]:
            raise ValueError("operator controls read failed: "+label)
        actual = control_record(json.loads(text), expected["name"])
        if actual != dict(expected, value=value):
            raise ValueError("live control range/value changed: "+label)
    def set_value(label, role, value):
        check_camera(label)
        observation, _ = invoke(label, role, value)
        return observation["returncode"] == 0 and not observation["timed_out"] and not observation["forced_termination"]
    try:
        read("original", original)
        for role in BASE.PROFILES[1:]:
            observation, _ = invoke(role+"-read", role)
            if observation["returncode"] == 0:
                raise ValueError("unauthorized role could read UVC controls: "+role)
            # This is an idempotent write of the already observed original value.
            # Any attempt can have reached hardware even when the CLI reports failure.
            changed = True
            if set_value(role+"-write-original", role, original):
                raise ValueError("unauthorized role could set UVC controls: "+role)
        read("before-change", original)
        changed = True
        if not set_value("permitted-change", "operator", target):
            raise ValueError("permitted UVC set did not complete")
        read("changed-readback", target)
        result["roundtrip_observed"] = True
    except BaseException as error:
        result["error"] = f"{type(error).__name__}: {error}"
        raise
    finally:
        # Give the owned bounded restore command its chance even after SIGTERM.
        # Repeated signals cannot interrupt restoration; never signal the Agent.
        for sig in old_handlers:
            signal.signal(sig, signal.SIG_IGN)
        try:
            if changed:
                result["restoration"] = "failed"
                try:
                    if not set_value("restore-original", "operator", original):
                        raise ValueError("original value restore failed")
                    read("restored-readback", original)
                    result["restoration"] = "observed"
                except BaseException as error:
                    result["restore_error"] = f"{type(error).__name__}: {error}"
            try:
                result["store_unchanged"] = store_snapshot(extension["store"]["path"]) == extension["store"]
            except BaseException as error:
                result["store_unchanged"] = False
                result["store_error"] = f"{type(error).__name__}: {error}"
            BASE.write(out/"observation.json", json.dumps(result, indent=2)+"\n")
        finally:
            for path in out.iterdir():
                path.chmod(0o600)
            for sig, handler in old_handlers.items():
                signal.signal(sig, handler)
        if result["restoration"] == "failed" or not result["store_unchanged"]:
            raise ValueError("UVC restoration or persistence preservation failed; owner review required")


PROCEDURE = '''Physical Mac companion, audits 2315 and 2222
ALL CASES ARE NOT RUN. Keep the existing procedure.txt and its 1924/2011 cases.
Generation reads supplied records only. It never opens cameras or invokes native
inventory, codesign, Keychain, permission changes, USB requests or capture.

1. Apply the original exact-bundle/host/listener/permissions/auth-profile checks.
The thermal camera is the primary camera in inventory. The additional USB camera
is {camera}, uniqueID {uid}, observed RPC id {camera_id}. Numeric IDs and names
alone are insufficient: freshly correlate native AVFoundation identity, USB
location/vendor/product, physical label and current RPC ordering. Mac RPC listing
does not expose uniqueID. Stop on hotplug, topology/mapping changes or a competing
camera owner. Do not reset permissions, seize/reset USB, enroll, or kill apps.

2. Thermal formats and instrumented startup, 2315
The checked-in TOPDON 2bdf:0102 fixture is transcribed from the original Swift test,
not a new raw hardware capture. Its provenance explicitly lacks a timestamp,
serial, pixel subtype and frame rates. Supply a separate fresh AVFoundation
format list for this exact USB uniqueID and retain the actual collection method,
observer/time and raw source evidence in thermal.provenance_record. Do not treat
system_profiler or CLI camera list as proof of AVFoundation's complete format
list. If no approved read-only format observation is available, keep this case
not_run. Require the recorded ordered dimensions, including vendor data channels,
and the expected picture at index 8, 512x384. A different firmware/format list
requires a reviewed new record, not relabeling the historical fixture.
The exact signed bundle must have runtime actor checks enabled in its saved build
record, including -enable-actor-data-race-checks on the camera target. The boolean
alone does not prove instrumentation. Confirm this before starting physical capture.
Run operator-capture-8 through the original bounded observer ten times, each to a
new directory, and the three original overlapping-stream cycles. Use an approved
nonhazardous temperature-contrast pattern that moves after capture begins. Require
at least 10 decoded frames per accepted capture, 512x384 decoded dimensions,
changing pixels, scheduled clean cancellation, physical release, and successful
immediate reopen. Retain actual decoder output, not only encoded bytes.
Keep the SAME packaged listener PID/start time across the cycles. Verify no
process abort/relaunch, new crash report, actor isolation failure, Metal texture/
PTSpillCorrection failure or AVFoundation configuration/startRunning exception.
Require independent native observation of the selected active format after
startRunning; if unavailable, retain that observation gap. Decoded dimensions
prove output shape, not every AVFoundation intermediate state. No retries into
pass, synthetic payloads, alternate capture tools or unsigned test executable.

3. Enrolled UVC round trip, 2222
Reserve the exact USB camera and its Agent account. Save actual `{cli} --json device
camera list --device {endpoint}` JSON in uvc.camera_list_record. Duplicate model
names or changed listings refuse writes. The helper checks the complete list
before each set and restoration; this still cannot distinguish an identical-model
replacement. Keep the physical USB topology fixed under the observer throughout;
if the exact identity cannot be maintained, leave the round trip not_run.
Save real `{cli} --json device
camera controls {camera_id} --device {endpoint}` output in uvc.controls_record.
Choose only a currently mutable scalar image control with a valid positive step,
no auto-mode dependency, and explicit owner permission for one step within range.
This plan selects {control}: original {original}, permitted temporary {target}.
No focus/zoom/motor, exposure-mode batch, vendor extension, reset or reset-all.
Record the actual account's camera-controls/controls.json existence/hash before
starting in uvc.store_record. The helper never writes that file. --no-persist
also does not remove earlier saved settings, so do not stream/reopen or run another
app during the round trip: first-frame restoration could legitimately change it.
Prepare a fresh identity-confirmation JSON within 15 minutes, including captured_at
with timezone, hostname, serial, hardware_uuid, os_version, architecture,
camera_unique_id, camera_id, bundle_sha256
{bundle_hash}, listener_pid, listener_start, observer, ownership_confirmed=true.
The human must compare serial/UUID and actual running signed executable too.
  {run} --identity-confirmation /PRIVATE/fresh-identity.json --output /PRIVATE/new-uvc-observation
The helper first re-reads original/range/step/mutability; changed state refuses the
write. It records unauthorized read and idempotent original-value write attempts
for anonymous, foreign operator and same-tenant device profiles. Every set uses
--no-persist. Correlate those failures with the actual server: TLS refusal for
foreign/no certificate, permissionDenied before USB opening for the device role.
An unavailable endpoint, expired certificate, client preflight refusal or timeout
is NOT auth acceptance. The helper leaves every case pending human review.
It then writes the permitted value, re-reads it, and in finally restores the
original value even after failed/ambiguous writes or interruption. It re-reads the
original and checks the persisted file is unchanged. Every CLI process is bounded
at 10 seconds with owned-child TERM/KILL cleanup; repeated signals are ignored
only during bounded restoration. It never signals the packaged Agent.
If restore fails, stop, retain failed cleanup, and ask the owner to restore ONLY
this recorded control on this same verified camera through the normal authorized
path using:
  env WENDY_CONFIG_DIR={profile} WENDY_SECRET_STORE=file {cli} device camera set-control {camera_id} {control}={original} --device {endpoint} --no-persist
First recheck the fresh native mapping; never reuse a shifted numeric ID. Do not reset
all controls, alter saved preferences, substitute another camera or hide failures.

4. Cleanup and review
Require successful CLI completions, fresh exact readbacks, unchanged store hash,
unchanged app PID/start time and no USB/capture process left active. Confirm the
owner observes original hardware state. Only then resume normal capture and verify
healthy decoded frames. Retain all role-boundary and no-abort evidence. observation
files are needs_human_review; hardware cases stay not_run until explicit reviewer
acceptance. Do not commit videos, private identities or fabricated capture records.
'''


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    group = parser.add_mutually_exclusive_group(required=True)
    group.add_argument("--inventory", type=Path)
    group.add_argument("--run-controls", type=Path)
    parser.add_argument("--identity-confirmation", type=Path)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    if args.inventory:
        generate(args.inventory, args.output)
    elif not args.identity_confirmation:
        parser.error("--run-controls requires --identity-confirmation")
    else:
        observe_controls(args.run_controls, args.identity_confirmation, args.output)

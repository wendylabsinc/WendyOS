"""Prepare a private native replay from a finished ESP32/Go2 simulation run.

Only presentation time changes. Robot poses, sensor timestamps, mission state,
and original evidence remain recorded values. No runtime command is issued.
"""
import argparse
import hashlib
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
SUFFIXES = (".png", ".wav", ".json", ".bin", ".jpg", ".jpeg")


def digest(raw):
    return hashlib.sha256(raw).hexdigest()


def bounded(path, limit):
    if path.is_symlink() or not path.is_file() or not 0 < path.stat().st_size <= limit:
        raise ValueError("Expected a bounded regular input: " + str(path))
    raw = path.read_bytes()
    if not 0 < len(raw) <= limit:
        raise ValueError("Input changed size while reading")
    return raw


def relative(root, name):
    if root.is_symlink() or not isinstance(name, str) or "\\" in name or Path(name).is_absolute():
        raise ValueError("Expected a relative evidence path")
    parts = name.split("/")
    if not parts or any(part in ("", ".", "..") for part in parts):
        raise ValueError("Invalid evidence path")
    path = root
    for part in parts:
        path = path / part
        if path.is_symlink():
            raise ValueError("Evidence path traverses a symlink")
    return path


def is_digest(value):
    return isinstance(value, str) and len(value) == 64 and all(c in "0123456789abcdef" for c in value)


def write_json(path, value):
    path.write_text(json.dumps(value, indent=2, allow_nan=False) + "\n")


def chapter(state):
    snapshot = state["snapshot"]
    patrol = snapshot.get("patrol") or {}
    phase = patrol.get("state")
    if phase == "completed":
        return "Observation mission complete", "The Go2 inspected the stand-off point and completed its return."
    if phase:
        return "Go2 responds to the camera", "Confirmed synthetic cue; recorded mission state: " + phase.replace("_", " ") + "."
    fusion = snapshot.get("fusion") or {}
    if fusion.get("state") == "hazard":
        return "Synthetic flame pixels detected", "The fixture-specific pixel rule examines the original firmware JPEG."
    return "A distant Go2 waits for the camera", "Recorded optical input passes through actual ESP32-S3 firmware and SensorLink."


def go2_preview(timeline, mapping, patrol, run_id):
    """Resolve the robot camera through the retained inspection manifest."""
    for visit in reversed((patrol.get("tour") or {}).get("visits") or []):
        manifest_sha = visit.get("manifest_sha256")
        if not is_digest(manifest_sha):
            raise ValueError("Inspection visit lacks a valid manifest digest")
        name = "evidence/" + manifest_sha + ".json"
        if name not in mapping:
            raise ValueError("Inspection manifest is missing from recorded assets")
        raw = bounded(relative(timeline, mapping[name]), 4 << 20)
        manifest = json.loads(raw)
        if digest(raw) != manifest_sha or manifest.get("run_id") != run_id:
            raise ValueError("Inspection manifest scope or digest differs")
        for sample in manifest.get("samples", []):
            if sample.get("sensor_id") != "camera-b" or sample.get("kind") != "camera":
                continue
            ref = sample.get("evidence") or {}
            if (sample.get("run_id") != run_id or sample.get("sample_ns") != visit.get("sample_simulation_ns")
                    or ref.get("content_type") != "image/png" or not is_digest(ref.get("sha256"))):
                raise ValueError("Robot camera sample does not match the recorded inspection")
            preview = "evidence/" + ref["sha256"] + ".png"
            if preview not in mapping:
                raise ValueError("Robot camera original is unavailable")
            image = bounded(relative(timeline, mapping[preview]), 4 << 20)
            if digest(image) != ref["sha256"] or len(image) != ref.get("size"):
                raise ValueError("Robot camera original differs from its manifest")
            return preview, {"sensor_id": "camera-b", "sha256": ref["sha256"], "manifest_sha256": manifest_sha,
                             "sample_simulation_ns": sample["sample_ns"], "captured_unix_ms": manifest.get("captured_unix_ms")}
    raise ValueError("No scoped Go2 camera-b PNG in the recorded inspection manifests")


def prepare(run, out, duration=48., final_hold=6., allow_incomplete_return=False, diagnostic=False):
    run, out = Path(run).resolve(), Path(out)
    if not out.is_absolute() or out.resolve().is_relative_to(ROOT) or out.exists():
        raise ValueError("Choose a fresh absolute private output directory outside the repository")
    if not (math.isfinite(duration) and 12 <= duration <= 60 and 3 <= final_hold <= 10 and final_hold < duration):
        raise ValueError("Use a 12..60 second video with a 3..10 second final hold")
    if diagnostic and allow_incomplete_return:
        raise ValueError("Choose either diagnostic or verified-inspection recording")
    inputs = {"run.json": bounded(run / "run.json", 16 << 20),
              "timeline.jsonl": bounded(run / "timeline.jsonl", 512 << 20),
              "viewer/geometry.json": bounded(run / "viewer/geometry.json", 64 << 20)}
    # Preserve firmware boot timestamps and first-receipt deadlines in their
    # original clock domains, alongside the displayed state timeline.
    for name in ("detections.jsonl", "response.json", "calibration.json", "operator-resume.json", "operator-stop.json"):
        if (run / name).exists():
            inputs[name] = bounded(run / name, 128 << 20)
    report = json.loads(inputs["run.json"])
    checks = report.get("checks") or {}
    terminal = (report.get("terminal") or {}).get("patrol") or {}
    required = {"no_walking_command_before_camera", "clean_frames_before_dispatch", "camera_view_remained_clear",
                "actual_standoff", "actual_travel", "articulated_gait", "original_frames_received",
                "confirmed_pixel_cue", "signed_mission_started", "camera_live_at_inspection"}
    partial = (allow_incomplete_return and report.get("passed") is False and checks.get("completed") is False
               and report.get("response_verified") is True and report.get("return_complete") is False
               and required.issubset(checks) and all(value is True for key, value in checks.items() if key != "completed")
               and terminal.get("state") == "stopped" and (terminal.get("tour") or {}).get("outcome") == "incomplete"
               and bool((terminal.get("tour") or {}).get("visits")) and "operator-stop.json" in inputs
               and report.get("error") is None)
    diagnostic_valid = (diagnostic and report.get("passed") is False and report.get("response_verified") is False
                        and report.get("return_complete") is False and report.get("error") is None
                        and all(checks.get(key) is True for key in required if key != "camera_live_at_inspection")
                        and {"detections.jsonl", "response.json", "calibration.json"}.issubset(inputs)
                        and is_digest(report.get("firmware_flash_sha256")))
    if diagnostic and not diagnostic_valid:
        raise ValueError("Diagnostic recording requires verified camera-triggered approach and retained firmware provenance")
    if (report.get("schema") != "wendy.xiao-go2-fire.run.v1"
            or report.get("simulation") is not True or report.get("hardware") is not False
            or not ((report.get("passed") is True and checks and all(value is True for value in checks.values())) or partial or diagnostic_valid)):
        raise ValueError("Recording requires a completed, passing simulation run")
    geometry = json.loads(inputs["viewer/geometry.json"])
    run_id = report["run_id"]
    if geometry.get("run_id") != run_id:
        raise ValueError("Geometry belongs to another run")
    resumes, stops = [], []
    receipts = sorted((run / "viewer/receipts").glob("*.json"))
    if len(receipts) > 4096:
        raise ValueError("Too many control receipts")
    for path in receipts:
        name = str(path.relative_to(run))
        raw = bounded(relative(run, name), 4096)
        receipt = json.loads(raw)
        if receipt.get("schema") != "wendy.macos-demo.command-result.v1" or receipt.get("run_id") != run_id:
            raise ValueError("Control receipt belongs to another run")
        inputs[name] = raw
        command = "viewer/commands/" + path.name
        if relative(run, command).exists():
            inputs[command] = bounded(relative(run, command), 4096)
        if receipt.get("accepted") is True and receipt.get("action") == "patrol_resume":
            resumes.append(receipt["sequence"])
        if receipt.get("accepted") is True and receipt.get("action") == "patrol_stop":
            stops.append(receipt["sequence"])
    rows = [json.loads(line) for line in inputs["timeline.jsonl"].splitlines() if line]
    if not 2 <= len(rows) <= 10000:
        raise ValueError("Expected 2..10000 recorded states")
    previous = (-1., -1, -1.)
    for row in rows:
        state = row["state"]
        current = (row["at_s"], state["sequence"], state["snapshot"]["time_s"])
        if (not all(type(v) in (int, float) and math.isfinite(v) for v in current)
                or current[0] < 0 or current[0] <= previous[0] or current[1] <= previous[1] or current[2] < previous[2]
                or state.get("mode") != "go2-fire-response" or state.get("run_id") != run_id
                or state["snapshot"].get("run_id") != run_id
                or not any(robot.get("id") == "go2" for robot in state["snapshot"].get("robots", []))):
            raise ValueError("Recorded scope, order or Go2 state differs")
        previous = current
    last = rows[-1]["state"]
    last_patrol = last["snapshot"].get("patrol") or {}
    resume_phases = {}
    for sequence in resumes:
        for index, row in enumerate(rows):
            if row["state"].get("last_command_sequence", 0) >= sequence:
                before = rows[max(0, index-1)]["state"]["snapshot"].get("patrol") or {}
                resume_phases[sequence] = before.get("state", "unknown")
                break
    if partial:
        if (last_patrol.get("state") != "stopped" or not (last_patrol.get("tour") or {}).get("visits")
                or not any(seq <= last.get("last_command_sequence", 0) for seq in stops)):
            raise ValueError("Partial recording requires an inspected tour and acknowledged operator stop in the final state")
    elif diagnostic:
        if not last_patrol.get("state", "").endswith("held"):
            raise ValueError("Diagnostic recording must retain the actual final hold state")
    elif last_patrol.get("state") != "completed":
        raise ValueError("Last recorded state does not show mission completion")

    # Native replay accepts at most 3600 states. Retain every chapter transition
    # and sample the remainder evenly; never invent intermediate robot poses.
    selected = set(range(len(rows)))
    if (partial or diagnostic) and len(rows) > 3598:
        raise ValueError("Partial replay must retain every source state within the native timeline bound")
    if len(rows) > 3598:
        required = {0, len(rows)-1}
        required.update(i for i in range(1, len(rows)) if chapter(rows[i]["state"]) != chapter(rows[i-1]["state"])
                        or rows[i]["state"].get("last_command_sequence") != rows[i-1]["state"].get("last_command_sequence"))
        if len(required) > 3598:
            raise ValueError("Too many state transitions for a bounded native replay")
        remaining = [i for i in range(len(rows)) if i not in required]
        count = 3598-len(required)
        selected = required | {remaining[min(len(remaining)-1, int(i*len(remaining)/count))] for i in range(count)}
    indices = sorted(selected)
    start, finish = rows[0]["at_s"], rows[-1]["at_s"]
    playback_span = duration-final_hold
    speed = (finish-start)/playback_span
    segments = [{"category": "all", "source_start_s": start, "source_end_s": finish,
                 "playback_start_s": 0., "playback_end_s": playback_span,
                 "source_seconds_per_playback_second": speed}]
    if partial or diagnostic:
        def category(state):
            patrol = state["snapshot"].get("patrol")
            if patrol is None:
                return "camera_wait"
            phase = patrol["state"]
            if phase.endswith("held"):
                return "held"
            return "return_attempt" if phase.startswith("return") else "approach_inspection"
        segments = []
        for row, following in zip(rows, rows[1:]):
            kind = category(row["state"])
            if not segments or segments[-1]["category"] != kind:
                segments.append({"category": kind, "source_start_s": row["at_s"], "source_end_s": following["at_s"]})
            else:
                segments[-1]["source_end_s"] = following["at_s"]
        weights = {"camera_wait": 4., "approach_inspection": 28., "return_attempt": 6., "held": 4.}
        totals = {kind: sum(s["source_end_s"]-s["source_start_s"] for s in segments if s["category"] == kind) for kind in weights}
        total_weight = sum(weight for kind, weight in weights.items() if totals[kind] > 0)
        cursor = 0.
        for segment in segments:
            kind = segment["category"]
            rate = totals[kind] / (playback_span * weights[kind] / total_weight)
            length = (segment["source_end_s"]-segment["source_start_s"]) / rate
            segment.update(playback_start_s=cursor, playback_end_s=cursor+length, source_seconds_per_playback_second=rate)
            cursor += length
    os.umask(0o077)
    out.mkdir(parents=True, mode=0o700)
    timeline = out / "timeline"; timeline.mkdir()
    (timeline / "assets").mkdir()
    provenance = out / "provenance"; provenance.mkdir()
    for name, raw in inputs.items():
        if name not in ("timeline.jsonl", "viewer/geometry.json"):
            (provenance / name).parent.mkdir(parents=True, exist_ok=True)
            (provenance / name).write_bytes(raw)
            (provenance / name).chmod(0o400)
    (timeline / "geometry.json").write_bytes(inputs["viewer/geometry.json"])
    assets, sources = {}, {}

    def pin(name, expected=None):
        source = relative(run / "viewer", name)
        raw = bounded(source, 4 << 20)
        sha = digest(raw)
        expected = expected or source.stem
        if not is_digest(expected) or sha != expected or source.suffix not in SUFFIXES:
            raise ValueError("Original evidence digest or format differs: " + name)
        asset = "assets/" + sha + source.suffix
        if asset not in assets:
            (timeline / asset).write_bytes(raw)
            (timeline / asset).chmod(0o400)
            assets[asset] = {"sha256": sha, "bytes": len(raw)}
        sources[name] = {"sha256": sha, "bytes": len(raw)}
        return asset

    def mapping(state):
        snapshot = state["snapshot"]
        result = {}
        observations = list((snapshot.get("fusion") or {}).get("observations") or [])
        observations += (snapshot.get("tracking") or {}).get("cameras") or []
        for observation in observations:
            name = observation.get("evidence_path")
            if name:
                result[name] = pin(name, observation.get("image_sha256"))
        patrol = snapshot.get("patrol") or {}
        refs = set()
        visits = (patrol.get("tour") or {}).get("visits") or []
        for item in (patrol.get("rooms") or []) + (patrol.get("recent_findings") or []) + visits:
            refs.update(item.get("evidence") or [])
        refs.update(item["manifest_sha256"] for item in visits if item.get("manifest_sha256"))
        for sha in sorted(refs):
            if not is_digest(sha):
                raise ValueError("Invalid patrol evidence digest")
            names = ["evidence/" + sha + suffix for suffix in SUFFIXES]
            found = [name for name in names if relative(run / "viewer", name).exists()]
            if not found:
                raise ValueError("Referenced patrol original is unavailable: " + sha)
            for name in found:
                result[name] = pin(name, sha)
        return result

    frames = []
    overview = False
    for index in indices:
        row = rows[index]
        segment = segments[-1] if row["at_s"] == finish else next(s for s in segments if s["source_start_s"] <= row["at_s"] < s["source_end_s"])
        at = (segment["playback_start_s"] + (row["at_s"]-segment["source_start_s"])/segment["source_seconds_per_playback_second"])
        if row["at_s"] == finish:
            at = playback_span
        scale_label = ("Recorded simulation · diagnostic · inspection incomplete" if diagnostic else "Recorded simulation") + f" · {segment['source_seconds_per_playback_second']:.6g}× host-time replay"
        title, detail = chapter(row["state"])
        resumed = any(sequence <= row["state"].get("last_command_sequence", 0) for sequence in resumes)
        phase = (row["state"]["snapshot"].get("patrol") or {}).get("state", "")
        if resumed:
            if phase.startswith("return"):
                return_resume = any(seq <= row["state"].get("last_command_sequence", 0) and source.startswith("return")
                                    for seq, source in resume_phases.items())
                title = ("Return held after operator retry" if phase.endswith("held") else "Return resumed by the operator") if return_resume else "Recorded return after an operator-assisted inspection"
                detail = "An acknowledged operator resume precedes this recorded state: " + phase.replace("_", " ") + "."
            elif phase == "completed":
                detail = "The mission completed after a recorded operator resume."
            elif phase != "stopped":
                title = "Inspection after operator resume"
                detail = "An acknowledged operator resume precedes this recorded state: " + phase.replace("_", " ") + "."
        if partial and phase == "stopped":
            title = "Inspection captured · return held"
            detail = ("The operator stopped the incomplete return. An earlier operator resume is retained." if resumes
                      else "The operator stopped the held return.") + " The full-tour check remains failed."
        if diagnostic and phase.endswith("held"):
            title = "Approach demonstrated · localization hold"
            detail = "The actual " + phase.replace("_", " ") + " is retained. No verified inspection capture."
            if resumed:
                detail += " Operator resume recorded."
        actions = []
        if not frames:
            actions = [{"kind": "select_robot", "target": "go2"},
                       {"kind": "camera", "target": "Follow robot"},
                       {"kind": "follow_input", "zoom_delta": 45},
                       {"kind": "layer", "target": "Localization", "enabled": True}]
        if not overview and at >= playback_span*.65:
            actions.append({"kind": "camera", "target": "Overview"})
            overview = True
        frames.append({"at_s": at, "state": row["state"], "assets": mapping(row["state"]), "actions": actions,
                       "source_at_s": row["at_s"], "source_row": index,
                       "chapter": {"series": "ESP32 CAMERA / GO2 RESPONSE" + (" / DIAGNOSTIC" if diagnostic else " / PARTIAL RUN" if partial else ""), "title": title,
                                   "detail": detail + " " + scale_label + "."}})
    # This is an explicitly labelled freeze of the final recorded state, not an
    # extension of the robot's runtime or of an observation's freshness.
    final = frames[-1]
    final["chapter"]["detail"] = f"Recorded simulation · final state held for {final_hold:g}s. Original sensor and mission timestamps retained."
    if diagnostic:
        final["chapter"]["title"] = "Approach demonstrated · localization hold"
        final["chapter"]["detail"] = f"Recorded simulation · diagnostic · inspection incomplete. Actual final hold shown for {final_hold:g}s; cleanup occurred after recording."
        if resumes:
            final["chapter"]["detail"] += " Operator resume recorded."
    elif partial:
        final["chapter"]["detail"] += " Inspection captured; return incomplete. " + ("Operator resume and stop recorded." if resumes else "Operator stop recorded.")
    elif resumes:
        final["chapter"]["detail"] += " Operator resume recorded."
    preview, preview_source = (None, None) if diagnostic else go2_preview(timeline, final["assets"], last_patrol, run_id)
    if preview:
        for at, action, title in ((playback_span+.25, {"kind": "open_patrol_evidence", "target": Path(preview).stem}, "Inspect the Go2's captured original"),
                                  (duration-1.5, {"kind": "close_patrol_evidence"}, "Inspection captured · return held" if partial else "Observation mission complete")):
            frames.append({**final, "at_s": at, "actions": [action], "presentation_hold": True,
                           "chapter": {**final["chapter"], "title": title}})
    elif not diagnostic:
        raise ValueError("Completed run lacks an original patrol PNG for the evidence panel")
    with (timeline / "frames.jsonl").open("x") as stream:
        for frame in frames:
            stream.write(json.dumps(frame, allow_nan=False, separators=(",", ":")) + "\n")
    # Recheck originals before certifying the adapter output.
    for name, original in inputs.items():
        if bounded(run / name, 512 << 20) != original:
            raise ValueError("Run input changed while preparing replay")
    for name, expected in sources.items():
        raw = bounded(relative(run / "viewer", name), 4 << 20)
        if digest(raw) != expected["sha256"]:
            raise ValueError("Evidence changed while preparing replay")
    from integration.nativeapp.render import timeline_inputs
    timeline_inputs(timeline)
    receipt = {"schema": "wendy.xiao-go2-fire.recording.v1", "run_id": run_id, "status": "prepared",
               "scope": "Offscreen native UI replay of recorded simulation; actual firmware with pre-rendered optical input",
               "hardware": False, "runtime_commands_issued": False, "sensor_interpolation": False,
               "recorded_operator_resume_sequences": resumes,
               "recorded_operator_resume_previous_phases": resume_phases,
               "recorded_operator_stop_sequences": stops, "source_run_passed": report["passed"],
               "source_response_verified": report.get("response_verified"),
               "robot_camera_preview": preview_source,
               "outcome": "diagnostic_approach_demonstrated_inspection_incomplete" if diagnostic else "inspection_captured_return_incomplete" if partial else "mission_completed",
               "duration_s": duration, "final_state_hold_s": final_hold,
               "source_host_interval_s": [start, finish], "playback_interval_s": [0., playback_span],
               "source_seconds_per_playback_second": None if (partial or diagnostic) else speed,
               "time_mapping": "Within each segment: playback_at_s = playback_start_s + (source_at_s - source_start_s) / source_seconds_per_playback_second",
               "time_segments": segments,
               "source_simulation_interval_s": [rows[0]["state"]["snapshot"]["time_s"], rows[-1]["state"]["snapshot"]["time_s"]],
               "source_rows": len(rows), "selected_source_rows": indices, "timeline_rows": len(frames),
               "inputs": {name: {"sha256": digest(raw), "bytes": len(raw)} for name, raw in inputs.items()},
               "assets": assets, "evidence_sources": sources,
               "timeline_sha256": digest((timeline / "frames.jsonl").read_bytes())}
    write_json(out / "capture.json", receipt)
    return timeline


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--run", required=True, type=Path)
    parser.add_argument("--out", required=True, type=Path)
    parser.add_argument("--duration", type=float, default=48.)
    parser.add_argument("--fps", type=int, default=12)
    parser.add_argument("--render", action="store_true", help="Also render the native UI and encode a private MP4")
    parser.add_argument("--app", type=Path, help="integrated Wendy.app or WendyDesktop executable")
    parser.add_argument("--allow-incomplete-return", action="store_true",
                        help="Explicitly record verified inspection with a failed full-tour check and acknowledged operator stop")
    parser.add_argument("--diagnostic", action="store_true",
                        help="Explicitly show verified camera-triggered approach with inspection incomplete and the actual final hold")
    args = parser.parse_args()
    if not 1 <= args.fps <= 24:
        parser.error("Use 1..24 fps")
    timeline = prepare(args.run, args.out, args.duration, allow_incomplete_return=args.allow_incomplete_return, diagnostic=args.diagnostic)
    if args.render:
        subprocess.run([sys.executable, "-m", "integration.nativeapp.render",
                        "--timeline", str(timeline), "--out", str(args.out / "render"),
                        "--duration", str(args.duration), "--fps", str(args.fps),
                        "--poster-at", str(args.duration*.7), *(["--app", str(args.app.resolve())] if args.app else [])], cwd=ROOT, check=True)
        shutil.copyfile(args.out / "render/wendy-world-distributed-fusion.mp4", args.out / "esp32-go2-fire-response.mp4")
    print(args.out)


if __name__ == "__main__":
    main()

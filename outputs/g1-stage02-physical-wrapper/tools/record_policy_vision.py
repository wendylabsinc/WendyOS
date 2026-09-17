#!/usr/bin/env python3
"""Record the exact physical policy RGB-D/mask frames beside policy progress."""

from __future__ import annotations

import argparse
import io
import json
import math
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

import numpy as np
from PIL import Image, ImageDraw


def get_json(url: str, timeout: float = 1.0) -> dict:
    request = urllib.request.Request(url, headers={"Cache-Control": "no-cache"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return json.load(response)


def get_bytes(url: str, timeout: float = 1.5) -> bytes:
    request = urllib.request.Request(url, headers={"Cache-Control": "no-cache"})
    with urllib.request.urlopen(request, timeout=timeout) as response:
        return response.read()


def compact_wrapper(state: dict) -> dict:
    runtime = state.get("policy_runtime") or {}
    io_state = state.get("io") or {}
    return {
        "observed_at_unix_ns": state.get("observed_at_unix_ns"),
        "healthy": state.get("healthy"),
        "running": runtime.get("running"),
        "command_id": runtime.get("command_id"),
        "policy_steps_completed": runtime.get("policy_steps_completed"),
        "policy_steps_total": runtime.get("policy_steps_total"),
        "policy_elapsed_s": runtime.get("policy_elapsed_s"),
        "effective_policy_hz": runtime.get("effective_policy_hz"),
        "policy_last_error": runtime.get("last_error"),
        "holding_after_fault": runtime.get("holding_after_fault"),
        "publishers_armed": io_state.get("publishers_armed"),
        "owner": io_state.get("owner"),
        "io_fault": io_state.get("fault"),
        "mode_machine": (state.get("live") or {}).get("mode_machine"),
    }


def make_preview(npz_path: Path, label: str, output_path: Path) -> None:
    with np.load(npz_path, allow_pickle=False) as data:
        bgr = data["color_bgr"]
        mask = data["mask"].astype(bool)
        depth = data["depth_m"]

    rgb = bgr[:, :, ::-1].copy()
    overlay = rgb.copy()
    overlay[mask] = np.array([255, 40, 40], dtype=np.uint8)
    rgb = ((0.55 * rgb + 0.45 * overlay) if mask.any() else rgb).astype(np.uint8)

    finite = np.isfinite(depth) & (depth > 0)
    if finite.any():
        lo, hi = np.percentile(depth[finite], [2, 98])
        span = max(float(hi - lo), 1e-6)
        normalized = np.clip((depth - lo) / span, 0, 1)
        depth_rgb = np.stack(
            [255 * normalized, 255 * (1 - np.abs(normalized - 0.5) * 2), 255 * (1 - normalized)],
            axis=-1,
        ).astype(np.uint8)
        depth_rgb[~finite] = 0
    else:
        depth_rgb = np.zeros_like(rgb)

    canvas = Image.new("RGB", (640, 270), "#101419")
    canvas.paste(Image.fromarray(rgb).resize((320, 240)), (0, 30))
    canvas.paste(Image.fromarray(depth_rgb).resize((320, 240)), (320, 30))
    draw = ImageDraw.Draw(canvas)
    draw.rectangle((0, 0, 640, 30), fill="#101419")
    draw.text((8, 8), label, fill="white")
    canvas.save(output_path, quality=92)


def build_contact_sheet(records: list[dict], frame_dir: Path, output_path: Path) -> list[dict]:
    candidates = [r for r in records if r.get("frame_saved")]
    if not candidates:
        return []

    target_steps = [0, 100, 250, 350, 400, 424, 440, 460, 480, 500, 550, 600]
    selected: list[dict] = []
    used: set[int] = set()
    for target in target_steps:
        eligible = [r for r in candidates if isinstance(r.get("policy_step"), int)]
        if not eligible:
            continue
        record = min(eligible, key=lambda r: abs(r["policy_step"] - target))
        frame_id = int(record["frame_id"])
        if frame_id not in used:
            selected.append(record)
            used.add(frame_id)

    if not selected:
        selected = candidates[:12]

    tiles: list[Image.Image] = []
    preview_dir = output_path.parent / "previews"
    preview_dir.mkdir(exist_ok=True)
    for record in selected:
        frame_id = int(record["frame_id"])
        label = (
            f"step {record.get('policy_step')}  frame {frame_id}  "
            f"mask={record.get('target_mask_valid')} px={record.get('mask_pixels')} "
            f"reason={record.get('selection_reason')}"
        )
        preview_path = preview_dir / f"frame-{frame_id:08d}.jpg"
        make_preview(frame_dir / record["frame_file"], label, preview_path)
        tile = Image.open(preview_path).resize((480, 203))
        tiles.append(tile.copy())
        tile.close()

    columns = 2
    rows = math.ceil(len(tiles) / columns)
    sheet = Image.new("RGB", (columns * 480, rows * 203), "#101419")
    for index, tile in enumerate(tiles):
        sheet.paste(tile, ((index % columns) * 480, (index // columns) * 203))
    sheet.save(output_path, quality=94)
    return selected


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--output-dir", type=Path, required=True)
    parser.add_argument("--wrapper-url", default="http://10.10.20.238:8121")
    parser.add_argument("--segmentation-url", default="http://10.10.20.238:8003")
    parser.add_argument("--command-id", required=True)
    parser.add_argument("--target-start", type=int, default=424)
    parser.add_argument("--target-end", type=int, default=480)
    parser.add_argument("--wait-for-run-s", type=float, default=180.0)
    parser.add_argument("--post-run-s", type=float, default=2.0)
    args = parser.parse_args()

    output_dir = args.output_dir.resolve()
    frame_dir = output_dir / "frames"
    frame_dir.mkdir(parents=True, exist_ok=False)
    observations_path = output_dir / "observations.jsonl"
    records: list[dict] = []
    seen_frames: set[tuple[str, int]] = set()
    started_at = time.monotonic()
    saw_run = False
    inactive_since: float | None = None
    latest_wrapper: dict = {}
    errors: list[str] = []

    print(f"RECORDER_READY output={output_dir} command_id={args.command_id}", flush=True)
    with observations_path.open("w", encoding="utf-8") as log:
        while True:
            loop_now = time.monotonic()
            try:
                latest_wrapper = compact_wrapper(get_json(f"{args.wrapper_url}/state"))
            except Exception as exc:  # keep camera capture alive through transient status failures
                errors.append(f"wrapper: {type(exc).__name__}: {exc}")

            if (
                latest_wrapper.get("running")
                and latest_wrapper.get("command_id") == args.command_id
            ):
                saw_run = True

            try:
                seg = get_json(f"{args.segmentation_url}/state")
                latest = seg.get("latest") or {}
                frame_id = latest.get("frame_id")
                stream_id = latest.get("stream_id")
                key = (str(stream_id), int(frame_id)) if stream_id and frame_id is not None else None
                if key and key not in seen_frames:
                    params = urllib.parse.urlencode({"frame_id": frame_id, "stream_id": stream_id})
                    payload = get_bytes(f"{args.segmentation_url}/frame.policy-rgbd?{params}")
                    # Validate the payload before committing it to the evidence bundle.
                    with np.load(io.BytesIO(payload), allow_pickle=False) as frame:
                        if not {"color_bgr", "depth_m", "mask", "metadata_json"}.issubset(frame.files):
                            raise ValueError(f"unexpected frame payload keys: {frame.files}")
                    frame_file = f"frame-{int(frame_id):08d}.npz"
                    (frame_dir / frame_file).write_bytes(payload)
                    selection = latest.get("selection") or {}
                    mask = latest.get("mask") or {}
                    record = {
                        "host_unix_ns": time.time_ns(),
                        "frame_id": int(frame_id),
                        "stream_id": stream_id,
                        "captured_at_unix_ns": latest.get("captured_at_unix_ns"),
                        "processed_at_unix_ns": latest.get("processed_at_unix_ns"),
                        "target_mask_valid": seg.get("target_mask_valid"),
                        "selection_valid": selection.get("valid"),
                        "selection_reason": selection.get("reason"),
                        "selection_confidence": selection.get("confidence"),
                        "mask_pixels": mask.get("pixels"),
                        "depth_valid_fraction": latest.get("depth_valid_fraction"),
                        "processing_ms": latest.get("processing_ms"),
                        "policy_step": latest_wrapper.get("policy_steps_completed"),
                        "wrapper": latest_wrapper,
                        "frame_saved": True,
                        "frame_file": frame_file,
                    }
                    log.write(json.dumps(record, separators=(",", ":")) + "\n")
                    log.flush()
                    records.append(record)
                    seen_frames.add(key)
            except urllib.error.HTTPError as exc:
                # The camera ring is intentionally small; a 409 means this frame was overwritten.
                errors.append(f"camera HTTP {exc.code}")
            except Exception as exc:
                errors.append(f"camera: {type(exc).__name__}: {exc}")

            if saw_run:
                active = bool(latest_wrapper.get("running")) or bool(
                    latest_wrapper.get("holding_after_fault")
                )
                if active:
                    inactive_since = None
                elif inactive_since is None:
                    inactive_since = loop_now
                elif loop_now - inactive_since >= args.post_run_s:
                    break
            elif loop_now - started_at > args.wait_for_run_s:
                errors.append("timed out waiting for matching policy run")
                break

            time.sleep(0.01)

    target_records = [
        r
        for r in records
        if isinstance(r.get("policy_step"), int)
        and args.target_start <= r["policy_step"] <= args.target_end
    ]
    valid_records = [r for r in records if r.get("target_mask_valid")]
    valid_target_records = [r for r in target_records if r.get("target_mask_valid")]
    first_valid = min(valid_records, key=lambda r: r["host_unix_ns"], default=None)
    contact_sheet_path = output_dir / "real-policy-vision-contact-sheet.jpg"
    selected = build_contact_sheet(records, frame_dir, contact_sheet_path)
    summary = {
        "schema": "wendy.g1.physical-policy-vision-recording.v1",
        "command_id": args.command_id,
        "saw_matching_run": saw_run,
        "frames_recorded": len(records),
        "first_frame_id": records[0]["frame_id"] if records else None,
        "last_frame_id": records[-1]["frame_id"] if records else None,
        "target_policy_step_window": [args.target_start, args.target_end],
        "frames_in_target_window": len(target_records),
        "valid_mask_frames_total": len(valid_records),
        "valid_mask_frames_in_target_window": len(valid_target_records),
        "first_valid_mask": (
            {
                "frame_id": first_valid["frame_id"],
                "policy_step": first_valid["policy_step"],
                "mask_pixels": first_valid["mask_pixels"],
                "confidence": first_valid["selection_confidence"],
            }
            if first_valid
            else None
        ),
        "final_wrapper": latest_wrapper,
        "selected_contact_sheet_frames": [
            {"frame_id": r["frame_id"], "policy_step": r.get("policy_step")} for r in selected
        ],
        "capture_errors_tail": errors[-20:],
        "artifacts": {
            "observations_jsonl": str(observations_path),
            "frames_directory": str(frame_dir),
            "contact_sheet": str(contact_sheet_path) if contact_sheet_path.exists() else None,
        },
    }
    (output_dir / "summary.json").write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")
    print(json.dumps(summary, indent=2), flush=True)
    return 0 if saw_run and records and target_records else 2


if __name__ == "__main__":
    raise SystemExit(main())

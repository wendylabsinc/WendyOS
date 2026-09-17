"""DDS-free exact policy service for the G1 physical control process.

Unitree's high-rate Python DDS callbacks run in the physical process. Keeping
Torch inference here gives it a separate GIL while the physical process remains
the sole owner and publisher of motor commands.
"""
from __future__ import annotations

import json
import os
import re
import socket
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any
from urllib.parse import parse_qs, urlsplit

import numpy as np

from .exact_policy import CachedReferenceResidualPolicy
from .observation_adapter import ExactEpisodeObservationAdapter, OrderedJointState
from .camera_pump import SegmentationCameraPump


BUNDLE = Path(os.environ.get("G1_POLICY_BUNDLE", "/bundle"))
PORT = int(os.environ.get("G1_INFERENCE_PORT", "8097"))
MAXIMUM_CAMERA_AGE_S = float(os.environ.get("G1_MAXIMUM_CAMERA_AGE_S", "1.0"))
RUN_LOG_DIR = Path(os.environ.get("G1_RUN_LOG_DIR", "/run-data"))
SESSION_ID_PATTERN = re.compile(r"[A-Za-z0-9][A-Za-z0-9._-]{15,63}")


class InferenceRuntime:
    motion_capability = False

    def __init__(self) -> None:
        policy = CachedReferenceResidualPolicy(
            BUNDLE,
            device=os.environ.get("G1_POLICY_DEVICE", "cuda"),
            control_device=os.environ.get("G1_POLICY_CONTROL_DEVICE", "cpu"),
            maximum_camera_age_s=MAXIMUM_CAMERA_AGE_S,
        )
        self.episode = ExactEpisodeObservationAdapter(policy, BUNDLE)
        self.camera = SegmentationCameraPump(self.episode)
        self.camera.start()
        self.lock = threading.RLock()
        self.session_id: str | None = None
        self.active = False
        self.proposals = 0
        self.last_error: str | None = None
        RUN_LOG_DIR.mkdir(parents=True, exist_ok=True)
        self.trace_path: Path | None = None
        self.trace_file: Any | None = None
        self.trace_samples = 0

    def preflight(self) -> dict[str, Any]:
        return {
            "accepted": True,
            "active_policy": self.episode.policy.identity.public(),
            "camera": self.camera.preflight(),
        }

    def contract(self) -> dict[str, Any]:
        reference = self.episode.reference.references
        return {
            "schema": "wendy.g1.reference-residual-active-contract.v1",
            "active_policy": self.episode.policy.identity.public(),
            "joint_names": list(self.episode.joint_names),
            "reference_frame0_q_43": reference[0].tolist(),
            "reference_frames": len(reference),
            "object_retarget": {
                "schema": "g1.object-relative-reference-retarget.v1",
                "gain": 1.0,
                "input": "detected minus nominal can in the same current camera frame",
                "physical_camera_calibration_qualified": False,
            },
        }

    def retarget_status(self) -> dict[str, Any]:
        return self.episode.retargeter.status()

    def retarget_project(self, yaw: float, roll: float, pitch: float) -> dict[str, Any]:
        return self.episode.retargeter.project(
            yaw=yaw,
            roll=roll,
            pitch=pitch,
            observed_at_ns=time.time_ns(),
        )

    def reset(self, session_id: str, started_at_ns: int) -> dict[str, Any]:
        with self.lock:
            if SESSION_ID_PATTERN.fullmatch(session_id) is None:
                raise ValueError("invalid inference session id")
            if self.active and self.session_id != session_id:
                raise RuntimeError("another inference session is active")
            self._close_trace(accepted=None)
            result = self.episode.reset_episode(started_at_ns=started_at_ns)
            self.session_id = session_id
            self.active = False
            self.proposals = 0
            self.trace_samples = 0
            self.last_error = None
            self.trace_path = RUN_LOG_DIR / f"{session_id}.can-alignment.jsonl"
            self.trace_file = self.trace_path.open("a", encoding="utf-8")
            self._write_trace(
                {
                    "schema": "wendy.g1.can-alignment-run-start.v1",
                    "session_id": session_id,
                    "started_at_unix_ns": started_at_ns,
                    "checkpoint_sha256": self.episode.policy.identity.checkpoint_sha256,
                    "distance_definition": (
                        "planar Euclidean distance between detected and nominal can "
                        "after expressing both in the same live camera frame and rotating "
                        "their relative vector into policy/world XY"
                    ),
                    "units": {"distance": "m", "display": "cm"},
                }
            )
            return result

    def activate(self, session_id: str) -> dict[str, Any]:
        with self.lock:
            self._require_session(session_id)
            self.camera.activate()
            self.active = True
            return {"active": True, "session_id": session_id}

    def deactivate(self, session_id: str) -> dict[str, Any]:
        with self.lock:
            if self.session_id not in {None, session_id}:
                raise RuntimeError("inference session identity changed")
            self.camera.deactivate()
            self.active = False
            self._close_trace(accepted=True)
            self.session_id = None
            return {"active": False, "session_id": session_id}

    def propose(self, body: dict[str, Any]) -> dict[str, Any]:
        with self.lock:
            session_id = str(body["session_id"])
            self._require_session(session_id)
            if not self.active:
                raise RuntimeError("inference session is not active")
            try:
                proposal = self.episode.propose(
                    OrderedJointState(
                        self.episode.joint_names,
                        body["q_43"],
                        body["dq_43"],
                        int(body["sampled_at_ns"]),
                    ),
                    control_at_ns=int(body["control_at_ns"]),
                )
                self.proposals += 1
                retarget = proposal.get("object_retarget") or {}
                self._write_trace(
                    {
                        "schema": "wendy.g1.can-alignment-sample.v1",
                        "session_id": session_id,
                        "proposal_index": self.proposals - 1,
                        "control_at_unix_ns": int(body["control_at_ns"]),
                        "reference_frame": retarget.get("reference_frame"),
                        "reason": retarget.get("reason"),
                        "measurement_frame_id": retarget.get("measurement_frame_id"),
                        "measurement_stream_id": retarget.get("measurement_stream_id"),
                        "measurement_captured_at_unix_ns": retarget.get(
                            "measurement_captured_at_unix_ns"
                        ),
                        "detection_age_ms": retarget.get("detection_age_ms"),
                        "distance_to_optimal_m": retarget.get("distance_to_optimal_m"),
                        "distance_to_optimal_cm": retarget.get("distance_to_optimal_cm"),
                        "filtered_distance_to_optimal_m": retarget.get(
                            "filtered_distance_to_optimal_m"
                        ),
                        "raw_offset_world_xy_m": retarget.get("raw_offset_world_xy_m"),
                        "clipped_offset_world_xy_m": retarget.get(
                            "clipped_offset_world_xy_m"
                        ),
                        "filtered_offset_world_xy_m": retarget.get("offset_world_xy_m"),
                        "pixel_displacement_px": retarget.get("pixel_displacement_px"),
                        "ideal_pixel_xy": retarget.get("ideal_pixel_xy"),
                        "detected_pixel_xy": retarget.get("detected_pixel_xy"),
                        "latched": retarget.get("latched"),
                    }
                )
                self.trace_samples += 1
                self.last_error = None
                # This 131-float diagnostic is reconstructable from the live
                # state and needlessly taxes the 40 Hz IPC response.
                proposal.pop("joint_features_131", None)
                return proposal
            except Exception as exc:
                self.last_error = f"{type(exc).__name__}: {exc}"
                raise

    def _write_trace(self, value: dict[str, Any]) -> None:
        if self.trace_file is None:
            return
        self.trace_file.write(
            json.dumps(value, separators=(",", ":"), allow_nan=False) + "\n"
        )
        self.trace_file.flush()

    def _close_trace(self, accepted: bool | None) -> None:
        if self.trace_file is None:
            return
        self._write_trace(
            {
                "schema": "wendy.g1.can-alignment-run-finish.v1",
                "session_id": self.session_id,
                "finished_at_unix_ns": time.time_ns(),
                "accepted": accepted,
                "proposal_count": self.trace_samples,
                "last_error": self.last_error,
            }
        )
        self.trace_file.close()
        self.trace_file = None

    def retarget_runs(self) -> dict[str, Any]:
        runs = []
        for path in RUN_LOG_DIR.glob("*.can-alignment.jsonl"):
            session_id = path.name.removesuffix(".can-alignment.jsonl")
            if SESSION_ID_PATTERN.fullmatch(session_id) is None:
                continue
            stat = path.stat()
            runs.append(
                {
                    "session_id": session_id,
                    "bytes": stat.st_size,
                    "updated_at_unix_ns": stat.st_mtime_ns,
                    "download_path": "/retarget/run?session_id=" + session_id,
                }
            )
        runs.sort(key=lambda value: value["updated_at_unix_ns"], reverse=True)
        return {"schema": "wendy.g1.can-alignment-run-index.v1", "runs": runs}

    def retarget_run(self, session_id: str) -> str:
        if SESSION_ID_PATTERN.fullmatch(session_id) is None:
            raise ValueError("invalid inference session id")
        path = RUN_LOG_DIR / f"{session_id}.can-alignment.jsonl"
        if not path.is_file():
            raise FileNotFoundError("can-alignment trace not found")
        if path.stat().st_size > 16 * 1024 * 1024:
            raise ValueError("can-alignment trace exceeds download limit")
        return path.read_text(encoding="utf-8")

    def _require_session(self, session_id: str) -> None:
        if len(session_id) < 16 or self.session_id != session_id:
            raise RuntimeError("inference session is unavailable")

    def status(self) -> dict[str, Any]:
        with self.lock:
            return {
                "schema": "wendy.g1.reference-residual-inference.v1",
                "healthy": True,
                "motion_capability": False,
                "physical_commands_sent": 0,
                "checkpoint_sha256": self.episode.policy.identity.checkpoint_sha256,
                "active_policy": self.episode.policy.identity.public(),
                "vision_device": str(self.episode.policy.vision_device),
                "control_device": str(self.episode.policy.device),
                "active": self.active,
                "session_id": self.session_id,
                "proposals": self.proposals,
                "last_error": self.last_error,
                "can_alignment_trace": {
                    "path": None if self.trace_path is None else str(self.trace_path),
                    "sample_count": self.trace_samples,
                },
                "camera": self.camera.status(),
                "object_retarget": self.episode.retargeter.status(),
            }

    def close(self) -> None:
        with self.lock:
            self._close_trace(accepted=None)
        self.camera.close()
        self.episode.policy.close()


def make_server(runtime: InferenceRuntime) -> ThreadingHTTPServer:
    class Handler(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def setup(self) -> None:
            super().setup()
            self.request.setsockopt(socket.IPPROTO_TCP, socket.TCP_NODELAY, 1)

        def log_message(self, *_args: Any) -> None:
            return

        def do_GET(self) -> None:
            parsed = urlsplit(self.path)
            path = parsed.path
            if path not in {
                "/health",
                "/contract",
                "/retarget/status",
                "/retarget/project",
                "/retarget/runs",
                "/retarget/run",
            }:
                return self.reply(404, {"error": "not found"})
            if path == "/health":
                value = runtime.status()
            elif path == "/contract":
                value = runtime.contract()
            elif path == "/retarget/status":
                value = runtime.retarget_status()
            elif path == "/retarget/runs":
                value = runtime.retarget_runs()
            elif path == "/retarget/run":
                try:
                    args = parse_qs(parsed.query, strict_parsing=True)
                    if set(args) != {"session_id"} or len(args["session_id"]) != 1:
                        raise ValueError("session_id is required exactly once")
                    session_id = args["session_id"][0]
                    return self.reply_jsonl(200, runtime.retarget_run(session_id), session_id)
                except FileNotFoundError as exc:
                    return self.reply(404, {"error": str(exc)})
                except ValueError as exc:
                    return self.reply(400, {"error": str(exc)})
            else:
                try:
                    args = parse_qs(parsed.query, strict_parsing=True)
                    if set(args) != {"yaw", "roll", "pitch"} or any(len(v) != 1 for v in args.values()):
                        raise ValueError("yaw, roll, and pitch are required exactly once")
                    values = [float(args[name][0]) for name in ("yaw", "roll", "pitch")]
                    if not all(np.isfinite(values)):
                        raise ValueError("waist angles must be finite")
                    value = runtime.retarget_project(*values)
                except (TypeError, ValueError) as exc:
                    return self.reply(400, {"error": f"{type(exc).__name__}: {exc}"})
            self.reply(200, value)

        def do_POST(self) -> None:
            path = self.path.split("?", 1)[0]
            if path not in {"/preflight", "/reset", "/activate", "/deactivate", "/propose"}:
                return self.reply(404, {"error": "not found"})
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length <= 0 or length > 65536:
                    raise ValueError("invalid request length")
                body = json.loads(self.rfile.read(length))
                if path == "/preflight":
                    value = runtime.preflight()
                elif path == "/reset":
                    value = runtime.reset(str(body["session_id"]), int(body["started_at_ns"]))
                elif path == "/activate":
                    value = runtime.activate(str(body["session_id"]))
                elif path == "/deactivate":
                    value = runtime.deactivate(str(body["session_id"]))
                else:
                    value = runtime.propose(body)
                self.reply(200, value)
            except Exception as exc:
                self.reply(409, {"error": f"{type(exc).__name__}: {exc}"})

        def reply(self, status: int, value: dict[str, Any]) -> None:
            body = json.dumps(value, separators=(",", ":"), allow_nan=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def reply_jsonl(self, status: int, value: str, session_id: str) -> None:
            body = value.encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/x-ndjson")
            self.send_header("Access-Control-Allow-Origin", "*")
            self.send_header(
                "Content-Disposition",
                f'attachment; filename="{session_id}.can-alignment.jsonl"',
            )
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.end_headers()
            self.wfile.write(body)

    return ThreadingHTTPServer(("0.0.0.0", PORT), Handler)


def main() -> None:
    runtime = InferenceRuntime()
    server = make_server(runtime)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        runtime.close()


if __name__ == "__main__":
    main()

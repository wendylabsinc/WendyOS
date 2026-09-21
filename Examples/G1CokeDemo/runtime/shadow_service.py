"""NX2 motion-zero async policy benchmark and observability service."""
from __future__ import annotations

import io
import json
import os
import secrets
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.request import Request, urlopen

import numpy as np
import torch

from .async_vision import VisionUnavailable
from .exact_policy import CachedReferenceResidualPolicy, EXPECTED_CHECKPOINT_SHA256


BUNDLE = Path(os.environ.get("G1_POLICY_BUNDLE", "/bundle"))
CAMERA_URL = os.environ.get("G1_CAMERA_URL", "http://127.0.0.1:8001")
SEGMENTATION_URL = os.environ.get("G1_SEGMENTATION_URL", "http://127.0.0.1:8003")
PORT = int(os.environ.get("G1_SHADOW_PORT", "8098"))
CONTROL_HZ = 40
CAMERA_HZ = 20
MAXIMUM_CAMERA_AGE_S = float(os.environ.get("G1_MAXIMUM_CAMERA_AGE_S", ".125"))
EXPECTED_MASK_SHA256 = "586135a02f5e6154169e1164636bb3c5b68aa7dea7228949bc453023ec999f49"
EXPECTED_RUNTIME_MANIFEST_SHA256 = "a23b0a44ae8c44d4c3f49d3fdd1c3f4e3d907e8bdbc69d5a73ef92c520e931f8"
EXPECTED_REFERENCE_SHA256 = "98f12d860335bbaf6e770d626c3fd0efc6bfeff7cb9aad6cae36e296020b41b4"
STATUS_SCHEMA = "wendy.g1.reference-residual-runtime-status.v1"


def readiness_status(state: dict, vision: dict, *, observed_at_unix_ns: int, revision: str) -> dict:
    """Translate shadow measurements into the dashboard's fail-closed contract.

    This status is evidence only. In particular, a successful shadow timing
    report never implies physical ownership, entry alignment, recovery, or
    policy qualification.
    """
    runtime_healthy = state.get("healthy") is True
    report = state.get("report") if runtime_healthy and isinstance(state.get("report"), dict) else {}
    compute = report.get("control_compute_ms") if isinstance(report.get("control_compute_ms"), dict) else {}
    provider = state.get("provider") if isinstance(state.get("provider"), dict) else {}
    provider_current = runtime_healthy and state.get("last_error") is None and provider.get("exact_frame_synchronized") is True
    provider_hz = provider.get("observed_hz_6s")
    capture_age_ms = vision.get("latest_capture_age_ms")
    provider_current = (provider_current and isinstance(capture_age_ms, (int, float))
                        and 0 <= capture_age_ms <= MAXIMUM_CAMERA_AGE_S * 1000)
    sample_count = report.get("steps") if isinstance(report.get("steps"), int) else 0
    episode_step = state.get("shadow_episode_step")

    return {
        "schema": STATUS_SCHEMA,
        "revision": revision,
        "observed_at_unix_ns": observed_at_unix_ns,
        "status": state.get("status", "unknown"),
        "healthy": runtime_healthy,
        "active": False,
        "motion_capability": False,
        "physical_commands_sent": 0,
        "identity": state["identity"],
        "timing": {
            "target_hz": CONTROL_HZ,
            "observed_hz": report.get("observed_hz"),
            "p99_ms": compute.get("p99"),
            "deadline_misses": report.get("deadline_misses"),
            "sample_count": sample_count,
            "deadline_qualified": report.get("deadline_qualified") is True,
            "measurement_scope": "motion-zero proposal compute and scheduled shadow loop",
        },
        "perception": {
            # RGB-D and mask are admitted as one exact-frame provider packet, so
            # their observed input rate and capture age are the same measurement.
            "camera_hz": provider_hz,
            "mask_hz": provider_hz,
            "camera_age_ms": capture_age_ms,
            "mask_age_ms": capture_age_ms,
            "stream_synchronized": provider_current,
            "target_mask_valid": provider_current and provider.get("target_mask_valid") is True,
            "mask_checkpoint_sha256": provider.get("mask_checkpoint_sha256"),
            "camera_parity_qualified": state.get("camera_parity_qualified") is True,
            "frame_id": provider.get("frame_id"),
            "stream_id": provider.get("stream_id"),
        },
        "recurrent": {
            "gru_reset_verified": False,
            "hidden_state_zero": False,
            "episode_step": episode_step,
            "reference_frame": None,
            "detail": "shadow reset is exercised, but live episode-zero admission is not implemented",
        },
        "ownership": {
            "exclusive": False,
            "waist_yaw": False,
            "right_arm": False,
            "right_hand": False,
            "other_joints": False,
            "detail": "motion-zero shadow owns no robot joints",
        },
        "alignment": {
            "current_state_aligned": False,
            "within_entry_limits": False,
            "detail": "shadow uses frozen trace joint features, not current G1 state",
        },
        "stop_recovery": {
            "stop_ready": False,
            "watchdog_ready": False,
            "recovery_ready": False,
            "tested_for_candidate": False,
            "detail": "no physical controller or candidate-specific stop/recovery path is attached",
        },
        "qualification": {
            "physical_policy_qualified": False,
            "supervisor_present": False,
            "workspace_clear": False,
            "detail": "motion-zero shadow evidence is not physical qualification",
        },
        "blockers": [
            "live GRU episode-zero admission is unverified",
            "exclusive waist/right-arm/right-hand ownership is unverified",
            "current G1 entry alignment is unavailable",
            "physical stop, watchdog, and recovery are unverified",
            "candidate is not physically qualified",
        ],
    }


def get(url: str, *, limit: int, timeout: float = .6) -> bytes:
    request = Request(url, headers={"User-Agent": "Wendy-G1-Async-Shadow/1"})
    with urlopen(request, timeout=timeout) as response:
        body = response.read(limit + 1)
    if len(body) > limit:
        raise ValueError("bounded HTTP read exceeded")
    return body


def decode_policy_frame(raw: bytes, expected: dict) -> tuple[torch.Tensor, dict]:
    """Decode one exact, preprocessed RGB/depth/mask packet from segmentation."""
    with np.load(io.BytesIO(raw), allow_pickle=False) as values:
        if set(values.files) != {"color_bgr", "depth_m", "mask", "metadata_json"}:
            raise ValueError("segmentation policy-frame contract changed")
        meta = json.loads(values["metadata_json"].item())
        bgr = values["color_bgr"].copy()
        depth = values["depth_m"].copy()
        mask = values["mask"].copy()
    for key in ("frame_id", "stream_id", "captured_at_unix_ns"):
        if meta.get(key) != expected.get(key):
            raise ValueError("segmentation policy-frame identity mismatch")
    if (
        bgr.shape != (240, 320, 3)
        or bgr.dtype != np.uint8
        or depth.shape != (240, 320)
        or mask.shape != (240, 320)
    ):
        raise ValueError("segmentation policy-frame dimensions changed")
    if depth.dtype.kind not in "fiu" or not np.isfinite(depth).all():
        raise ValueError("segmentation depth must contain finite numeric values")
    rgb = bgr[:, :, ::-1]
    quantized_depth = np.clip(np.rint(depth * 1000) * .001, 0, 5)
    # Pack with NumPy and expose it to Torch without a copy. The previous
    # sequence of tiny Torch CPU operations competed with the batch-1 actor's
    # global thread pool in the physical process.
    packed = np.empty((1, 5, 240, 320), dtype=np.float32)
    packed[0, :3] = np.transpose(rgb, (2, 0, 1)).astype(np.float32) / 255
    packed[0, 3] = quantized_depth.astype(np.float32) / 5
    packed[0, 4] = (mask > 0).astype(np.float32)
    return torch.from_numpy(packed), meta


def percentiles(values: list[float]) -> dict[str, float | None]:
    if not values:
        return {"p50": None, "p95": None, "p99": None, "maximum": None}
    return {
        "p50": float(np.percentile(values, 50)),
        "p95": float(np.percentile(values, 95)),
        "p99": float(np.percentile(values, 99)),
        "maximum": float(max(values)),
    }


class ShadowRuntime:
    def __init__(self) -> None:
        self.lock = threading.Lock()
        self.stop = threading.Event()
        self.policy: CachedReferenceResidualPolicy | None = None
        self.runtime_instance_id = secrets.token_hex(16)
        self.report_generation = 0
        self.state_value = {
            "schema": "wendy.g1.reference-residual-shadow.v1",
            "status": "loading",
            "healthy": False,
            "motion_capability": False,
            "physical_commands_sent": 0,
            "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
            "identity": {
                "candidate_id": "reference-residual-gru-u002525",
                "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
                "runtime_manifest_sha256": EXPECTED_RUNTIME_MANIFEST_SHA256,
                "reference_id": "attempt-000159",
                "reference_contract_sha256": EXPECTED_REFERENCE_SHA256,
                "mask_checkpoint_sha256": EXPECTED_MASK_SHA256,
                "source_commit": "8c2e592172393abefb4c3c27404a8b03e0740e87",
            },
            "control_hz": CONTROL_HZ,
            "camera_hz_required": CAMERA_HZ,
            "maximum_camera_age_s": MAXIMUM_CAMERA_AGE_S,
            "provider_pack_contract": "benchmark only: RGB INTER_AREA; aligned depth and mask INTER_NEAREST",
            "camera_parity_qualified": False,
            "report": None,
            "provider": {},
            "last_error": None,
            "shadow_episode_step": None,
            "runtime_diagnostics": {
                "torch_version": torch.__version__,
                "torch_cuda_version": torch.version.cuda,
                "nvidia_visible_devices": os.environ.get("NVIDIA_VISIBLE_DEVICES"),
                "nvidia_driver_capabilities": os.environ.get("NVIDIA_DRIVER_CAPABILITIES"),
                "ld_library_path": os.environ.get("LD_LIBRARY_PATH"),
            },
        }
        self.provider_thread: threading.Thread | None = None
        self.control_thread: threading.Thread | None = None

    def start(self) -> None:
        self.policy = CachedReferenceResidualPolicy(
            BUNDLE,
            device="cuda",
            maximum_camera_age_s=MAXIMUM_CAMERA_AGE_S,
        )
        with self.lock:
            self.state_value.update(status="waiting_for_camera", healthy=True)
        self.provider_thread = threading.Thread(target=self._provider_loop, name="camera-provider", daemon=True)
        self.control_thread = threading.Thread(target=self._control_loop, name="control-shadow", daemon=True)
        self.provider_thread.start()
        self.control_thread.start()

    def start_safely(self) -> None:
        """Keep the evidence endpoint alive when platform initialization fails."""
        try:
            self.start()
        except Exception as exc:
            with self.lock:
                self.state_value.update(status="failed", healthy=False)
            self._set_error(exc)

    def close(self) -> None:
        self.stop.set()
        for thread in (self.provider_thread, self.control_thread):
            if thread:
                thread.join(timeout=2)
        if self.policy:
            self.policy.close()

    def state(self) -> dict:
        with self.lock:
            value = json.loads(json.dumps(self.state_value, allow_nan=False))
        if self.policy:
            value["vision"] = self.policy.vision.status()
        value["generated_at_unix_ns"] = time.time_ns()
        return value

    def dashboard_status(self) -> dict:
        with self.lock:
            state = json.loads(json.dumps(self.state_value, allow_nan=False))
            revision = f"{self.runtime_instance_id}:{self.report_generation}"
        vision = self.policy.vision.status() if self.policy else {}
        return readiness_status(state, vision, observed_at_unix_ns=time.time_ns(), revision=revision)

    def _set_error(self, error: Exception) -> None:
        with self.lock:
            self.state_value["last_error"] = f"{type(error).__name__}: {error}"

    def _provider_loop(self) -> None:
        assert self.policy is not None
        seen: tuple[str, int] | None = None
        submitted_at: list[float] = []
        while not self.stop.is_set():
            try:
                state = json.loads(get(SEGMENTATION_URL + "/state", limit=1_000_000))
                latest = state.get("latest") or {}
                if not state.get("healthy") or not latest:
                    raise ValueError(state.get("last_error") or "segmentation unavailable")
                if (latest.get("model") or {}).get("checkpoint_sha256") != EXPECTED_MASK_SHA256:
                    raise ValueError("segmentation checkpoint identity changed")
                stream_id = str(latest["stream_id"])
                frame_id = int(latest["frame_id"])
                identity = (stream_id, frame_id)
                if identity == seen:
                    self.stop.wait(.005)
                    continue
                packet_url = (
                    SEGMENTATION_URL + "/frame.policy-rgbd"
                    + f"?frame_id={frame_id}&stream_id={stream_id}"
                )
                packet_raw = get(packet_url, limit=1_000_000)
                image, meta = decode_policy_frame(packet_raw, latest)
                if not state.get("target_mask_valid"):
                    image[:, 4].zero_()
                self.policy.submit_camera(
                    image=image,
                    frame_id=frame_id,
                    stream_id=stream_id,
                    captured_at_ns=int(meta["captured_at_unix_ns"]),
                )
                seen = identity
                now = time.monotonic()
                submitted_at.append(now)
                submitted_at = [stamp for stamp in submitted_at if now - stamp <= 6]
                rate = (len(submitted_at) - 1) / max(.001, submitted_at[-1] - submitted_at[0]) if len(submitted_at) > 1 else 0
                with self.lock:
                    self.state_value["provider"] = {
                        "frame_id": frame_id,
                        "stream_id": stream_id,
                        "target_mask_valid": bool(state.get("target_mask_valid")),
                        "observed_hz_6s": rate,
                        "exact_frame_synchronized": True,
                        "mask_checkpoint_sha256": EXPECTED_MASK_SHA256,
                    }
                    self.state_value["last_error"] = None
            except Exception as exc:
                self._set_error(exc)
                self.stop.wait(.05)

    def _control_loop(self) -> None:
        assert self.policy is not None
        try:
            with np.load(BUNDLE / "golden-trace-100.npz", allow_pickle=False) as trace:
                joints = trace["joint_features_raw"].copy()
                references = trace["reference_full"].copy()
                velocities = trace["reference_velocity"].copy()
            deadline = 1 / CONTROL_HZ
            while not self.stop.is_set():
                self.policy.reset_episode()
                compute_ms: list[float] = []
                start_lateness_ms: list[float] = []
                camera_age_ms: list[float] = []
                misses = stale = proposals = 0
                next_tick = time.perf_counter()
                with self.lock:
                    self.state_value["status"] = "benchmarking"
                    self.state_value["shadow_episode_step"] = None
                for index in range(400):
                    if self.stop.is_set():
                        return
                    started = time.perf_counter()
                    lateness = max(0.0, started - next_tick)
                    start_lateness_ms.append(lateness * 1000)
                    try:
                        proposal = self.policy.propose(
                            joint_features=torch.from_numpy(joints[index % 100:(index % 100) + 1]),
                            reference_full=references[index % 100],
                            reference_velocity=velocities[index % 100],
                            control_at_ns=time.time_ns(),
                        )
                        elapsed_ms = (time.perf_counter() - started) * 1000
                        compute_ms.append(elapsed_ms)
                        camera_age_ms.append(proposal["camera_age_ms"])
                        proposals += 1
                        if elapsed_ms > deadline * 1000:
                            misses += 1
                    except VisionUnavailable:
                        stale += 1
                    next_tick += deadline
                    wait = next_tick - time.perf_counter()
                    if wait > 0:
                        self.stop.wait(wait)
                elapsed_s = max(.001, time.perf_counter() - (next_tick - 400 * deadline))
                compute_percentiles = percentiles(compute_ms)
                report = {
                    "steps": 400,
                    "proposals": proposals,
                    "stale_or_missing_vision": stale,
                    "deadline_ms": 25,
                    "deadline_misses": misses,
                    "deadline_miss_fraction": misses / max(1, proposals),
                    "observed_hz": 400 / elapsed_s,
                    "control_compute_ms": compute_percentiles,
                    "loop_start_lateness_ms": percentiles(start_lateness_ms),
                    "camera_age_ms": percentiles(camera_age_ms),
                    "deadline_qualified": (
                        proposals == 400
                        and misses == 0
                        and compute_percentiles["p99"] is not None
                        and compute_percentiles["p99"] < 25
                    ),
                    "motion_capability": False,
                    "physical_commands_sent": 0,
                }
                with self.lock:
                    self.report_generation += 1
                    self.state_value.update(status="ready", report=report, healthy=True, shadow_episode_step=400)
                self.stop.wait(1.0)
        except Exception as exc:
            with self.lock:
                self.state_value.update(status="failed", healthy=False)
            self._set_error(exc)


def make_server(runtime: ShadowRuntime, host: str = "127.0.0.1", port: int = PORT):
    class Handler(BaseHTTPRequestHandler):
        def do_GET(self):
            path = self.path.split("?", 1)[0]
            if path == "/api/reference-residual/status":
                value = runtime.dashboard_status()
                # Status is always readable so consumers can render explicit
                # false gates while unhealthy. /health retains HTTP health semantics.
                return self.reply(200, value)
            if path not in ("/state", "/health"):
                return self.reply(404, {"error": "not found"})
            value = runtime.state()
            if path == "/health":
                value.pop("report", None)
            self.reply(200 if value["healthy"] else 503, value)

        def do_POST(self):
            self.reply(405, {"error": "shadow runtime has no command route"})

        def reply(self, status: int, value: dict):
            body = json.dumps(value, allow_nan=False).encode()
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.send_header("Cache-Control", "no-store")
            self.send_header("X-Content-Type-Options", "nosniff")
            self.end_headers()
            self.wfile.write(body)

        def log_message(self, *_args):
            return

    return ThreadingHTTPServer((host, port), Handler)


def main() -> None:
    if os.environ.get("G1_MOTION_ENABLED", "false") != "false":
        raise RuntimeError("G1_MOTION_ENABLED must be exactly false")
    runtime = ShadowRuntime()
    server = make_server(runtime)
    threading.Thread(target=runtime.start_safely, name="runtime-bootstrap", daemon=True).start()
    try:
        server.serve_forever()
    finally:
        runtime.close()
        server.server_close()


if __name__ == "__main__":
    main()

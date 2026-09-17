"""Fail-closed, single-owner reference-entry to physical Stage 2 execution.

The motion-zero policy and observation adapter remain independently testable.
This module is the intentionally narrow seam that lets the physical service
hold its existing Unitree publisher ownership across reference-frame-0 entry,
GRU reset, camera admission, and policy step 0.
"""
from __future__ import annotations

import json
import os
import threading
import time
from collections import deque
from pathlib import Path
from typing import Any

import numpy as np

from physical_io.service import (
    CHECKPOINT_SHA256,
    COMMAND_LOWER_BY_INDEX,
    COMMAND_POLICY_INDICES,
    COMMAND_UPPER_BY_INDEX,
    ENTRY_MAXIMUM_DURATION_S,
    ENTRY_MAXIMUM_STEP_RAD,
    ENTRY_MAXIMUM_VELOCITY_RAD_S,
    ENTRY_MINIMUM_DURATION_S,
    ENTRY_POSITION_LIMIT_RAD,
    ENTRY_SETTLE_S,
    ENTRY_SETTLE_TIMEOUT_S,
    ENTRY_SPEED_LIMIT_RAD_S,
    ENTRY_TRACKING_ABORT_RAD,
    READY_FSM_MODES,
    RUNNING_FSM,
    TIMING_RETRY_DELAY_S,
    PhysicalProbeRuntime,
)
from physical_io.unitree_io import InterlockError
from runtime.async_vision import VisionUnavailable
from runtime.exact_policy import CachedReferenceResidualPolicy
from runtime.observation_adapter import (
    CONTROL_HZ,
    ExactEpisodeObservationAdapter,
    OrderedJointState,
    SynchronizedPolicyPacket,
)
from runtime.shadow_service import (
    EXPECTED_MASK_SHA256,
    decode_policy_frame,
    get,
)

POLICY_CONTRACT_HZ = CONTROL_HZ
ALLOWED_EXECUTION_HZ = (15.0, 25.0, 30.0, 40.0)
DEFAULT_EXECUTION_HZ = float(
    os.environ.get("G1_PHYSICAL_CONTROL_HZ", str(POLICY_CONTRACT_HZ))
)
if DEFAULT_EXECUTION_HZ not in ALLOWED_EXECUTION_HZ:
    raise ValueError("G1_PHYSICAL_CONTROL_HZ must be 15, 25, 30, or 40")
EXECUTION_HZ = DEFAULT_EXECUTION_HZ
TICK_S = 1.0 / DEFAULT_EXECUTION_HZ
ENFORCE_CONTROL_DEADLINE = os.environ.get(
    "G1_ENFORCE_CONTROL_DEADLINE", "true"
).lower() == "true"
REQUIRE_ENTRY_SETTLE = os.environ.get(
    "G1_REQUIRE_ENTRY_SETTLE", "true"
).lower() == "true"
# Preserve the qualified wall-clock ramp durations when intentionally testing
# a slower physical cadence.
ENTRY_OWNERSHIP_RAMP_DURATION_S = float(
    os.environ.get("G1_ENTRY_OWNERSHIP_RAMP_S", "10")
)
MAXIMUM_POLICY_STEP_RAD = float(
    os.environ.get("G1_MAXIMUM_POLICY_STEP_RAD", "0.02")
)
if not 0.02 <= MAXIMUM_POLICY_STEP_RAD <= 0.052:
    raise ValueError("G1_MAXIMUM_POLICY_STEP_RAD must be between 0.02 and 0.052")
MAXIMUM_POLICY_TRACKING_ERROR_RAD = float(
    os.environ.get("G1_MAXIMUM_POLICY_TRACKING_ERROR_RAD", "0.10")
)
if not 0.20 <= MAXIMUM_POLICY_TRACKING_ERROR_RAD <= 0.44:
    raise ValueError(
        "G1_MAXIMUM_POLICY_TRACKING_ERROR_RAD must be between 0.20 and 0.44"
    )
# The Jetson's userspace scheduler showed 0.441 ms of lateness after the two
# DDS writes were parallelized. Preserve the configured absolute schedule and
# catch up on the next tick, but fail closed if lateness exceeds 2 ms.
MAXIMUM_SCHEDULER_JITTER_S = 0.002
MAXIMUM_TIMING_GAP_RETRIES = 3
CAMERA_ADMISSION_TIMEOUT_S = 2.0
MAXIMUM_POLICY_STEPS = 1200
RUN_LOG_DIR = Path(os.environ.get("G1_RUN_LOG_DIR", "/var/lib/wendy/g1-stage02-runs"))


def _can_alignment_sample(
    command_id: str,
    policy_step: int,
    control_at_unix_ns: int,
    proposal: dict[str, Any],
) -> dict[str, Any]:
    retarget = proposal.get("object_retarget") or {}
    return {
        "schema": "wendy.g1.can-alignment-sample.v1",
        "command_id": command_id,
        "policy_step": policy_step,
        "control_at_unix_ns": control_at_unix_ns,
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
        "clipped_offset_world_xy_m": retarget.get("clipped_offset_world_xy_m"),
        "filtered_offset_world_xy_m": retarget.get("offset_world_xy_m"),
        "pixel_displacement_px": retarget.get("pixel_displacement_px"),
        "ideal_pixel_xy": retarget.get("ideal_pixel_xy"),
        "detected_pixel_xy": retarget.get("detected_pixel_xy"),
        "latched": retarget.get("latched"),
    }
SEGMENTATION_URL = os.environ.get("G1_SEGMENTATION_URL", "http://127.0.0.1:8003").rstrip("/")
MAXIMUM_CAMERA_AGE_S = float(os.environ.get("G1_MAXIMUM_CAMERA_AGE_S", ".125"))
POLICY_DEVICE = os.environ.get("G1_POLICY_DEVICE", "cuda")
POLICY_CONTROL_DEVICE = os.environ.get("G1_POLICY_CONTROL_DEVICE")
POLICY_INFERENCE_URL = "http://127.0.0.1:8117"
FAULT_RELEASE_DURATION_S = float(os.environ.get("G1_FAULT_RELEASE_DURATION_S", "1.0"))
RETREAT_MAXIMUM_SPEED_RAD_S = float(
    os.environ.get("G1_RETREAT_MAXIMUM_SPEED_RAD_S", "0.08")
)
RETREAT_TRACKING_ABORT_RAD = float(
    os.environ.get("G1_RETREAT_TRACKING_ABORT_RAD", "0.22")
)
if not 0.02 <= RETREAT_MAXIMUM_SPEED_RAD_S <= 0.12:
    raise ValueError("G1_RETREAT_MAXIMUM_SPEED_RAD_S must be between 0.02 and 0.12")
if not 0.08 <= RETREAT_TRACKING_ABORT_RAD <= 0.22:
    raise ValueError("G1_RETREAT_TRACKING_ABORT_RAD must be between 0.08 and 0.22")
TIMING_LOG_INTERVAL_STEPS = max(
    1, int(os.environ.get("G1_TIMING_LOG_INTERVAL_STEPS", "40"))
)
FAULT_HOLD_LOG_INTERVAL_STEPS = max(
    1, int(os.environ.get("G1_FAULT_HOLD_LOG_INTERVAL_STEPS", "400"))
)


class PolicyStopRequested(InterlockError):
    """Operator requested a controlled pose-latched authority release."""


class TimingMetrics:
    """Low-overhead timing aggregation with bounded recent quantiles."""

    def __init__(self, *, recent_limit: int = 512) -> None:
        self.recent_limit = recent_limit
        self.lock = threading.Lock()
        self.values: dict[str, dict[str, Any]] = {}

    def reset(self) -> None:
        with self.lock:
            self.values = {}

    def record_seconds(self, name: str, value: float) -> float:
        return self.record_ms(name, value * 1000.0)

    def record_ms(self, name: str, value: float) -> float:
        value = max(0.0, float(value))
        with self.lock:
            bucket = self.values.setdefault(
                name,
                {
                    "count": 0,
                    "total_ms": 0.0,
                    "maximum_ms": 0.0,
                    "recent": deque(maxlen=self.recent_limit),
                },
            )
            bucket["count"] += 1
            bucket["total_ms"] += value
            bucket["maximum_ms"] = max(bucket["maximum_ms"], value)
            bucket["recent"].append(value)
        return value

    @staticmethod
    def _percentile(values: list[float], fraction: float) -> float:
        if not values:
            return 0.0
        ordered = sorted(values)
        index = min(len(ordered) - 1, max(0, round((len(ordered) - 1) * fraction)))
        return ordered[index]

    def snapshot(self) -> dict[str, dict[str, float | int]]:
        with self.lock:
            copied = {
                name: {
                    "count": int(bucket["count"]),
                    "total_ms": float(bucket["total_ms"]),
                    "maximum_ms": float(bucket["maximum_ms"]),
                    "recent": list(bucket["recent"]),
                }
                for name, bucket in self.values.items()
            }
        result: dict[str, dict[str, float | int]] = {}
        for name, bucket in copied.items():
            count = int(bucket["count"])
            recent = list(bucket["recent"])
            result[name] = {
                "count": count,
                "mean_ms": float(bucket["total_ms"]) / count if count else 0.0,
                "p50_ms": self._percentile(recent, 0.50),
                "p95_ms": self._percentile(recent, 0.95),
                "p99_ms": self._percentile(recent, 0.99),
                "maximum_ms": float(bucket["maximum_ms"]),
            }
        return result


class SegmentationCameraPump:
    """Continuously submit exact-frame RGB-D-mask packets to one episode adapter."""

    def __init__(
        self,
        adapter: ExactEpisodeObservationAdapter,
        *,
        segmentation_url: str = SEGMENTATION_URL,
    ) -> None:
        self.adapter = adapter
        self.segmentation_url = segmentation_url.rstrip("/")
        self.stop = threading.Event()
        self.active = threading.Event()
        self.thread: threading.Thread | None = None
        self.lock = threading.Lock()
        self.latest: dict[str, Any] = {}
        self.last_error: str | None = None
        self.timing = TimingMetrics()

    def preflight(self) -> dict[str, Any]:
        started = time.perf_counter()
        try:
            state = json.loads(get(self.segmentation_url + "/state", limit=1_000_000))
        finally:
            self.timing.record_seconds(
                "segmentation_state_fetch", time.perf_counter() - started
            )
        latest = state.get("latest") or {}
        if not state.get("healthy") or not latest:
            raise InterlockError(state.get("last_error") or "segmentation is unavailable")
        model = latest.get("model") or {}
        if model.get("checkpoint_sha256") != EXPECTED_MASK_SHA256:
            raise InterlockError("segmentation checkpoint identity changed")
        captured_at_ns = int(latest.get("captured_at_unix_ns", 0))
        age_s = (time.time_ns() - captured_at_ns) / 1_000_000_000
        if captured_at_ns <= 0 or age_s < 0 or age_s > MAXIMUM_CAMERA_AGE_S:
            raise InterlockError(f"segmentation capture is stale ({age_s * 1000:.1f} ms)")
        return state

    def start(self) -> None:
        if self.thread is not None:
            raise RuntimeError("camera pump already started")
        self.thread = threading.Thread(target=self._run, name="physical-policy-camera", daemon=True)
        self.thread.start()

    def status(self) -> dict[str, Any]:
        with self.lock:
            value = {"latest": dict(self.latest), "last_error": self.last_error}
        value["timing"] = self.timing.snapshot()
        return value

    def close(self) -> None:
        self.stop.set()
        self.active.set()
        if self.thread is not None:
            self.thread.join(timeout=2.0)
            if self.thread.is_alive():
                raise RuntimeError("camera pump did not stop")

    def _run(self) -> None:
        seen: tuple[str, int] | None = None
        while not self.stop.is_set():
            if not self.active.wait(timeout=.1):
                continue
            if self.stop.is_set():
                break
            cycle_started = time.perf_counter()
            try:
                state = self.preflight()
                latest = state["latest"]
                stream_id = str(latest["stream_id"])
                frame_id = int(latest["frame_id"])
                identity = (stream_id, frame_id)
                if identity == seen:
                    self.stop.wait(.005)
                    continue
                fetch_started = time.perf_counter()
                try:
                    raw = get(
                        self.segmentation_url
                        + f"/frame.policy-rgbd?frame_id={frame_id}&stream_id={stream_id}",
                        limit=1_000_000,
                    )
                finally:
                    self.timing.record_seconds(
                        "policy_frame_fetch", time.perf_counter() - fetch_started
                    )
                decode_started = time.perf_counter()
                image, meta = decode_policy_frame(raw, latest)
                target_mask_valid = state.get("target_mask_valid") is True
                if not target_mask_valid:
                    # The frozen sensor contract represents an invalid or
                    # off-screen detection as a synchronized RGB-D frame with
                    # an all-zero mask.  This is required for the reference's
                    # opening waist turn, which may bring the can into view.
                    image[:, 4].zero_()
                packet = SynchronizedPolicyPacket(
                    image=image,
                    frame_id=frame_id,
                    stream_id=stream_id,
                    captured_at_ns=int(meta["captured_at_unix_ns"]),
                    detection_valid=target_mask_valid,
                )
                # Before reset_episode this is expected to reject; retain no
                # fabricated frame and try the next source frame.
                try:
                    admitted = self.adapter.submit_camera(packet)
                except RuntimeError as exc:
                    if "reset_episode must be called" not in str(exc):
                        raise
                    admitted = False
                self.timing.record_seconds(
                    "policy_frame_decode_submit", time.perf_counter() - decode_started
                )
                seen = identity
                with self.lock:
                    self.latest = {
                        "frame_id": frame_id,
                        "stream_id": stream_id,
                        "captured_at_unix_ns": packet.captured_at_ns,
                        "admitted": admitted,
                    }
                    self.last_error = None
            except Exception as exc:
                with self.lock:
                    self.last_error = f"{type(exc).__name__}: {exc}"
                self.stop.wait(.025)
            finally:
                self.timing.record_seconds(
                    "camera_cycle", time.perf_counter() - cycle_started
                )

    def activate(self) -> None:
        self.active.set()

    def deactivate(self) -> None:
        self.active.clear()


class IntegratedPhysicalPolicyRunner:
    """Execute bounded exact-policy steps without releasing entry ownership."""

    def __init__(
        self,
        physical: PhysicalProbeRuntime,
        episode: ExactEpisodeObservationAdapter,
        camera: SegmentationCameraPump,
        *,
        monotonic=time.monotonic,
        wall_time_ns=time.time_ns,
        sleeper=time.sleep,
    ) -> None:
        self.physical = physical
        self.episode = episode
        self.camera = camera
        self.monotonic = monotonic
        self.wall_time_ns = wall_time_ns
        self.sleeper = sleeper
        self.maximum_schedule_lateness_s = 0.0
        self.timing = TimingMetrics()
        self.stop_requested = threading.Event()
        self.retreat_requested = threading.Event()
        self.status_lock = threading.Lock()
        self.running = False
        self.command_id: str | None = None
        self.policy_steps_completed = 0
        self.policy_started_at_monotonic: float | None = None
        self.policy_finished_at_monotonic: float | None = None
        self.last_error: str | None = None
        self.last_release: dict[str, Any] | None = None
        self.holding_after_fault = False
        self.fault_hold_started_at_monotonic: float | None = None
        self.fault_hold_publish_count = 0
        self.last_fault_hold: dict[str, Any] | None = None
        self.retreat_active = False
        self.retreat_available = False
        self.retreat_waypoints_completed = 0
        self.retreat_waypoints_total = 0
        self.last_retreat: dict[str, Any] | None = None
        self.reference_targets_43: np.ndarray | None = None
        self.execution_hz = DEFAULT_EXECUTION_HZ
        self.tick_s = 1.0 / self.execution_hz

    @classmethod
    def load(cls, physical: PhysicalProbeRuntime, bundle: Path) -> IntegratedPhysicalPolicyRunner:
        if POLICY_INFERENCE_URL:
            from runtime.inference_client import remote_components

            episode, camera = remote_components(POLICY_INFERENCE_URL, bundle)
            physical.reference_frame0 = np.asarray(episode.reference_frame0, dtype=float)
        else:
            policy = CachedReferenceResidualPolicy(
                bundle,
                device=POLICY_DEVICE,
                control_device=POLICY_CONTROL_DEVICE,
                maximum_camera_age_s=MAXIMUM_CAMERA_AGE_S,
            )
            episode = ExactEpisodeObservationAdapter(policy, bundle)
            camera = SegmentationCameraPump(episode)
            camera.start()
        runner = cls(physical, episode, camera)
        if not POLICY_INFERENCE_URL:
            with np.load(bundle / "reference-contract.npz", allow_pickle=False) as values:
                reference_targets_43 = np.asarray(
                    values["reference_joint_targets_43"], dtype=float
                )
            if (
                reference_targets_43.ndim != 2
                or reference_targets_43.shape[1] != 43
                or not np.isfinite(reference_targets_43).all()
            ):
                raise RuntimeError("sealed reference_joint_targets_43 is invalid")
            runner.reference_targets_43 = reference_targets_43
        return runner

    def refresh_contract(self) -> dict[str, Any]:
        refresh = getattr(self.episode, "refresh_contract", None)
        if callable(refresh):
            value = refresh()
            self.physical.reference_frame0 = np.asarray(
                self.episode.reference_frame0, dtype=float
            )
            return value
        return {
            "reference_frames": len(self.episode.reference.references),
            "active_policy": None,
        }

    def preflight(self) -> dict[str, Any]:
        with self.status_lock:
            if self.running:
                raise InterlockError("a policy run is already active")
            if self.holding_after_fault:
                raise InterlockError("fault hold must be released before another run")
        io = self.physical.io.status()
        if io.get("publishers_armed"):
            raise InterlockError("command publishers are already armed")
        if not self.physical.motion_enabled:
            raise InterlockError("physical motion is disabled")
        contract = self.refresh_contract()
        camera = self.camera.preflight()
        live = self.physical._live_guard()
        return {
            "schema": "wendy.g1.reference-residual-play-preflight.v1",
            "accepted": True,
            "active_policy": contract.get("active_policy")
            or getattr(self.episode, "active_policy", None),
            "reference_frames": len(self.episode.reference.references),
            "camera": camera,
            "mode_machine": live.get("mode_machine"),
            "remote_neutral": (live.get("remote") or {}).get("neutral") is True,
            "publishers_armed": False,
            "motion_commands_sent": 0,
        }

    def close(self) -> None:
        self.stop_requested.set()
        self.retreat_requested.set()
        self.camera.close()
        self.episode.policy.close()

    def request_stop(self, command_id: str) -> dict[str, Any]:
        self.stop_requested.set()
        with self.status_lock:
            running = self.running
            holding = self.holding_after_fault
            active_command = self.command_id
        accepted = running or holding
        return {
            "schema": "wendy.g1.reference-residual-controlled-stop.v1",
            "accepted": accepted,
            "state": (
                "fault_hold_release_requested"
                if holding
                else ("controlled_release_requested" if running else "already_idle")
            ),
            "command_id": command_id,
            "active_command_id": active_command,
            "behavior": (
                "explicitly release the latched SDK-weight-1 fault hold"
                if holding
                else (
                    "hold measured pose while ramping arm SDK authority to zero; "
                    "no high-level arm stop action"
                )
            ),
        }

    def request_retreat(self, command_id: str) -> dict[str, Any]:
        with self.status_lock:
            holding = self.holding_after_fault
            active = self.retreat_active
            available = self.retreat_available
            active_command = self.command_id
        if command_id != active_command:
            raise InterlockError("retreat command id does not match the active fault hold")
        if not holding:
            raise InterlockError("safe retreat is available only during a fault hold")
        if not available:
            raise InterlockError("no commanded policy path is available to reverse")
        if not active:
            self.retreat_requested.set()
        return {
            "schema": "wendy.g1.reference-residual-safe-retreat.v1",
            "accepted": True,
            "state": "already_retreating" if active else "retreat_requested",
            "command_id": command_id,
            "behavior": (
                "reverse the actual commanded joint path at capped speed while retaining "
                "SDK weight 1, then hold the entry pose until explicit release"
            ),
            "maximum_speed_rad_s": RETREAT_MAXIMUM_SPEED_RAD_S,
            "tracking_abort_rad": RETREAT_TRACKING_ABORT_RAD,
            "automatic_release": False,
        }

    def status(self) -> dict[str, Any]:
        with self.status_lock:
            running = self.running
            command_id = self.command_id
            steps = self.policy_steps_completed
            started = self.policy_started_at_monotonic
            finished = self.policy_finished_at_monotonic
            error = self.last_error
            release = None if self.last_release is None else dict(self.last_release)
            holding = self.holding_after_fault
            hold_started = self.fault_hold_started_at_monotonic
            hold_publishes = self.fault_hold_publish_count
            fault_hold = (
                None if self.last_fault_hold is None else dict(self.last_fault_hold)
            )
            retreat = None if self.last_retreat is None else dict(self.last_retreat)
            retreat_active = self.retreat_active
            retreat_available = self.retreat_available
            retreat_completed = self.retreat_waypoints_completed
            retreat_total = self.retreat_waypoints_total
        elapsed = (
            max(0.0, (finished if finished is not None else self.monotonic()) - started)
            if started is not None
            else 0.0
        )
        camera_status = (
            self.camera.status()
            if callable(getattr(self.camera, "status", None))
            else {"latest": {}, "last_error": None, "timing": {}}
        )
        policy = getattr(self.episode, "policy", None)
        active_policy = camera_status.get("active_policy") or getattr(
            self.episode, "active_policy", None
        )
        return {
            "schema": "wendy.g1.reference-residual-policy-runtime-status.v1",
            "vision_device": str(getattr(policy, "vision_device", "unknown")),
            "control_device": str(getattr(policy, "device", "unknown")),
            "running": running,
            "command_id": command_id,
            "policy_steps_completed": steps,
            "policy_steps_total": min(
                MAXIMUM_POLICY_STEPS, len(self.episode.reference.references)
            ),
            "active_policy": active_policy,
            "maximum_policy_step_rad": MAXIMUM_POLICY_STEP_RAD,
            "maximum_policy_tracking_error_rad": MAXIMUM_POLICY_TRACKING_ERROR_RAD,
            "policy_elapsed_s": elapsed,
            "effective_policy_hz": steps / elapsed if elapsed > 0 else 0.0,
            "selected_execution_hz": self.execution_hz,
            "allowed_execution_hz": list(ALLOWED_EXECUTION_HZ),
            "policy_contract_hz": POLICY_CONTRACT_HZ,
            "timing_log_stream": "adapter stdout lines prefixed G1_POLICY_TIMING",
            "timing_quantiles": (
                "most recent 512 observations; mean and maximum cover the full run"
            ),
            "timing": self.timing.snapshot(),
            "camera": camera_status,
            "last_error": error,
            "last_release": release,
            "holding_after_fault": holding,
            "fault_hold": {
                "active": holding,
                "sdk_weight": 1.0 if holding else None,
                "publish_count": hold_publishes,
                "elapsed_s": (
                    max(0.0, self.monotonic() - hold_started)
                    if holding and hold_started is not None
                    else 0.0
                ),
                "release_required": holding,
                "behavior": (
                    "captured pose is continuously republished; native handoff is blocked"
                    if holding
                    else "not holding"
                ),
            },
            "last_fault_hold": fault_hold,
            "safe_retreat": {
                "available": holding and retreat_available,
                "active": retreat_active,
                "waypoints_completed": retreat_completed,
                "waypoints_total": retreat_total,
                "maximum_speed_rad_s": RETREAT_MAXIMUM_SPEED_RAD_S,
                "tracking_abort_rad": RETREAT_TRACKING_ABORT_RAD,
                "automatic_release": False,
                "last_result": retreat,
            },
        }

    def _retreat_along_commanded_path(
        self,
        owner: str,
        *,
        io_step: int,
        held_target: np.ndarray,
        command_history: list[np.ndarray],
    ) -> tuple[dict[str, Any], int, np.ndarray]:
        """Reverse the exact sent path slowly, retaining authority at the end."""
        indices = np.asarray(COMMAND_POLICY_INDICES, dtype=int)
        current = np.asarray(held_target, dtype=float).copy()
        waypoints = [np.asarray(value, dtype=float) for value in reversed(command_history)]
        started = self.monotonic()
        published = 0
        completed = 0
        maximum_tracking_error = 0.0
        error: str | None = None
        next_tick = self.monotonic()
        with self.status_lock:
            self.retreat_active = True
            self.retreat_waypoints_completed = 0
            self.retreat_waypoints_total = len(waypoints)
        self._log_timing(
            "safe_retreat_started",
            command_id=self.command_id,
            waypoints=len(waypoints),
            maximum_speed_rad_s=RETREAT_MAXIMUM_SPEED_RAD_S,
            sdk_weight=1.0,
        )
        try:
            for waypoint in waypoints:
                if self.stop_requested.is_set():
                    error = "operator requested controlled release during retreat"
                    break
                maximum_delta = float(np.max(np.abs(waypoint[indices] - current[indices])))
                samples = max(
                    1,
                    int(
                        np.ceil(
                            1.875
                            * maximum_delta
                            / (RETREAT_MAXIMUM_SPEED_RAD_S * self.tick_s)
                        )
                    ),
                )
                segment_start = current.copy()
                previous_target = current.copy()
                for sample in range(1, samples + 1):
                    if self.stop_requested.is_set():
                        error = "operator requested controlled release during retreat"
                        break
                    blend = self.physical._minimum_jerk(sample / samples)
                    target = segment_start + blend * (waypoint - segment_start)
                    live = self._live_guard_with_timing_retries(expected_remote_sequence=0)
                    tracking_errors = np.abs(
                        self._controlled(live["q_43"])
                        - self._controlled(previous_target)
                    )
                    tracking = float(np.max(tracking_errors))
                    maximum_tracking_error = max(maximum_tracking_error, tracking)
                    if tracking > RETREAT_TRACKING_ABORT_RAD:
                        tracking_slot = int(np.argmax(tracking_errors))
                        joint = int(COMMAND_POLICY_INDICES[tracking_slot])
                        raise InterlockError(
                            "safe retreat tracking error exceeded limit: "
                            f"joint={joint}, error={tracking:.6f} rad, "
                            f"limit={RETREAT_TRACKING_ABORT_RAD:.6f} rad"
                        )
                    self._publish_with_timing_retries(
                        owner,
                        step=io_step,
                        target_q_43=target.tolist(),
                        arm_sdk_weight=1.0,
                    )
                    io_step += 1
                    published += 1
                    previous_target = target
                    current = target
                    next_tick += self.tick_s
                    self._sleep_to_tick(next_tick)
                if error is not None:
                    break
                current = waypoint.copy()
                completed += 1
                with self.status_lock:
                    self.retreat_waypoints_completed = completed
        except Exception as exc:
            error = f"{type(exc).__name__}: {exc}"
            try:
                live = self._live_guard_with_timing_retries(expected_remote_sequence=0)
                current = np.asarray(live["q_43"], dtype=float)
            except Exception:
                pass
        result = {
            "completed": completed == len(waypoints) and error is None,
            "command_id": self.command_id,
            "waypoints_completed": completed,
            "waypoints_total": len(waypoints),
            "published_steps": published,
            "duration_s": max(0.0, self.monotonic() - started),
            "maximum_speed_rad_s": RETREAT_MAXIMUM_SPEED_RAD_S,
            "maximum_tracking_error_rad": maximum_tracking_error,
            "tracking_abort_rad": RETREAT_TRACKING_ABORT_RAD,
            "sdk_weight": 1.0,
            "automatic_release": False,
            "end_behavior": "hold retreat endpoint until explicit controlled release",
            "error": error,
        }
        with self.status_lock:
            self.retreat_active = False
            self.last_retreat = dict(result)
        self._log_timing("safe_retreat_stopped", **result)
        return result, io_step, current

    def _log_timing(self, event: str, **extra: Any) -> None:
        value = {"event": event, "at_unix_ns": self.wall_time_ns(), **extra}
        print("G1_POLICY_TIMING " + json.dumps(value, separators=(",", ":")), flush=True)

    def _controlled_release(
        self,
        owner: str,
        *,
        io_step: int,
        last_target: np.ndarray | None,
        reason: str,
    ) -> dict[str, Any]:
        """Transfer authority at the measured pose without an arm action/return."""
        # UnitreePhysicalIO treats ``requested_stop`` as a clean ownership
        # release. Keep the operator-facing cause in this method's result, but
        # do not leave a completed run or controlled operator stop recorded as
        # an I/O fault. Real policy faults retain their fault reason.
        disarm_reason = (
            "requested_stop"
            if reason in {"policy_completed", "operator_controlled_stop"}
            else reason
        )
        status = self.physical.io.status()
        if status.get("publishers_armed") is not True:
            return {
                "completed": False,
                "reason": reason,
                "detail": "publisher ownership was already released",
                "high_level_stop_called": False,
            }
        anchor_source = "last_target"
        measured: np.ndarray | None = None
        try:
            measured = np.asarray(self.physical.io.snapshot()["q_43"], dtype=float)
            anchor_source = "measured_pose"
        except Exception:
            pass
        if last_target is None and measured is None:
            self.physical.io.disarm(disarm_reason)
            return {
                "completed": False,
                "reason": reason,
                "detail": "no finite pose was available for a release ramp",
                "high_level_stop_called": False,
            }
        start = np.asarray(last_target if last_target is not None else measured, dtype=float)
        end = np.asarray(measured if measured is not None else start, dtype=float)
        # The I/O owner is authoritative because camera-hold and partial-entry
        # commands may advance without advancing the learned-policy counter.
        io_step = int(status.get("last_step", io_step - 1)) + 1
        next_tick = self.monotonic()
        published = 0
        release_error: str | None = None
        release_steps = max(1, round(FAULT_RELEASE_DURATION_S * self.execution_hz))
        for index in range(1, release_steps + 1):
            blend = self.physical._minimum_jerk(index / release_steps)
            target = start + blend * (end - start)
            weight = 1.0 - index / release_steps
            publish_started = self.monotonic()
            try:
                publish_started = self.monotonic()
                self._publish_with_timing_retries(
                    owner,
                    step=io_step,
                    target_q_43=target.tolist(),
                    arm_sdk_weight=weight,
                )
            except Exception as exc:
                release_error = f"{type(exc).__name__}: {exc}"
                break
            self.timing.record_seconds(
                "controlled_release_publish", self.monotonic() - publish_started
            )
            io_step += 1
            published += 1
            next_tick += self.tick_s
            self._sleep_to_tick(next_tick)
        try:
            self.physical.io.disarm(disarm_reason)
        except Exception as exc:
            release_error = release_error or f"{type(exc).__name__}: {exc}"
        result = {
            "completed": published == release_steps and release_error is None,
            "reason": reason,
            "anchor_source": anchor_source,
            "duration_s": FAULT_RELEASE_DURATION_S,
            "planned_steps": release_steps,
            "published_steps": published,
            "high_level_stop_called": False,
            "error": release_error,
        }
        with self.status_lock:
            self.last_release = dict(result)
        self._log_timing("controlled_release", **result)
        return result

    def _hold_after_fault(
        self,
        owner: str,
        *,
        io_step: int,
        last_target: np.ndarray | None,
        command_history: list[np.ndarray],
        error: str,
    ) -> dict[str, Any]:
        """Continuously hold the captured pose at SDK weight one until released.

        The policy request thread remains the sole command owner while held.
        This deliberately blocks a new run and keeps the command watchdog fed;
        an explicit stop request is the only normal transition back to native
        authority.  If live feedback, mode, hardware state, or DDS publishing
        cannot support the hold, ownership is released and the failure is
        reported rather than claiming the pose remains controlled.
        """
        status = self.physical.io.status()
        if status.get("publishers_armed") is not True:
            return {
                "engaged": False,
                "active": False,
                "sdk_weight": None,
                "error": "publisher ownership was already released",
                "release": None,
            }

        try:
            live = self._live_guard_with_timing_retries(expected_remote_sequence=0)
            target = np.asarray(live["q_43"], dtype=float)
            if target.shape != (43,) or not np.isfinite(target).all():
                raise InterlockError("fault hold could not capture a finite 43-joint pose")
        except Exception as exc:
            try:
                self.physical.io.disarm("fault_hold_preflight_failed")
            except Exception:
                pass
            return {
                "engaged": False,
                "active": False,
                "sdk_weight": None,
                "error": f"{type(exc).__name__}: {exc}",
                "release": None,
            }

        io_step = int(status.get("last_step", io_step - 1)) + 1
        started = self.monotonic()
        published = 0
        release: dict[str, Any] | None = None
        hold_error: str | None = None
        with self.status_lock:
            self.holding_after_fault = True
            self.retreat_available = bool(command_history)
            self.retreat_active = False
            self.retreat_waypoints_completed = 0
            self.retreat_waypoints_total = len(command_history)
            self.last_retreat = None
            self.fault_hold_started_at_monotonic = started
            self.fault_hold_publish_count = 0
            self.last_fault_hold = {
                "engaged": True,
                "active": True,
                "sdk_weight": 1.0,
                "captured_pose_source": "measured_pose",
                "trigger": error,
                "release_required": True,
            }
        self._log_timing(
            "fault_hold_started",
            command_id=self.command_id,
            trigger=error,
            sdk_weight=1.0,
        )

        next_tick = self.monotonic()
        try:
            while not self.stop_requested.is_set():
                if self.retreat_requested.is_set():
                    self.retreat_requested.clear()
                    retreat, io_step, target = self._retreat_along_commanded_path(
                        owner,
                        io_step=io_step,
                        held_target=target,
                        command_history=command_history,
                    )
                    with self.status_lock:
                        self.last_retreat = dict(retreat)
                    if self.stop_requested.is_set():
                        break
                self._live_guard_with_timing_retries(expected_remote_sequence=0)
                publish_started = self.monotonic()
                self._publish_with_timing_retries(
                    owner,
                    step=io_step,
                    target_q_43=target.tolist(),
                    arm_sdk_weight=1.0,
                )
                self.timing.record_seconds(
                    "fault_hold_publish", self.monotonic() - publish_started
                )
                io_step += 1
                published += 1
                with self.status_lock:
                    self.fault_hold_publish_count = published
                if published % FAULT_HOLD_LOG_INTERVAL_STEPS == 0:
                    self._log_timing(
                        "fault_hold_progress",
                        command_id=self.command_id,
                        publish_count=published,
                        elapsed_s=max(0.0, self.monotonic() - started),
                        sdk_weight=1.0,
                    )
                next_tick += self.tick_s
                remaining = next_tick - self.monotonic()
                if remaining > 0:
                    self.stop_requested.wait(remaining)
                else:
                    next_tick = self.monotonic()

            release = self._controlled_release(
                owner,
                io_step=io_step,
                last_target=target,
                reason="operator_controlled_stop",
            )
        except Exception as exc:
            hold_error = f"{type(exc).__name__}: {exc}"
            try:
                self.physical.io.disarm(f"fault_hold_failed: {hold_error}")
            except Exception as disarm_exc:
                hold_error += f"; disarm_failed: {type(disarm_exc).__name__}: {disarm_exc}"

        result = {
            "engaged": published > 0,
            "active": False,
            "sdk_weight": 1.0 if published > 0 else None,
            "captured_pose_source": "measured_pose",
            "trigger": error,
            "held_until_explicit_release": release is not None,
            "publish_count": published,
            "duration_s": max(0.0, self.monotonic() - started),
            "release": release,
            "error": hold_error,
        }
        with self.status_lock:
            self.holding_after_fault = False
            self.retreat_available = False
            self.retreat_active = False
            self.last_fault_hold = dict(result)
        self._log_timing("fault_hold_stopped", command_id=self.command_id, **result)
        return result

    @staticmethod
    def _controlled(values: Any) -> np.ndarray:
        return np.asarray(values, dtype=float)[np.asarray(COMMAND_POLICY_INDICES)]

    def _sleep_to_tick(self, next_tick: float) -> None:
        remaining = next_tick - self.monotonic()
        if remaining < -MAXIMUM_SCHEDULER_JITTER_S:
            self.maximum_schedule_lateness_s = max(
                self.maximum_schedule_lateness_s, -remaining
            )
            if not ENFORCE_CONTROL_DEADLINE:
                return
            raise InterlockError(
                f"{self.execution_hz:g} Hz control deadline missed by {-remaining * 1000:.3f} ms"
            )
        if remaining <= 0:
            self.maximum_schedule_lateness_s = max(
                self.maximum_schedule_lateness_s, -remaining
            )
            return
        self.sleeper(remaining)

    def _publish_entry_tick(
        self,
        owner: str,
        io_step: int,
        target: np.ndarray,
        weight: float,
        next_tick: float,
    ) -> tuple[int, float]:
        self._publish_with_timing_retries(
            owner,
            step=io_step,
            target_q_43=target.tolist(),
            arm_sdk_weight=weight,
        )
        next_tick += self.tick_s
        self._sleep_to_tick(next_tick)
        return io_step + 1, next_tick

    def _publish_with_timing_retries(self, owner: str, **command: Any) -> dict[str, Any]:
        for retry in range(MAXIMUM_TIMING_GAP_RETRIES + 1):
            try:
                return self.physical.io.publish_policy_target(owner, **command)
            except InterlockError as exc:
                if not self.physical._timing_gap(exc) or retry == MAXIMUM_TIMING_GAP_RETRIES:
                    raise
                # The rejected attempt sent no command and did not advance the
                # IO sequence. Retry the identical step while preserving the
                # sealed 20 ms freshness/skew limits and watchdog.
                self.sleeper(TIMING_RETRY_DELAY_S)
        raise AssertionError("unreachable timing retry state")

    def _live_guard_with_timing_retries(self, *, expected_remote_sequence: int) -> dict[str, Any]:
        for retry in range(MAXIMUM_TIMING_GAP_RETRIES + 1):
            try:
                return self.physical._live_guard(
                    expected_remote_sequence=expected_remote_sequence
                )
            except InterlockError as exc:
                if not self.physical._timing_gap(exc) or retry == MAXIMUM_TIMING_GAP_RETRIES:
                    raise
                self.sleeper(TIMING_RETRY_DELAY_S)
        raise AssertionError("unreachable timing retry state")

    def _enter_and_hold(self, owner: str) -> dict[str, Any]:
        fsm = self.physical.enter_running_fsm()
        before = self.physical._wait_settled_state()
        remote_sequence = int(before["remote"]["button_sequence"])
        arm = self.physical.io.arm(owner, self.physical.command_profile())
        # arm() waits for publisher discovery and fresh feedback. Re-sample the
        # actual pose after that unbounded setup work so the prime/entry target
        # cannot inherit an older pre-discovery measurement.
        priming_state = self._live_guard_with_timing_retries(
            expected_remote_sequence=remote_sequence
        )
        start = np.asarray(priming_state["q_43"], dtype=float)
        goal = start.copy()
        indices = np.asarray(COMMAND_POLICY_INDICES, dtype=int)
        goal[indices] = self.physical.reference_frame0[indices]
        maximum_delta = float(np.max(np.abs(goal[indices] - start[indices])))
        duration_s = min(
            ENTRY_MAXIMUM_DURATION_S,
            max(
                ENTRY_MINIMUM_DURATION_S,
                1.875 * maximum_delta / ENTRY_MAXIMUM_VELOCITY_RAD_S,
            ),
        )
        sample_count = int(np.ceil(duration_s / self.tick_s))
        io_step = 0
        previous_target = start.copy()
        command_history = [start.copy()]
        # The first DDS Write may pay one-time matching/allocation latency even
        # after publisher Init has returned. Prime both writers with the fresh
        # measured pose before the control epoch: arm_sdk weight zero prevents
        # body takeover, while the right hand is commanded to its measured pose.
        publisher_prime = self._publish_with_timing_retries(
            owner,
            step=io_step,
            target_q_43=start.tolist(),
            arm_sdk_weight=0.0,
        )
        io_step += 1
        # Strict execution scheduling begins with ownership-ramp step 1, after all
        # publisher creation, discovery and first-write work has completed.
        next_tick = self.monotonic()
        entry_ownership_ramp_steps = max(
            1, round(ENTRY_OWNERSHIP_RAMP_DURATION_S * self.execution_hz)
        )
        for index in range(1, entry_ownership_ramp_steps + 1):
            io_step, next_tick = self._publish_entry_tick(
                owner,
                io_step,
                start,
                index / entry_ownership_ramp_steps,
                next_tick,
            )
        maximum_tracking_error = 0.0
        for sample in range(1, sample_count + 1):
            blend = self.physical._minimum_jerk(sample / sample_count)
            target = start.copy()
            target[indices] = start[indices] + blend * (goal[indices] - start[indices])
            maximum_step = float(np.max(np.abs(target[indices] - previous_target[indices])))
            if maximum_step > ENTRY_MAXIMUM_STEP_RAD:
                raise InterlockError(
                    f"entry planner exceeded maximum step: {maximum_step:.6f} rad"
                )
            live = self._live_guard_with_timing_retries(
                expected_remote_sequence=remote_sequence
            )
            tracking_errors = np.abs(
                self._controlled(live["q_43"]) - previous_target[indices]
            )
            tracking_slot = int(np.argmax(tracking_errors))
            tracking = float(tracking_errors[tracking_slot])
            maximum_tracking_error = max(maximum_tracking_error, tracking)
            if tracking > ENTRY_TRACKING_ABORT_RAD:
                tracking_joint = int(COMMAND_POLICY_INDICES[tracking_slot])
                raise InterlockError(
                    "entry tracking error exceeded limit: "
                    f"joint={tracking_joint}, error={tracking:.6f} rad, "
                    f"limit={ENTRY_TRACKING_ABORT_RAD:.6f} rad"
                )
            io_step, next_tick = self._publish_entry_tick(
                owner, io_step, target, 1.0, next_tick
            )
            previous_target = target
            command_history.append(target.copy())

        actuation_goal = goal.copy()
        good_since: float | None = None
        deadline = self.monotonic() + ENTRY_SETTLE_TIMEOUT_S
        while REQUIRE_ENTRY_SETTLE and self.monotonic() < deadline:
            live = self._live_guard_with_timing_retries(
                expected_remote_sequence=remote_sequence
            )
            error = float(np.max(np.abs(self._controlled(live["q_43"]) - goal[indices])))
            settled, _, _ = self.physical._entry_velocity_status(live["dq_43"])
            maximum_tracking_error = max(maximum_tracking_error, error)
            now = self.monotonic()
            if error <= ENTRY_POSITION_LIMIT_RAD and settled:
                good_since = now if good_since is None else good_since
                if now - good_since >= ENTRY_SETTLE_S:
                    break
            else:
                good_since = None
            actuation_goal = self.physical._trim_actuation_target(
                goal,
                actuation_goal,
                np.asarray(live["q_43"], dtype=float),
                indices,
            )
            io_step, next_tick = self._publish_entry_tick(
                owner, io_step, actuation_goal, 1.0, next_tick
            )
            command_history.append(actuation_goal.copy())
        else:
            if not REQUIRE_ENTRY_SETTLE:
                live = self._live_guard_with_timing_retries(
                    expected_remote_sequence=remote_sequence
                )
                errors = np.abs(self._controlled(live["q_43"]) - goal[indices])
                error_slot = int(np.argmax(errors))
                error = float(errors[error_slot])
                maximum_tracking_error = max(maximum_tracking_error, error)
                if error > ENTRY_TRACKING_ABORT_RAD:
                    error_joint = int(COMMAND_POLICY_INDICES[error_slot])
                    raise InterlockError(
                        "entry tracking error exceeded limit: "
                        f"joint={error_joint}, error={error:.6f} rad, "
                        f"limit={ENTRY_TRACKING_ABORT_RAD:.6f} rad"
                    )
            else:
                raise InterlockError("reference frame 0 did not attain a settled dwell")

        return {
            "fsm": fsm,
            "arm": arm,
            "publisher_prime": publisher_prime,
            "goal": goal,
            "actuation_goal": actuation_goal,
            "remote_sequence": remote_sequence,
            "io_step": io_step,
            "next_tick": next_tick,
            "maximum_start_delta_rad": maximum_delta,
            "planned_duration_s": duration_s,
            "trajectory_samples": sample_count,
            "settled_dwell_required": REQUIRE_ENTRY_SETTLE,
            "maximum_tracking_error_rad": maximum_tracking_error,
            "command_history": command_history,
        }

    def _policy_proposal(self, live: dict[str, Any], control_at_ns: int) -> dict[str, Any]:
        sampled_at_ns = int(
            live.get("sampled_at_unix_ns", live["received_at_unix_ns"])
        )
        return self.episode.propose(
            OrderedJointState(
                self.episode.joint_names,
                live["q_43"],
                live["dq_43"],
                sampled_at_ns,
            ),
            control_at_ns=control_at_ns,
        )

    def run(
        self,
        command_id: str,
        *,
        maximum_policy_steps: int,
        expected_checkpoint_sha256: str | None = None,
        execution_hz: float | None = None,
    ) -> dict[str, Any]:
        selected_execution_hz = (
            DEFAULT_EXECUTION_HZ if execution_hz is None else float(execution_hz)
        )
        if selected_execution_hz not in ALLOWED_EXECUTION_HZ:
            raise ValueError("execution_hz must be 15, 25, 30, or 40")
        contract = self.refresh_contract()
        active_policy = contract.get("active_policy") or getattr(
            self.episode, "active_policy", None
        )
        active_checkpoint = (
            active_policy.get("checkpoint_sha256")
            if isinstance(active_policy, dict)
            else None
        )
        if (
            expected_checkpoint_sha256 is not None
            and active_checkpoint != expected_checkpoint_sha256
        ):
            raise InterlockError(
                "active checkpoint changed after Play-policy preflight"
            )
        if not 1 <= maximum_policy_steps <= min(
            MAXIMUM_POLICY_STEPS, len(self.episode.reference.references)
        ):
            raise ValueError("maximum_policy_steps is outside the sealed reference")
        if not self.physical.motion_enabled:
            raise InterlockError("physical motion is disabled")
        if command_id in self.physical.command_results:
            return {**self.physical.command_results[command_id], "duplicate_request": True}
        if not self.physical.command_lock.acquire(blocking=False):
            raise InterlockError("another physical command is active")

        self.execution_hz = selected_execution_hz
        self.tick_s = 1.0 / selected_execution_hz

        owner = f"u2525-integrated-policy-{command_id}"
        armed = False
        policy_steps = 0
        started_ns = self.wall_time_ns()
        entry: dict[str, Any] | None = None
        last_target: np.ndarray | None = None
        io_step = 0
        deadline_misses = 0
        proposal_trace: list[dict[str, Any]] = []
        command_history: list[np.ndarray] = []
        alignment_trace_path: Path | None = None
        alignment_trace_file: Any | None = None
        self.maximum_schedule_lateness_s = 0.0
        self.timing.reset()
        self.stop_requested.clear()
        self.retreat_requested.clear()
        with self.status_lock:
            self.running = True
            self.command_id = command_id
            self.policy_steps_completed = 0
            self.policy_started_at_monotonic = None
            self.policy_finished_at_monotonic = None
            self.last_error = None
            self.last_release = None
            self.holding_after_fault = False
            self.fault_hold_started_at_monotonic = None
            self.fault_hold_publish_count = 0
            self.last_fault_hold = None
            self.retreat_active = False
            self.retreat_available = False
            self.retreat_waypoints_completed = 0
            self.retreat_waypoints_total = 0
            self.last_retreat = None
        self._log_timing(
            "run_started", command_id=command_id, control_hz=self.execution_hz
        )
        try:
            RUN_LOG_DIR.mkdir(parents=True, exist_ok=True)
            alignment_trace_path = RUN_LOG_DIR / f"{command_id}.can-alignment.jsonl"
            alignment_trace_file = alignment_trace_path.open("x", encoding="utf-8")
            alignment_trace_file.write(
                json.dumps(
                    {
                        "schema": "wendy.g1.can-alignment-run-start.v1",
                        "command_id": command_id,
                        "started_at_unix_ns": started_ns,
                        "checkpoint_sha256": active_checkpoint,
                        "distance_definition": (
                            "planar Euclidean distance between detected and nominal can "
                            "after expressing both in the same live camera frame and rotating "
                            "their relative vector into policy/world XY"
                        ),
                        "units": {"distance": "m", "display": "cm"},
                    },
                    separators=(",", ":"),
                    allow_nan=False,
                )
                + "\n"
            )
            alignment_trace_file.flush()
            # Verify the model/mask source before creating DDS publishers.
            self.camera.preflight()
            entry = self._enter_and_hold(owner)
            armed = True
            command_history = [
                np.asarray(value, dtype=float).copy()
                for value in entry.get("command_history", [])
            ]
            last_desired = np.asarray(entry["goal"], dtype=float)
            last_target = np.asarray(entry["actuation_goal"], dtype=float)
            static_compensation = last_target - last_desired
            io_step = int(entry["io_step"])
            next_tick = float(entry["next_tick"])

            episode_start_ns = self.wall_time_ns()
            reset = self.episode.reset_episode(started_at_ns=episode_start_ns)
            policy_started = self.monotonic()
            with self.status_lock:
                self.policy_started_at_monotonic = policy_started
            self.camera.activate()
            camera_deadline = self.monotonic() + CAMERA_ADMISSION_TIMEOUT_S
            while policy_steps < maximum_policy_steps:
                if self.stop_requested.is_set():
                    raise PolicyStopRequested("operator requested controlled policy stop")
                tick_started = self.monotonic()
                guard_started = self.monotonic()
                live = self._live_guard_with_timing_retries(
                    expected_remote_sequence=int(entry["remote_sequence"])
                )
                guard_ms = self.timing.record_seconds(
                    "live_guard", self.monotonic() - guard_started
                )
                tracking_errors = np.abs(
                    self._controlled(live["q_43"])
                    - self._controlled(last_desired)
                )
                tracking_slot = int(np.argmax(tracking_errors))
                tracking = float(tracking_errors[tracking_slot])
                if tracking > MAXIMUM_POLICY_TRACKING_ERROR_RAD:
                    tracking_joint = int(COMMAND_POLICY_INDICES[tracking_slot])
                    raise InterlockError(
                        "policy tracking error exceeded limit: "
                        f"joint={tracking_joint}, error={tracking:.6f} rad, "
                        f"limit={MAXIMUM_POLICY_TRACKING_ERROR_RAD:.6f} rad"
                    )
                try:
                    proposal_started = self.monotonic()
                    proposal_control_ns = self.wall_time_ns()
                    proposal = self._policy_proposal(live, proposal_control_ns)
                except VisionUnavailable:
                    if self.monotonic() >= camera_deadline:
                        raise InterlockError("no post-reset camera embedding before timeout")
                    publish_started = self.monotonic()
                    self._publish_with_timing_retries(
                        owner,
                        step=io_step,
                        target_q_43=last_target.tolist(),
                        arm_sdk_weight=1.0,
                    )
                    self.timing.record_seconds(
                        "dds_publish", self.monotonic() - publish_started
                    )
                    io_step += 1
                    next_tick += self.tick_s
                    sleep_started = self.monotonic()
                    self._sleep_to_tick(next_tick)
                    self.timing.record_seconds(
                        "scheduler_sleep", self.monotonic() - sleep_started
                    )
                    self.timing.record_seconds(
                        "policy_tick", self.monotonic() - tick_started
                    )
                    continue
                # This is a rolling admission deadline, not a startup-only
                # absolute deadline. A single transient miss after hundreds
                # of valid embeddings must hold the last measured target; it
                # must not be mislabeled as a missing post-reset embedding.
                camera_deadline = self.monotonic() + CAMERA_ADMISSION_TIMEOUT_S
                proposal_ms = self.timing.record_seconds(
                    "policy_proposal", self.monotonic() - proposal_started
                )
                self.timing.record_ms(
                    "model_control_compute", proposal.get("control_compute_ms", 0.0)
                )
                self.timing.record_ms(
                    "vision_encode", proposal.get("vision_encode_ms", 0.0)
                )

                if proposal.get("reference_frame") != policy_steps or proposal.get("step") != policy_steps:
                    raise InterlockError("policy/reference step synchronization changed")
                desired_target = np.asarray(proposal.get("target_q_43"), dtype=float)
                if desired_target.shape != (43,) or not np.isfinite(desired_target).all():
                    raise InterlockError("policy returned an invalid 43-joint target")
                jump = float(
                    np.max(
                        np.abs(
                            self._controlled(desired_target)
                            - self._controlled(last_desired)
                        )
                    )
                )
                if jump > MAXIMUM_POLICY_STEP_RAD:
                    raise InterlockError(
                        f"policy target step exceeded limit: {jump:.6f} rad"
                    )
                target = desired_target.copy()
                indices = np.asarray(COMMAND_POLICY_INDICES, dtype=int)
                remote_reference = proposal.get("reference_target_q_43")
                if remote_reference is not None:
                    reference_target = np.asarray(remote_reference, dtype=float)
                elif self.reference_targets_43 is not None:
                    reference_target = self.reference_targets_43[policy_steps]
                else:
                    raise RuntimeError("policy proposal omitted its sealed reference target")
                if reference_target.shape != (43,) or not np.isfinite(reference_target).all():
                    raise InterlockError("policy returned an invalid reference target")
                alignment_sample = _can_alignment_sample(
                    command_id,
                    policy_steps,
                    proposal_control_ns,
                    proposal,
                )
                alignment_trace_file.write(
                    json.dumps(
                        alignment_sample,
                        separators=(",", ":"),
                        allow_nan=False,
                    )
                    + "\n"
                )
                alignment_trace_file.flush()
                proposal_trace.append(
                    {
                        "step": policy_steps,
                        "frame_id": proposal.get("frame_id"),
                        "stream_id": proposal.get("stream_id"),
                        "camera_age_ms": proposal.get("camera_age_ms"),
                        "raw_action_mean": proposal.get("raw_action_mean"),
                        "can_alignment": alignment_sample,
                        "owned_target_delta_from_reference_rad": (
                            desired_target[indices] - reference_target[indices]
                        ).tolist(),
                    }
                )
                target[indices] += static_compensation[indices]
                for index in indices:
                    target[index] = np.clip(
                        target[index],
                        COMMAND_LOWER_BY_INDEX[int(index)],
                        COMMAND_UPPER_BY_INDEX[int(index)],
                    )
                publish_started = self.monotonic()
                self._publish_with_timing_retries(
                    owner,
                    step=io_step,
                    target_q_43=target.tolist(),
                    arm_sdk_weight=1.0,
                )
                publish_ms = self.timing.record_seconds(
                    "dds_publish", self.monotonic() - publish_started
                )
                io_step += 1
                policy_steps += 1
                command_history.append(target.copy())
                with self.status_lock:
                    self.policy_steps_completed = policy_steps
                last_target = target
                last_desired = desired_target
                next_tick += self.tick_s
                if self.monotonic() - tick_started > self.tick_s:
                    deadline_misses += 1
                    if ENFORCE_CONTROL_DEADLINE:
                        raise InterlockError(
                            f"{self.execution_hz:g} Hz policy tick exceeded "
                            f"{self.tick_s * 1000:g} ms"
                        )
                sleep_started = self.monotonic()
                self._sleep_to_tick(next_tick)
                sleep_ms = self.timing.record_seconds(
                    "scheduler_sleep", self.monotonic() - sleep_started
                )
                tick_ms = self.timing.record_seconds(
                    "policy_tick", self.monotonic() - tick_started
                )
                if policy_steps % TIMING_LOG_INTERVAL_STEPS == 0:
                    elapsed = max(self.monotonic() - policy_started, 1e-9)
                    self._log_timing(
                        "policy_progress",
                        command_id=command_id,
                        policy_step=policy_steps,
                        effective_policy_hz=policy_steps / elapsed,
                        latest_ms={
                            "live_guard": guard_ms,
                            "policy_proposal": proposal_ms,
                            "dds_publish": publish_ms,
                            "scheduler_sleep": sleep_ms,
                            "policy_tick": tick_ms,
                        },
                        buckets=self.timing.snapshot(),
                    )

            with self.status_lock:
                self.policy_finished_at_monotonic = self.monotonic()
            self.camera.deactivate()
            release = self._controlled_release(
                owner,
                io_step=io_step,
                last_target=last_target,
                reason="policy_completed",
            )
            armed = False
            fsm_after = self.physical._rpc("read-fsm")
            if (
                fsm_after.get("fsm_id") != RUNNING_FSM
                or fsm_after.get("fsm_mode") not in READY_FSM_MODES
            ):
                raise InterlockError(f"FSM changed during policy run: {fsm_after}")
            result = {
                "schema": "wendy.g1.reference-residual-physical-run.v1",
                "accepted": True,
                "command_id": command_id,
                "checkpoint_sha256": proposal.get("checkpoint_sha256", CHECKPOINT_SHA256),
                "candidate_id": proposal.get("candidate_id"),
                "checkpoint_update": proposal.get("checkpoint_update"),
                "started_at_unix_ns": started_ns,
                "finished_at_unix_ns": self.wall_time_ns(),
                "control_hz": self.execution_hz,
                "policy_contract_hz": POLICY_CONTRACT_HZ,
                "policy_steps_completed": policy_steps,
                "deadline_misses": deadline_misses,
                "maximum_scheduler_lateness_ms": self.maximum_schedule_lateness_s * 1000,
                "control_deadline_enforced": ENFORCE_CONTROL_DEADLINE,
                "same_owner_entry_through_step0": policy_steps > 0,
                "post_release_fsm": fsm_after,
                "entry": {
                    key: value
                    for key, value in entry.items()
                    if key not in {"goal", "actuation_goal", "next_tick", "command_history"}
                },
                "episode_reset": reset,
                "last_proposal": proposal,
                "proposal_trace": proposal_trace,
                "can_alignment_trace": {
                    "path": str(alignment_trace_path),
                    "sample_count": len(proposal_trace),
                    "download_path": (
                        "/api/reference-residual/can-alignment?command_id=" + command_id
                    ),
                },
                "release": release,
                "io": self.physical.io.status(),
                "meaning": "bounded harnessed physical policy execution; not policy qualification or task success",
            }
            with self.status_lock:
                self.running = False
            status = self.status()
            result["timing"] = {
                "effective_policy_hz": status["effective_policy_hz"],
                "buckets": status["timing"],
            }
            result["runtime_status"] = status
            alignment_trace_file.write(
                json.dumps(
                    {
                        "schema": "wendy.g1.can-alignment-run-finish.v1",
                        "command_id": command_id,
                        "finished_at_unix_ns": result["finished_at_unix_ns"],
                        "accepted": True,
                        "policy_steps_completed": policy_steps,
                    },
                    separators=(",", ":"),
                    allow_nan=False,
                )
                + "\n"
            )
            alignment_trace_file.flush()
            self.physical.command_results[command_id] = result
            return result
        except Exception as exc:
            error = f"{type(exc).__name__}: {exc}"
            if alignment_trace_file is not None:
                alignment_trace_file.write(
                    json.dumps(
                        {
                            "schema": "wendy.g1.can-alignment-run-finish.v1",
                            "command_id": command_id,
                            "finished_at_unix_ns": self.wall_time_ns(),
                            "accepted": False,
                            "policy_steps_completed": policy_steps,
                            "error": error,
                        },
                        separators=(",", ":"),
                        allow_nan=False,
                    )
                    + "\n"
                )
                alignment_trace_file.flush()
            release = None
            fault_hold = None
            with self.status_lock:
                if self.policy_started_at_monotonic is not None:
                    self.policy_finished_at_monotonic = self.monotonic()
                self.running = False
                self.last_error = error
            self.camera.deactivate()
            if armed or self.physical.io.status().get("publishers_armed"):
                try:
                    if isinstance(exc, PolicyStopRequested):
                        release = self._controlled_release(
                            owner,
                            io_step=io_step,
                            last_target=last_target,
                            reason="operator_controlled_stop",
                        )
                    else:
                        fault_hold = self._hold_after_fault(
                            owner,
                            io_step=io_step,
                            last_target=last_target,
                            command_history=command_history,
                            error=error,
                        )
                        release = fault_hold.get("release")
                except Exception as release_exc:
                    release = {
                        "completed": False,
                        "high_level_stop_called": False,
                        "error": f"{type(release_exc).__name__}: {release_exc}",
                    }
            with self.status_lock:
                self.last_error = error
                self.last_release = release
                if fault_hold is not None:
                    self.last_fault_hold = dict(fault_hold)
            self._log_timing(
                "run_stopped",
                command_id=command_id,
                policy_steps_completed=policy_steps,
                error=error,
                release=release,
                fault_hold=fault_hold,
                buckets=self.timing.snapshot(),
            )
            raise
        finally:
            if alignment_trace_file is not None:
                alignment_trace_file.close()
            self.camera.deactivate()
            with self.status_lock:
                self.running = False
            self.physical.command_lock.release()

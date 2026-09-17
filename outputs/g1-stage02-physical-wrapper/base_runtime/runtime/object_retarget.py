"""Camera-relative, motion-zero object retargeting for the selected Stage 2 policy.

The simulator-selected controller is the untouched Stage 2 checkpoint plus a
deterministic per-frame joint-space retarget gain.  The only live input here is
the Coke displacement measured from the synchronized RGB-D mask.  Nominal and
detected can positions are compared in the *same current camera frame* before
the relative vector is rotated into policy/world XY.

This module has no Unitree, DDS, or publisher imports.  It only changes policy
proposals returned by the already motion-zero inference service.
"""
from __future__ import annotations

from dataclasses import dataclass
import hashlib
import json
import math
from pathlib import Path
import threading
from typing import Any

import numpy as np


EXPECTED_GAINS_SHA256 = "07f4dd464045dbf02fe9ab54a47a947b139682e1f84547eaafe28e89e605a57e"
EXPECTED_WEIGHTS_SHA256 = "e3213b0a9c54201e6cf1f06ad18d0b74300550b51161b0c130a58d6ddb7435e4"
EXPECTED_CAMERA_REFERENCE_SHA256 = "9933529153856d924ffa4c14ca0e4375a364a51529ecf9a369e951f3aedecf08"
EXPECTED_HANDOFF_SHA256 = "efc3962d27886377a195ae2fc34f18f5b02922e5842701b347b2c5322508f881"
EXPECTED_CHECKPOINT_SHA256 = "3aaf0c8279e9190a6d47740f6dfdd427ff4fb495ab7e1ae11334e1e1d3573738"

POLICY_WIDTH = 320
POLICY_HEIGHT = 240
MAXIMUM_OFFSET_M = 0.04
MAXIMUM_DETECTION_AGE_S = 0.35
MINIMUM_MASK_PIXELS = 20
MINIMUM_VALID_DEPTH_FRACTION = 0.50
MEASUREMENT_EMA = 0.35
MAXIMUM_ADDED_JOINT_STEP_RAD = 0.005
CONTROL_DT_S = 1.0 / 40.0
CAN_RADIUS_M = 0.0315

# Frozen robot_rgbd mounting from the selected MuJoCo model.  It is a sim-seeded
# extrinsic, not a physical hand-eye qualification; that limitation is exposed
# in every public status response.
PELVIS_WORLD_POS = np.asarray([0.0, 0.0, 0.793], dtype=np.float64)
WAIST_ROLL_LINK_POS = np.asarray([-0.0039635, 0.0, 0.044], dtype=np.float64)
CAMERA_LOCAL_POS = np.asarray([0.08, 0.0, 0.47], dtype=np.float64)
CAMERA_LOCAL_QUAT_WXYZ = np.asarray(
    [0.62721137512625, 0.3265055756219769, -0.3265055756219769, -0.62721137512625],
    dtype=np.float64,
)
OPTICAL_FROM_MUJOCO = np.diag([1.0, -1.0, -1.0])


def _sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        for chunk in iter(lambda: source.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _axis_rotation(axis: str, angle: float) -> np.ndarray:
    c, s = math.cos(angle), math.sin(angle)
    if axis == "x":
        return np.asarray([[1, 0, 0], [0, c, -s], [0, s, c]], dtype=np.float64)
    if axis == "y":
        return np.asarray([[c, 0, s], [0, 1, 0], [-s, 0, c]], dtype=np.float64)
    if axis == "z":
        return np.asarray([[c, -s, 0], [s, c, 0], [0, 0, 1]], dtype=np.float64)
    raise ValueError("unknown rotation axis")


def _quat_matrix(value: np.ndarray) -> np.ndarray:
    w, x, y, z = np.asarray(value, dtype=np.float64)
    return np.asarray(
        [
            [1 - 2 * (y * y + z * z), 2 * (x * y - z * w), 2 * (x * z + y * w)],
            [2 * (x * y + z * w), 1 - 2 * (x * x + z * z), 2 * (y * z - x * w)],
            [2 * (x * z - y * w), 2 * (y * z + x * w), 1 - 2 * (x * x + y * y)],
        ],
        dtype=np.float64,
    )


def camera_pose_from_live_joints(q_43: np.ndarray) -> tuple[np.ndarray, np.ndarray]:
    """Return camera position and world-from-optical rotation from live waist q."""
    q = np.asarray(q_43, dtype=np.float64)
    if q.shape != (43,) or not np.isfinite(q).all():
        raise ValueError("live q must contain 43 finite values")
    yaw = _axis_rotation("z", float(q[12]))
    roll = _axis_rotation("x", float(q[13]))
    pitch = _axis_rotation("y", float(q[14]))
    torso_rotation = yaw @ roll @ pitch
    torso_position = PELVIS_WORLD_POS + yaw @ WAIST_ROLL_LINK_POS
    camera_position = torso_position + torso_rotation @ CAMERA_LOCAL_POS
    world_from_mujoco_camera = torso_rotation @ _quat_matrix(CAMERA_LOCAL_QUAT_WXYZ)
    world_from_optical = world_from_mujoco_camera @ OPTICAL_FROM_MUJOCO
    return camera_position, world_from_optical


@dataclass(frozen=True)
class CanMeasurement:
    frame_id: int
    stream_id: str
    captured_at_ns: int
    detected_optical_xyz: np.ndarray
    detected_pixel_xy: np.ndarray
    mask_pixels: int
    valid_depth_fraction: float
    intrinsics: np.ndarray


class ObjectRelativeRetargeter:
    """Latch a pre-grasp visual offset and apply the selected gain-1 reference."""

    motion_capability = False

    def __init__(self, bundle: Path) -> None:
        root = bundle / "retarget"
        paths = {
            "gains": root / "retarget-gains-f32.npy",
            "weights": root / "retarget-shift-weights-f32.npy",
            "camera": root / "nominal-can-camera-reference.npz",
            "handoff": root / "handoff-manifest.json",
        }
        expected = {
            "gains": EXPECTED_GAINS_SHA256,
            "weights": EXPECTED_WEIGHTS_SHA256,
            "camera": EXPECTED_CAMERA_REFERENCE_SHA256,
            "handoff": EXPECTED_HANDOFF_SHA256,
        }
        for key, path in paths.items():
            if _sha256(path) != expected[key]:
                raise ValueError(f"sealed object-retarget {key} hash changed")
        manifest = json.loads(paths["handoff"].read_text())
        if (
            manifest.get("schema") != "g1.object-retarget.motion-zero-handoff.v1"
            or manifest.get("physical_robot_authority") is not False
            or manifest.get("selected_checkpoint", {}).get("sha256") != EXPECTED_CHECKPOINT_SHA256
        ):
            raise ValueError("object-retarget handoff identity changed")

        self.gains = np.load(paths["gains"], allow_pickle=False).astype(np.float64)
        self.shift_weights = np.load(paths["weights"], allow_pickle=False).astype(np.float64)
        with np.load(paths["camera"], allow_pickle=False) as values:
            self.nominal_can_world = np.asarray(values["nominal_can_world"], dtype=np.float64)
            self.reference_q = np.asarray(values["reference_q"], dtype=np.float64)
            self.joint_names = tuple(str(value) for value in values["joint_names"].tolist())
        if (
            self.gains.shape != (1200, 43, 2)
            or self.shift_weights.shape != (1200,)
            or self.nominal_can_world.shape != (1200, 3)
            or self.reference_q.shape != (1200, 43)
            or len(self.joint_names) != 43
            or not all(np.isfinite(value).all() for value in (self.gains, self.shift_weights, self.nominal_can_world, self.reference_q))
        ):
            raise ValueError("object-retarget array contract changed")
        self.nominal_initial_world = self.nominal_can_world[0].copy()
        moved = np.linalg.norm(self.nominal_can_world[:, :2] - self.nominal_initial_world[:2], axis=1)
        moving_frames = np.flatnonzero(moved > 0.001)
        self.measurement_close_frame = int(moving_frames[0]) if len(moving_frames) else len(self.gains)
        self.lock = threading.RLock()
        self.reset_episode()

    def reset_episode(self) -> None:
        with getattr(self, "lock", threading.RLock()):
            self.measurement: CanMeasurement | None = None
            self.filtered_offset_xy: np.ndarray | None = None
            self.raw_offset_xy: np.ndarray | None = None
            self.clipped_offset_xy: np.ndarray | None = None
            self.pixel_displacement: float | None = None
            self.applied_correction = np.zeros(43, dtype=np.float64)
            self.previous_correction = np.zeros(43, dtype=np.float64)
            self.last_consumed_identity: tuple[str, int] | None = None
            self.latched = False
            self.current_detection_valid = False
            self.latest_intrinsics: np.ndarray | None = None
            self.latest_status: dict[str, Any] = {
                "available": False,
                "reason": "waiting_for_visible_can",
                "calibration_source": "sim-seeded robot_rgbd extrinsics with live waist FK",
                "physical_camera_calibration_qualified": False,
            }

    @staticmethod
    def _scaled_intrinsics(metadata: dict[str, Any]) -> np.ndarray:
        source = metadata.get("source_intrinsics") or {}
        width = float(metadata.get("source_width") or source.get("width") or 0)
        height = float(metadata.get("source_height") or source.get("height") or 0)
        values = [source.get(key) for key in ("fx", "fy", "ppx", "ppy")]
        if width <= 0 or height <= 0 or any(not isinstance(value, (int, float)) for value in values):
            raise ValueError("live RGB-D intrinsics are unavailable")
        fx, fy, ppx, ppy = (float(value) for value in values)
        result = np.asarray(
            [[fx * POLICY_WIDTH / width, 0, ppx * POLICY_WIDTH / width],
             [0, fy * POLICY_HEIGHT / height, ppy * POLICY_HEIGHT / height],
             [0, 0, 1]],
            dtype=np.float64,
        )
        if not np.isfinite(result).all() or result[0, 0] <= 0 or result[1, 1] <= 0:
            raise ValueError("live RGB-D intrinsics are invalid")
        return result

    def observe(
        self,
        *,
        depth_m: np.ndarray,
        mask: np.ndarray,
        metadata: dict[str, Any],
        detection_valid: bool,
    ) -> None:
        """Extract one robust can center in the live RGB-D optical frame."""
        try:
            intrinsics = self._scaled_intrinsics(metadata)
        except ValueError:
            intrinsics = None
        with self.lock:
            self.latest_intrinsics = intrinsics
        if not detection_valid:
            with self.lock:
                self.current_detection_valid = False
                self.latest_status = {**self.latest_status, "detection_valid": False}
            return
        depth = np.asarray(depth_m, dtype=np.float64)
        visible = np.asarray(mask).astype(bool)
        pixels = int(visible.sum())
        if depth.shape != (POLICY_HEIGHT, POLICY_WIDTH) or visible.shape != depth.shape or pixels < MINIMUM_MASK_PIXELS:
            return
        valid = visible & np.isfinite(depth) & (depth > 0.10) & (depth < 3.0)
        valid_fraction = float(valid.sum() / pixels)
        if valid_fraction < MINIMUM_VALID_DEPTH_FRACTION:
            return
        ys, xs = np.nonzero(valid)
        u = float(np.median(xs))
        v = float(np.median(ys))
        surface_z = float(np.median(depth[valid]))
        center_z = surface_z + CAN_RADIUS_M
        if intrinsics is None:
            return
        optical = np.asarray(
            [
                (u - intrinsics[0, 2]) * center_z / intrinsics[0, 0],
                (v - intrinsics[1, 2]) * center_z / intrinsics[1, 1],
                center_z,
            ],
            dtype=np.float64,
        )
        measurement = CanMeasurement(
            frame_id=int(metadata["frame_id"]),
            stream_id=str(metadata["stream_id"]),
            captured_at_ns=int(metadata["captured_at_unix_ns"]),
            detected_optical_xyz=optical,
            detected_pixel_xy=np.asarray([u, v], dtype=np.float64),
            mask_pixels=pixels,
            valid_depth_fraction=valid_fraction,
            intrinsics=intrinsics,
        )
        with self.lock:
            self.measurement = measurement
            self.current_detection_valid = True

    @staticmethod
    def _project(point: np.ndarray, intrinsics: np.ndarray) -> list[float] | None:
        if point[2] <= 0:
            return None
        return [
            float(intrinsics[0, 0] * point[0] / point[2] + intrinsics[0, 2]),
            float(intrinsics[1, 1] * point[1] / point[2] + intrinsics[1, 2]),
        ]

    def apply(
        self,
        *,
        frame: int,
        reference_q_43: np.ndarray,
        reference_velocity_15: np.ndarray,
        live_q_43: np.ndarray,
        control_at_ns: int,
    ) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
        if not 0 <= frame < len(self.gains):
            raise ValueError("retarget frame is outside the sealed reference")
        base = np.asarray(reference_q_43, dtype=np.float64)
        velocity = np.asarray(reference_velocity_15, dtype=np.float64)
        live_q = np.asarray(live_q_43, dtype=np.float64)
        camera_position, world_from_optical = camera_pose_from_live_joints(live_q)
        nominal_optical = world_from_optical.T @ (self.nominal_initial_world - camera_position)

        with self.lock:
            measurement = self.measurement
            reason = "waiting_for_visible_can"
            if frame >= self.measurement_close_frame:
                self.latched = True
            if measurement is not None and self.current_detection_valid:
                identity = (measurement.stream_id, measurement.frame_id)
                age_s = (control_at_ns - measurement.captured_at_ns) / 1_000_000_000
                if age_s < 0 or age_s > MAXIMUM_DETECTION_AGE_S:
                    reason = "visible_can_stale"
                elif self.latched:
                    reason = "pregrasp_offset_latched"
                elif identity != self.last_consumed_identity:
                    # Translation cancels: both points are expressed in this
                    # same current camera frame before rotating the relative
                    # vector into policy/world XY.
                    delta_optical = measurement.detected_optical_xyz - nominal_optical
                    offset_xy = (world_from_optical @ delta_optical)[:2]
                    clipped = np.clip(offset_xy, -MAXIMUM_OFFSET_M, MAXIMUM_OFFSET_M)
                    self.raw_offset_xy = offset_xy.copy()
                    self.clipped_offset_xy = clipped.copy()
                    ideal_pixel_for_distance = self._project(nominal_optical, measurement.intrinsics)
                    self.pixel_displacement = (
                        None
                        if ideal_pixel_for_distance is None
                        else float(
                            np.linalg.norm(
                                measurement.detected_pixel_xy
                                - np.asarray(ideal_pixel_for_distance, dtype=np.float64)
                            )
                        )
                    )
                    if self.filtered_offset_xy is None:
                        self.filtered_offset_xy = clipped
                    else:
                        self.filtered_offset_xy += MEASUREMENT_EMA * (clipped - self.filtered_offset_xy)
                    self.last_consumed_identity = identity
                    reason = "visible_can_offset_updated"
                else:
                    reason = "visible_can_offset_current"

            offset = np.zeros(2, dtype=np.float64) if self.filtered_offset_xy is None else self.filtered_offset_xy
            desired_correction = self.gains[frame] @ offset
            step = np.clip(
                desired_correction - self.applied_correction,
                -MAXIMUM_ADDED_JOINT_STEP_RAD,
                MAXIMUM_ADDED_JOINT_STEP_RAD,
            )
            self.previous_correction = self.applied_correction.copy()
            self.applied_correction += step
            retargeted = base + self.applied_correction
            owned = np.asarray([12, *range(29, 43)], dtype=np.int64)
            retargeted_velocity = velocity + (
                self.applied_correction[owned] - self.previous_correction[owned]
            ) / CONTROL_DT_S

            ideal_pixel = None
            detected_pixel = None
            age_ms = None
            if measurement is not None:
                ideal_pixel = self._project(nominal_optical, measurement.intrinsics)
                detected_pixel = measurement.detected_pixel_xy.tolist()
                age_ms = (control_at_ns - measurement.captured_at_ns) / 1e6
            self.latest_status = {
                "schema": "wendy.g1.object-relative-retarget-status.v1",
                "available": self.filtered_offset_xy is not None,
                "reason": reason,
                "reference_frame": frame,
                "measurement_close_frame": self.measurement_close_frame,
                "latched": self.latched,
                "offset_world_xy_m": offset.tolist(),
                "raw_offset_world_xy_m": (
                    None if self.raw_offset_xy is None else self.raw_offset_xy.tolist()
                ),
                "clipped_offset_world_xy_m": (
                    None if self.clipped_offset_xy is None else self.clipped_offset_xy.tolist()
                ),
                "distance_to_optimal_m": (
                    None
                    if self.raw_offset_xy is None
                    else float(np.linalg.norm(self.raw_offset_xy))
                ),
                "distance_to_optimal_cm": (
                    None
                    if self.raw_offset_xy is None
                    else float(np.linalg.norm(self.raw_offset_xy) * 100.0)
                ),
                "filtered_distance_to_optimal_m": (
                    None
                    if self.filtered_offset_xy is None
                    else float(np.linalg.norm(self.filtered_offset_xy))
                ),
                "pixel_displacement_px": self.pixel_displacement,
                "offset_clip_per_axis_m": MAXIMUM_OFFSET_M,
                "maximum_added_joint_step_rad": MAXIMUM_ADDED_JOINT_STEP_RAD,
                "maximum_applied_correction_rad": float(np.max(np.abs(self.applied_correction))),
                "ideal_pixel_xy": ideal_pixel,
                "detected_pixel_xy": detected_pixel,
                "nominal_can_optical_xyz_m": nominal_optical.tolist(),
                "detected_can_optical_xyz_m": (
                    None if measurement is None else measurement.detected_optical_xyz.tolist()
                ),
                "detection_age_ms": age_ms,
                "measurement_frame_id": None if measurement is None else measurement.frame_id,
                "measurement_stream_id": None if measurement is None else measurement.stream_id,
                "measurement_captured_at_unix_ns": (
                    None if measurement is None else measurement.captured_at_ns
                ),
                "mask_pixels": None if measurement is None else measurement.mask_pixels,
                "valid_depth_fraction": (
                    None if measurement is None else measurement.valid_depth_fraction
                ),
                "gain": 1.0,
                "checkpoint_sha256": EXPECTED_CHECKPOINT_SHA256,
                "calibration_source": "sim-seeded robot_rgbd extrinsics with live waist FK",
                "physical_camera_calibration_qualified": False,
                "motion_capability": False,
            }
            return retargeted.astype(np.float32), retargeted_velocity.astype(np.float32), dict(self.latest_status)

    def project(self, *, yaw: float, roll: float, pitch: float, observed_at_ns: int) -> dict[str, Any]:
        """Project the nominal can into the current live camera for the UI only."""
        q = np.zeros(43, dtype=np.float64)
        q[12:15] = [yaw, roll, pitch]
        camera_position, world_from_optical = camera_pose_from_live_joints(q)
        nominal_optical = world_from_optical.T @ (self.nominal_initial_world - camera_position)
        with self.lock:
            measurement = self.measurement
            valid = self.current_detection_valid and measurement is not None
            age_ms = None
            if measurement is not None:
                age_ms = (observed_at_ns - measurement.captured_at_ns) / 1e6
                valid = valid and 0 <= age_ms <= MAXIMUM_DETECTION_AGE_S * 1000
            intrinsics = self.latest_intrinsics
            ideal_pixel = None if intrinsics is None else self._project(nominal_optical, intrinsics)
            detected_pixel = (
                measurement.detected_pixel_xy.tolist() if valid and measurement is not None else None
            )
            raw_offset_xy = None
            if valid and measurement is not None:
                delta_optical = measurement.detected_optical_xyz - nominal_optical
                raw_offset_xy = (world_from_optical @ delta_optical)[:2]
            pixel_displacement = (
                None
                if ideal_pixel is None or detected_pixel is None
                else float(
                    np.linalg.norm(
                        np.asarray(detected_pixel, dtype=np.float64)
                        - np.asarray(ideal_pixel, dtype=np.float64)
                    )
                )
            )
            return {
                "schema": "wendy.g1.object-relative-retarget-projection.v1",
                "ideal_pixel_xy": ideal_pixel,
                "detected_pixel_xy": detected_pixel,
                "nominal_can_optical_xyz_m": nominal_optical.tolist(),
                "detection_valid": bool(valid),
                "detection_age_ms": age_ms,
                "measurement_frame_id": None if measurement is None else measurement.frame_id,
                "measurement_stream_id": None if measurement is None else measurement.stream_id,
                "raw_offset_world_xy_m": (
                    None if raw_offset_xy is None else raw_offset_xy.tolist()
                ),
                "distance_to_optimal_m": (
                    None if raw_offset_xy is None else float(np.linalg.norm(raw_offset_xy))
                ),
                "distance_to_optimal_cm": (
                    None if raw_offset_xy is None else float(np.linalg.norm(raw_offset_xy) * 100.0)
                ),
                "pixel_displacement_px": pixel_displacement,
                "calibration_source": "sim-seeded robot_rgbd extrinsics with live waist FK",
                "physical_camera_calibration_qualified": False,
                "motion_capability": False,
            }

    def status(self) -> dict[str, Any]:
        with self.lock:
            return dict(self.latest_status)

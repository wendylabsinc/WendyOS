"""Calibrated, short-lived person observations; never a motion controller.

Depth must already be registered to the rectified detection image and converted
to metres. Timestamps, ``now``, and TF lookup must use the same clock. A depth
measurement cannot establish identity or rule out mirrors: reflective surfaces
can produce plausible, consistent depth. Deployment must independently exclude
those hazards. ``floor_z`` is a commissioned navigation plane, not inferred
terrain. Every returned approach endpoint still requires local planner checks.
"""

from __future__ import annotations

from dataclasses import dataclass
import math
import statistics
import threading
from typing import Iterable, Sequence
import uuid


class GroundingError(ValueError):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


@dataclass(frozen=True)
class Detection:
    id: str
    label: str
    score: float
    bbox: tuple[float, float, float, float]  # xmin, ymin, xmax, ymax, pixels


@dataclass(frozen=True)
class DepthImage:
    width: int
    height: int
    values: Sequence[float]  # row-major, optical-axis distance in metres
    frame_id: str
    stamp: float
    aligned: bool = True


@dataclass(frozen=True)
class Intrinsics:
    width: int
    height: int
    fx: float
    fy: float
    cx: float
    cy: float
    frame_id: str
    rectified: bool = True


@dataclass(frozen=True)
class RigidTransform:
    source_frame: str
    target_frame: str
    stamp: float  # time at which this transform was evaluated
    translation: tuple[float, float, float]
    rotation: tuple[float, ...]  # nine row-major values; camera optical -> nav

    @classmethod
    def from_quaternion(cls, source_frame: str, target_frame: str, stamp: float,
                        translation: tuple[float, float, float],
                        quaternion: tuple[float, float, float, float]) -> RigidTransform:
        """Construct from a unit quaternion in ROS order (x, y, z, w)."""
        if len(quaternion) != 4 or not _finite(*quaternion):
            raise GroundingError("invalid_transform", "TF quaternion must be finite")
        norm = math.sqrt(sum(v * v for v in quaternion))
        if abs(norm - 1.0) > 0.01:
            raise GroundingError("invalid_transform", "TF quaternion must be normalized")
        x, y, z, w = (v / norm for v in quaternion)
        return cls(source_frame, target_frame, stamp, translation, (
            1 - 2 * (y*y + z*z), 2 * (x*y - z*w), 2 * (x*z + y*w),
            2 * (x*y + z*w), 1 - 2 * (x*x + z*z), 2 * (y*z - x*w),
            2 * (x*z - y*w), 2 * (y*z + x*w), 1 - 2 * (x*x + y*y),
        ))

    def apply(self, point: tuple[float, float, float]) -> tuple[float, float, float]:
        return tuple(sum(self.rotation[3*i+j] * point[j] for j in range(3))
                     + self.translation[i] for i in range(3))


@dataclass(frozen=True)
class RobotPose:
    x: float
    y: float
    yaw: float
    frame_id: str
    stamp: float
    uncertainty: float = 0.0


@dataclass(frozen=True)
class GroundingConfig:
    nav_frame: str = "map"
    floor_z: float = 0.0
    min_standoff: float = 1.0
    default_standoff: float = 1.5
    max_observation_age: float = 0.75
    target_ttl: float = 0.75
    max_pose_age: float = 0.5
    max_sync_delta: float = 0.05
    max_future_skew: float = 0.05
    min_score: float = 0.7
    min_depth: float = 0.2
    max_depth: float = 8.0
    min_valid_fraction: float = 0.8
    min_depth_samples: int = 25
    max_depth_samples: int = 400
    max_depth_spread: float = 0.35
    cluster_gap: float = 0.3
    min_cluster_fraction: float = 0.2
    calibration_uncertainty: float = 0.05
    min_uncertainty: float = 0.1
    max_uncertainty: float = 0.75
    min_torso_height: float = 0.35
    max_torso_height: float = 2.4
    max_track_jump: float = 0.8
    max_target_speed: float = 2.0
    min_goal_translation: float = 0.1
    robot_radius: float = 0.55
    goal_tolerance: float = 0.15
    target_drift_margin: float = 0.3
    max_targets: int = 256

    def __post_init__(self):
        positive = (self.max_observation_age, self.target_ttl, self.max_pose_age,
                    self.max_sync_delta, self.min_depth, self.max_depth,
                    self.max_depth_spread, self.cluster_gap, self.min_uncertainty,
                    self.max_uncertainty, self.max_track_jump, self.max_target_speed)
        if not self.nav_frame or not _finite(*positive) or min(positive) <= 0:
            raise ValueError("grounding limits and nav_frame must be valid")
        if (not _finite(self.min_standoff, self.default_standoff, self.floor_z,
                        self.max_future_skew, self.calibration_uncertainty,
                        self.min_torso_height, self.max_torso_height,
                        self.min_goal_translation, self.robot_radius,
                        self.goal_tolerance, self.target_drift_margin)
                or self.min_standoff < 1 or self.default_standoff < self.min_standoff
                or self.max_future_skew < 0 or self.calibration_uncertainty < 0
                or self.min_goal_translation < 0
                or self.robot_radius <= 0 or self.goal_tolerance < 0 or self.target_drift_margin < 0
                or self.max_depth <= self.min_depth
                or self.max_uncertainty < self.min_uncertainty
                or self.min_torso_height < 0 or self.max_torso_height <= self.min_torso_height):
            raise ValueError("invalid grounding geometry or time configuration")
        if not (0 <= self.min_score <= 1 and 0 < self.min_valid_fraction <= 1
                and 0 < self.min_cluster_fraction <= 0.5):
            raise ValueError("invalid grounding confidence thresholds")
        counts = (self.min_depth_samples, self.max_depth_samples, self.max_targets)
        if any(isinstance(v, bool) or not isinstance(v, int) for v in counts):
            raise ValueError("grounding sample and target limits must be integers")
        if not (3 <= self.min_depth_samples <= self.max_depth_samples <= 4096
                and 1 <= self.max_targets <= 10000):
            raise ValueError("invalid grounding sample or target limits")


def _finite(*values: float) -> bool:
    try:
        return all(math.isfinite(v) for v in values)
    except (TypeError, ValueError):
        return False


def _check_time(stamp: float, now: float, max_age: float, future: float) -> None:
    if not _finite(stamp, now) or stamp < 0 or now < 0:
        raise GroundingError("invalid_time", "timestamps must share a finite nonnegative clock")
    if stamp - now > future:
        raise GroundingError("future_observation", "source timestamp is ahead of the local clock")
    if now - stamp > max_age:
        raise GroundingError("stale", "source observation has expired")


def _validate_transform(transform: RigidTransform) -> None:
    r = transform.rotation
    if len(r) != 9 or len(transform.translation) != 3 or not _finite(*r, *transform.translation):
        raise GroundingError("invalid_transform", "TF must be a finite rigid transform")
    for i in range(3):
        for j in range(3):
            dot = sum(r[3*i+k] * r[3*j+k] for k in range(3))
            if abs(dot - (1.0 if i == j else 0.0)) > 0.001:
                raise GroundingError("invalid_transform", "TF rotation is not orthonormal")
    det = r[0]*(r[4]*r[8]-r[5]*r[7]) - r[1]*(r[3]*r[8]-r[5]*r[6]) + r[2]*(r[3]*r[7]-r[4]*r[6])
    if abs(det - 1.0) > 0.001:
        raise GroundingError("invalid_transform", "TF rotation must not reflect coordinates")


@dataclass(frozen=True)
class GroundedObservation:
    detection_id: str
    label: str
    score: float
    bbox: tuple[float, float, float, float]
    position: tuple[float, float, float]
    uncertainty: float
    frame_id: str
    source_frame: str
    source_stamp: float
    depth_stamp: float
    transform_stamp: float
    expires_at: float
    valid_depth_fraction: float
    depth_spread: float
    depth_samples: int

    def as_dict(self) -> dict:
        return {
            "detection_id": self.detection_id, "label": self.label, "score": self.score,
            "bbox": list(self.bbox),
            "pose": dict(zip(("x", "y", "z"), self.position)),
            "frame_id": self.frame_id, "uncertainty_m": self.uncertainty,
            "source_frame": self.source_frame, "source_stamp": self.source_stamp,
            "depth_stamp": self.depth_stamp, "transform_stamp": self.transform_stamp,
            "expires_at": self.expires_at, "valid_depth_fraction": self.valid_depth_fraction,
            "depth_spread_m": self.depth_spread, "depth_samples": self.depth_samples,
            "depth_supported": True, "mirror_exclusion": "unverified",
        }


def ground_detection(detection: Detection, depth: DepthImage, intrinsics: Intrinsics,
                     transform: RigidTransform, *, stamp: float, frame_id: str,
                     now: float, config: GroundingConfig | None = None) -> GroundedObservation:
    """Ground one tracked detection, or raise with a specific rejection code.

    A single observation is not proof of a navigable route or person identity.
    Monocular bearings and depth-free estimates are intentionally unsupported.
    """
    cfg = config or GroundingConfig()
    _check_time(stamp, now, min(cfg.max_observation_age, cfg.target_ttl), cfg.max_future_skew)
    if not isinstance(detection.id, str) or not detection.id.strip() or len(detection.id) > 256:
        raise GroundingError("missing_track_id", "a persistent detector track ID is required")
    if detection.label != "person" or not _finite(detection.score) or not cfg.min_score <= detection.score <= 1:
        raise GroundingError("low_confidence", "a sufficiently confident person detection is required")
    if depth is None or intrinsics is None or transform is None:
        raise GroundingError("missing_calibration", "aligned depth, camera calibration and capture-time TF are required")
    if not frame_id or not (frame_id == depth.frame_id == intrinsics.frame_id == transform.source_frame):
        raise GroundingError("frame_mismatch", "detections, depth, calibration and TF must share the optical frame")
    if transform.target_frame != cfg.nav_frame:
        raise GroundingError("frame_mismatch", "TF must target the configured navigation frame")
    if not depth.aligned or not intrinsics.rectified:
        raise GroundingError("uncalibrated", "depth must align to the rectified detection image")
    if (not isinstance(depth.width, int) or not isinstance(depth.height, int)
            or not 1 <= depth.width <= 8192 or not 1 <= depth.height <= 8192
            or (depth.width, depth.height) != (intrinsics.width, intrinsics.height)
            or len(depth.values) != depth.width * depth.height):
        raise GroundingError("dimension_mismatch", "depth data and calibration image dimensions must match")
    for other_stamp in (depth.stamp, transform.stamp):
        _check_time(other_stamp, now, cfg.max_observation_age, cfg.max_future_skew)
        if abs(other_stamp - stamp) > cfg.max_sync_delta:
            raise GroundingError("time_mismatch", "depth and TF must correspond to the detection capture time")
    if (not _finite(intrinsics.fx, intrinsics.fy, intrinsics.cx, intrinsics.cy)
            or min(intrinsics.fx, intrinsics.fy) <= 0
            or not 0 <= intrinsics.cx < depth.width or not 0 <= intrinsics.cy < depth.height):
        raise GroundingError("invalid_intrinsics", "finite rectified camera intrinsics are required")
    _validate_transform(transform)
    if len(detection.bbox) != 4 or not _finite(*detection.bbox):
        raise GroundingError("invalid_bbox", "bbox must contain four finite pixel coordinates")
    x0, y0, x1, y1 = detection.bbox
    if not (0 <= x0 < x1 <= depth.width and 0 <= y0 < y1 <= depth.height):
        raise GroundingError("invalid_bbox", "bbox must be fully inside the rectified image")
    # A small central torso region avoids the silhouette and most background.
    u0, u1 = math.ceil(x0 + 0.35*(x1-x0)), math.floor(x0 + 0.65*(x1-x0))
    v0, v1 = math.ceil(y0 + 0.30*(y1-y0)), math.floor(y0 + 0.65*(y1-y0))
    if u1 <= u0 or v1 <= v0:
        raise GroundingError("insufficient_depth", "person torso occupies too few pixels")
    stride = max(1, math.ceil(math.sqrt((u1-u0)*(v1-v0) / cfg.max_depth_samples)))
    positions = [(u, v) for v in range(v0, v1, stride) for u in range(u0, u1, stride)]
    positions = positions[:cfg.max_depth_samples]
    values = [depth.values[v*depth.width+u] for u, v in positions]
    valid = sorted(float(z) for z in values if _finite(z) and cfg.min_depth <= z <= cfg.max_depth)
    fraction = len(valid) / len(values)
    if len(valid) < cfg.min_depth_samples or fraction < cfg.min_valid_fraction:
        raise GroundingError("insufficient_depth", "too few valid aligned depth samples in the torso")
    # Do not silently choose one of two material modes (person/background,
    # occluder, or depth ambiguity), even when the larger mode looks plausible.
    cluster_min = math.ceil(cfg.min_cluster_fraction * len(valid))
    for i in range(cluster_min, len(valid)-cluster_min+1):
        if valid[i] - valid[i-1] >= cfg.cluster_gap:
            raise GroundingError("ambiguous_depth", "multiple substantial torso depth clusters")
    p10, p90 = valid[int(0.1*(len(valid)-1))], valid[math.ceil(0.9*(len(valid)-1))]
    spread = p90 - p10
    if spread > cfg.max_depth_spread:
        raise GroundingError("ambiguous_depth", "torso depth spread exceeds the configured limit")
    z = statistics.median(valid)
    u, v = (u0+u1-1)/2, (v0+v1-1)/2
    optical = ((u-intrinsics.cx)*z/intrinsics.fx, (v-intrinsics.cy)*z/intrinsics.fy, z)
    position = transform.apply(optical)
    if not _finite(*position) or not cfg.min_torso_height <= position[2]-cfg.floor_z <= cfg.max_torso_height:
        raise GroundingError("invalid_person_height", "grounded torso is inconsistent with the configured floor plane")
    angular_extent = math.hypot((u1-u0)*z/(2*intrinsics.fx), (v1-v0)*z/(2*intrinsics.fy))
    uncertainty = cfg.min_uncertainty + cfg.calibration_uncertainty + angular_extent + spread/2
    if uncertainty > cfg.max_uncertainty:
        raise GroundingError("uncertain_position", "grounded position exceeds the uncertainty limit")
    return GroundedObservation(detection.id, detection.label, detection.score, tuple(detection.bbox),
                               position, uncertainty, cfg.nav_frame, frame_id, stamp,
                               depth.stamp, transform.stamp, stamp+min(cfg.target_ttl, cfg.max_observation_age),
                               fraction, spread, len(valid))


@dataclass
class _Target:
    id: str
    observation: GroundedObservation
    state: str = "observed"
    reason: str = ""

    def as_dict(self) -> dict:
        return {"target_id": self.id, "status": self.state, "reason": self.reason,
                **self.observation.as_dict()}


class TargetRegistry:
    """Keep selection IDs separate from detector IDs; never revive a lost ID.

    ``update`` represents a complete detection frame, including empty frames.
    A rejected, absent, expired or jumped target invalidates that selection ID.
    If its detector ID reappears, it receives a new selection ID. Callers must
    explicitly select it again. No positional nearest-person reassociation occurs.
    """

    def __init__(self, config: GroundingConfig | None = None):
        self.config = config or GroundingConfig()
        self._targets: dict[str, _Target] = {}
        self._tracks: dict[str, str] = {}
        self._lock = threading.RLock()
        self._last_stamp = -math.inf
        self.last_rejections: list[dict] = []

    def _retire(self, target: _Target, state: str, reason: str) -> None:
        target.state, target.reason = state, reason
        if self._tracks.get(target.observation.detection_id) == target.id:
            del self._tracks[target.observation.detection_id]

    def _expire(self, now: float) -> None:
        if not _finite(now) or now < 0:
            raise GroundingError("invalid_time", "now must be a finite nonnegative timestamp")
        for target in self._targets.values():
            if target.state == "observed" and now >= target.observation.expires_at:
                self._retire(target, "stale", "target observation expired")
            elif target.state == "observed" and target.observation.source_stamp-now > self.config.max_future_skew:
                self._retire(target, "invalid", "clock changed relative to source observation")

    def invalidate_all(self, reason: str = "perception unavailable") -> None:
        with self._lock:
            for target in self._targets.values():
                if target.state == "observed":
                    self._retire(target, "lost", reason)

    def update(self, detections: Iterable[Detection], depth: DepthImage,
               intrinsics: Intrinsics, transform: RigidTransform, *, stamp: float,
               frame_id: str, now: float) -> list[dict]:
        with self._lock:
            self._expire(now)
            self.last_rejections = []
            # Older or duplicate delivery must not erase a newer frame, refresh
            # expiry, or silently mint a replacement selection ID.
            if not _finite(stamp) or stamp < 0:
                self.invalidate_all("invalid source timestamp")
                raise GroundingError("invalid_time", "source timestamp must be finite and nonnegative")
            if stamp <= self._last_stamp:
                self.last_rejections = [{"code": "out_of_order", "message": "ignored duplicate or out-of-order detection frame"}]
                return self.observed_targets(now)
            try:
                _check_time(stamp, now, min(self.config.max_observation_age, self.config.target_ttl), self.config.max_future_skew)
            except GroundingError as error:
                self.invalidate_all(str(error))
                self.last_rejections = [{"code": error.code, "message": str(error)}]
                return []
            self._last_stamp = stamp
            items = list(detections)
            if len(items) > self.config.max_targets:
                self.invalidate_all("too many detections")
                raise GroundingError("too_many_detections", "detection frame exceeds registry capacity")
            ids = [d.id for d in items]
            duplicates = {id_ for id_ in ids if ids.count(id_) > 1}
            for detector_id, target_id in list(self._tracks.items()):
                if detector_id not in ids:
                    self._retire(self._targets[target_id], "lost", "target absent from current detection frame")
            for detection in items:
                old = self._targets.get(self._tracks.get(detection.id, ""))
                try:
                    if detection.id in duplicates:
                        raise GroundingError("ambiguous_id", "detector reused an ID within one frame")
                    observation = ground_detection(detection, depth, intrinsics, transform,
                                                   stamp=stamp, frame_id=frame_id, now=now, config=self.config)
                    if old:
                        previous = old.observation
                        delta = math.dist(observation.position, previous.position)
                        elapsed = stamp-previous.source_stamp
                        speed_limit = self.config.max_target_speed*elapsed + previous.uncertainty + observation.uncertainty
                        if delta > min(self.config.max_track_jump, speed_limit):
                            raise GroundingError("jumped", "tracked target position jumped; explicit reselection is required")
                        old.observation = observation
                    else:
                        target_id = "person-" + uuid.uuid4().hex
                        self._targets[target_id] = _Target(target_id, observation)
                        self._tracks[detection.id] = target_id
                except GroundingError as error:
                    self.last_rejections.append({"detection_id": detection.id, "code": error.code, "message": str(error)})
                    if old:
                        self._retire(old, "jumped" if error.code == "jumped" else "invalid", str(error))
            while len(self._targets) > self.config.max_targets:
                retired = next((key for key, value in self._targets.items() if value.state != "observed"), None)
                if retired is None:
                    break
                del self._targets[retired]
            return self.observed_targets(now)

    def observed_targets(self, now: float) -> list[dict]:
        with self._lock:
            self._expire(now)
            return [target.as_dict() for target in self._targets.values() if target.state == "observed"]

    def status(self, target_id: str, now: float) -> dict:
        with self._lock:
            self._expire(now)
            target = self._targets.get(target_id)
            if target is None:
                return {"target_id": target_id, "status": "unknown", "reason": "unknown or retired target ID"}
            return target.as_dict()

    def choose_goal(self, target_id: str, robot_pose: RobotPose, now: float,
                    standoff: float | None = None) -> dict:
        """Choose a floor endpoint toward a selected observation, never reverse.

        This is geometry only. The robot runtime must validate a current Nav2
        plan, footprint clearance, motion readiness, and target state before
        accepting the endpoint and throughout navigation.
        """
        with self._lock:
            self._expire(now)
            target = self._targets.get(target_id)
            if target is None or target.state != "observed":
                raise GroundingError("target_unavailable", "selected target is unknown, lost, invalid or stale")
            cfg = self.config
            distance = cfg.default_standoff if standoff is None else standoff
            if not _finite(distance) or distance < cfg.min_standoff:
                raise GroundingError("invalid_standoff", f"standoff must be at least {cfg.min_standoff:g} metres")
            if robot_pose.frame_id != cfg.nav_frame or not _finite(robot_pose.x, robot_pose.y, robot_pose.yaw, robot_pose.uncertainty) or robot_pose.uncertainty < 0:
                raise GroundingError("invalid_pose", "robot pose must be finite and in the navigation frame")
            _check_time(robot_pose.stamp, now, cfg.max_pose_age, cfg.max_future_skew)
            observation = target.observation
            dx, dy = observation.position[0]-robot_pose.x, observation.position[1]-robot_pose.y
            separation = math.hypot(dx, dy)
            protected = (distance + observation.uncertainty + robot_pose.uncertainty
                         + cfg.robot_radius + cfg.goal_tolerance + cfg.target_drift_margin)
            result = {"target_id": target_id, "target_source_stamp": observation.source_stamp,
                      "expires_at": observation.expires_at, "standoff_m": distance,
                      "protected_standoff_m": protected, "distance_to_target_m": separation,
                      "uncertainty_m": observation.uncertainty,
                      "margins_m": {"target_uncertainty": observation.uncertainty,
                                    "robot_pose_uncertainty": robot_pose.uncertainty,
                                    "robot_radius": cfg.robot_radius,
                                    "goal_tolerance": cfg.goal_tolerance,
                                    "target_drift": cfg.target_drift_margin}, "goal": None}
            if separation-protected <= cfg.min_goal_translation:
                return {**result, "status": "already_within_standoff"}
            yaw = math.atan2(dy, dx)
            goal = {"frame_id": cfg.nav_frame, "x": observation.position[0]-dx/separation*protected,
                    "y": observation.position[1]-dy/separation*protected, "z": cfg.floor_z, "yaw": yaw}
            return {**result, "status": "requires_plan_validation", "goal": goal}

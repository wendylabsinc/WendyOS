"""Readiness and durable goal supervision, independent of ROS and transport.

Call tick() from the local control timer. Backend methods must enqueue work and
return promptly; they are never called with the state lock held. A separate
device/controller watchdog must inhibit motion if this process stops ticking.
"""

from __future__ import annotations

from collections import deque
from dataclasses import dataclass
import json
import math
import re
import sqlite3
import threading
import time
from typing import Callable, Protocol
import uuid


@dataclass(frozen=True)
class SensorRequirement:
    name: str
    frame_id: str
    max_age: float = 0.5
    max_receipt_age: float = 0.5


@dataclass(frozen=True)
class RuntimeConfig:
    navigation_frame: str = "map"
    odometry_frame: str = "odom"
    base_frame: str = "base_link"
    sensors: tuple[SensorRequirement, ...] = (
        SensorRequirement("scan", "base_link"),
        SensorRequirement("transforms", "map"),
    )
    motion_enabled: bool = False
    watchdog_commissioned: bool = False
    odometry_max_age: float = 0.5
    heartbeat_max_age: float = 0.5
    future_tolerance: float = 0.05
    max_position_variance: float = 0.25
    max_yaw_variance: float = 0.25
    allowed_postures: tuple[str, ...] = ("standing",)
    max_linear_speed: float = 0.2
    max_angular_speed: float = 0.5
    max_goal_distance: float = 10.0
    max_coordinate: float = 10000.0
    lease_seconds: float = 10.0
    max_lease_seconds: float = 30.0
    goal_timeout_seconds: float = 120.0
    max_goal_timeout_seconds: float = 600.0
    stopped_linear_speed: float = 0.02
    stopped_angular_speed: float = 0.03
    stopped_dwell_seconds: float = 0.5
    stopped_min_samples: int = 3
    target_max_age: float = 0.75
    target_max_displacement: float = 0.3
    max_history: int = 256


class Backend(Protocol):
    def send_goal(self, goal_id: str, pose: dict, max_speed: float) -> None: ...
    def cancel_goal(self, goal_id: str) -> None: ...
    def set_enabled(self, enabled: bool) -> None: ...
    def stop(self) -> None: ...


_TERMINAL = frozenset(("succeeded", "failed", "rejected", "cancelled", "abandoned"))
_REQUEST_ID = re.compile(r"^[A-Za-z0-9_.:-]{1,128}$")


def _finite(value, name: str, minimum=None, maximum=None) -> float:
    if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value):
        raise ValueError(f"{name} must be finite")
    value = float(value)
    if (minimum is not None and value < minimum) or (maximum is not None and value > maximum):
        raise ValueError(f"{name} is outside configured limits")
    return value


class NavigationRuntime:
    def __init__(self, config: RuntimeConfig, db_path: str, backend: Backend,
                 source_clock: Callable[[], float] = time.time,
                 monotonic_clock: Callable[[], float] = time.monotonic):
        self.config, self.backend = config, backend
        self._validate_config()
        self._source_clock, self._monotonic = source_clock, monotonic_clock
        self._lock, self._dispatch_lock = threading.RLock(), threading.Lock()
        self._actions = deque()
        self._sensors, self._targets, self._watermarks = {}, {}, {}
        self._receipts = {}
        self._odom = self._guard = self._navigation = self._navigation_pose = None
        self._active = None
        self._closed = False
        self._db = sqlite3.connect(db_path, check_same_thread=False)
        self._db.execute("PRAGMA journal_mode=WAL")
        self._db.execute("PRAGMA synchronous=FULL")
        self._db.execute("CREATE TABLE IF NOT EXISTS goals (goal_id TEXT PRIMARY KEY, request_id TEXT UNIQUE NOT NULL, payload TEXT NOT NULL, data TEXT NOT NULL, terminal INTEGER NOT NULL)")
        with self._db:
            for payload, data in self._db.execute("SELECT payload, data FROM goals WHERE terminal=0").fetchall():
                goal = json.loads(data)
                goal["payload"] = payload
                goal.update(state="abandoned", reason="runtime_restarted", stopped_confirmed=False)
                self._save(goal)
            self._prune()
        self._queue("set_enabled", False)
        self._queue("stop")
        self._flush()

    def _validate_config(self):
        c = self.config
        for name in ("navigation_frame", "odometry_frame", "base_frame"):
            if not isinstance(getattr(c, name), str) or not getattr(c, name):
                raise ValueError(f"{name} must be nonempty")
        for name in ("odometry_max_age", "heartbeat_max_age", "max_position_variance", "max_yaw_variance",
                     "max_linear_speed", "max_angular_speed", "max_goal_distance", "max_coordinate",
                     "lease_seconds", "max_lease_seconds", "goal_timeout_seconds", "max_goal_timeout_seconds",
                     "stopped_linear_speed", "stopped_angular_speed", "stopped_dwell_seconds", "target_max_age", "target_max_displacement"):
            if _finite(getattr(c, name), name, 0) == 0:
                raise ValueError(f"{name} must be positive")
        _finite(c.future_tolerance, "future_tolerance", 0)
        if type(c.motion_enabled) is not bool or type(c.watchdog_commissioned) is not bool:
            raise ValueError("commissioning flags must be booleans")
        if type(c.stopped_min_samples) is not int or c.stopped_min_samples < 2:
            raise ValueError("stopped_min_samples must be an integer >=2")
        if type(c.max_history) is not int or not 1 <= c.max_history <= 4096:
            raise ValueError("max_history must be an integer in 1..4096")
        if c.lease_seconds > c.max_lease_seconds or c.goal_timeout_seconds > c.max_goal_timeout_seconds:
            raise ValueError("default supervision duration exceeds maximum")
        if c.stopped_linear_speed >= c.max_linear_speed or c.stopped_angular_speed >= c.max_angular_speed:
            raise ValueError("stopped velocity thresholds must be below motion limits")
        if not c.sensors or len({r.name for r in c.sensors}) != len(c.sensors):
            raise ValueError("unique required sensors must be configured")
        for requirement in c.sensors:
            if not requirement.name or not requirement.frame_id or _finite(requirement.max_age, "sensor max_age", 0) == 0 or _finite(requirement.max_receipt_age, "sensor max_receipt_age", 0) == 0:
                raise ValueError("invalid sensor requirement")

    def _queue(self, method, *args):
        self._actions.append((method, args))

    def _flush(self):
        # A nonblocking acquire also permits a backend callback to synchronously
        # report acceptance without deadlocking the current dispatcher.
        if not self._dispatch_lock.acquire(blocking=False):
            return
        acquired = True
        try:
            while True:
                with self._lock:
                    if not self._actions:
                        self._dispatch_lock.release()
                        acquired = False
                        return
                    method, args = self._actions.popleft()
                    if method == "send_goal" and (not self._active or self._active["goal_id"] != args[0] or self._active["state"] != "submitting"):
                        continue
                    if method == "set_enabled" and args[0] and (not self._active or self._active["state"] != "running"):
                        continue
                try:
                    getattr(self.backend, method)(*args)
                except Exception as exc:
                    with self._lock:
                        if self._active and not self._closed:
                            self._active["backend_error"] = f"{method}: {type(exc).__name__}"[:200]
                            self._begin_stop("failed", "backend_command_failed")
                            try:
                                self._save(self._active)
                            except sqlite3.Error:
                                self._active["persistence_error"] = True
        finally:
            if acquired:
                self._dispatch_lock.release()

    def _require_open(self):
        if self._closed:
            raise RuntimeError("runtime is closed")

    def _record(self, key, stamp, frame_id, expected_frame, valid=True):
        now, received = self._source_clock(), self._monotonic()
        reason = None
        try:
            stamp = _finite(stamp, "source timestamp", 0)
            _finite(now, "source clock", 0)
            if stamp > now + self.config.future_tolerance:
                reason = "future_timestamp"
            elif stamp < self._watermarks.get(key, stamp):
                reason = "regressing_timestamp"
            elif frame_id != expected_frame:
                reason = "wrong_frame"
            elif valid is not True:
                reason = "unhealthy"
        except ValueError:
            stamp, reason = None, "invalid_timestamp"
        if reason is None:
            previous = self._receipts.get(key)
            if previous is not None and previous[0] == stamp:
                received = previous[1]
            else:
                self._receipts[key] = (stamp, received)
            self._watermarks[key] = stamp
        return {"stamp": stamp, "received": received, "frame_id": frame_id,
                "expected_frame": expected_frame, "valid": reason is None, "reason": reason}

    def _fresh_reason(self, record, max_age, max_receipt_age):
        if record is None:
            return "missing"
        if not record["valid"]:
            return record["reason"]
        source_age = self._source_clock() - record["stamp"]
        receipt_age = self._monotonic() - record["received"]
        if not math.isfinite(source_age) or source_age < -self.config.future_tolerance:
            return "future_timestamp"
        if source_age > max_age:
            return "stale_source"
        if not math.isfinite(receipt_age) or not 0 <= receipt_age <= max_receipt_age:
            return "stale_heartbeat"
        return None

    def _readiness(self):
        c, blockers = self.config, []
        def block(source, reason):
            if reason:
                blockers.append({"source": source, "reason": reason})
        block("configuration", None if c.motion_enabled else "motion_disabled")
        block("configuration", None if c.watchdog_commissioned else "watchdog_not_commissioned")
        for requirement in c.sensors:
            block(requirement.name, self._fresh_reason(self._sensors.get(requirement.name), requirement.max_age, requirement.max_receipt_age))
        block("odometry", self._fresh_reason(self._odom, c.odometry_max_age, c.heartbeat_max_age))
        block("navigation_pose", self._fresh_reason(self._navigation_pose, c.odometry_max_age, c.heartbeat_max_age))
        now = self._monotonic()
        for name, record in (("guard", self._guard), ("navigation", self._navigation)):
            if record is None:
                block(name, "missing")
            elif not 0 <= now - record["received"] <= c.heartbeat_max_age:
                block(name, "stale_heartbeat")
            elif not record["ready"]:
                block(name, "not_ready")
        if self._guard and self._guard["stop_latched"]:
            block("guard", "stop_latched")
        return {"ready": not blockers, "blockers": blockers, "motion_enabled": c.motion_enabled,
                "watchdog_commissioned": c.watchdog_commissioned}

    def readiness(self):
        with self._lock:
            self._require_open()
            return self._readiness()

    def diagnostics(self):
        """A bounded observation snapshot; receipt ages use the monotonic clock."""
        with self._lock:
            self._require_open()
            def snapshot(record):
                if record is None:
                    return {"status": "unknown", "reason": "missing"}
                result = {key: value for key, value in record.items() if key != "received"}
                result["receipt_age_seconds"] = self._monotonic() - record["received"]
                if record.get("stamp") is not None:
                    result["source_age_seconds"] = self._source_clock() - record["stamp"]
                return result
            return {"readiness": self._readiness(),
                    "sensors": {r.name: snapshot(self._sensors.get(r.name)) for r in self.config.sensors},
                    "odometry": snapshot(self._odom), "navigation_pose": snapshot(self._navigation_pose),
                    "guard": snapshot(self._guard), "navigation": snapshot(self._navigation)}

    def update_sensor(self, name, stamp, frame_id, healthy=True, coverage_ok=True):
        with self._lock:
            self._require_open()
            requirement = next((r for r in self.config.sensors if r.name == name), None)
            if requirement is None:
                raise ValueError("sensor is not configured")
            self._sensors[name] = self._record("sensor:" + name, stamp, frame_id, requirement.frame_id, healthy is True and coverage_ok is True)
            self._sensors[name].update(healthy=healthy is True, coverage_ok=coverage_ok is True)
            self._supervise()
        self._flush()

    def update_odometry(self, stamp, frame_id, child_frame_id, x, y, yaw,
                        linear_speed, angular_speed, position_variance, yaw_variance, posture):
        with self._lock:
            self._require_open()
            c = self.config
            record = self._record("odometry", stamp, frame_id, c.odometry_frame, child_frame_id == c.base_frame)
            try:
                values = {name: _finite(value, name) for name, value in dict(x=x, y=y, yaw=yaw, linear_speed=linear_speed, angular_speed=angular_speed, position_variance=position_variance, yaw_variance=yaw_variance).items()}
                if not 0 <= position_variance <= c.max_position_variance or not 0 <= yaw_variance <= c.max_yaw_variance:
                    raise ValueError("invalid covariance")
                if posture not in c.allowed_postures:
                    raise ValueError("invalid posture")
                record.update(values, posture=posture, child_frame_id=child_frame_id)
            except (ValueError, TypeError):
                record.update(valid=False, reason="invalid_motion_feedback")
            self._odom = record
            self._supervise()
            self._confirm_stop()
        self._flush()

    def update_guard(self, ready, stop_latched=False):
        with self._lock:
            self._require_open()
            self._guard = {"ready": ready is True, "stop_latched": stop_latched is not False, "received": self._monotonic()}
            self._supervise()
        self._flush()

    def update_navigation_pose(self, stamp, frame_id, x, y, yaw):
        with self._lock:
            self._require_open()
            record = self._record("navigation_pose", stamp, frame_id, self.config.navigation_frame)
            try:
                record.update(x=_finite(x, "x", -self.config.max_coordinate, self.config.max_coordinate),
                              y=_finite(y, "y", -self.config.max_coordinate, self.config.max_coordinate),
                              yaw=_finite(yaw, "yaw", -math.pi, math.pi))
            except ValueError:
                record.update(valid=False, reason="invalid_navigation_pose")
            self._navigation_pose = record
            self._supervise()
        self._flush()

    def update_navigation_available(self, available):
        with self._lock:
            self._require_open()
            self._navigation = {"ready": available is True, "received": self._monotonic()}
            self._supervise()
        self._flush()

    def update_target(self, target_id, stamp, x, y, frame_id, valid=True):
        if not isinstance(target_id, str) or not _REQUEST_ID.fullmatch(target_id):
            raise ValueError("invalid target_id")
        with self._lock:
            self._require_open()
            record = self._record("target:" + target_id, stamp, frame_id, self.config.navigation_frame, valid)
            try:
                record.update(x=_finite(x, "target x", -self.config.max_coordinate, self.config.max_coordinate), y=_finite(y, "target y", -self.config.max_coordinate, self.config.max_coordinate))
            except ValueError:
                record.update(valid=False, reason="invalid_target_position")
            self._targets[target_id] = record
            if len(self._targets) > 256:
                protected = self._active["target"]["target_id"] if self._active and self._active["target"] else None
                oldest = min((key for key in self._targets if key != protected), key=lambda key: self._targets[key]["received"])
                del self._targets[oldest]
                self._watermarks.pop("target:" + oldest, None)
                self._receipts.pop("target:" + oldest, None)
            self._supervise()
        self._flush()

    def _pose(self, pose):
        if not isinstance(pose, dict) or set(pose) != {"x", "y", "yaw", "frame_id"} or pose["frame_id"] != self.config.navigation_frame:
            raise ValueError("pose must contain x, y, yaw and the configured navigation frame_id")
        return {"x": _finite(pose["x"], "x", -self.config.max_coordinate, self.config.max_coordinate),
                "y": _finite(pose["y"], "y", -self.config.max_coordinate, self.config.max_coordinate),
                "yaw": _finite(pose["yaw"], "yaw", -math.pi, math.pi), "frame_id": pose["frame_id"]}

    def navigate(self, request_id, pose, max_speed, lease_seconds=None, timeout_seconds=None, target_id=None, metadata=None, expected_target_stamp=None):
        try:
            return self._navigate(request_id, pose, max_speed, lease_seconds, timeout_seconds, target_id, metadata, expected_target_stamp)
        finally:
            self._flush()

    def _navigate(self, request_id, pose, max_speed, lease_seconds, timeout_seconds, target_id, metadata, expected_target_stamp):
        if not isinstance(request_id, str) or not _REQUEST_ID.fullmatch(request_id):
            raise ValueError("request_id must be 1..128 identifier characters")
        c = self.config
        pose = self._pose(pose)
        speed = _finite(max_speed, "max_speed", 0, c.max_linear_speed)
        lease = _finite(c.lease_seconds if lease_seconds is None else lease_seconds, "lease_seconds", 0, c.max_lease_seconds)
        timeout = _finite(c.goal_timeout_seconds if timeout_seconds is None else timeout_seconds, "timeout_seconds", 0, c.max_goal_timeout_seconds)
        if min(speed, lease, timeout) <= 0:
            raise ValueError("speed, lease and timeout must be positive")
        if target_id is not None and (not isinstance(target_id, str) or not _REQUEST_ID.fullmatch(target_id)):
            raise ValueError("invalid target_id")
        if expected_target_stamp is not None:
            expected_target_stamp = _finite(expected_target_stamp, "expected_target_stamp", 0)
            if target_id is None:
                raise ValueError("expected_target_stamp requires a target_id")
        if metadata is not None:
            if not isinstance(metadata, dict):
                raise ValueError("metadata must be a JSON object")
            try:
                encoded = json.dumps(metadata, sort_keys=True, allow_nan=False)
            except (ValueError, TypeError) as exc:
                raise ValueError("metadata must be finite JSON") from exc
            if len(encoded.encode("utf-8")) > 4096:
                raise ValueError("metadata exceeds 4096 bytes")
            metadata = json.loads(encoded)
        payload = json.dumps(dict(pose=pose, max_speed=speed, lease_seconds=lease, timeout_seconds=timeout, target_id=target_id, metadata=metadata, expected_target_stamp=expected_target_stamp), sort_keys=True, allow_nan=False)
        with self._lock:
            self._require_open()
            self._supervise()
            existing = self._db.execute("SELECT payload, data FROM goals WHERE request_id=?", (request_id,)).fetchone()
            if existing:
                if existing[0] != payload:
                    raise ValueError("request_id was already used with a different payload")
                return self._get(json.loads(existing[1])["goal_id"])
            if self._active:
                raise RuntimeError("another goal is active or still stopping")
            readiness = self._readiness()
            if not readiness["ready"]:
                raise RuntimeError("navigation not ready: " + json.dumps(readiness["blockers"]))
            if math.hypot(pose["x"] - self._navigation_pose["x"], pose["y"] - self._navigation_pose["y"]) > c.max_goal_distance:
                raise ValueError("goal exceeds max_goal_distance from the navigation pose")
            target = None
            if target_id is not None:
                observed = self._targets.get(target_id)
                if self._fresh_reason(observed, c.target_max_age, c.heartbeat_max_age):
                    raise RuntimeError("target observation is unavailable or stale")
                if expected_target_stamp is not None and observed["stamp"] != expected_target_stamp:
                    raise RuntimeError("target observation changed while preparing the goal")
                target = {"target_id": target_id, "stamp": observed["stamp"], "x": observed["x"], "y": observed["y"]}
            now = self._monotonic()
            goal = dict(goal_id=str(uuid.uuid4()), request_id=request_id, payload=payload, pose=pose,
                        max_speed=speed, state="submitting", reason=None, stopped_confirmed=False,
                        lease_seconds=lease, lease_deadline=now + lease, goal_deadline=now + timeout,
                        target=target, metadata=metadata, backend_settled=False, measured_stopped=False,
                        stop_started=None, stop_samples=0, stop_last_stamp=None, stop_last_received=None)
            self._active = goal
            self._save(goal)
            self._prune()
            self._queue("send_goal", goal["goal_id"], pose.copy(), speed)
            result = self._snapshot(goal)
        return result

    def backend_accepted(self, goal_id):
        with self._lock:
            if self._closed or not self._active or self._active["goal_id"] != goal_id or self._active["state"] != "submitting":
                return
            self._active["state"] = "running"
            self._save(self._active)
            self._supervise()
            if self._active["state"] == "running":
                self._queue("set_enabled", True)
        self._flush()

    def backend_result(self, goal_id, outcome):
        if outcome not in ("succeeded", "failed", "rejected", "cancelled"):
            raise ValueError("invalid backend outcome")
        with self._lock:
            if self._closed or not self._active or self._active["goal_id"] != goal_id:
                return
            if self._active["state"] in ("submitting", "running"):
                self._begin_stop(outcome, "backend_" + outcome)
            self._mark_settled()
            self._save(self._active)
            self._confirm_stop()
        self._flush()

    def backend_settled(self, goal_id):
        """Confirm that the backend can no longer emit commands for this goal."""
        with self._lock:
            if self._closed or not self._active or self._active["goal_id"] != goal_id or self._active["state"] not in ("cancel_requested", "stopping"):
                return
            self._mark_settled()
            self._save(self._active)
            self._confirm_stop()
        self._flush()

    def _mark_settled(self):
        if not self._active["backend_settled"]:
            self._active.update(backend_settled=True, measured_stopped=False, stop_started=None,
                                stop_samples=0, stop_last_received=None,
                                stop_last_stamp=self._odom.get("stamp") if self._odom else None)

    def backend_uncertain(self, goal_id, reason="backend_uncertain"):
        """Inhibit on an action transport failure without claiming cancellation."""
        if not isinstance(reason, str) or not reason or len(reason) > 128:
            raise ValueError("reason must be a short nonempty string")
        with self._lock:
            if self._closed or not self._active or self._active["goal_id"] != goal_id:
                return
            self._begin_stop("failed", reason)
        self._flush()

    def _begin_stop(self, outcome, reason, cancel_requested=False):
        goal = self._active
        if not goal or goal["state"] not in ("submitting", "running"):
            return
        goal.update(state="cancel_requested" if cancel_requested else "stopping", reason=reason,
                    pending_outcome=outcome, stop_started=None, stop_samples=0, stop_last_stamp=None, stop_last_received=None)
        self._queue("set_enabled", False)
        self._queue("cancel_goal", goal["goal_id"])
        self._queue("stop")
        try:
            self._save(goal)
        except sqlite3.Error:
            # A full/read-only disk must not prevent the physical inhibit. The
            # previous durable record remains nonterminal and is abandoned on
            # restart. Keep the live goal blocking further submissions.
            goal["persistence_error"] = True

    def _supervise(self):
        goal = self._active
        if not goal or goal["state"] not in ("submitting", "running"):
            return
        now = self._monotonic()
        if now >= goal["lease_deadline"]:
            self._begin_stop("failed", "lease_expired")
        elif now >= goal["goal_deadline"]:
            self._begin_stop("failed", "goal_timeout")
        elif not self._readiness()["ready"]:
            self._begin_stop("failed", "readiness_lost")
        elif goal["target"]:
            reference = goal["target"]
            target = self._targets.get(reference["target_id"])
            if self._fresh_reason(target, self.config.target_max_age, self.config.heartbeat_max_age):
                self._begin_stop("failed", "target_lost")
            elif math.hypot(target["x"] - reference["x"], target["y"] - reference["y"]) > self.config.target_max_displacement:
                self._begin_stop("failed", "target_moved")

    def _confirm_stop(self):
        goal, odom, c = self._active, self._odom, self.config
        if not goal or goal["state"] not in ("cancel_requested", "stopping"):
            return
        changed = goal["state"] != "stopping"
        goal["state"] = "stopping"
        if not goal["backend_settled"]:
            if changed:
                try:
                    self._save(goal)
                except sqlite3.Error:
                    goal["persistence_error"] = True
            return
        if self._fresh_reason(odom, c.odometry_max_age, c.heartbeat_max_age) or abs(odom["linear_speed"]) > c.stopped_linear_speed or abs(odom["angular_speed"]) > c.stopped_angular_speed:
            goal.update(stop_started=None, stop_samples=0, stop_last_stamp=None, stop_last_received=None, measured_stopped=False)
        elif goal["stop_last_stamp"] is None or odom["stamp"] > goal["stop_last_stamp"]:
            now = self._monotonic()
            if goal["stop_last_received"] is not None and not 0 <= now - goal["stop_last_received"] <= c.heartbeat_max_age:
                goal.update(stop_started=None, stop_samples=0)
            if goal["stop_started"] is None:
                goal["stop_started"] = now
            goal["stop_samples"] += 1
            goal["stop_last_stamp"] = odom["stamp"]
            goal["stop_last_received"] = now
            if goal["stop_samples"] >= c.stopped_min_samples and now - goal["stop_started"] >= c.stopped_dwell_seconds:
                goal["measured_stopped"] = True
        terminal = goal["measured_stopped"] and goal["backend_settled"]
        if terminal:
            goal.update(state=goal["pending_outcome"], stopped_confirmed=True)
        try:
            self._save(goal)
        except sqlite3.Error:
            goal.update(state="stopping", stopped_confirmed=False, persistence_error=True)
        else:
            if terminal:
                self._active = None

    def tick(self):
        with self._lock:
            self._require_open()
            self._supervise()
            if self._active and self._active["state"] in ("cancel_requested", "stopping") and self._fresh_reason(self._odom, self.config.odometry_max_age, self.config.heartbeat_max_age):
                self._active.update(stop_started=None, stop_samples=0, stop_last_stamp=None, stop_last_received=None, measured_stopped=False)
        self._flush()

    def renew(self, goal_id):
        with self._lock:
            self._require_open()
            self._supervise()
            if not self._active or self._active["goal_id"] != goal_id or self._active["state"] not in ("submitting", "running"):
                result = self._get(goal_id)
            else:
                self._active["lease_deadline"] = self._monotonic() + self._active["lease_seconds"]
                self._save(self._active)
                result = self._snapshot(self._active)
        self._flush()
        return result

    def status(self, goal_id=None, renew_lease=False):
        if renew_lease:
            if goal_id is None:
                raise ValueError("goal_id is required to renew a lease")
            return self.renew(goal_id)
        with self._lock:
            self._require_open()
            if goal_id is None:
                return {"readiness": self._readiness(), "active_goal": self._snapshot(self._active) if self._active else None}
            return self._get(goal_id)

    def request_status(self, request_id):
        with self._lock:
            self._require_open()
            if not isinstance(request_id, str) or not _REQUEST_ID.fullmatch(request_id):
                raise ValueError("invalid request_id")
            if self._active and self._active["request_id"] == request_id:
                return self._snapshot(self._active)
            row = self._db.execute("SELECT data FROM goals WHERE request_id=?", (request_id,)).fetchone()
            return json.loads(row[0]) if row else None

    def cancel(self, goal_id):
        with self._lock:
            self._require_open()
            if self._active and self._active["goal_id"] == goal_id:
                self._begin_stop("cancelled", "user_cancelled", cancel_requested=True)
                result = self._snapshot(self._active)
            else:
                result = self._get(goal_id)
        self._flush()
        return result

    def stop(self):
        with self._lock:
            self._require_open()
            if self._active:
                self._begin_stop("cancelled", "user_stop", cancel_requested=True)
            self._queue("set_enabled", False)
            self._queue("stop")
        self._flush()

    def _snapshot(self, goal):
        result = {key: value for key, value in goal.items() if key != "payload"}
        active = goal["state"] not in _TERMINAL
        now = self._monotonic()
        result["lease_remaining_seconds"] = max(0.0, goal["lease_deadline"] - now) if active else 0.0
        result["timeout_remaining_seconds"] = max(0.0, goal["goal_deadline"] - now) if active else 0.0
        return json.loads(json.dumps(result))

    def _get(self, goal_id):
        if self._active and self._active["goal_id"] == goal_id:
            return self._snapshot(self._active)
        row = self._db.execute("SELECT data FROM goals WHERE goal_id=?", (goal_id,)).fetchone()
        if row is None:
            raise ValueError("unknown goal_id")
        return json.loads(row[0])

    def _save(self, goal):
        with self._db:
            self._db.execute("INSERT INTO goals VALUES (?, ?, ?, ?, ?) ON CONFLICT(goal_id) DO UPDATE SET data=excluded.data, terminal=excluded.terminal", (goal["goal_id"], goal["request_id"], goal["payload"], json.dumps(self._snapshot(goal), allow_nan=False), int(goal["state"] in _TERMINAL)))

    def _prune(self):
        with self._db:
            self._db.execute("DELETE FROM goals WHERE terminal=1 AND goal_id NOT IN (SELECT goal_id FROM goals ORDER BY rowid DESC LIMIT ?)", (self.config.max_history,))

    def close(self):
        with self._lock:
            if self._closed:
                return
            if self._active:
                self._begin_stop("failed", "runtime_closed")
            self._queue("set_enabled", False)
            self._queue("stop")
            self._closed = True
        self._flush()
        with self._lock:
            self._db.close()

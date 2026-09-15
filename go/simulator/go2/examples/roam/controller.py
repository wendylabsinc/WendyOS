"""Transport-independent, conservative planar-scan roaming controller.

Call tick at 20 Hz and send its (forward m/s, yaw rad/s) command. Timestamps are
monotonic capture/receipt times, never simulation time. A stale or invalid scan
stops and disarms the controller; receiving another scan does not rearm it.
Unknown returns are blocked space. Distant unknown rear rays do not prevent
driving through a fully observed forward corridor. No reverse command is used.
"""

import math
from numbers import Real


def finite(value):
    return isinstance(value, Real) and not isinstance(value, bool) and math.isfinite(value)


def angle(value):
    return (value + math.pi) % (2 * math.pi) - math.pi


class RoamController:
    SCAN_TIMEOUT = 0.35
    POSE_TIMEOUT = 0.5
    # The Go2 ignores forward speed requests below 0.5 m/s.
    FORWARD_SPEED = 0.55
    TURN_SPEED = 0.4
    STOP_DISTANCE = 0.85
    CLEAR_DISTANCE = 1.15
    TURN_CLEARANCE = 0.35
    TURN_ANGLE = math.pi / 2
    TURN_TIMEOUT = 6.5
    PROGRESS_WINDOW = 3.0
    MIN_PROGRESS = 0.12

    def __init__(self, *, allow_scan_gaps=False):
        self.allow_scan_gaps = allow_scan_gaps
        self.active = False
        self.state = "stopped"
        self.reason = "not_started"
        self._scan_at = None
        self._scan = None
        self._pose_at = None
        self._pose = None
        self._last_tick = None
        self._command = (0.0, 0.0)
        self._turn_direction = 0
        self._preferred_direction = 1
        self._turn_started = None
        self._turn_yaw = None
        self._turn_paused = 0.0
        self._coverage_gap_at = None
        self._progress_at = None
        self._progress_pose = None
        self._recoveries = 0

    def observe_scan(self, ranges, angle_min, angle_increment, range_min, range_max, now):
        if not finite(now):
            self.stop("invalid_scan_time")
            return False
        if self._scan_at is not None and now <= self._scan_at:
            return False
        self._scan_at = float(now)
        self._scan = None
        try:
            readings = list(ranges) if not isinstance(ranges, (str, bytes)) else []
        except TypeError:
            readings = []
        headers = (angle_min, angle_increment, range_min, range_max)
        if (not all(finite(value) for value in headers)
                or not 0 < abs(angle_increment) <= math.radians(5.01)
                or not 0 <= range_min < range_max or not 36 <= len(readings) <= 8192
                or not 2 * math.pi - 2 * abs(angle_increment)
                <= (len(readings) - 1) * abs(angle_increment)
                <= 2 * math.pi + 2 * abs(angle_increment)):
            if self.active:
                self.stop("invalid_scan")
            return False
        if self.allow_scan_gaps and any(
                not (finite(value) and range_min <= value <= range_max) and value != math.inf
                for value in readings):
            self.stop("invalid_scan")
            return False
        rays = [(angle(angle_min + index * angle_increment),
                 float(value) if finite(value) and range_min <= value <= range_max else None)
                for index, value in enumerate(readings)]

        def sector(low, high):
            values = [distance for direction, distance in rays
                      if math.radians(low) - 1e-8 <= direction <= math.radians(high) + 1e-8]
            measured = [value for value in values if value is not None]
            complete = bool(values) and len(measured) == len(values)
            usable = bool(measured) and (complete or self.allow_scan_gaps)
            return {
                "complete": complete, "usable": usable,
                "observed": len(measured), "total": len(values),
                "clearance": min(measured) if usable else None,
                "observed_clearance": min(measured, default=None),
                "score": sum(min(value, 3.0) for value in measured) / len(measured) if usable else 0.0,
            }

        self._scan = {"front": sector(-25, 25), "left": sector(30, 100),
                      "right": sector(-100, -30), "left_sweep": sector(-10, 110),
                      "right_sweep": sector(-110, 10)}
        return True

    def observe_pose(self, x, y, yaw, now):
        if not finite(now) or (self._pose_at is not None and now <= self._pose_at):
            return False
        self._pose_at = float(now)
        if not all(finite(value) for value in (x, y, yaw)):
            self._pose = None
            return False
        self._pose = (float(x), float(y), angle(float(yaw)))
        return True

    def _scan_fresh(self, now):
        return (finite(now) and self._scan_at is not None
                and 0 <= now - self._scan_at <= self.SCAN_TIMEOUT)

    def _pose_fresh(self, now):
        return (self._pose is not None and self._pose_at is not None
                and 0 <= now - self._pose_at <= self.POSE_TIMEOUT)

    def start(self, now):
        if not self._scan_fresh(now):
            self.stop("stale_scan")
            return False
        if self._scan is None:
            self.stop("invalid_scan")
            return False
        if not self._scan["front"]["usable"]:
            self.stop("unknown_forward_path")
            return False
        self.active = True
        self.state, self.reason = "cruising", "exploring"
        self._last_tick = float(now)
        self._command = (0.0, 0.0)
        self._recoveries = 0
        self._turn_direction = 0
        self._coverage_gap_at = None
        self._progress_at = float(now)
        self._progress_pose = self._pose[:2] if self._pose_fresh(now) else None
        return True

    def stop(self, reason="stopped"):
        self.active = False
        self.state, self.reason = "stopped", str(reason)
        self._command = (0.0, 0.0)
        self._turn_direction = 0
        self._turn_started = self._turn_yaw = None
        self._turn_paused = 0.0
        self._coverage_gap_at = None
        self._progress_at = self._progress_pose = None

    def _begin_turn(self, now, reason):
        left, right = self._scan["left"], self._scan["right"]
        allowed = {}
        for direction, side, sweep in ((1, left, self._scan["left_sweep"]),
                                       (-1, right, self._scan["right_sweep"])):
            if (side["usable"] and side["clearance"] >= self.TURN_CLEARANCE
                    and sweep["usable"] and sweep["clearance"] >= self.TURN_CLEARANCE):
                allowed[direction] = side["score"]
        if not allowed:
            self.stop("no_clear_turn")
            return
        # A meaningful clearance advantage wins; close scores retain the prior
        # preference. Once chosen, a turn never flips side with noisy readings.
        if len(allowed) == 2 and abs(allowed[1] - allowed[-1]) < 0.25:
            direction = self._preferred_direction
        else:
            direction = max(allowed, key=allowed.get)
        self._turn_direction = direction
        self._preferred_direction = direction
        self._turn_started = float(now)
        self._turn_paused = 0.0
        self._turn_yaw = self._pose[2] if self._pose_fresh(now) else None
        self.state, self.reason = "turning", reason
        self._progress_at = self._progress_pose = None

    def tick(self, now):
        if not finite(now) or (self._last_tick is not None and now < self._last_tick):
            self.stop("invalid_clock")
            return self._command
        self._last_tick = float(now)
        if not self.active:
            return (0.0, 0.0)
        if not self._scan_fresh(now):
            self.stop("stale_scan")
            return self._command
        if self._scan is None:
            self.stop("invalid_scan")
            return self._command
        # Native lidar clouds can briefly omit the entire forward sector.
        # Wait at zero velocity in gap mode; a prolonged loss still disarms.
        # Check the deadline before accepting a recovered scan so a delayed
        # tick cannot silently resume after the gap has expired.
        if (self._coverage_gap_at is not None
                and now - self._coverage_gap_at >= self.SCAN_TIMEOUT):
            self.stop("unknown_forward_path")
            return self._command
        if not self._scan["front"]["usable"]:
            if self.allow_scan_gaps:
                if self._coverage_gap_at is None:
                    self._coverage_gap_at = float(now)
                self._command = (0.0, 0.0)
            else:
                self.stop("unknown_forward_path")
            return self._command
        if self._coverage_gap_at is not None:
            if self.state == "turning":
                self._turn_paused += now - self._coverage_gap_at
            self._coverage_gap_at = None
            self._progress_at = self._progress_pose = None
        front = self._scan["front"]["clearance"]
        if self.state == "cruising":
            if front <= self.STOP_DISTANCE:
                self._begin_turn(now, "obstacle")
            elif self._pose_fresh(now):
                if self._progress_pose is None:
                    self._progress_at, self._progress_pose = float(now), self._pose[:2]
                elif now - self._progress_at >= self.PROGRESS_WINDOW:
                    distance = math.hypot(self._pose[0] - self._progress_pose[0],
                                          self._pose[1] - self._progress_pose[1])
                    if distance < self.MIN_PROGRESS:
                        self._recoveries += 1
                        if self._recoveries > 2:
                            self.stop("stuck")
                        else:
                            self._begin_turn(now, "stuck_recovery")
                    else:
                        self._recoveries = 0
                        self._progress_at, self._progress_pose = float(now), self._pose[:2]
        if self.state == "turning":
            sweep = self._scan["left_sweep" if self._turn_direction > 0 else "right_sweep"]
            # The initial turn sector met the configured coverage requirement.
            # Rotation can bring distant, out-of-range rear rays into that
            # sector; they never justify forward motion or a new turn choice.
            # Keep the forward corridor known and stop for any close return.
            if sweep["observed_clearance"] is None or sweep["observed_clearance"] < self.TURN_CLEARANCE:
                self.stop("turn_path_blocked")
                return self._command
            elapsed = now - self._turn_started
            progress = max(0.0, elapsed - self._turn_paused) * self.TURN_SPEED
            if self._turn_yaw is not None and self._pose_fresh(now):
                progress = self._turn_direction * angle(self._pose[2] - self._turn_yaw)
            if progress >= self.TURN_ANGLE and front >= self.CLEAR_DISTANCE:
                self.state, self.reason = "cruising", "exploring"
                self._turn_direction = 0
                self._progress_at = float(now)
                self._progress_pose = self._pose[:2] if self._pose_fresh(now) else None
            elif elapsed >= self.TURN_TIMEOUT:
                self.stop("turn_timeout")
        if not self.active:
            self._command = (0.0, 0.0)
        elif self.state == "turning":
            self._command = (0.0, self._turn_direction * self.TURN_SPEED)
        else:
            self._command = (self.FORWARD_SPEED, 0.0)
        return self._command

    def status(self, now):
        def age(timestamp):
            return max(0.0, float(now) - timestamp) if finite(now) and timestamp is not None else None

        def clearance(side):
            return self._scan[side]["clearance"] if self._scan else None

        return {
            "active": self.active,
            "state": "waiting_scan" if self._coverage_gap_at is not None else self.state,
            "reason": "waiting_for_front_returns" if self._coverage_gap_at is not None else self.reason,
            "allow_scan_gaps": self.allow_scan_gaps,
            "scan_coverage": {name: {"observed": sector["observed"], "total": sector["total"]}
                              for name, sector in self._scan.items()} if self._scan else None,
            "scan_age": age(self._scan_at), "pose_age": age(self._pose_at),
            "front_clearance": clearance("front"), "left_clearance": clearance("left"),
            "right_clearance": clearance("right"),
            "turn_direction": "left" if self._turn_direction > 0 else "right" if self._turn_direction < 0 else None,
            "linear": self._command[0], "angular": self._command[1],
        }

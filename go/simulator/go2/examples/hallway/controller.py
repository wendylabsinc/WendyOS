"""Bounded hallway exploration using body-frame lidar and odometry only."""
import math
from numbers import Real


def finite(value):
    return isinstance(value, Real) and not isinstance(value, bool) and math.isfinite(value)


def angle(value):
    return math.atan2(math.sin(value), math.cos(value))


class HallwayController:
    SENSOR_TIMEOUT = 0.35
    SPEED = 0.55
    TURN_SPEED = 0.35
    FRONT_STOP = 0.85
    SIDE_STOP = 0.36
    TURN_RADIUS = 0.48
    MAX_SECONDS = 180.0
    MAX_DISTANCE = 20.0

    def __init__(self, *, max_distance=MAX_DISTANCE, max_seconds=MAX_SECONDS, prefer="left"):
        if not finite(max_distance) or not 0.5 <= max_distance <= 50:
            raise ValueError("max_distance must be between 0.5 and 50 metres")
        if not finite(max_seconds) or not 5 <= max_seconds <= 300:
            raise ValueError("max_seconds must be between 5 and 300 seconds")
        if prefer not in ("left", "right"):
            raise ValueError("prefer must be left or right")
        self.max_distance, self.max_seconds, self.prefer = max_distance, max_seconds, prefer
        self.active = False
        self.state, self.reason = "stopped", "not_started"
        self.pose = self.pose_at = self.scan_at = None
        self.points = self.scan = None
        self.command = (0.0, 0.0)
        self.started_at = self.last_tick = None
        self.distance = 0.0
        self.visits = {}
        self.turn_target = self.turn_at = None
        self.corridor_heading = None
        self.turns = 0
        self.progress_pose = self.progress_at = None

    def stop(self, reason):
        self.active = False
        self.state, self.reason = "stopped", reason
        self.command = (0.0, 0.0)

    def observe_pose(self, x, y, yaw, now):
        if (not all(finite(v) for v in (x, y, yaw, now))
                or self.pose_at is not None and now <= self.pose_at):
            self.pose = self.pose_at = None
            self.stop("invalid_odometry")
            return False
        if self.pose is not None:
            step = math.dist((x, y), self.pose[:2])
            if step > max(0.15, 1.0 * (now - self.pose_at)):
                self.pose = self.pose_at = None
                self.stop("odometry_jump")
                return False
            if self.active:
                self.distance += step
        self.pose, self.pose_at = (x, y, angle(yaw)), now
        if self.active:
            cell = (round(x), round(y))
            self.visits[cell] = self.visits.get(cell, 0) + 1
        return True

    def reject_scan(self, reason):
        self.scan = self.points = self.scan_at = None
        self.stop(reason)
        return False

    def observe_scan(self, ranges, angle_min, increment, now):
        if (not all(finite(v) for v in (angle_min, increment, now))
                or not 0 < increment <= math.radians(5.01)
                or self.scan_at is not None and now <= self.scan_at):
            return self.reject_scan("invalid_scan")
        ranges = list(ranges)
        if (not 72 <= len(ranges) <= 8192
                or abs(len(ranges) * increment - 2 * math.pi) > 1e-3
                or any(not (finite(v) and 0 < v <= 12 or v == math.inf) for v in ranges)):
            return self.reject_scan("invalid_scan")
        rays = [(angle(angle_min + i * increment), v) for i, v in enumerate(ranges)]
        points = [(r * math.cos(a), r * math.sin(a)) for a, r in rays if finite(r)]

        def sector(degrees, half_width=10):
            values = [r for a, r in rays if abs(angle(a - math.radians(degrees))) <= math.radians(half_width) + 1e-8]
            valid = [r for r in values if finite(r)]
            # A partial cloud can be useful, but an entirely unobserved sector cannot authorize motion.
            return min(valid) if len(valid) >= max(1, math.ceil(len(values) * 0.6)) else None

        left = [y for x, y in points if -.35 <= x <= .40 and y > .15]
        right = [-y for x, y in points if -.35 <= x <= .40 and y < -.15]
        front = [x for x, y in points if x > 0 and abs(y) <= .32]
        self.scan = {"front": min(front, default=None) if sector(0) is not None else None,
                     "left": min(left, default=None), "right": min(right, default=None),
                     "left_exit": sector(90), "right_exit": sector(-90),
                     "radius": min((math.hypot(x, y) for x, y in points), default=None),
                     "observed": len(points), "total": len(ranges)}
        self.points, self.scan_at = points, now
        if self.active and self._body_blocked():
            self.stop("side_obstacle")
        return True

    def observation_error(self, now):
        if not finite(now):
            return "invalid_clock"
        for key, stamp in (("scan", self.scan_at), ("odom", self.pose_at)):
            if stamp is None or not 0 <= now - stamp < self.SENSOR_TIMEOUT:
                return "stale_" + key
        if self.scan is None or self.pose is None:
            return "missing_observation"
        if any(self.scan[key] is None for key in ("front", "left", "right")):
            return "unknown_corridor"
        return None

    def _body_blocked(self):
        return any(self.scan[key] is not None and self.scan[key] <= self.SIDE_STOP for key in ("left", "right"))

    def start(self, now):
        error = self.observation_error(now)
        if error or self._body_blocked():
            self.stop(error or "side_obstacle")
            return False
        if self.active:
            return True
        self.active, self.state, self.reason = True, "exploring", "following_corridor"
        self.started_at = self.progress_at = now
        self.progress_pose = self.pose[:2]
        self.corridor_heading = self.pose[2]
        self.distance, self.turns, self.visits = 0.0, 0, {}
        self.turn_target = None
        return True

    def _choose_turn(self, now):
        if self.scan["radius"] is None or self.scan["radius"] <= self.TURN_RADIUS:
            self.stop("insufficient_turn_space")
            return
        candidates = []
        for name, sign in (("left", 1), ("right", -1)):
            clearance = self.scan[name + "_exit"]
            if clearance is not None and clearance > 1.25:
                heading = self.pose[2] + sign * math.pi / 2
                cell = (round(self.pose[0] + math.cos(heading)), round(self.pose[1] + math.sin(heading)))
                candidates.append((self.visits.get(cell, 0), name != self.prefer, sign))
        if not candidates:
            self.stop("blocked_or_dead_end")
            return
        sign = min(candidates)[2]
        self.turn_target = angle(self.pose[2] + sign * math.pi / 2)
        self.turn_at, self.state, self.reason = now, "turning", "taking_branch"
        self.turns += 1

    def tick(self, now):
        self.command = (0.0, 0.0)
        if not finite(now) or self.last_tick is not None and now < self.last_tick:
            self.stop("invalid_clock")
            return self.command
        self.last_tick = now
        if not self.active:
            return self.command
        error = self.observation_error(now)
        if error:
            self.stop(error)
            return self.command
        if self.distance >= self.max_distance or now - self.started_at >= self.max_seconds:
            self.stop("exploration_limit")
            return self.command
        if self._body_blocked():
            self.stop("side_obstacle")
            return self.command
        if self.turn_target is None and self.scan["front"] <= self.FRONT_STOP:
            self._choose_turn(now)
        if not self.active:
            return self.command
        if self.turn_target is not None:
            if self.scan["radius"] <= self.TURN_RADIUS:
                self.stop("turn_obstacle")
                return self.command
            error = angle(self.turn_target - self.pose[2])
            if now - self.turn_at < .6:
                return self.command
            if now - self.turn_at > 10:
                self.stop("turn_timeout")
                return self.command
            if abs(error) > .10:
                self.command = (0.0, math.copysign(self.TURN_SPEED, error))
                return self.command
            self.corridor_heading = self.turn_target
            self.turn_target = None
            self.progress_pose, self.progress_at = self.pose[:2], now
            return self.command
        if now - self.progress_at > 5:
            if math.dist(self.pose[:2], self.progress_pose) < .15:
                self.stop("stuck")
                return self.command
            self.progress_pose, self.progress_at = self.pose[:2], now
        # Fit the nearby corridor walls in body coordinates. Their slope reveals
        # heading error; their separation supplies a small centring correction.
        slopes = []
        for sign in (1, -1):
            wall = [(x, y) for x, y in self.points if -.2 < x < 1.2 and .2 < sign*y < 1.2]
            if len(wall) >= 3:
                mx = sum(x for x, _ in wall) / len(wall)
                my = sum(y for _, y in wall) / len(wall)
                variance = sum((x-mx)**2 for x, _ in wall)
                if variance > .03:
                    slopes.append(math.atan(sum((x-mx)*(y-my) for x, y in wall) / variance))
        heading = sum(slopes) / len(slopes) if slopes else 0.0
        centre = max(-.2, min(.2, self.scan["left"] - self.scan["right"]))
        if max(self.scan["left"], self.scan["right"]) > 1.2:
            heading = angle(self.corridor_heading - self.pose[2])
            centre = 0.0
        yaw = max(-.3, min(.3, 1.5 * heading + .8 * centre))
        self.command = (self.SPEED, yaw)
        self.state, self.reason = "exploring", "following_corridor"
        return self.command

    def status(self, now):
        return {"active": self.active, "state": self.state, "reason": self.reason,
                "distance_m": self.distance, "turns": self.turns, "visited_cells": len(self.visits),
                "command": self.command, "scan": self.scan,
                "scan_age": None if self.scan_at is None else max(0, now-self.scan_at),
                "odom_age": None if self.pose_at is None else max(0, now-self.pose_at)}

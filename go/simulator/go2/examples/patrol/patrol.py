"""A finite waypoint patrol with no ROS or simulator dependencies."""

import math
from numbers import Real


DEFAULT_WAYPOINTS = ((1.0, 0.0), (1.0, 1.0), (0.0, 1.0), (0.0, 0.0))
OBSERVATION_TIMEOUT = 0.35


def finite(value):
    return isinstance(value, Real) and not isinstance(value, bool) and math.isfinite(value)


def angle(value):
    return (value + math.pi) % (2 * math.pi) - math.pi


def validate_route(waypoints, laps):
    if isinstance(laps, bool) or not isinstance(laps, int) or not 1 <= laps <= 5:
        raise ValueError("laps must be an integer from 1 to 5")
    if not isinstance(waypoints, (list, tuple)) or not 1 <= len(waypoints) <= 16:
        raise ValueError("provide 1 to 16 [forward, left] waypoints")
    route = []
    for point in waypoints:
        if (not isinstance(point, (list, tuple)) or len(point) != 2
                or not all(finite(value) for value in point)
                or math.hypot(*point) > 3.0):
            raise ValueError("each waypoint must be a finite [forward, left] pair within 3 m of the start")
        route.append(tuple(float(value) for value in point))
    previous, length = (0.0, 0.0), 0.0
    for point in route * laps:
        length += math.dist(previous, point)
        previous = point
    if not 0.25 < length <= 30.0:
        raise ValueError("total route length must be greater than 0.25 m and at most 30 m")
    return tuple(route)


class PatrolController:
    """Follow a bounded route once; every fault requires an explicit new start."""

    SPEED = 0.35
    TURN_SPEED = 0.4
    ARRIVAL_DISTANCE = 0.25
    FRONT_CLEARANCE = 0.85
    BODY_CLEARANCE = 0.55
    ROOM_LIMIT = 5.0
    WAYPOINT_TIMEOUT = 45.0
    ROUTE_TIMEOUT = 300.0

    def __init__(self, waypoints=DEFAULT_WAYPOINTS, laps=1):
        self.waypoints = validate_route(waypoints, laps)
        self.laps = laps
        self.active = False
        self.state, self.reason = "stopped", "not_started"
        self.command = (0.0, 0.0)
        self.pose = self.pose_at = self.scan_at = None
        self.front_clearance = self.body_clearance = None
        self.targets, self.index = (), 0
        self.started_at = self.waypoint_at = self.last_tick = None
        self.distance = None

    def stop(self, reason="stopped"):
        self.active = False
        self.state, self.reason = "stopped", reason
        self.command = (0.0, 0.0)

    def observe_pose(self, x, y, yaw, captured):
        if (not all(finite(value) for value in (x, y, yaw, captured))
                or (self.pose_at is not None and captured <= self.pose_at)):
            self.pose = self.pose_at = None
            self.stop("invalid_odometry")
            return False
        self.pose = (float(x), float(y), angle(float(yaw)))
        self.pose_at = float(captured)
        if max(abs(x), abs(y)) > self.ROOM_LIMIT:
            self.stop("outside_patrol_bounds")
            return False
        return True

    def observe_scan(self, ranges, angle_min, increment, minimum, maximum, captured):
        if (not all(finite(value) for value in (angle_min, increment, minimum, maximum, captured))
                or not 0 < increment <= math.radians(5.01)
                or not 0 <= minimum < maximum
                or (self.scan_at is not None and captured <= self.scan_at)):
            return self.reject_scan("invalid_scan")
        if not isinstance(ranges, (list, tuple)):
            try:
                ranges = list(ranges)
            except TypeError:
                return self.reject_scan("invalid_scan")
        if (not 72 <= len(ranges) <= 8192
                or not 2 * math.pi - increment - 1e-4
                <= (len(ranges) - 1) * increment <= 2 * math.pi + 1e-4):
            return self.reject_scan("invalid_scan_geometry")
        # A patrol may turn either way. Require the whole horizontal circle to
        # be observed; infinity/NaN are unknown, not invented clear space.
        if not all(finite(value) and minimum <= value <= maximum for value in ranges):
            return self.reject_scan("unknown_scan")
        front = [value for index, value in enumerate(ranges)
                 if abs(angle(angle_min + index * increment)) <= math.radians(30) + 1e-8]
        if not front:
            return self.reject_scan("invalid_scan_geometry")
        self.front_clearance, self.body_clearance = float(min(front)), float(min(ranges))
        self.scan_at = float(captured)
        if self.active and self.blocked():
            self.stop("obstacle")
        return True

    def reject_scan(self, reason):
        self.scan_at = self.front_clearance = self.body_clearance = None
        self.stop(reason)
        return False

    def blocked(self):
        return (self.front_clearance is None or self.body_clearance is None
                or self.front_clearance <= self.FRONT_CLEARANCE
                or self.body_clearance <= self.BODY_CLEARANCE)

    def observation_error(self, now):
        if not finite(now):
            return "invalid_clock"
        for topic, captured in (("scan", self.scan_at), ("odom", self.pose_at)):
            if captured is None or not 0 <= now - captured < OBSERVATION_TIMEOUT:
                return "stale_" + topic
        if self.pose is None:
            return "invalid_odometry"
        if max(abs(self.pose[0]), abs(self.pose[1])) > self.ROOM_LIMIT:
            return "outside_patrol_bounds"
        if self.blocked():
            return "obstacle"
        return None

    def start(self, now):
        error = self.observation_error(now)
        if error:
            self.stop(error)
            return False
        if self.active:
            return True
        x, y, heading = self.pose
        cosine, sine = math.cos(heading), math.sin(heading)
        targets = tuple((x + forward * cosine - left * sine,
                         y + forward * sine + left * cosine)
                        for forward, left in self.waypoints) * self.laps
        if any(max(abs(tx), abs(ty)) > self.ROOM_LIMIT for tx, ty in targets):
            self.stop("route_outside_patrol_bounds")
            return False
        self.targets, self.index = targets, 0
        self.active = True
        self.state, self.reason = "running", "started"
        self.started_at = self.waypoint_at = self.last_tick = float(now)
        self.command = (0.0, 0.0)
        self.distance = None
        return True

    def tick(self, now):
        self.command = (0.0, 0.0)
        if not finite(now) or (self.last_tick is not None and now < self.last_tick):
            self.stop("invalid_clock")
            return self.command
        self.last_tick = float(now)
        if not self.active:
            return self.command
        error = self.observation_error(now)
        if error:
            self.stop(error)
            return self.command
        if now - self.started_at >= self.ROUTE_TIMEOUT:
            self.stop("route_timeout")
            return self.command
        if now - self.waypoint_at >= self.WAYPOINT_TIMEOUT:
            self.stop("waypoint_timeout")
            return self.command
        x, y, heading = self.pose
        while self.index < len(self.targets):
            dx, dy = self.targets[self.index][0] - x, self.targets[self.index][1] - y
            self.distance = math.hypot(dx, dy)
            if self.distance > self.ARRIVAL_DISTANCE:
                break
            self.index += 1
            self.waypoint_at = float(now)
        if self.index == len(self.targets):
            self.stop("route_complete")
            self.state = "complete"
            return self.command
        error = angle(math.atan2(dy, dx) - heading)
        angular = max(-self.TURN_SPEED, min(self.TURN_SPEED, 1.4 * error))
        # .35 m/s sustains the simulator walking policy. Turn in place first
        # and use the explicit arrival radius rather than creeping indefinitely.
        linear = self.SPEED if abs(error) < 0.25 else 0.0
        self.command = (linear, angular)
        self.state, self.reason = ("walking" if linear else "turning"), "following_waypoint"
        return self.command

    def status(self, now):
        def age(captured):
            return max(0.0, now - captured) if finite(now) and captured is not None else None

        return {"active": self.active, "state": self.state, "reason": self.reason,
                "waypoint": self.index + 1 if self.index < len(self.targets) else None,
                "waypoint_count": len(self.targets), "laps": self.laps,
                "target": self.targets[self.index] if self.index < len(self.targets) else None,
                "distance": self.distance, "linear": self.command[0], "angular": self.command[1],
                "front_clearance": self.front_clearance, "body_clearance": self.body_clearance,
                "scan_age": age(self.scan_at), "odom_age": age(self.pose_at)}

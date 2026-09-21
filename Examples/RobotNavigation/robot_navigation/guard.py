"""Independent velocity interlock; importing this module cannot access hardware."""

from dataclasses import dataclass
import math
import time


@dataclass(frozen=True)
class GuardConfig:
    # The circle must enclose the robot throughout its commissioned gait.
    footprint_radius: float = 0.55
    clearance: float = 0.25
    max_linear: float = 0.25
    max_angular: float = 0.4
    reaction_seconds: float = 0.35
    braking_deceleration: float = 0.3
    sensor_timeout: float = 0.3
    command_timeout: float = 0.2
    permit_timeout: float = 0.3
    base_frame: str = "base_link"
    allow_infinite_ranges: bool = False
    commissioned: bool = False
    max_scan_increment: float = 0.02

    def __post_init__(self):
        for name in ("footprint_radius", "clearance", "max_linear", "max_angular",
                     "reaction_seconds", "braking_deceleration", "sensor_timeout",
                     "command_timeout", "permit_timeout", "max_scan_increment"):
            value = getattr(self, name)
            if isinstance(value, bool) or not math.isfinite(value) or value <= 0:
                raise ValueError(f"{name} must be positive and finite")
        if not self.base_frame:
            raise ValueError("base_frame is required")
        if self.reaction_seconds < max(self.sensor_timeout, self.command_timeout, self.permit_timeout):
            raise ValueError("reaction_seconds must cover all sensor, command and permit timeouts")
        if type(self.commissioned) is not bool or type(self.allow_infinite_ranges) is not bool:
            raise ValueError("commissioned and allow_infinite_ranges must be booleans")


class VelocityGuard:
    """Single-thread owner. ROS callbacks and its timer use one executor thread.

    Requires a complete, valid horizontal scan and monotonic command/permit
    receipts. Sensor acquisition times use the injected ROS clock separately.
    A stop latch can only be reset locally while measured motion is stopped.
    """

    def __init__(self, config=GuardConfig(), *, source_clock=time.time, clock=time.monotonic):
        self.config, self.source_clock, self.clock = config, source_clock, clock
        self.scan = None
        self.odom = None
        self.command = None
        self.permit = None
        self.latched = False
        self.last_reason = "no_sensor_data"
        self.scan_highwater = self.odom_highwater = None
        self.command_invalid = False

    def update_scan(self, *, stamp, frame_id, angle_min, angle_increment,
                    ranges, range_min, range_max, origin=(0.0, 0.0, 0.0)):
        self.scan = None
        values = (stamp, angle_min, angle_increment, range_min, range_max, *origin)
        if not all(_finite(v) for v in values) or stamp < 0 or stamp > self.source_clock():
            return
        if self.scan_highwater is not None and stamp <= self.scan_highwater:
            return
        self.scan_highwater = stamp
        if frame_id != self.config.base_frame or not 8 <= len(ranges) <= 8192:
            return
        if angle_increment <= 0 or range_min < 0 or range_max <= range_min:
            return
        if angle_increment > self.config.max_scan_increment:
            return  # Nominal 360-degree coverage still has gaps between rays.
        if math.hypot(origin[0], origin[1]) + range_min > self.config.footprint_radius:
            return  # A blind near-field region outside the body is unknown.
        if (len(ranges) - 1) * angle_increment < 2 * math.pi - 1.5 * angle_increment:
            return  # Partial coverage is unknown even if all returned rays are clear.
        if len(ranges) * angle_increment > 2 * math.pi + 2 * angle_increment:
            return
        points = []
        ox, oy, yaw = origin
        for i, distance in enumerate(ranges):
            if distance == math.inf and self.config.allow_infinite_ranges:
                distance = range_max
            if not _finite(distance) or not range_min <= distance <= range_max:
                return
            angle = angle_min + i * angle_increment + yaw
            points.append((ox + distance * math.cos(angle), oy + distance * math.sin(angle)))
        self.scan = (stamp, self.clock(), points)

    def update_odometry(self, *, stamp, frame_id, linear_speed, angular_speed):
        self.odom = None
        if not all(_finite(v) for v in (stamp, linear_speed, angular_speed)) or stamp < 0 or stamp > self.source_clock():
            return
        if self.odom_highwater is not None and stamp <= self.odom_highwater:
            return
        self.odom_highwater = stamp
        if frame_id != self.config.base_frame:
            return
        self.odom = (stamp, self.clock(), abs(linear_speed), abs(angular_speed))

    def update_command(self, vx, vy, wz):
        if not all(_finite(v) for v in (vx, vy, wz)):
            self.command = None
            self.command_invalid = True
            return
        # Unsupported lateral motion is rejected; never silently reinterpret it.
        if vy != 0 or abs(vx) > self.config.max_linear or abs(wz) > self.config.max_angular:
            self.command = None
            self.command_invalid = True
            return
        if self.permit is None or not 0 <= self.clock() - self.permit[0] <= self.config.permit_timeout:
            self.command = None
            return
        self.command = (self.clock(), vx, wz)
        self.command_invalid = False

    def update_permit(self, enabled, max_speed=None):
        if enabled is not True:
            self.permit = None
            self.command = None  # A subsequent permit cannot replay an older command.
            self.command_invalid = False
            return
        speed = self.config.max_linear if max_speed is None else max_speed
        if not _finite(speed) or not 0 < speed <= self.config.max_linear:
            self.permit = None
            self.command = None
            return
        if self.permit is None or not 0 <= self.clock() - self.permit[0] <= self.config.permit_timeout:
            self.command = None
            self.command_invalid = False
        self.permit = (self.clock(), speed)

    def stop(self):
        self.latched = True
        self.permit = self.command = None

    def reset_local(self):
        if self.sensor_problem() or self.odom[2] > 0.02 or self.odom[3] > 0.03:
            raise RuntimeError("fresh, stopped odometry is required to reset the stop latch")
        self.latched = False
        self.permit = self.command = None

    def sensor_problem(self):
        for name, sample in (("scan", self.scan), ("odometry", self.odom)):
            if sample is None:
                return f"{name}_unknown"
            age, receipt_age = self.source_clock() - sample[0], self.clock() - sample[1]
            if not 0 <= age <= self.config.sensor_timeout or not 0 <= receipt_age <= self.config.sensor_timeout:
                return f"{name}_stale_or_clock_invalid"
        return None

    def evaluate(self):
        problem = self.sensor_problem()
        if not self.config.commissioned:
            problem = "guard_not_commissioned"
        if self.latched:
            problem = "stop_latched"
        # Check obstacles and measured motion even without a permit/command.
        # A circle expanded in every direction avoids assuming an old scan has
        # retained its orientation while the robot moved or turned.
        vx = wz = 0.0
        if problem is None:
            if self.odom[2] > self.config.max_linear or self.odom[3] > self.config.max_angular:
                problem = "measured_speed_exceeded"
            else:
                commanded_speed = abs(self.command[1]) if self.command else self.config.max_linear
                speed = max(commanded_speed, self.odom[2])
                scan_age = max(self.source_clock() - self.scan[0], self.clock() - self.scan[1])
                # Current feedback cannot reconstruct motion since acquisition.
                # Bound that unknown travel by the commissioned maximum speed.
                reach = self.config.max_linear * scan_age + speed * self.config.reaction_seconds + speed * speed / (2 * self.config.braking_deceleration)
                radius = self.config.footprint_radius + self.config.clearance + reach
                if any(math.hypot(x, y) <= radius for x, y in self.scan[2]):
                    problem = "obstacle_in_stopping_envelope"
            if self.command_invalid:
                problem = "invalid_command"
        ready = problem is None
        if problem is None:
            if self.permit is None or not 0 <= self.clock() - self.permit[0] <= self.config.permit_timeout:
                problem = "permit_expired"
            elif self.command is None or not 0 <= self.clock() - self.command[0] <= self.config.command_timeout:
                problem = "command_expired"
            else:
                _, vx, wz = self.command
                if abs(vx) > self.permit[1]:
                    problem = "goal_speed_exceeded"
                    ready = False
        if problem is not None:
            vx = wz = 0.0
        self.last_reason = problem or "clear"
        return {"ready": ready, "stop_latched": self.latched, "reason": self.last_reason,
                "linear_x": vx, "angular_z": wz, "source_stamp": self.source_clock()}


def _finite(value):
    return not isinstance(value, bool) and isinstance(value, (int, float)) and math.isfinite(value)

"""Deterministic planar Ackermann model. SI units, rear-axle base_link.

This is an ideal rolling model, not a tire/contact dynamics model. Geometry is
an explicit approximation until measured R2 calibration data is available.
"""

import math
from dataclasses import asdict, dataclass


@dataclass(frozen=True)
class Geometry:
    wheelbase: float = 0.26
    track: float = 0.24
    wheel_radius: float = 0.05
    length: float = 0.40
    width: float = 0.32
    max_speed: float = 1.8
    max_steering: float = math.radians(30)
    acceleration: float = 1.2
    braking: float = 2.4
    steering_rate: float = math.radians(90)


OBSTACLES = (
    {"x": 2.5, "y": 0.0, "width": 0.8, "depth": 1.8, "height": 0.65},
    {"x": -1.6, "y": 2.0, "width": 1.5, "depth": 0.7, "height": 0.9},
    {"x": 1.0, "y": -2.2, "width": 0.7, "depth": 0.7, "height": 0.5},
    {"x": -2.8, "y": -1.2, "width": 0.6, "depth": 1.4, "height": 0.7},
)
ROOM_HALF_SIZE = 5.0
LIDAR_OFFSET = (0.13, 0.0, 0.28)
SCAN_COUNT = 360
SCAN_MIN = 0.08
SCAN_MAX = 12.0


def number(value):
    if isinstance(value, bool) or not isinstance(value, (float, int)) or not math.isfinite(value):
        raise ValueError("commands must contain finite numbers")
    return float(value)


def clamp(value, limit):
    return max(-limit, min(limit, value))


def approach(current, target, step):
    return current + clamp(target - current, step)


class Simulation:
    def __init__(self, geometry=None, obstacles=OBSTACLES):
        self.geometry = geometry or Geometry()
        self.obstacles = [dict(item) for item in obstacles]
        self.reset()

    def reset(self):
        self.x = self.y = self.yaw = self.speed = self.steering = 0.0
        self.target_speed = self.target_steering = self.yaw_rate = 0.0
        self.acceleration = self.lateral_acceleration = self.distance = self.elapsed = 0.0
        self.wheel_positions = [0.0] * 4
        self.wheel_velocities = [0.0] * 4
        self.collision = False

    def drive(self, speed, steering):
        speed, steering = number(speed), number(steering)
        self.target_speed = clamp(speed, self.geometry.max_speed)
        self.target_steering = clamp(steering, self.geometry.max_steering)

    def twist(self, forward, lateral, yaw_rate):
        forward, lateral, yaw_rate = map(number, (forward, lateral, yaw_rate))
        if abs(lateral) > 1e-6:
            raise ValueError("R2 cannot move sideways; linear.y must be zero")
        # Preserve curvature when bounding speed. A zero-speed yaw command stops.
        steering = math.atan(self.geometry.wheelbase * yaw_rate / forward) if abs(forward) > 1e-6 else 0.0
        self.drive(forward, steering)

    def stop(self):
        self.target_speed = self.speed = self.yaw_rate = 0.0
        self.target_steering = self.steering
        self.acceleration = self.lateral_acceleration = 0.0
        self.wheel_velocities = [0.0] * 4

    def wheel_state(self):
        g = self.geometry
        curvature = math.tan(self.steering) / g.wheelbase
        left, right = 1 - curvature * g.track / 2, 1 + curvature * g.track / 2
        angles = [math.atan2(g.wheelbase * curvature, left),
                  math.atan2(g.wheelbase * curvature, right)]
        velocities = [self.speed * math.hypot(left, g.wheelbase * curvature) / g.wheel_radius,
                      self.speed * math.hypot(right, g.wheelbase * curvature) / g.wheel_radius,
                      self.speed * left / g.wheel_radius, self.speed * right / g.wheel_radius]
        return angles, velocities

    def intersects(self, x, y, yaw):
        g = self.geometry
        c, s = math.cos(yaw), math.sin(yaw)
        center = (x + c * g.wheelbase / 2, y + s * g.wheelbase / 2)
        axes = ((c, s), (-s, c), (1, 0), (0, 1))
        corners = [(center[0] + c * dx - s * dy, center[1] + s * dx + c * dy)
                   for dx in (-g.length / 2, g.length / 2) for dy in (-g.width / 2, g.width / 2)]
        if any(abs(px) >= ROOM_HALF_SIZE or abs(py) >= ROOM_HALF_SIZE for px, py in corners):
            return True
        for box in self.obstacles:
            dx, dy = box["x"] - center[0], box["y"] - center[1]
            for ax, ay in axes:
                car_radius = g.length / 2 * abs(ax * c + ay * s) + g.width / 2 * abs(-ax * s + ay * c)
                box_radius = box["width"] / 2 * abs(ax) + box["depth"] / 2 * abs(ay)
                if abs(dx * ax + dy * ay) > car_radius + box_radius:
                    break
            else:
                return True
        return False

    def step(self, dt):
        dt = number(dt)
        if not 0 < dt <= 0.1:
            raise ValueError("step must be greater than zero and at most 0.1 seconds")
        # Small substeps prevent tunneling through obstacles even at full speed.
        steps = math.ceil(dt / 0.005)
        for _ in range(steps):
            self._step(dt / steps)

    def _step(self, dt):
        g = self.geometry
        previous_speed = self.speed
        # Brake before reversing, with a stronger deceleration than acceleration.
        braking = self.speed * self.target_speed < 0 or abs(self.target_speed) < abs(self.speed)
        target = 0. if self.speed * self.target_speed < 0 else self.target_speed
        self.speed = approach(self.speed, target, (g.braking if braking else g.acceleration) * dt)
        self.steering = approach(self.steering, self.target_steering, g.steering_rate * dt)
        omega = self.speed * math.tan(self.steering) / g.wheelbase
        yaw = self.yaw + omega * dt
        if abs(omega) > 1e-9:
            x = self.x + self.speed / omega * (math.sin(yaw) - math.sin(self.yaw))
            y = self.y - self.speed / omega * (math.cos(yaw) - math.cos(self.yaw))
        else:
            x = self.x + self.speed * dt * math.cos(self.yaw)
            y = self.y + self.speed * dt * math.sin(self.yaw)
        self.collision = self.intersects(x, y, yaw)
        if self.collision:
            self.speed = omega = 0.0
        else:
            self.x, self.y, self.yaw = x, y, math.atan2(math.sin(yaw), math.cos(yaw))
        self.yaw_rate = omega
        self.acceleration = (self.speed - previous_speed) / dt
        self.lateral_acceleration = self.speed * omega
        _, self.wheel_velocities = self.wheel_state()
        self.wheel_positions = [p + v * dt for p, v in zip(self.wheel_positions, self.wheel_velocities)]
        self.distance += abs(self.speed) * dt
        self.elapsed += dt

    def scan(self):
        ox = self.x + math.cos(self.yaw) * LIDAR_OFFSET[0]
        oy = self.y + math.sin(self.yaw) * LIDAR_OFFSET[0]
        boxes = list(self.obstacles) + [
            {"x": x, "y": y, "width": w, "depth": d}
            for x, y, w, d in ((-5.05, 0, .1, 10.2), (5.05, 0, .1, 10.2),
                               (0, -5.05, 10.2, .1), (0, 5.05, 10.2, .1))]
        ranges = []
        for index in range(SCAN_COUNT):
            angle = self.yaw - math.pi + index * math.tau / SCAN_COUNT
            dx, dy = math.cos(angle), math.sin(angle)
            nearest = SCAN_MAX
            for box in boxes:
                near, far = 0.0, SCAN_MAX
                for origin, direction, center, size in ((ox, dx, box["x"], box["width"]),
                                                         (oy, dy, box["y"], box["depth"])):
                    low, high = center - size / 2, center + size / 2
                    if abs(direction) < 1e-10:
                        if not low <= origin <= high:
                            far = -1
                            break
                    else:
                        a, b = sorted(((low - origin) / direction, (high - origin) / direction))
                        near, far = max(near, a), min(far, b)
                if near <= far and far >= 0:
                    nearest = min(nearest, near)
            ranges.append(nearest if SCAN_MIN <= nearest < SCAN_MAX else None)
        return {"origin": [ox, oy, LIDAR_OFFSET[2]], "yaw": self.yaw,
                "angle_min": -math.pi, "angle_increment": math.tau / SCAN_COUNT,
                "range_min": SCAN_MIN, "range_max": SCAN_MAX, "ranges": ranges}

    def state(self):
        return {"x": self.x, "y": self.y, "yaw": self.yaw, "speed": self.speed,
                "steering": self.steering, "yaw_rate": self.yaw_rate,
                "acceleration": self.acceleration, "lateral_acceleration": self.lateral_acceleration,
                "distance": self.distance, "elapsed": self.elapsed, "collision": self.collision,
                "steering_angles": self.wheel_state()[0], "wheel_positions": list(self.wheel_positions),
                "wheel_velocities": list(self.wheel_velocities)}

    def scene(self):
        return {"geometry": asdict(self.geometry), "obstacles": self.obstacles,
                "room_half_size": ROOM_HALF_SIZE, "lidar_offset": LIDAR_OFFSET}

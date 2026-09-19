"""Native ROS observation admission shared by the live app and physics tests."""
import math
import time
from collections import deque
from go2_io import sensor_wall_ns, cloud_scan
from controller import HallwayController, finite


class ExposureGate:
    """Admit advancing captures, optionally using arrival time for their age."""

    def __init__(self, *, ignore_capture_age=False):
        self.ignore_capture_age = ignore_capture_age
        self.last_stamps = {}
        self.clock_anchor = None
        self.rejections = {}
        self.ages = {}

    def capture_time(self, topic, stamp, *, wall_ns=None, monotonic=None):
        wall_ns = sensor_wall_ns(time.time_ns() if wall_ns is None else wall_ns)
        monotonic = time.monotonic() if monotonic is None else monotonic
        self.ages.pop(topic, None)
        if (type(stamp.sec) is not int or type(stamp.nanosec) is not int
                or not 0 <= stamp.nanosec < 1_000_000_000
                or not finite(wall_ns) or not finite(monotonic)):
            self.rejections[topic] = "malformed_capture_stamp"
            return None
        source = stamp.sec * 1_000_000_000 + stamp.nanosec
        age = (wall_ns - source) / 1e9
        self.ages[topic] = age
        reason = ("nonpositive_capture_stamp" if source <= 0 else
                  "capture_in_future" if not self.ignore_capture_age and age < -0.05 else
                  "capture_too_old" if not self.ignore_capture_age and age >= HallwayController.SENSOR_TIMEOUT else
                  "capture_not_advancing" if source <= self.last_stamps.get(topic, -1) else None)
        if reason:
            self.rejections[topic] = reason
            return None
        self.rejections.pop(topic, None)
        self.last_stamps[topic] = source
        if self.ignore_capture_age:
            return monotonic
        if self.clock_anchor is None:
            self.clock_anchor = (wall_ns, monotonic)
        anchor_wall, anchor_monotonic = self.clock_anchor
        return min(monotonic, anchor_monotonic + (source - anchor_wall) / 1e9)


def pose_values(message):
    if message.header.frame_id != "odom" or message.child_frame_id != "base_link":
        return None
    pose = message.pose.pose
    q = pose.orientation
    values = (pose.position.x, pose.position.y, pose.position.z, q.x, q.y, q.z, q.w)
    if not all(finite(value) for value in values):
        return None
    norm = math.hypot(q.x, q.y, q.z, q.w)
    if abs(norm - 1.0) > 0.01:
        return None
    qx, qy, qz, qw = (value / norm for value in (q.x, q.y, q.z, q.w))
    heading = math.atan2(2 * (qw * qz + qx * qy), 1 - 2 * (qy * qy + qz * qz))
    return pose.position.x, pose.position.y, heading


class HallwayApp:
    def __init__(self, *, ignore_capture_age=False, **limits):
        self.controller = HallwayController(**limits)
        self.gate = ExposureGate(ignore_capture_age=ignore_capture_age)
        self.latest_odom = None
        self.odom_history = deque(maxlen=64)
        self.sensor_errors = {}

    def odom(self, message, *, wall_ns=None, now=None):
        pose = pose_values(message)
        captured = self.gate.capture_time("odom", message.header.stamp,
            wall_ns=wall_ns, monotonic=now) if pose else None
        if pose is None or captured is None:
            self.latest_odom = None
            self.odom_history.clear()
            self.controller.pose = self.controller.pose_at = None
            reason = "invalid_odometry" if pose is None else self.gate.rejections["odom"]
            self.sensor_errors["odom"] = reason
            self.controller.stop(reason)
            return False
        if not self.controller.observe_pose(*pose, captured):
            self.latest_odom = None
            self.odom_history.clear()
            self.sensor_errors["odom"] = self.controller.reason
            return False
        self.latest_odom = message
        source = self.gate.last_stamps["odom"]
        self.odom_history.append((source, message))
        while self.odom_history[0][0] < source - 500_000_000:
            self.odom_history.popleft()
        self.sensor_errors.pop("odom", None)
        return True

    def cloud(self, message, *, wall_ns=None, now=None):
        try:
            if self.latest_odom is None:
                raise ValueError("waiting_for_odometry")
            captured = self.gate.capture_time("scan", message.header.stamp,
                wall_ns=wall_ns, monotonic=now)
            if captured is None:
                raise ValueError(self.gate.rejections["scan"])
            source = self.gate.last_stamps["scan"]
            # DDS may deliver a cloud after newer odometry. Use the closest
            # capture for projection; controller freshness still uses latest pose.
            _, odom = min(self.odom_history, key=lambda item: abs(item[0] - source))
            scan = cloud_scan(message, odom)
            if not self.controller.observe_scan(scan.ranges, scan.angle_min, scan.angle_increment, captured):
                raise ValueError(self.controller.reason)
            self.sensor_errors.pop("scan", None)
            return True
        except (ValueError, TypeError, AttributeError) as error:
            self.sensor_errors["scan"] = str(error)
            return self.controller.reject_scan(str(error))

    def status(self, now):
        return {**self.controller.status(now), "sensor_errors": dict(self.sensor_errors),
                "ignore_capture_age": self.gate.ignore_capture_age,
                "capture_age_seconds": dict(self.gate.ages)}

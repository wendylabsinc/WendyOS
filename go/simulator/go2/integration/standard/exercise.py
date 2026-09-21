"""A separately deployed ROS application tests the virtual Go2 end to end.

Motion is sent exclusively over /cmd_vel. HTTP identifies the simulator,
explicitly grants its observed ingress source, and controls test lifecycle.
"""

import argparse
from collections import deque
import json
import math
import os
from pathlib import Path
import threading
import time
import xml.etree.ElementTree as ET
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import Request, urlopen

import rclpy
from geometry_msgs.msg import PoseStamped, Twist
from nav_msgs.msg import Odometry
from rclpy.executors import SingleThreadedExecutor
from rclpy.node import Node
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import CameraInfo, Image, Imu, JointState, LaserScan, PointCloud2
from std_msgs.msg import UInt64
from tf2_msgs.msg import TFMessage


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def stamp_ns(stamp):
    return stamp.sec * 1_000_000_000 + stamp.nanosec


def vector(value):
    return (value.x, value.y, value.z)


def norm(values):
    return math.sqrt(sum(value * value for value in values))


def yaw(orientation):
    x, y, z, w = orientation.x, orientation.y, orientation.z, orientation.w
    return math.atan2(2 * (w * z + x * y), 1 - 2 * (y * y + z * z))


def angle_difference(after, before):
    return math.atan2(math.sin(after - before), math.cos(after - before))


def wait_until(predicate, message, timeout=10.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        result = predicate()
        if result:
            return result
        time.sleep(0.025)
    raise AssertionError(message)


class SimulatorAPI:
    def __init__(self, base_url):
        parsed = urlsplit(base_url)
        require(parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "localhost", "::1"},
                "the harness only accepts a guest-local HTTP simulator endpoint")
        require(not parsed.username and not parsed.password and parsed.path in {"", "/"}
                and not parsed.query and not parsed.fragment, "invalid simulator base URL")
        self.base = base_url.rstrip("/")

    def _request(self, path, body=None):
        request = Request(self.base + path,
                          data=None if body is None else json.dumps(body).encode(),
                          headers={} if body is None else {"Content-Type": "application/json"})
        try:
            with urlopen(request, timeout=3.0) as response:
                return response.status, json.load(response)
        except HTTPError as error:
            return error.code, json.load(error)

    def status(self):
        code, result = self._request("/api/status")
        require(code == 200 and isinstance(result, dict), f"simulator status failed: {code}, {result}")
        require(result.get("simulation") is True and result.get("robot") == "go2"
                and result.get("profile_version") == 1,
                "endpoint did not identify itself as the Wendy Go2 simulation profile")
        return result

    def post(self, path, body=None, expected=200):
        # Check identity before every mutation, including cleanup after failure.
        self.status()
        code, result = self._request(path, body or {})
        require(code == expected, f"{path}: expected HTTP {expected}, got {code}: {result}")
        return result


class Probe(Node):
    def __init__(self, *, full_sensors=False):
        super().__init__("wendy_go2_standard_integration")
        self.lock = threading.RLock()
        self.publisher = None
        self.publishing = False
        self.velocity = (0.0, 0.0, 0.0)
        self.latest = {}
        self.receipts = {}
        self.stamps = {}
        self.received_counts = {}
        self.nonadvancing_stamps = {}
        self.source_stamps = {}
        self.full = None
        self.command_count = 0
        self.command_gaps = 0
        self.last_command_sent = None
        self.transforms = {}
        self.odom_by_stamp = {}
        self.tf_by_stamp = {}
        self.joint_history = deque(maxlen=2000)
        self.epoch = None
        sensor_qos = QoSProfile(depth=100, reliability=ReliabilityPolicy.BEST_EFFORT)
        topics = [
            ("odom", Odometry, "/odom"), ("imu", Imu, "/imu/data"),
            ("joints", JointState, "/joint_states"), ("scan", LaserScan, "/scan"),
            ("truth", PoseStamped, "/simulation/ground_truth"),
        ]
        if full_sensors:
            from full_sensors import FullSensorChecks
            from unitree_go.msg import LowState
            self.full = FullSensorChecks()
            topics.extend((("image", Image, "/camera/color/image_raw"),
                           ("camera_info", CameraInfo, "/camera/color/camera_info"),
                           ("cloud", PointCloud2, "/utlidar/cloud"),
                           ("lowstate", LowState, "/lowstate")))
        for name, kind, topic in topics:
            self.stamps[name] = deque(maxlen=10000)
            self.received_counts[name] = self.nonadvancing_stamps[name] = 0
            qos = (QoSProfile(depth=5, reliability=ReliabilityPolicy.BEST_EFFORT)
                   if name in {"image", "camera_info", "cloud"} else sensor_qos)
            self.create_subscription(kind, topic, lambda message, key=name: self.observe(key, message), qos)
        self.create_subscription(TFMessage, "/tf", self.observe_tf, QoSProfile(depth=100))
        static_qos = QoSProfile(depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL)
        self.create_subscription(TFMessage, "/tf_static", self.observe_tf, static_qos)
        self.create_subscription(UInt64, "/simulation/epoch", self.observe_epoch, static_qos)
        self.create_timer(0.05, self.publish_command)

    def observe(self, name, message):
        with self.lock:
            self.latest[name] = message
            self.receipts[name] = time.monotonic()
            # LowState carries a simulation tick in milliseconds, not a ROS
            # wall-clock header. Keep its domain explicit; never invent one.
            timestamp = int(message.tick) * 1_000_000 if name == "lowstate" else stamp_ns(message.header.stamp)
            if self.stamps[name] and timestamp <= self.stamps[name][-1]:
                self.nonadvancing_stamps[name] += 1
            self.stamps[name].append(timestamp)
            self.received_counts[name] += 1
            if name != "lowstate":
                self.source_stamps[name] = timestamp
            if self.full:
                self.full.observe(name, message, timestamp)
            if name == "odom":
                self.odom_by_stamp[stamp_ns(message.header.stamp)] = message
                if len(self.odom_by_stamp) > 300:
                    self.odom_by_stamp.pop(next(iter(self.odom_by_stamp)))
            elif name == "joints":
                self.joint_history.append((time.monotonic(), list(message.position)))

    def observe_tf(self, message):
        with self.lock:
            for transform in message.transforms:
                key = (transform.header.frame_id, transform.child_frame_id)
                self.transforms[key] = transform
                if key == ("odom", "base_link"):
                    self.tf_by_stamp[stamp_ns(transform.header.stamp)] = transform
                    if len(self.tf_by_stamp) > 300:
                        self.tf_by_stamp.pop(next(iter(self.tf_by_stamp)))

    def observe_epoch(self, message):
        with self.lock:
            self.epoch = message.data

    def publish_command(self):
        with self.lock:
            if not self.publishing or self.publisher is None:
                return
            message = Twist()
            message.linear.x, message.linear.y, message.angular.z = self.velocity
            self.publisher.publish(message)
            now = time.monotonic()
            if self.last_command_sent is not None and now - self.last_command_sent >= 0.2:
                self.command_gaps += 1
            self.last_command_sent = now
            self.command_count += 1

    def replace_publisher(self):
        with self.lock:
            self.publishing = False
            if self.publisher is not None:
                self.destroy_publisher(self.publisher)
            self.publisher = self.create_publisher(Twist, "/cmd_vel", QoSProfile(depth=1))
            self.velocity = (0.0, 0.0, 0.0)
            self.publishing = True

    def command(self, values, *, publishing=True):
        with self.lock:
            self.velocity = tuple(values)
            self.publishing = publishing

    def messages(self):
        with self.lock:
            return dict(self.latest)

    def counters(self):
        with self.lock:
            return {"received": self.received_counts.copy(),
                    "nonadvancing_stamps": self.nonadvancing_stamps.copy(),
                    "command_count": self.command_count, "command_gaps": self.command_gaps,
                    "receipts": self.receipts.copy(), "source_stamps": self.source_stamps.copy(),
                    "full_sensors": self.full.status() if self.full else None}

    def rates(self, since_ns):
        with self.lock:
            samples = {key: [stamp for stamp in stamps if stamp >= since_ns]
                       for key, stamps in self.stamps.items() if key != "lowstate"}
        result = {}
        for name, stamps in samples.items():
            require(len(stamps) >= 3, f"insufficient {name} samples for a rate measurement")
            require(all(after > before for before, after in zip(stamps, stamps[1:])),
                    f"{name} source timestamps did not advance")
            result[name] = (len(stamps) - 1) * 1e9 / (stamps[-1] - stamps[0])
        return result


def report(check, **details):
    print(json.dumps({"check": check, "passed": True, **details}, allow_nan=False), flush=True)


def sources(status):
    return {source["publisher_gid"]: source for source in status["ros_commands"]["sources"]}


def discover_and_arm(api, probe):
    before = set(sources(api.status()))
    probe.replace_publisher()

    def newly_observed():
        candidates = [gid for gid, source in sources(api.status()).items()
                      if gid not in before and source["kind"] == "twist"
                      and source["age_ms"] < 300 and not source["requires_restart"]]
        require(len(candidates) <= 1, "multiple new ingress sources appeared; refusing an ambiguous grant")
        return candidates[0] if candidates else None

    gid = wait_until(newly_observed, "the new zero-command publisher was not observed")
    api.post("/api/arm_ros", {"publisher_gid": gid})
    wait_until(lambda: api.status()["ros_commands"]["owner"] == gid,
               "explicit ROS command ownership was not established")
    report("explicit_source_grant", publisher_gid=gid)
    return gid


def validate_sensors(probe):
    required = {"odom", "imu", "joints", "scan", "truth"}
    if probe.full:
        required.update(("image", "camera_info", "cloud", "lowstate"))
    wait_until(lambda: required <= set(probe.messages()), "required ROS sensor topics are missing")
    messages = probe.messages()
    now = time.monotonic()
    with probe.lock:
        require(all(now - probe.receipts[name] < 0.5 for name in required), "sensor receipt is stale")
    odom, imu, joints, scan, truth = (messages[name] for name in ("odom", "imu", "joints", "scan", "truth"))
    require(odom.header.frame_id == "odom" and odom.child_frame_id == "base_link", "odometry frame mismatch")
    require(imu.header.frame_id == "imu_link", "IMU frame mismatch")
    require(scan.header.frame_id == "lidar_link", "scan frame mismatch")
    require(truth.header.frame_id == "simulation_world", "ground-truth frame mismatch")
    require(len(joints.name) == len(set(joints.name)) == len(joints.position)
            == len(joints.velocity) == len(joints.effort) == 12, "joint arrays do not describe twelve unique joints")
    require(all(math.isfinite(value) for values in (joints.position, joints.velocity, joints.effort)
                for value in values), "joint state contains nonfinite values")
    require(all(math.isfinite(value) for value in vector(imu.angular_velocity) + vector(imu.linear_acceleration)),
            "IMU state contains nonfinite values")
    require(0.99 < norm((imu.orientation.x, imu.orientation.y, imu.orientation.z, imu.orientation.w)) < 1.01,
            "invalid IMU quaternion")
    require(norm(vector(imu.linear_acceleration)) > 1.0, "stationary IMU is missing gravity/specific force")
    require(all(odom.pose.covariance[index] > 0 for index in (0, 7, 35)), "odometry covariance is missing")
    require(len(scan.ranges) >= 180, "scan angular sampling is incomplete")
    finite_ranges = [value for value in scan.ranges if math.isfinite(value)]
    require(len(finite_ranges) >= 0.8 * len(scan.ranges), "insufficient scan returns in the bounded sandbox")
    require(all(scan.range_min <= value <= scan.range_max for value in finite_ranges), "scan contains out-of-range returns")

    def matching_transform():
        with probe.lock:
            required_tf = {("odom", "base_link"), ("base_link", "imu_link"), ("base_link", "lidar_link")}
            common = set(probe.odom_by_stamp) & set(probe.tf_by_stamp)
            if not required_tf <= set(probe.transforms) or not common:
                return None
            timestamp = max(common)
            return probe.odom_by_stamp[timestamp], probe.tf_by_stamp[timestamp]

    matched_odom, transform = wait_until(matching_transform, "coherent dynamic/static TF is missing")
    require(norm(tuple(a - b for a, b in zip(vector(matched_odom.pose.pose.position),
                                           vector(transform.transform.translation)))) < 1e-6,
            "same-timestamp odometry and TF translation disagree")
    q, r = matched_odom.pose.pose.orientation, transform.transform.rotation
    require(abs(q.x*r.x + q.y*r.y + q.z*r.z + q.w*r.w) > 0.999999,
            "same-timestamp odometry and TF orientation disagree")
    report("sensor_contract", scan_returns=len(finite_ranges), joints=len(joints.name))
    if probe.full:
        def coherent_full_sensors():
            with probe.lock:
                require(not probe.full.errors, f"full sensor validation failed: {probe.full.errors}")
                if probe.full.crc_checked == 0:
                    return None
                return probe.full.coherent_pairs()
        pair = wait_until(coherent_full_sensors, "coherent full sensor payloads are missing")
        report("full_sensor_contract", **pair, **probe.counters()["full_sensors"])


def exercise(api, probe):
    initial = api.status()
    require(initial.get("ros", {}).get("running") is True, "the simulator ROS observation bridge is not running")
    require(initial.get("ros_commands") is not None, "the simulator ROS command ingress is not running")
    api.post("/api/reset")
    wait_until(lambda: api.status()["ready"], "robot did not become ready after reset", timeout=20.0)
    gid = discover_and_arm(api, probe)
    validate_sensors(probe)
    rate_started_ns = time.time_ns()
    time.sleep(2.0)
    rates = probe.rates(rate_started_ns)
    for topic, minimum in (("imu", 50), ("odom", 25), ("joints", 25), ("scan", 5), ("truth", 25)):
        require(rates[topic] >= minimum, f"{topic} rate {rates[topic]:.2f} Hz is below {minimum} Hz")
    report("observed_source_rates", hz=rates)

    for name, command, axis, direction, minimum in (
        ("forward", (0.35, 0.0, 0.0), 0, 1, 0.2),
        ("backward", (-0.35, 0.0, 0.0), 0, -1, 0.2),
        ("left", (0.0, 0.25, 0.0), 1, 1, 0.12),
        ("right", (0.0, -0.25, 0.0), 1, -1, 0.12),
        ("yaw_left", (0.0, 0.0, 0.5), 2, 1, 0.3),
        ("yaw_right", (0.0, 0.0, -0.5), 2, -1, 0.3),
    ):
        before = probe.messages()
        phase_started = time.monotonic()
        probe.command(command)
        time.sleep(2.5)
        after = probe.messages()
        probe.command((0.0, 0.0, 0.0))
        before_pose, after_pose = before["odom"].pose.pose, after["odom"].pose.pose
        heading = yaw(before_pose.orientation)
        if axis == 2:
            displacement = angle_difference(yaw(after_pose.orientation), heading)
        else:
            dx = after_pose.position.x - before_pose.position.x
            dy = after_pose.position.y - before_pose.position.y
            displacement = (dx * math.cos(heading) + dy * math.sin(heading) if axis == 0
                            else -dx * math.sin(heading) + dy * math.cos(heading))
        require(direction * displacement > minimum, f"{name} had incorrect/insufficient ROS odometry displacement: {displacement}")
        require(0.15 < after["truth"].pose.position.z < 0.6, f"{name} did not maintain a supported body height")
        with probe.lock:
            moving_joints = [positions for received, positions in probe.joint_history if received >= phase_started]
        require(len(moving_joints) >= 10, "insufficient moving joint observations")
        travel = max(max(values) - min(values) for values in zip(*moving_joints))
        require(travel > 0.05, f"{name} did not produce a moving gait")
        range_change = max((abs(a - b) for a, b in zip(before["scan"].ranges, after["scan"].ranges)
                            if math.isfinite(a) and math.isfinite(b)), default=0.0)
        require(range_change > 0.03, f"{name} did not change lidar observations")
        require(api.status()["ros_commands"]["owner"] == gid, f"{name} lost command ownership")
        report("signed_motion", direction=name, displacement=displacement, joint_travel=travel,
               maximum_scan_change=range_change)
        time.sleep(0.8)

    probe.command((0.35, 0.0, 0.0))
    time.sleep(0.8)
    probe.command((0.35, 0.0, 0.0), publishing=False)
    stopped_publishing = time.monotonic()
    wait_until(lambda: api.status()["command"] == [0.0, 0.0, 0.0], "command watchdog did not clear an expired publisher", timeout=1.0)
    expiry_seconds = time.monotonic() - stopped_publishing
    time.sleep(2.0)
    settled = probe.messages()["odom"].twist.twist
    require(norm((settled.linear.x, settled.linear.y)) < 0.15 and abs(settled.angular.z) < 0.25,
            "robot did not settle after command expiry")
    report("publisher_expiry", target_cleared_seconds=expiry_seconds)

    # Leave the original endpoint actively publishing nonzero commands during
    # reset, proving that fresh arrivals from a revoked source cannot rearm it.
    probe.command((0.35, 0.0, 0.0))
    time.sleep(0.4)
    reset = api.post("/api/reset")
    wait_until(lambda: probe.epoch == reset["epoch"], "ROS epoch notification did not follow reset")
    for _ in range(15):
        state = api.status()
        require(state["command"] == [0.0, 0.0, 0.0] and state["ros_commands"]["owner"] is None,
                "the continuing old publisher reactivated motion after reset")
        require(sources(state)[gid]["requires_restart"], "old publisher was not fenced across reset")
        time.sleep(0.05)
    api.post("/api/arm_ros", {"publisher_gid": gid}, expected=403)
    replacement = discover_and_arm(api, probe)
    require(replacement != gid, "recreated ROS publisher reused the revoked ingress identity")
    before = probe.messages()["odom"].pose.pose
    probe.command((-0.25, 0.0, 0.0))
    time.sleep(1.0)
    after = probe.messages()["odom"].pose.pose
    heading = yaw(before.orientation)
    backward = ((after.position.x - before.position.x) * math.cos(heading)
                + (after.position.y - before.position.y) * math.sin(heading))
    require(backward < -0.05, "rearmed replacement publisher could not move the robot")
    probe.command((0.0, 0.0, 0.0))
    report("reset_and_fresh_publisher", old_gid=gid, new_gid=replacement, epoch=reset["epoch"])
    return {"source_rates_hz": rates, "final_epoch": reset["epoch"], "directions": 6}


def soak_interval(first, last):
    """Compare independent receiver counts with runtime cumulative counters."""
    wall = last["received_at"] - first["received_at"]
    runtime_wall = last["metrics"]["wall_seconds"] - first["metrics"]["wall_seconds"]
    require(wall > 0 and runtime_wall > 0, "soak measurement clocks did not advance")
    result = {"wall_seconds": wall,
              "real_time_factor": (last["time"] - first["time"]) / runtime_wall}
    for counter, name in (("policy_updates", "policy_hz"), ("camera_frames", "camera_fps"),
                          ("physics_steps", "physics_hz")):
        delta = last["metrics"][counter] - first["metrics"][counter]
        require(delta >= 0, f"runtime {counter} counter reset during soak")
        result[name] = delta / runtime_wall
    result["received_hz"], result["published_hz"] = {}, {}
    for topic, count in last["probe"]["received"].items():
        result["received_hz"][topic] = (count - first["probe"]["received"][topic]) / wall
        published = {"truth": "odom", "image": "camera", "camera_info": "camera"}.get(topic, topic)
        stream = "native_published" if topic == "lowstate" else "published"
        delta = last[stream][published] - first[stream][published]
        require(delta >= 0, f"runtime {topic} publication counter reset during soak")
        result["published_hz"][topic] = delta / runtime_wall
    result["command_hz"] = (last["probe"]["command_count"] - first["probe"]["command_count"]) / wall
    result["command_gaps_over_200ms"] = last["probe"]["command_gaps"] - first["probe"]["command_gaps"]
    return result


def soak(api, probe, duration):
    """Run representative ROS app load; all movement remains ordinary Twist."""
    probe.command((0.0, 0.0, 0.0))
    time.sleep(1.0)
    initial = api.status()
    gid = initial["ros_commands"]["owner"]
    require(gid is not None, "soak requires the explicitly granted replacement publisher")
    anchor = probe.messages()["truth"].pose
    require(norm((anchor.position.x, anchor.position.y)) < 0.5,
            "soak must begin near the sandbox spawn")
    anchor_heading = yaw(anchor.orientation)
    samples, motions, errors = [], [], []
    started = time.monotonic()
    deadline = started + duration
    next_sample = started
    next_timing_report = started
    no_contact_since = None

    def sample(phase):
        nonlocal no_contact_since, next_timing_report
        status = api.status()
        received_at = time.monotonic()
        counters = probe.counters()
        record = {"received_at": received_at, "time": status["time"],
                  "metrics": status["metrics"], "published": status["ros"]["samples"],
                  "native_published": status["ros"].get("native_samples", {}),
                  "probe": counters, "ncontact": status["ncontact"],
                  "position": status["position"], "mode": status["mode"],
                  "error": status["error"]}
        samples.append(record)
        position, quaternion = status["position"], status["quaternion_wxyz"]
        radius = norm((position[0] - anchor.position.x, position[1] - anchor.position.y))
        joint_values = status["joints"]
        # Limits from the pinned profile-v1 MJCF. Position constraints are soft
        # in MuJoCo, so allow 0.05 rad of contact/limit solver tolerance.
        def joint_limits(name):
            if "_hip_" in name:
                return -1.0472, 1.0472, 23.7
            if "_calf_" in name:
                return -2.7227, -0.83776, 35.55
            require("_thigh_" in name, f"unexpected profile-v1 joint: {name}")
            return (-1.5708, 3.4907, 23.7) if name.startswith("F") else (-0.5236, 4.5379, 23.7)

        within_limits = all(low - 0.05 <= q <= high + 0.05 and abs(torque) <= limit + 1e-5
                            for name, q, torque in zip(joint_values["name"], joint_values["q"], joint_values["torque"])
                            for low, high, limit in (joint_limits(name),))
        physical = {
            "runtime_ready": status["ready"] and status["error"] is None,
            "standing_or_moving": status["mode"] in {"standing", "moving"},
            "epoch_unchanged": status["epoch"] == initial["epoch"],
            "owner_unchanged": status["ros_commands"]["owner"] == gid,
            "finite_state": all(math.isfinite(value) for value in position + quaternion
                                + joint_values["q"] + joint_values["dq"] + joint_values["torque"]),
            "joint_position_and_torque_limits": within_limits,
            "supported_height": 0.15 < position[2] < 0.6,
            "upright": 1 - 2 * (quaternion[1] ** 2 + quaternion[2] ** 2) > 0.7,
            "near_spawn": radius <= 1.25,
            "valid_contacts": type(record["ncontact"]) is int and record["ncontact"] >= 0,
            "fresh_ros_receipts": all(received_at - stamp < 0.5 for stamp in counters["receipts"].values()),
            "fresh_ros_source_stamps": all(-0.05 <= (time.time_ns() - stamp) / 1e9 < 0.5
                                            for stamp in counters["source_stamps"].values()),
            "advancing_ros_stamps": counters["nonadvancing_stamps"] == samples[0]["probe"]["nonadvancing_stamps"],
        }
        if probe.full:
            full = counters["full_sensors"]
            tick_age = status["time"] - full["latest_tick_ms"] / 1000 if full["latest_tick_ms"] is not None else None
            physical["native_tick_tracks_physics"] = tick_age is not None and -0.1 < tick_age < 0.1
            physical["valid_full_sensor_payloads"] = not full["errors"] and full["crc_checked"] > 0
            record["native_tick_age_seconds"] = tick_age
        if record["ncontact"] > 0:
            no_contact_since = None
        elif no_contact_since is None:
            no_contact_since = received_at
        physical["contact_support_returns"] = no_contact_since is None or received_at - no_contact_since <= 1.0
        record["physical"] = physical
        interval = soak_interval(samples[-2], record) if len(samples) > 1 else None
        timing_diagnostics = {}
        if not all(physical.values()) or received_at >= next_timing_report:
            # Use this exact HTTP observation, not a later query after the
            # subscriber has stopped. Keep summaries small and infrequent.
            def compact(summary):
                return {key: round(value, 3) if isinstance(value, float) else value
                        for key, value in summary.items() if key in {"samples", "mean", "p95", "max"}}
            timing_diagnostics = {
                "render_stage_ms": {stage: compact(summary) for stage, summary in
                                    status["metrics"].get("render_stage_ms", {}).items()},
                "image_publish_ms": compact(status["ros"].get("image_publish_ms", {})),
            }
            next_timing_report = received_at + 10.0
        print(json.dumps({"event": "soak_sample", "elapsed_seconds": received_at - started,
                          "phase": phase, "position": position, "anchor_radius_m": radius,
                          "contacts": record["ncontact"], "mode": status["mode"],
                          "physical_constraints": physical, "interval": interval,
                          "runtime_trailing_command_p95_ms": status["metrics"].get("command_p95_ms"),
                          "runtime_process_peak_rss_bytes": status["metrics"]["rss_bytes"],
                          "full_sensors": counters["full_sensors"],
                          # Negative means the received native tick is newer
                          # than the separately captured HTTP physics sample.
                          "native_tick_age_seconds": record.get("native_tick_age_seconds"),
                          "source_age_ms": {name: (time.time_ns() - stamp) / 1e6
                                            for name, stamp in counters["source_stamps"].items()},
                          **timing_diagnostics,
                          "error": status["error"]}, allow_nan=False), flush=True)
        require(all(physical.values()), f"soak physical constraints failed: {physical}")
        return record

    def tick(phase, end):
        nonlocal next_sample
        scheduled = next_sample + 0.5
        time.sleep(max(0.0, min(end, scheduled) - time.monotonic()))
        # A command phase can end between telemetry deadlines. Changing its
        # target does not need a second sample a few milliseconds after the
        # preceding one. Capture the final run boundary for the duration gate.
        if time.monotonic() < scheduled and end < deadline:
            return None
        if time.monotonic() >= scheduled:
            next_sample = max(scheduled, time.monotonic())
        return sample(phase)

    def segment(name, command, axis=None, direction=0):
        before = probe.messages()["truth"].pose
        phase_started = time.monotonic()
        end = min(deadline, phase_started + 2.0)
        probe.command(command)
        while time.monotonic() < end:
            tick(name, end)
        if axis is not None and time.monotonic() - phase_started >= 1.8:
            after = probe.messages()["truth"].pose
            heading = yaw(before.orientation)
            dx, dy = after.position.x - before.position.x, after.position.y - before.position.y
            displacement = (angle_difference(yaw(after.orientation), heading) if axis == 2 else
                            dx * math.cos(heading) + dy * math.sin(heading) if axis == 0 else
                            -dx * math.sin(heading) + dy * math.cos(heading))
            passed = direction * displacement > (0.06 if axis == 2 else 0.04)
            motions.append({"direction": name, "displacement": displacement, "passed": passed})
            report("soak_signed_motion", **motions[-1])
            require(passed, f"soak {name} failed to produce signed physical displacement")

    summary = None
    try:
        sample("begin")
        deadline = samples[0]["received_at"] + duration
        while time.monotonic() < deadline:
            for phase in (("forward", (0.25, 0.0, 0.0), 0, 1),
                          ("backward", (-0.25, 0.0, 0.0), 0, -1),
                          ("left", (0.0, 0.20, 0.0), 1, 1),
                          ("right", (0.0, -0.20, 0.0), 1, -1),
                          ("yaw_left", (0.0, 0.0, 0.4), 2, 1),
                          ("yaw_right", (0.0, 0.0, -0.4), 2, -1),
                          ("stand", (0.0, 0.0, 0.0), None, 0)):
                if time.monotonic() >= deadline:
                    break
                segment(*phase)
            # Opposite open-loop motions slowly accumulate gait error. Return
            # using observed ground truth and bounded ROS velocity commands;
            # never reset, teleport, or renew ownership inside the measurement.
            recenter_deadline = min(deadline, time.monotonic() + 12.0)
            centered = False
            while time.monotonic() < recenter_deadline:
                pose = probe.messages()["truth"].pose
                heading = yaw(pose.orientation)
                dx, dy = anchor.position.x - pose.position.x, anchor.position.y - pose.position.y
                turn = angle_difference(anchor_heading, heading)
                # This recenters a stability soak, not a precision navigation
                # test. The walking policy has a small-command deadband.
                centered = norm((dx, dy)) < 0.15 and abs(turn) < 0.15
                if centered:
                    break
                clip = lambda value, limit: max(-limit, min(limit, value))
                probe.command((clip(0.8 * (dx * math.cos(heading) + dy * math.sin(heading)), 0.25),
                               clip(0.8 * (-dx * math.sin(heading) + dy * math.cos(heading)), 0.20),
                               clip(0.8 * turn, 0.4)))
                tick("recenter", recenter_deadline)
            require(centered or time.monotonic() >= deadline, "soak could not return near its spawn anchor")
            probe.command((0.0, 0.0, 0.0))
    except Exception as error:
        errors.append(f"{type(error).__name__}: {error}")
        raise
    finally:
        probe.command((0.0, 0.0, 0.0))
        measured = None
        try:
            if len(samples) > 1:
                measured = soak_interval(samples[0], samples[-1])
        except (AssertionError, KeyError, TypeError, ValueError) as error:
            # A reset/counter rollback is itself a test failure. Preserve its
            # partial diagnostic report instead of raising again in cleanup.
            errors.append(f"summary measurement: {type(error).__name__}: {error}")
        p95_values = [record["metrics"].get("command_p95_ms") for record in samples]
        observed_p95 = [value for value in p95_values if value is not None]
        rss = [record["metrics"]["rss_bytes"] for record in samples]
        physical_passed = (not errors and bool(samples) and
                           all(all(record["physical"].values()) for record in samples) and
                           all(motion["passed"] for motion in motions))
        targets = {}
        if measured:
            targets = {"window_at_least_600_seconds": measured["wall_seconds"] >= 600,
                       "real_time_factor_at_least_0_95": measured["real_time_factor"] >= 0.95,
                       "policy_hz_at_least_47_5": measured["policy_hz"] >= 47.5,
                       "all_runtime_trailing_command_p95_under_100ms":
                           len(observed_p95) == len(samples) and max(observed_p95, default=100) < 100,
                       "no_command_refresh_gaps_over_200ms": measured["command_gaps_over_200ms"] == 0}
            for topic, minimum in (("imu", 190), ("odom", 47.5), ("joints", 47.5),
                                   ("scan", 9.5), ("truth", 47.5)):
                targets[f"received_{topic}_hz_at_least_{minimum}"] = measured["received_hz"][topic] >= minimum
            if probe.full:
                for topic, minimum in (("image", 14), ("camera_info", 14), ("cloud", 9.5), ("lowstate", 475)):
                    targets[f"received_{topic}_hz_at_least_{minimum}"] = measured["received_hz"][topic] >= minimum
                targets["native_crc_samples_cover_receipts"] = (
                    samples[-1]["probe"]["full_sensors"]["crc_checked"] -
                    samples[0]["probe"]["full_sensors"]["crc_checked"] >=
                    (samples[-1]["probe"]["received"]["lowstate"] -
                     samples[0]["probe"]["received"]["lowstate"]) // 10 - 1)
        summary = {"requested_seconds": duration, "measured": measured,
                   "physical_constraints_passed": physical_passed,
                   "performance_targets": targets,
                   "acceptance_passed": physical_passed and bool(targets) and all(targets.values()),
                   "sample_count": len(samples), "signed_motion_segments": len(motions),
                   "fall_samples": sum(record["mode"] == "fallen" for record in samples),
                   "fault_samples": sum(record["mode"] == "fault" for record in samples),
                   "contact_samples": sum(record["ncontact"] > 0 for record in samples),
                   "minimum_contacts": min((record["ncontact"] for record in samples), default=None),
                   "maximum_contacts": max((record["ncontact"] for record in samples), default=None),
                   "maximum_runtime_trailing_command_p95_ms": max(observed_p95, default=None),
                   "runtime_peak_rss_bytes": max(rss, default=None),
                   "runtime_peak_rss_growth_bytes": rss[-1] - rss[0] if rss else None,
                   "full_sensor_load": bool(probe.full),
                   "full_sensors": samples[-1]["probe"]["full_sensors"] if samples else None,
                   "errors": errors,
                   "runtime_errors": sorted({record["error"] for record in samples if record["error"]}),
                   "measurement_notes": "Receipt rates use this app's callbacks and monotonic wall time; publication, policy and rendering rates use runtime counter deltas. Ground-truth publication uses the odometry counter; Image and CameraInfo share the camera counter. Native LowState has no ROS header: freshness compares its genuine millisecond tick against current physics time, and every tenth receipt is checked with the pinned SDK's independent bitwise CRC. Other source stamps must be under 500ms old. Runtime latency is trailing-window admission-to-policy p95 and excludes DDS transit; raw per-command latency is unavailable. RSS is ru_maxrss (process lifetime peak), not current allocation. Physical checks are sampled at 2Hz; a short run cannot pass the 600s gate."}
        print(json.dumps({"event": "soak_summary", **summary}, allow_nan=False), flush=True)
    require(summary["physical_constraints_passed"], f"soak failed: {summary['errors']}")
    if duration >= 600:
        require(summary["acceptance_passed"], f"soak acceptance criteria failed: {summary['performance_targets']}")
    return summary


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--simulator-url", default=os.environ.get("GO2_SIMULATOR_URL", "http://127.0.0.1:8890"))
    parser.add_argument("--soak-seconds", type=float, default=0.0,
                        help="additional ROS motion/load measurement; use 600 for the sustained acceptance gate")
    parser.add_argument("--full-sensors", action="store_true",
                        help="include camera/cloud/native readers and SDK CRC checks; requires Dockerfile.soak")
    args = parser.parse_args()
    require(math.isfinite(args.soak_seconds) and args.soak_seconds >= 0,
            "--soak-seconds must be finite and nonnegative")
    api = SimulatorAPI(args.simulator_url)
    # No command publisher exists until the local endpoint proves its identity.
    api.status()
    require(os.environ.get("ROS_DOMAIN_ID") == "0", "the harness requires explicit ROS domain 0")
    if os.environ.get("ROS_LOCALHOST_ONLY") != "1":
        require(args.full_sensors and os.environ.get("ROS_LOCALHOST_ONLY") == "0" and
                os.environ.get("CYCLONEDDS_URI") == "file:///app/cyclonedds.xml",
                "the harness requires loopback-only ROS discovery")
        namespace = {"dds": "https://cdds.io/config"}
        config = ET.parse(Path("/app/cyclonedds.xml"))
        interfaces = config.findall(".//dds:NetworkInterface", namespace)
        peers = config.findall(".//dds:Peer", namespace)
        multicast = config.findall(".//dds:AllowMulticast", namespace)
        require(len(interfaces) == 1 and interfaces[0].get("name") == "lo" and peers and
                all(peer.get("Address") == "127.0.0.1" for peer in peers) and
                multicast and all(element.text.strip().lower() == "false" for element in multicast),
                "the full-sensor Cyclone configuration must use only loopback")
    require(os.environ.get("RMW_IMPLEMENTATION") == "rmw_cyclonedds_cpp", "the harness requires CycloneDDS")
    rclpy.init()
    probe = Probe(full_sensors=args.full_sensors)
    executor = SingleThreadedExecutor()
    executor.add_node(probe)
    stopping = threading.Event()
    spin_errors = []

    def spin():
        try:
            while not stopping.is_set():
                executor.spin_once(timeout_sec=0.05)
        except Exception as error:
            if not stopping.is_set():
                spin_errors.append(str(error))

    thread = threading.Thread(target=spin, daemon=True)
    thread.start()
    started = time.monotonic()
    try:
        results = exercise(api, probe)
        if args.soak_seconds:
            results["soak"] = soak(api, probe, args.soak_seconds)
        require(not spin_errors, f"ROS executor failed: {spin_errors}")
    finally:
        probe.command((0.0, 0.0, 0.0))
        try:
            api.post("/api/disarm_ros")
        finally:
            stopping.set()
            thread.join(timeout=2.0)
            executor.shutdown(timeout_sec=2.0)
            probe.destroy_node()
            if rclpy.ok():
                rclpy.shutdown()
    print(json.dumps({"result": "passed", "wall_seconds": time.monotonic() - started,
                      **results}, allow_nan=False), flush=True)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        print(json.dumps({"result": "failed", "error": f"{type(error).__name__}: {error}"}), flush=True)
        raise

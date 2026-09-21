"""Finite G1 acceptance from a separate ROS process; commands use DDS only."""

import argparse
from collections import deque
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import threading
import time
from urllib.error import HTTPError
from urllib.parse import urlsplit
from urllib.request import HTTPRedirectHandler, ProxyHandler, Request, build_opener

import rclpy
from geometry_msgs.msg import PoseStamped, Twist
from nav_msgs.msg import Odometry
from rclpy.executors import SingleThreadedExecutor
from rclpy.node import Node
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import CameraInfo, Image, Imu, JointState, LaserScan, PointCloud2
from std_msgs.msg import UInt64
from tf2_msgs.msg import TFMessage
from unitree_hg.msg import LowState


CHECKS = []
TOPICS = (
    ("odom", Odometry, "/odom"), ("truth", PoseStamped, "/simulation/ground_truth"),
    ("imu", Imu, "/imu/data"), ("joints", JointState, "/joint_states"),
    ("scan", LaserScan, "/scan"), ("cloud", PointCloud2, "/utlidar/cloud"),
    ("image", Image, "/camera/color/image_raw"),
    ("camera_info", CameraInfo, "/camera/color/camera_info"),
    ("lowstate", LowState, "/lowstate"),
)


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def report(check, **details):
    result = {"check": check, "passed": True, **details}
    CHECKS.append(result)
    print(json.dumps(result, allow_nan=False), flush=True)


def wait_until(predicate, message, timeout=15):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        value = predicate()
        if value:
            return value
        time.sleep(0.025)
    raise AssertionError(message)


def stamp_ns(stamp):
    return stamp.sec * 1_000_000_000 + stamp.nanosec


def xyz(value):
    return value.x, value.y, value.z


def yaw(q):
    return math.atan2(2 * (q.w*q.z + q.x*q.y), 1 - 2 * (q.y*q.y + q.z*q.z))


def displacement(before, after, turn=False):
    heading = yaw(before.orientation)
    if turn:
        delta = yaw(after.orientation) - heading
        return math.atan2(math.sin(delta), math.cos(delta))
    return ((after.position.x - before.position.x) * math.cos(heading)
            + (after.position.y - before.position.y) * math.sin(heading))


class NoRedirects(HTTPRedirectHandler):
    def redirect_request(self, *args, **kwargs):
        raise RuntimeError("simulator endpoint redirected")


class API:
    def __init__(self, url, expected_vm=None, expected_source=None):
        parsed = urlsplit(url)
        require(parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "::1"}
                and parsed.path in {"", "/"} and not parsed.username and not parsed.password
                and not parsed.query and not parsed.fragment, "expected guest-local simulator HTTP")
        self.url, self.expected_vm, self.expected_source = url.rstrip("/"), expected_vm, expected_source
        self.http = build_opener(ProxyHandler({}), NoRedirects())
        self.identity = None

    def request(self, path, body=None):
        request = Request(self.url + path, data=None if body is None else json.dumps(body).encode(),
                          headers={} if body is None else {"Content-Type": "application/json"})
        try:
            response = self.http.open(request, timeout=3)
        except HTTPError as error:
            response = error
        with response:
            raw = response.read((1 << 20) + 1)
            require(len(raw) <= 1 << 20, "oversized simulator response")
            return response.code, json.loads(raw)

    def status(self):
        code, state = self.request("/api/status")
        require(code == 200 and isinstance(state, dict) and state.get("simulation") is True
                and state.get("robot") == "g1" and state.get("robot_kind") == "g1"
                and type(state.get("profile_version")) is int and state["profile_version"] == 1
                and state.get("clock_mode") == "device"
                and state.get("policy_bundle") == "g1-29dof-velocity-v0-4960b847-v1",
                "endpoint is not the pinned G1 virtual robot")
        if self.expected_vm:
            require(state.get("vm_name") == self.expected_vm, "VM identity mismatch")
        if self.expected_source:
            require(state.get("source_digest") == self.expected_source, "runtime source mismatch")
        identity = state.get("vm_name"), state.get("source_digest"), state["policy_bundle"]
        require(self.identity is None or self.identity == identity, "simulator identity changed during acceptance")
        self.identity = identity
        return state

    def post(self, path, body=None, expected=200):
        require(path in {"/api/arm_ros", "/api/disarm_ros", "/api/pause", "/api/resume", "/api/reset"},
                "HTTP is restricted to explicit grants and lifecycle; motion must use DDS")
        self.status()
        code, value = self.request(path, body or {})
        require(code == expected, f"{path}: HTTP {code}, expected {expected}: {value}")
        return value


class Probe(Node):
    def __init__(self):
        super().__init__("wendy_g1_standard_acceptance")
        self.lock = threading.RLock()
        self.latest, self.receipts, self.counts, self.stamps = {}, {}, {}, {}
        self.pairs, self.transforms = {}, {}
        self.joints = deque(maxlen=1000)
        self.epoch = None
        self.publisher = None
        self.velocity = (0.0, 0.0, 0.0)
        self.publishing = False
        self.commands = 0
        self.spin_error = None
        for name, kind, topic in TOPICS:
            self.counts[name] = 0
            self.create_subscription(kind, topic, lambda msg, key=name: self.observe(key, msg),
                                     QoSProfile(depth=5, reliability=ReliabilityPolicy.BEST_EFFORT))
        self.create_subscription(TFMessage, "/tf", self.observe_tf, QoSProfile(depth=20))
        static = QoSProfile(depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL)
        self.create_subscription(TFMessage, "/tf_static", self.observe_tf, static)
        self.create_subscription(UInt64, "/simulation/epoch", self.observe_epoch, static)
        self.create_timer(0.05, self.publish)

    def remember(self, name, stamp, message):
        entries = self.pairs.setdefault(name, {})
        entries[stamp] = message
        while len(entries) > 12:
            entries.pop(next(iter(entries)))

    def observe(self, name, message):
        with self.lock:
            self.latest[name], self.receipts[name] = message, time.monotonic()
            self.counts[name] += 1
            stamp = int(message.tick) * 1_000_000 if name == "lowstate" else stamp_ns(message.header.stamp)
            self.stamps[name] = stamp
            if name in {"image", "camera_info", "scan", "cloud", "odom"}:
                self.remember(name, stamp, message)
            if name == "joints":
                self.joints.append((time.monotonic(), list(message.position)))

    def observe_tf(self, message):
        with self.lock:
            for transform in message.transforms:
                key = transform.header.frame_id, transform.child_frame_id
                self.transforms[key] = transform
                if key == ("odom", "base_link"):
                    self.remember("tf", stamp_ns(transform.header.stamp), transform)

    def observe_epoch(self, message):
        with self.lock:
            self.epoch = message.data

    def publish(self):
        with self.lock:
            if self.publisher is not None and self.publishing:
                message = Twist()
                message.linear.x, message.linear.y, message.angular.z = self.velocity
                self.publisher.publish(message)
                self.commands += 1

    def replace_publisher(self):
        with self.lock:
            if self.publisher is not None:
                self.destroy_publisher(self.publisher)
            self.publisher = self.create_publisher(Twist, "/cmd_vel", QoSProfile(depth=1))
            self.velocity, self.publishing = (0.0, 0.0, 0.0), True

    def command(self, value, publishing=True):
        with self.lock:
            self.velocity, self.publishing = tuple(value), publishing

    def messages(self):
        with self.lock:
            require(self.spin_error is None, f"ROS executor failed: {self.spin_error}")
            return self.latest.copy()

    def pair(self, first, second):
        with self.lock:
            a, b = self.pairs.get(first, {}), self.pairs.get(second, {})
            common = set(a) & set(b)
            return (a[max(common)], b[max(common)]) if common else None


def sources(state):
    return {entry["publisher_gid"]: entry for entry in state["ros_commands"]["sources"]}


def grant(api, probe):
    before = set(sources(api.status()))
    probe.replace_publisher()

    def discovered():
        candidates = [gid for gid, value in sources(api.status()).items()
                      if gid not in before and value["kind"] == "twist"
                      and value["age_ms"] < 300 and not value["requires_restart"]]
        require(len(candidates) <= 1, "ambiguous new Twist sources; refusing to guess ownership")
        return candidates[0] if candidates else None

    gid = wait_until(discovered, "new zero-command DDS publisher was not discovered")
    api.post("/api/arm_ros", {"publisher_gid": gid})
    wait_until(lambda: api.status()["ros_commands"]["owner"] == gid, "DDS grant did not become active")
    report("explicit_twist_grant", publisher_gid=gid)
    return gid


def sensor_contract(api, probe):
    names = {name for name, _, _ in TOPICS}
    wait_until(lambda: names <= set(probe.messages()), "standard/HG ROS observations are missing")
    messages = probe.messages()
    with probe.lock:
        require(all(time.monotonic() - probe.receipts[name] < 1 for name in names), "stale ROS receipts")
        require(all(-0.05 <= (time.time_ns() - probe.stamps[name]) / 1e9 < 1
                    for name in names - {"lowstate"}), "stale ROS capture stamps")
    joints, low = messages["joints"], messages["lowstate"]
    require(len(joints.name) == len(set(joints.name)) == len(joints.position)
            == len(joints.velocity) == len(joints.effort) == 29, "G1 must expose 29 physical joints")
    require(all(math.isfinite(value) for values in (joints.position, joints.velocity, joints.effort)
                for value in values), "nonfinite G1 joints")
    require(len(low.motor_state) == 35 and low.mode_pr == 0, "HG PR LowState wire shape mismatch")
    require(all(math.isfinite(value) for motor in low.motor_state[:29]
                for value in (motor.q, motor.dq, motor.ddq, motor.tau_est)), "nonfinite HG motor feedback")
    require(all(motor.mode == 0 and motor.q == motor.dq == motor.tau_est == 0
                for motor in low.motor_state[29:]), "inactive HG motor slots are not zero")
    joint_error = max(abs(motor.q - value) for motor, value in zip(low.motor_state[:29], joints.position))
    require(joint_error < 0.15, f"HG active motor order disagrees with JointState: {joint_error}")
    tick_age = api.status()["time"] - low.tick / 1000
    require(-0.1 <= tick_age < 1, f"HG tick is not fresh physics time: age={tick_age}")
    imu = messages["imu"]
    require(imu.header.frame_id == "imu_link" and all(math.isfinite(v) for v in xyz(imu.linear_acceleration)),
            "invalid IMU frame or acceleration")
    require(sum(v*v for v in xyz(imu.linear_acceleration)) > 1, "IMU is missing specific force")
    require(all(messages["odom"].pose.covariance[i] > 0 for i in (0, 7, 35)), "missing odometry covariance")
    odom, transform = wait_until(lambda: probe.pair("odom", "tf"), "same-stamp odometry/TF missing")
    require(odom.header.frame_id == "odom" and odom.child_frame_id == "base_link"
            and max(abs(a-b) for a, b in zip(xyz(odom.pose.pose.position), xyz(transform.transform.translation))) < 1e-6,
            "odometry frame/transform mismatch")
    for child, wanted in (("lidar_link", (0.16, 0.0, -0.10)), ("camera_link", (0.12, 0.0, 0.45))):
        mount = wait_until(lambda: probe.transforms.get(("base_link", child)), f"missing {child} transform")
        require(max(abs(a-b) for a, b in zip(xyz(mount.transform.translation), wanted)) < 1e-6,
                f"incorrect declared virtual G1 {child} mount")
    image, info = wait_until(lambda: probe.pair("image", "camera_info"), "same-stamp camera pair missing")
    require((image.width, image.height, image.encoding) == (640, 360, "rgb8")
            and image.step == 1920 and len(image.data) == 640*360*3
            and (info.width, info.height) == (640, 360) and info.k[0] > 0 and info.k[4] > 0
            and image.header.frame_id == info.header.frame_id == "camera_optical_frame", "invalid camera contract")
    scan, cloud = wait_until(lambda: probe.pair("scan", "cloud"), "same-stamp scan/cloud pair missing")
    require(scan.header.frame_id == cloud.header.frame_id == "lidar_link" and len(scan.ranges) == 360,
            "invalid lidar frames/sampling")
    require(cloud.point_step == 12 and cloud.height == 1 and cloud.row_step == cloud.width*12
            and len(cloud.data) == cloud.row_step and not cloud.is_bigendian
            and [(f.name, f.offset, f.datatype, f.count) for f in cloud.fields]
            == [("x", 0, 7, 1), ("y", 4, 7, 1), ("z", 8, 7, 1)], "invalid XYZ point cloud layout")
    points = list(struct.iter_unpack("<fff", bytes(cloud.data)))
    require(points and all(math.isfinite(v) for point in points for v in point), "invalid cloud coordinates")
    horizontal = {round((math.atan2(y, x) - scan.angle_min) / scan.angle_increment) % 360: math.hypot(x, y)
                  for x, y, z in points if abs(z) < 1e-5}
    finite = {i: value for i, value in enumerate(scan.ranges) if math.isfinite(value)}
    require(len(finite) >= 180 and set(horizontal) == set(finite)
            and all(scan.range_min <= distance <= scan.range_max
                    and abs(horizontal[i] - distance) < 1e-3 for i, distance in finite.items()),
            "horizontal cloud ring disagrees with the paired scan")
    report("g1_sensor_contract", physical_joints=29, hg_slots=35, maximum_motor_joint_error=joint_error,
           image_shape=[640, 360, "rgb8"], cloud_points=len(points), horizontal_returns=len(horizontal),
           hg_crc_checked=False)


def measure_rates(api, probe, seconds=12):
    with probe.lock:
        before, stamps = probe.counts.copy(), probe.stamps.copy()
    initial = api.status()
    started = time.monotonic()
    time.sleep(seconds)
    elapsed = time.monotonic() - started
    with probe.lock:
        rates = {name: (probe.counts[name] - count) / elapsed for name, count in before.items()}
        require(all(probe.stamps[name] > stamp for name, stamp in stamps.items()), "sensor timestamps froze")
    final = api.status()
    require(final["epoch"] == initial["epoch"], "world reset during rate measurement")
    floors = {"odom": 40, "truth": 40, "joints": 40, "imu": 160, "scan": 8,
              "cloud": 8, "image": 12, "camera_info": 12, "lowstate": 400}
    result = {"elapsed_seconds": elapsed, "received_hz": rates, "minimum_hz": floors,
              "physics_hz": (final["metrics"]["physics_steps"] - initial["metrics"]["physics_steps"]) / elapsed,
              "policy_hz": (final["metrics"]["policy_updates"] - initial["metrics"]["policy_updates"]) / elapsed}
    print(json.dumps({"event": "rate_measurement", **result}, allow_nan=False), flush=True)
    require(all(rates[name] >= minimum for name, minimum in floors.items()), f"receiver rate gate failed: {rates}")
    report("finite_full_reader_rates", **result)


def motion(api, probe, gid, label, command, direction, *, turn=False, seconds=3):
    before = probe.messages()
    started = time.monotonic()
    probe.command(command)
    time.sleep(seconds)
    probe.command((0.0, 0.0, 0.0))
    after = probe.messages()
    observed = displacement(before["odom"].pose.pose, after["odom"].pose.pose, turn)
    physical = displacement(before["truth"].pose, after["truth"].pose, turn)
    minimum = 0.25 if turn else 0.12
    require(direction*observed > minimum and direction*physical > minimum,
            f"{label}: insufficient/incorrect physical and odometry motion: {physical}, {observed}")
    with probe.lock:
        history = [positions for received, positions in probe.joints if received >= started]
    require(len(history) >= 20, "insufficient gait observations")
    travel = max(max(values)-min(values) for values in zip(*history))
    require(travel > 0.03 and 0.45 < after["truth"].pose.position.z < 1.15,
            f"{label}: G1 did not maintain an articulated supported posture")
    change = max((abs(a-b) for a, b in zip(before["scan"].ranges, after["scan"].ranges)
                  if math.isfinite(a) and math.isfinite(b)), default=0)
    require(change > 0.03, f"{label}: motion did not change lidar observations")
    camera_changed = hashlib.sha256(bytes(before["image"].data)).digest() != hashlib.sha256(bytes(after["image"].data)).digest()
    require(camera_changed, f"{label}: robot camera did not change with physical motion")
    state = api.status()
    require(state["ros_commands"]["owner"] == gid and state["healthy"] is True,
            f"{label}: lost owner or runtime health")
    report("physical_signed_motion", direction=label, command=list(command), command_seconds=seconds,
           physical_displacement=physical,
           odometry_displacement=observed, joint_travel=travel, maximum_scan_change=change,
           robot_camera_changed=camera_changed)
    time.sleep(1)


def fence(api, probe, gid, action):
    probe.command((0.3, 0.0, 0.0))
    time.sleep(0.3)
    result = api.post("/api/" + action)
    if action == "pause":
        paused = api.status()
        time.sleep(0.4)
        require(api.status()["time"] == paused["time"], "physics advanced while paused")
        api.post("/api/resume")
    else:
        wait_until(lambda: probe.epoch == result["epoch"], "ROS reset epoch did not arrive")
    wait_until(lambda: api.status()["ready"], f"robot did not recover after {action}")
    for _ in range(10):
        state = api.status()
        require(state["command"] == [0.0, 0.0, 0.0] and state["ros_commands"]["owner"] is None
                and sources(state)[gid]["requires_restart"], f"old DDS source rearmed after {action}")
        time.sleep(0.05)
    api.post("/api/arm_ros", {"publisher_gid": gid}, expected=403)
    replacement = grant(api, probe)
    require(replacement != gid, f"{action}: replacement publisher reused the blocked identity")
    report("stale_writer_fence", operation=action, old_gid=gid, replacement_gid=replacement,
           epoch=api.status()["epoch"])
    return replacement


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--url", default=os.environ.get("G1_SIMULATOR_URL", "http://127.0.0.1:8890"))
    parser.add_argument("--expected-vm")
    parser.add_argument("--expected-source")
    parser.add_argument("--seconds", type=float, default=60, help="minimum total duration, 45–120 seconds")
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    require(math.isfinite(args.seconds) and 45 <= args.seconds <= 120, "seconds must be 45–120")
    api = API(args.url, args.expected_vm, args.expected_source)
    identity = api.status()
    rclpy.init(args=[])
    probe = Probe()
    executor = SingleThreadedExecutor()
    executor.add_node(probe)

    def spin():
        try:
            executor.spin()
        except Exception as error:
            probe.spin_error = str(error)

    thread = threading.Thread(target=spin, daemon=True)
    thread.start()
    started = time.monotonic()
    result = {"passed": False, "checks": CHECKS, "environment": {
        "vm_name": identity.get("vm_name"), "source_digest": identity.get("source_digest"),
        "robot_kind": "g1", "profile_version": 1},
        "limitations": "Finite standard/HG ROS check; not the 600-second performance gate, native API/CRC acceptance or hardware fidelity."}
    try:
        api.post("/api/reset")
        wait_until(lambda: api.status()["ready"], "G1 did not become ready", timeout=25)
        gid = grant(api, probe)
        sensor_contract(api, probe)
        measure_rates(api, probe)
        for label, command, direction, turn, seconds in (
            ("forward", (0.3, 0.0, 0.0), 1, False, 3),
            ("backward", (-0.3, 0.0, 0.0), -1, False, 3),
            ("left_turn", (0.3, 0.0, 0.2), 1, True, 6),
        ):
            motion(api, probe, gid, label, command, direction, turn=turn, seconds=seconds)
        gid = fence(api, probe, gid, "pause")
        gid = fence(api, probe, gid, "reset")
        motion(api, probe, gid, "fresh_publisher_forward", (0.3, 0.0, 0.0), 1, seconds=2)
        while time.monotonic() - started < args.seconds:
            require(api.status()["healthy"] is True, "runtime failed during final observation")
            time.sleep(0.5)
        sensor_contract(api, probe)
        result["passed"] = True
    except Exception as error:
        result["error"] = str(error)
    finally:
        probe.command((0.0, 0.0, 0.0), publishing=False)
        try:
            api.post("/api/disarm_ros")
        except Exception as error:
            result["cleanup_error"] = str(error)
            result["passed"] = False
        executor.shutdown(timeout_sec=2)
        thread.join(timeout=2)
        probe.destroy_node()
        rclpy.shutdown()
    result["elapsed_seconds"] = time.monotonic() - started
    result["received_counts"] = probe.counts.copy()
    result["overall_received_hz"] = {
        name: count / result["elapsed_seconds"] for name, count in probe.counts.items()}
    result["overall_rate_scope"] = "Entire run, including discovery, deliberate pause/reset and cleanup; threshold gate uses its separate uninterrupted window."
    encoded = json.dumps(result, indent=2, allow_nan=False)
    print(encoded, flush=True)
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        args.output.write_text(encoded + "\n")
    return 0 if result["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())

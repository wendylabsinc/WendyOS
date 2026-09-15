"""ROS-only navigation acceptance against a separately running virtual Go2.

The tested controller runs in a child process and has no simulator imports or
HTTP access. This driver uses HTTP for virtual-world lifecycle and an explicit
command grant; goals, observations and every motion command traverse DDS.
"""

import argparse
from collections import deque
import hashlib
import json
import math
import os
from pathlib import Path
import struct
import subprocess
import sys
import threading
import time
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
from std_msgs.msg import String
from tf2_msgs.msg import TFMessage

from controller import stamp_ns

CHECKS = []


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def report(check, **details):
    result = {"check": check, "passed": True, **details}
    CHECKS.append(result)
    print(json.dumps(result, allow_nan=False), flush=True)


class SimulatorAPI:
    def __init__(self, base):
        parsed = urlsplit(base)
        require(parsed.scheme == "http" and parsed.hostname in {"127.0.0.1", "localhost", "::1"}
                and not parsed.username and not parsed.password and parsed.path in {"", "/"}
                and not parsed.query and not parsed.fragment,
                "acceptance only permits a guest-local simulator endpoint")
        self.base = base.rstrip("/")

    def request(self, path, body=None):
        request = Request(self.base + path, data=None if body is None else json.dumps(body).encode(),
                          headers={} if body is None else {"Content-Type": "application/json"})
        try:
            with urlopen(request, timeout=3) as response:
                return response.status, json.load(response)
        except HTTPError as error:
            return error.code, json.load(error)

    def status(self):
        code, value = self.request("/api/status")
        require(code == 200 and value.get("simulation") is True and value.get("robot") == "go2"
                and value.get("profile_version") == 1, "endpoint is not the Wendy Go2 virtual robot")
        return value

    def post(self, path, body=None):
        require(path in {"/api/reset", "/api/arm_ros", "/api/disarm_ros", "/api/obstacle"},
                "acceptance HTTP operations must not command robot motion")
        self.status()
        code, value = self.request(path, body or {})
        require(code == 200, f"{path}: HTTP {code}: {value}")
        return value


class Probe(Node):
    def __init__(self):
        super().__init__("wendy_go2_navigation_acceptance")
        self.lock = threading.RLock()
        self.latest = {}
        self.receipts = {}
        self.counts = {}
        self.transforms = set()
        self.images = deque(maxlen=120)
        self.history = deque(maxlen=20000)
        self.scans = {}
        self.clouds = {}
        self.relay_enabled = True
        self.controller = None
        self.command = None
        self.goal_pub = self.create_publisher(PoseStamped, "/goal_pose", QoSProfile(depth=1))
        self.scan_relay = self.create_publisher(LaserScan, "/navigation/test_scan", QoSProfile(depth=1))
        sensors = QoSProfile(depth=5, reliability=ReliabilityPolicy.BEST_EFFORT)
        for name, kind, topic in (
            ("odom", Odometry, "/odom"), ("truth", PoseStamped, "/simulation/ground_truth"),
            ("imu", Imu, "/imu/data"), ("joints", JointState, "/joint_states"),
            ("scan", LaserScan, "/scan"), ("image", Image, "/camera/color/image_raw"),
            ("camera_info", CameraInfo, "/camera/color/camera_info"),
            ("cloud", PointCloud2, "/utlidar/cloud"),
        ):
            self.create_subscription(kind, topic, lambda msg, key=name: self.observe(key, msg), sensors)
        self.create_subscription(String, "/navigation/status", self.controller_status, 10)
        self.create_subscription(Twist, "/cmd_vel", self.observe_command, 10)
        self.create_subscription(TFMessage, "/tf", self.tf, 10)
        self.create_subscription(TFMessage, "/tf_static", self.tf, QoSProfile(
            depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL))

    def observe(self, key, message):
        now = time.monotonic()
        with self.lock:
            self.latest[key] = message
            self.receipts[key] = now
            self.counts[key] = self.counts.get(key, 0) + 1
            if key == "scan":
                self.scans[stamp_ns(message.header.stamp)] = message
                if len(self.scans) > 30:
                    self.scans.pop(next(iter(self.scans)))
                if self.relay_enabled:
                    self.scan_relay.publish(message)
            elif key == "cloud":
                self.clouds[stamp_ns(message.header.stamp)] = message
                if len(self.clouds) > 30:
                    self.clouds.pop(next(iter(self.clouds)))
            elif key == "image":
                self.images.append((now, hashlib.sha256(bytes(message.data)).hexdigest()))
            elif key == "truth":
                p = message.pose.position
                self.history.append((now, p.x, p.y, p.z))

    def controller_status(self, message):
        with self.lock:
            self.controller = json.loads(message.data)
            self.receipts["controller"] = time.monotonic()

    def observe_command(self, message):
        with self.lock:
            self.command = (message.linear.x, message.linear.y, message.angular.z)
            self.receipts["command"] = time.monotonic()

    def tf(self, message):
        with self.lock:
            self.transforms.update((tf.header.frame_id, tf.child_frame_id) for tf in message.transforms)

    def state(self):
        with self.lock:
            return {"latest": dict(self.latest), "counts": dict(self.counts),
                    "receipts": dict(self.receipts), "controller": self.controller,
                    "command": self.command, "transforms": set(self.transforms),
                    "images": list(self.images), "history": list(self.history),
                    "scans": dict(self.scans), "clouds": dict(self.clouds)}

    def goal(self, x, y):
        message = PoseStamped()
        message.header.frame_id = "odom"
        message.header.stamp = self.get_clock().now().to_msg()
        message.pose.position.x, message.pose.position.y = x, y
        message.pose.orientation.w = 1.0
        self.goal_pub.publish(message)
        return stamp_ns(message.header.stamp)


class Acceptance:
    def __init__(self, api, probe):
        self.api, self.probe = api, probe
        self.process = None
        self.epoch = None
        self.gid = None
        self.last_health = 0.0

    def healthy(self):
        if self.process is not None:
            require(self.process.poll() is None, "navigation controller process exited")
        if time.monotonic() - self.last_health < 0.3:
            return
        status = self.api.status()
        self.last_health = time.monotonic()
        require(status.get("error") is None, f"simulator fault: {status.get('error')}")
        require(status["mode"] not in {"fallen", "fault", "paused", "damping"},
                f"robot unexpectedly entered {status['mode']}")
        require(self.epoch is None or status["epoch"] == self.epoch, "world epoch changed during navigation")
        require(self.gid is None or status["ros_commands"]["owner"] == self.gid,
                "navigation lost its explicit command ownership")
        position = status["position"]
        require(math.hypot(*position[:2]) < 5.4 and position[2] > 0.18,
                f"robot left the bounded upright navigation sandbox: {position}")

    def wait(self, predicate, message, timeout=10):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            self.healthy()
            result = predicate(self.probe.state())
            if result:
                return result
            time.sleep(0.025)
        raise AssertionError(message + ": " + json.dumps(self.probe.state()["controller"]))

    def hold(self, seconds):
        deadline = time.monotonic() + seconds
        while time.monotonic() < deadline:
            self.healthy()
            time.sleep(0.025)

    def stop_controller(self):
        if self.process is not None:
            self.process.terminate()
            try:
                self.process.wait(timeout=3)
            except subprocess.TimeoutExpired:
                self.process.kill()
                self.process.wait(timeout=3)
            self.process = None
        self.gid = None
        self.api.post("/api/disarm_ros")

    def start_scenario(self):
        self.stop_controller()
        self.epoch = self.api.post("/api/reset")["epoch"]
        self.api.post("/api/obstacle", {"position": [0.0, 3.0]})
        with self.probe.lock:
            self.probe.controller = None
            self.probe.command = None
            self.probe.relay_enabled = True
            self.probe.latest.clear()
            self.probe.receipts.clear()
            self.probe.history.clear()
            self.probe.images.clear()
        before = {item["publisher_gid"] for item in self.api.status()["ros_commands"]["sources"]}
        self.process = subprocess.Popen([sys.executable, str(Path(__file__).with_name("controller.py")),
                                         "--scan-topic", "/navigation/test_scan"])
        self.wait(lambda state: {"odom", "truth", "scan", "imu", "joints"}.issubset(state["latest"])
                  and state["controller"] and state["controller"]["tf_ready"]
                  and all(state["controller"]["ages_ms"].get(key, 1e6) < 200
                          for key in ("odom", "imu", "scan")), "navigation inputs were not ready")
        def discover(_):
            candidates = [source["publisher_gid"] for source in self.api.status()["ros_commands"]["sources"]
                          if source["publisher_gid"] not in before and source["kind"] == "twist"
                          and source["age_ms"] < 200 and not source["requires_restart"]]
            require(len(candidates) <= 1, "multiple new ROS command sources appeared; refusing ambiguous grant")
            return candidates[0] if candidates else None
        gid = self.wait(discover, "controller's unique ROS publisher was not observed")
        self.api.post("/api/arm_ros", {"publisher_gid": gid})
        self.gid = gid
        self.hold(0.4)
        require(self.probe.state()["command"] == (0.0, 0.0, 0.0), "startup emitted motion without a goal")
        return self.probe.state()

    def send_goal(self, x, y):
        goal_stamp = self.probe.goal(x, y)
        self.wait(lambda state: state["controller"] and state["controller"]["goal_stamp"] == goal_stamp,
                  "controller did not receive the ROS goal")
        return goal_stamp

    def arrive(self, goal_stamp, x, y, timeout=35):
        state = self.wait(lambda state: state if state["controller"]
                          and state["controller"]["goal_stamp"] == goal_stamp
                          and state["controller"]["state"] == "arrived" else None,
                          "goal did not arrive", timeout)
        odom = state["latest"]["odom"].pose.pose.position
        error = math.hypot(odom.x - x, odom.y - y)
        require(error <= 0.27, f"arrival exceeded the 25 cm tolerance plus a 2 cm delivery margin: {error}")
        self.settle()
        return error

    def settle(self):
        settled_since = None
        def settled(state):
            nonlocal settled_since
            velocity = state["latest"]["odom"].twist.twist
            low = math.hypot(velocity.linear.x, velocity.linear.y) < 0.12 and abs(velocity.angular.z) < 0.2
            settled_since = (settled_since or time.monotonic()) if low else None
            return settled_since is not None and time.monotonic() - settled_since >= 0.5
        self.wait(settled, "physical robot did not settle after zero command", timeout=5)

    def readiness(self, state):
        odom = state["latest"]["odom"]
        require(all(odom.pose.covariance[index] > 0 for index in (0, 7, 35)),
                "odometry did not declare positive planar pose covariance")
        require({("odom", "base_link"), ("base_link", "lidar_link"), ("base_link", "imu_link")}
                .issubset(state["transforms"]), "required navigation TF mounts are missing")
        require(state["latest"]["imu"].header.frame_id == "imu_link", "unexpected IMU frame")
        report("navigation_readiness", pose_covariance=[odom.pose.covariance[i] for i in (0, 7, 35)],
               publisher_gid=self.gid)

    def perception(self, before_image_hashes):
        state = self.wait(lambda state: state if {"image", "camera_info", "cloud"}.issubset(state["latest"])
                          else None, "robot RGB camera and 3D lidar topics were not available", timeout=15)
        rgb, info, cloud = (state["latest"][name] for name in ("image", "camera_info", "cloud"))
        require(rgb.encoding == "rgb8" and (rgb.width, rgb.height) == (640, 360)
                and rgb.step == rgb.width * 3 and len(rgb.data) == rgb.step * rgb.height,
                "robot camera RGB payload is malformed")
        require(rgb.header.frame_id == info.header.frame_id == "camera_optical_frame"
                and (info.width, info.height) == (rgb.width, rgb.height), "camera metadata/frame mismatch")
        expected_focal = 360 / (2 * math.tan(math.radians(60) / 2))
        require(abs(info.k[0] - expected_focal) < 1e-4 and abs(info.k[4] - expected_focal) < 1e-4
                and abs(info.k[2] - 319.5) < 1e-4 and abs(info.k[5] - 179.5) < 1e-4,
                "camera intrinsics do not describe its rendered pinhole model")
        require(("base_link", "camera_link") in state["transforms"]
                and ("camera_link", "camera_optical_frame") in state["transforms"], "camera TF chain missing")
        require(before_image_hashes and any(digest not in before_image_hashes for _, digest in state["images"]),
                "robot camera pixels did not change following physical motion")
        require(cloud.header.frame_id == "lidar_link" and cloud.width > 360 and cloud.height == 1
                and len(cloud.data) == cloud.row_step, "3D lidar payload is malformed or flat")
        fields = {field.name: field for field in cloud.fields}
        require(all(name in fields and fields[name].datatype == 7 and fields[name].count == 1
                    for name in ("x", "y", "z")), "point cloud is missing float32 XYZ fields")
        endian = ">" if cloud.is_bigendian else "<"
        points = [tuple(struct.unpack_from(endian + "f", cloud.data, index * cloud.point_step + fields[name].offset)[0]
                        for name in ("x", "y", "z")) for index in range(cloud.width)]
        require(all(all(math.isfinite(value) for value in point) for point in points), "cloud contains non-finite hits")
        require(any(abs(point[2]) > 0.25 for point in points), "cloud does not contain elevated/depressed rays")
        common = state["scans"].keys() & state["clouds"].keys()
        require(common, "scan and cloud do not share capture timestamps")
        matching_stamp = max(common)
        scan = state["scans"][matching_stamp]
        same_cloud = state["clouds"][matching_stamp]
        same_fields = {field.name: field.offset for field in same_cloud.fields}
        same_endian = ">" if same_cloud.is_bigendian else "<"
        horizontal = []
        for index in range(same_cloud.width):
            point = tuple(struct.unpack_from(same_endian + "f", same_cloud.data,
                          index * same_cloud.point_step + same_fields[name])[0] for name in ("x", "y", "z"))
            if abs(point[2]) < 1e-6:
                horizontal.append(point)
        require(len(horizontal) >= 200, "cloud's horizontal ring is missing")
        for x, y, _ in horizontal:
            angle = math.atan2(y, x)
            index = round((angle - scan.angle_min) / scan.angle_increment) % len(scan.ranges)
            require(math.isfinite(scan.ranges[index]) and abs(math.hypot(x, y) - scan.ranges[index]) < 2e-4,
                    "scan differs from the same capture's horizontal cloud ring")
        report("robot_perception", image=[rgb.width, rgb.height, rgb.encoding], points=cloud.width,
               matching_horizontal_returns=len(horizontal), capture_stamp_ns=matching_stamp)

    def goal_arrival(self, require_perception):
        state = self.start_scenario()
        self.readiness(state)
        if require_perception:
            self.wait(lambda state: len(state["images"]) >= 3, "camera did not become ready", timeout=15)
        image_hashes = {digest for _, digest in self.probe.state()["images"]}
        origin = state["latest"]["truth"].pose.position
        initial_odom = state["latest"]["odom"].pose.pose.position
        initial_joints = list(state["latest"]["joints"].position)
        x, y = initial_odom.x + 1.2, initial_odom.y + 0.2
        goal_stamp = self.send_goal(x, y)
        error = self.arrive(goal_stamp, x, y)
        end = self.probe.state()
        position = end["latest"]["truth"].pose.position
        displacement = math.hypot(position.x - origin.x, position.y - origin.y)
        require(displacement > 0.7, "goal completion did not correspond to physical displacement")
        require(any(abs(a - b) > 0.02 for a, b in zip(initial_joints, end["latest"]["joints"].position)),
                "goal motion did not articulate the robot joints")
        report("goal_arrival", goal=[x, y], odometry_error_m=error, actual_displacement_m=displacement)
        if require_perception:
            self.perception(image_hashes)

    def obstacle(self):
        state = self.start_scenario()
        odom, truth = state["latest"]["odom"].pose.pose.position, state["latest"]["truth"].pose.position
        box = (truth.x + 1.7, truth.y)
        self.api.post("/api/obstacle", {"position": list(box)})
        goal = (odom.x + 3.0, odom.y)
        started = time.monotonic()
        goal_stamp = self.send_goal(*goal)
        self.wait(lambda state: state["controller"]["state"] == "obstacle",
                  "in-path physical obstacle did not stop the controller", timeout=20)
        self.settle()
        held_from = self.probe.state()["latest"]["truth"].pose.position
        self.hold(2)
        stopped = self.probe.state()
        held_to = stopped["latest"]["truth"].pose.position
        require(stopped["controller"]["state"] == "obstacle" and stopped["command"] == (0.0, 0.0, 0.0),
                "controller did not hold zero command for the obstacle")
        drift = math.hypot(held_to.x - held_from.x, held_to.y - held_from.y)
        require(drift < 0.15, f"robot continued progressing into the obstacle while stopped: {drift}")
        minimum_center_clearance = min(math.hypot(max(abs(x - box[0]) - 0.4, 0),
                                                 max(abs(y - box[1]) - 0.5, 0))
                                       for stamp, x, y, _ in stopped["history"] if stamp >= started)
        require(minimum_center_clearance > 0.5, "robot approached the box within its body clearance")
        require(math.hypot(held_to.x - truth.x, held_to.y - truth.y) > 0.05,
                "obstacle test did not first demonstrate actual approach")
        self.api.post("/api/obstacle", {"position": [0.0, 3.0]})
        error = self.arrive(goal_stamp, *goal, timeout=35)
        report("obstacle_stop_and_resume", box=list(box), held_drift_m=drift,
               minimum_body_center_to_box_m=minimum_center_clearance, arrival_error_m=error)

    def stale_scan(self):
        state = self.start_scenario()
        odom, truth = state["latest"]["odom"].pose.pose.position, state["latest"]["truth"].pose.position
        self.send_goal(odom.x + 2.0, odom.y)
        self.wait(lambda state: state["controller"]["state"] == "moving"
                  and state["latest"]["truth"].pose.position.x - truth.x > 0.2,
                  "stale-scan test did not first demonstrate walking", timeout=10)
        before = self.probe.state()
        physics_before = self.api.status()["time"]
        disabled = time.monotonic()
        with self.probe.lock:
            self.probe.relay_enabled = False
        self.wait(lambda state: state["controller"]["state"] == "stale_scan"
                  and state["command"] == (0.0, 0.0, 0.0), "stale scan did not cancel and zero the goal", timeout=0.7)
        stop_delay = time.monotonic() - disabled
        require(stop_delay <= 0.45, f"scan loss stop exceeded 300 ms freshness plus delivery margin: {stop_delay}")
        self.settle()
        self.hold(1.2)
        after = self.probe.state()
        physics_after = self.api.status()["time"]
        require(physics_after - physics_before > 1 and after["counts"]["scan"] - before["counts"]["scan"] >= 10
                and after["counts"]["odom"] - before["counts"]["odom"] >= 50,
                "stale-data stop was not exercised while physics and raw ROS sensors kept running")
        with self.probe.lock:
            self.probe.relay_enabled = True
        self.hold(1)
        after = self.probe.state()
        require(after["controller"]["state"] == "stale_scan" and after["controller"]["goal"] is None
                and after["controller"]["ages_ms"]["scan"] < 200
                and after["command"] == (0.0, 0.0, 0.0), "fresh scans restarted the cancelled old goal")
        odom = after["latest"]["odom"].pose.pose.position
        goal = (odom.x + 0.9, odom.y)
        goal_stamp = self.send_goal(*goal)
        error = self.arrive(goal_stamp, *goal)
        report("stale_scan_stop", zero_command_delay_ms=stop_delay * 1000,
               physics_advanced_seconds=physics_after - physics_before,
               raw_scan_messages=after["counts"]["scan"] - before["counts"]["scan"],
               fresh_goal_arrival_error_m=error, automatic_resume=False)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--simulator-url", default=os.getenv("GO2_SIMULATOR_URL", "http://127.0.0.1:8890"))
    parser.add_argument("--without-perception", action="store_true",
                        help="run only motion scenarios before the camera/cloud rollout; not full acceptance")
    parser.add_argument("--output", type=Path, help="write the complete machine-readable acceptance report")
    args = parser.parse_args()
    api = SimulatorAPI(args.simulator_url)
    identity = api.status()
    rclpy.init()
    probe = Probe()
    executor = SingleThreadedExecutor()
    executor.add_node(probe)
    thread = threading.Thread(target=executor.spin, daemon=True)
    thread.start()
    acceptance = Acceptance(api, probe)
    started = time.monotonic()
    failure = None
    try:
        acceptance.goal_arrival(not args.without_perception)
        acceptance.obstacle()
        acceptance.stale_scan()
    except Exception as error:
        failure = error
    finally:
        try:
            acceptance.stop_controller()
        except Exception as error:
            failure = failure or error
        executor.shutdown(timeout_sec=3)
        thread.join(timeout=3)
        probe.destroy_node()
        rclpy.shutdown()
    result = {"result": "passed" if failure is None else "failed",
              "full_sensor_acceptance": not args.without_perception and failure is None,
              "elapsed_seconds": time.monotonic() - started,
              "error": None if failure is None else str(failure)}
    if args.output:
        args.output.parent.mkdir(parents=True, exist_ok=True)
        artifact = {**result, "checks": CHECKS, "finished_at_unix_ns": time.time_ns(),
                    "runtime": {key: identity.get(key) for key in
                                ("source_digest", "policy_bundle", "clock_mode", "world", "visual_detail")},
                    "source_sha256": {name: hashlib.sha256(Path(__file__).with_name(name).read_bytes()).hexdigest()
                                      for name in ("exercise.py", "controller.py")}}
        args.output.write_text(json.dumps(artifact, indent=2, allow_nan=False) + "\n")
    print(json.dumps(result, allow_nan=False), flush=True)
    if failure is not None:
        raise failure


if __name__ == "__main__":
    main()

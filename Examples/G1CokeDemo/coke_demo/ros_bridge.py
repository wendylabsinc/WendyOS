"""ROS 2 connection to the 43-joint Coke world, without renderer ownership.

ROS imports are lazy so local physics and policy checks do not require ROS.
The application owns MuJoCo and passes copied observations to ``publish``.
Commands pass through DDS, including commands from ``send_target``. A single
point JointTrajectory is a position setpoint, not a trajectory interpolation
service. Reset/pause/resume services acknowledge queuing; the application
worker applies them and reports the resulting state on simulation/status.
Idle frames replay cached ROS messages at 1 Hz for late subscribers. Those
messages retain their capture timestamps and do not represent new exposures.
"""
from __future__ import annotations

import json
import math
import queue
import threading
import time

import numpy as np


def validate_target(names, positions, expected_names, bounds=None):
    """Require all named joints once; reorder positions to the model order."""
    names, expected_names = list(names), list(expected_names)
    if len(names) != len(expected_names) or len(set(names)) != len(names) or set(names) != set(expected_names):
        raise ValueError("command must name every model joint exactly once")
    values = np.asarray(positions, dtype=np.float64)
    if values.shape != (len(names),) or not np.isfinite(values).all():
        raise ValueError("command must contain one finite position per joint")
    result = values[[names.index(name) for name in expected_names]]
    if bounds is not None:
        limits = np.asarray(bounds, dtype=np.float64)
        if np.any(result < limits[:, 0] - 1e-6) or np.any(result > limits[:, 1] + 1e-6):
            raise ValueError("command exceeds model joint bounds")
    return result.copy()


def camera_intrinsics(width, height, fovy):
    focal = height / (2 * math.tan(math.radians(float(fovy)) / 2))
    return [focal, 0., (width - 1) / 2, 0., focal, (height - 1) / 2, 0., 0., 1.]


def rotation_quaternion(matrix):
    """Convert a proper rotation matrix to a ROS XYZW quaternion."""
    m = np.asarray(matrix, dtype=np.float64).reshape(3, 3)
    q = np.empty(4)
    trace = np.trace(m)
    if trace > 0:
        s = 2 * math.sqrt(trace + 1)
        q[:] = [(m[2, 1] - m[1, 2]) / s, (m[0, 2] - m[2, 0]) / s,
                (m[1, 0] - m[0, 1]) / s, s / 4]
    else:
        i = int(np.argmax(np.diag(m)))
        j, k = (i + 1) % 3, (i + 2) % 3
        s = 2 * math.sqrt(max(0., 1 + m[i, i] - m[j, j] - m[k, k]))
        q[i], q[j], q[k], q[3] = s / 4, (m[j, i] + m[i, j]) / s, (m[k, i] + m[i, k]) / s, (m[k, j] - m[j, k]) / s
    return q / np.linalg.norm(q)


class ROSBridge:
    def __init__(self, joint_names, namespace="/coke/g1", camera_fovy=58., joint_bounds=None):
        self.joint_names = list(joint_names)
        if len(self.joint_names) != 43 or len(set(self.joint_names)) != 43:
            raise ValueError("Coke ROS bridge requires 43 unique joint names")
        self.namespace = "/" + namespace.strip("/")
        self.frame_prefix = self.namespace.strip("/")
        self.camera_fovy = float(camera_fovy)
        self.joint_bounds = joint_bounds
        self._commands = queue.Queue(maxsize=1)
        self._events = queue.Queue(maxsize=16)
        self._stop = threading.Event()
        self._lock = threading.RLock()
        self._thread = self.node = self._context = self._executor = None
        self._last_sample = self._last_camera = None
        self._last_publish_monotonic = None
        self._cached_messages = {}
        self._cached_clock = None
        self._motion_transforms = []
        self._camera_transform = None
        self._counts = dict(observations=0, camera_frames=0, heartbeats=0, sent=0, accepted=0, rejected=0)
        self._error = None

    def start(self):
        if self.node is not None:
            return
        import rclpy
        from rclpy.context import Context
        from rclpy.executors import SingleThreadedExecutor
        from rclpy.node import Node
        from rclpy.qos import QoSProfile, ReliabilityPolicy, DurabilityPolicy
        from builtin_interfaces.msg import Time
        from geometry_msgs.msg import TransformStamped
        from sensor_msgs.msg import JointState, Image, CameraInfo, Imu
        from nav_msgs.msg import Odometry
        from rosgraph_msgs.msg import Clock
        from std_msgs.msg import Bool, String, UInt64
        from std_srvs.srv import Trigger
        from trajectory_msgs.msg import JointTrajectory, JointTrajectoryPoint
        from tf2_ros import TransformBroadcaster

        self._types = locals().copy()
        self._context = Context()
        rclpy.init(context=self._context)
        self.node = Node("coke_virtual_g1", namespace=self.namespace, context=self._context)
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE)
        persistent = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE,
                                durability=DurabilityPolicy.TRANSIENT_LOCAL)
        self._publishers = {}
        for name, msg in (("joint_states", JointState), ("camera/color/image_raw", Image),
                          ("camera/depth/image_rect_raw", Image), ("camera/color/camera_info", CameraInfo),
                          ("camera/depth/camera_info", CameraInfo), ("perception/can_mask", Image),
                          ("perception/can_visible", Bool), ("imu/data", Imu), ("odom", Odometry)):
            self._publishers[name] = self.node.create_publisher(msg, name, qos)
        self._publishers["simulation/status"] = self.node.create_publisher(String, "simulation/status", persistent)
        self._publishers["simulation/epoch"] = self.node.create_publisher(UInt64, "simulation/epoch", persistent)
        self._clock = self.node.create_publisher(Clock, "/clock", qos)
        self._command_pub = self.node.create_publisher(JointTrajectory, "joint_trajectory", qos)
        self.node.create_subscription(JointTrajectory, "joint_trajectory", self._receive_command, qos)
        self._tf = TransformBroadcaster(self.node)
        for event in ("reset", "pause", "resume"):
            self.node.create_service(Trigger, "simulation/" + event,
                                     lambda request, response, name=event: self._receive_event(name, response))
        self._executor = SingleThreadedExecutor(context=self._context)
        self._executor.add_node(self.node)
        self._thread = threading.Thread(target=self._spin, name="coke-ros2", daemon=True)
        self._thread.start()
        deadline = time.monotonic() + 5
        while self._command_pub.get_subscription_count() < 1:
            if time.monotonic() >= deadline or self._stop.wait(.01):
                self.close()
                raise RuntimeError("ROS command loopback did not discover its subscriber")

    def _spin(self):
        try:
            while not self._stop.is_set():
                self._executor.spin_once(timeout_sec=.05)
        except Exception as exc:
            if not self._stop.is_set():
                self._error = str(exc)
                self._stop.set()

    def _receive_command(self, message):
        try:
            if len(message.points) != 1:
                raise ValueError("expected one position setpoint; multi-point trajectories are unsupported")
            point = message.points[0]
            if point.velocities or point.accelerations or point.effort:
                raise ValueError("only position setpoints are supported")
            duration = point.time_from_start
            if duration.sec != 0 or not 0 <= duration.nanosec <= 25_000_000:
                raise ValueError("setpoint time_from_start must be between zero and 25 ms")
            command = validate_target(message.joint_names, point.positions, self.joint_names, self.joint_bounds)
            with self._lock:
                try:
                    self._commands.get_nowait()
                except queue.Empty:
                    # The queue is already empty; there is no stale command to discard.
                    pass
                self._commands.put_nowait((time.monotonic(), command))
                self._counts["accepted"] += 1
        except (ValueError, TypeError) as exc:
            with self._lock:
                self._counts["rejected"] += 1
                self._error = str(exc)
            self.node.get_logger().warning(str(exc))

    def _receive_event(self, name, response):
        try:
            self._events.put_nowait(name)
            response.success, response.message = True, name + " queued for simulation worker"
        except queue.Full:
            response.success, response.message = False, "simulation event queue is full"
        return response

    def take_command(self, timeout=0):
        try:
            received, command = self._commands.get(timeout=timeout)
        except queue.Empty:
            return None
        if time.monotonic() - received > .5:
            self._counts["rejected"] += 1
            self._error = "expired position setpoint"
            return None
        return command

    def clear_commands(self):
        with self._lock:
            while True:
                try:
                    self._commands.get_nowait()
                except queue.Empty:
                    break

    def take_event(self):
        try:
            return self._events.get_nowait()
        except queue.Empty:
            return None

    def send_target(self, target43):
        command = validate_target(self.joint_names, target43, self.joint_names, self.joint_bounds)
        with self._lock:
            if self.node is None or self._stop.is_set():
                raise RuntimeError("ROS bridge is not running")
            msg = self._types["JointTrajectory"]()
            msg.joint_names = self.joint_names
            point = self._types["JointTrajectoryPoint"]()
            point.positions = command.tolist()
            point.time_from_start.nanosec = 25_000_000
            msg.points = [point]
            self._command_pub.publish(msg)
            self._counts["sent"] += 1

    def _stamp(self, seconds):
        sec, nanosec = divmod(round(float(seconds) * 1e9), 1_000_000_000)
        return self._types["Time"](sec=sec, nanosec=nanosec)

    def _header(self, msg, stamp, frame):
        msg.header.stamp, msg.header.frame_id = stamp, self.frame_prefix + "/" + frame

    @staticmethod
    def _vector(target, values):
        target.x, target.y, target.z = map(float, values)

    @staticmethod
    def _quaternion(target, xyzw):
        target.x, target.y, target.z, target.w = map(float, xyzw)

    def _transform(self, stamp, parent, child, position, xyzw):
        tf = self._types["TransformStamped"]()
        self._header(tf, stamp, parent)
        tf.child_frame_id = self.frame_prefix + "/" + child
        self._vector(tf.transform.translation, position)
        self._quaternion(tf.transform.rotation, xyzw)
        return tf

    def publish(self, observation, status=None):
        with self._lock:
            if self.node is None or self._stop.is_set():
                raise RuntimeError("ROS bridge is not running")
            obs, types = observation, self._types
            if status is not None:
                self._publishers["simulation/status"].publish(types["String"](
                    data=json.dumps(status, separators=(",", ":"), allow_nan=False)))
            identity = (obs.get("epoch", 0), obs["frame"])
            now = time.monotonic()
            if identity == self._last_sample:
                if now - self._last_publish_monotonic >= 1.:
                    self._republish_cached()
                    self._last_publish_monotonic = now
                    self._counts["heartbeats"] += 1
                return
            if self._last_sample is None or self._last_sample[0] != identity[0]:
                self._publishers["simulation/epoch"].publish(types["UInt64"](data=int(identity[0])))
            self._last_sample = identity
            self._last_publish_monotonic = now
            stamp = self._stamp(obs["sim_time"])
            self._cached_clock = types["Clock"](clock=stamp)
            self._clock.publish(self._cached_clock)
            joints = types["JointState"]()
            self._header(joints, stamp, "base_link")
            joints.name = self.joint_names
            joints.position = np.asarray(obs["q43"], dtype=float).tolist()
            joints.velocity = np.asarray(obs["dq43"], dtype=float).tolist()
            if "joint_effort43" in obs:
                joints.effort = np.asarray(obs["joint_effort43"], dtype=float).tolist()
            self._emit("joint_states", joints)
            self._publish_motion(obs, stamp)
            self._counts["observations"] += 1
            camera_identity = (identity[0], obs["camera_frame"])
            if camera_identity != self._last_camera:
                self._publish_camera(obs)
                self._last_camera = camera_identity
                self._counts["camera_frames"] += 1

    def _emit(self, topic, message):
        self._publishers[topic].publish(message)
        self._cached_messages[topic] = message

    def _republish_cached(self):
        self._clock.publish(self._cached_clock)
        for topic, message in self._cached_messages.items():
            self._publishers[topic].publish(message)
        transforms = self._motion_transforms.copy()
        if self._camera_transform is not None:
            transforms.append(self._camera_transform)
        if transforms:
            self._tf.sendTransform(transforms)

    def _publish_motion(self, obs, stamp):
        if "base_position" not in obs:
            return
        xyzw = np.asarray(obs["base_quaternion_wxyz"])[[1, 2, 3, 0]]
        odom = self._types["Odometry"]()
        self._header(odom, stamp, "world")
        odom.child_frame_id = self.frame_prefix + "/base_link"
        self._vector(odom.pose.pose.position, obs["base_position"])
        self._quaternion(odom.pose.pose.orientation, xyzw)
        self._vector(odom.twist.twist.linear, obs["base_linear_velocity"])
        self._vector(odom.twist.twist.angular, obs["base_angular_velocity"])
        # Exact simulated state, without an invented estimator or noise model.
        self._emit("odom", odom)
        imu = self._types["Imu"]()
        self._header(imu, stamp, "base_link")
        self._quaternion(imu.orientation, xyzw)
        self._vector(imu.angular_velocity, obs["base_angular_velocity"])
        self._vector(imu.linear_acceleration, obs["imu_specific_force"])
        self._emit("imu/data", imu)
        transforms = [self._transform(stamp, "world", "base_link", obs["base_position"], xyzw)]
        for body in obs.get("body_transforms", []):
            transforms.append(self._transform(stamp, body.get("parent", "world"), body["name"],
                                              body["position"], np.asarray(body["quaternion_wxyz"])[[1, 2, 3, 0]]))
        self._motion_transforms = transforms
        self._tf.sendTransform(transforms)

    def _publish_camera(self, obs):
        stamp = self._stamp(obs["camera_sim_time"])
        rgb = np.ascontiguousarray(obs["rgb_u8"], dtype=np.uint8)
        depth = np.ascontiguousarray(obs["depth_m"], dtype="<f4")
        mask = np.ascontiguousarray(np.asarray(obs["mask"], dtype=np.uint8) * 255)
        for name, array, encoding in (("camera/color/image_raw", rgb, "rgb8"),
                                      ("camera/depth/image_rect_raw", depth, "32FC1"),
                                      ("perception/can_mask", mask, "mono8")):
            msg = self._types["Image"]()
            self._header(msg, stamp, "camera_optical_frame")
            msg.height, msg.width = map(int, array.shape[:2])
            msg.encoding, msg.is_bigendian, msg.step = encoding, 0, int(array.strides[0])
            msg.data = array.tobytes()
            self._emit(name, msg)
        info = self._types["CameraInfo"]()
        self._header(info, stamp, "camera_optical_frame")
        info.height, info.width = map(int, rgb.shape[:2])
        info.distortion_model, info.d = "plumb_bob", [0.] * 5
        info.k = camera_intrinsics(info.width, info.height, obs.get("camera_fovy", self.camera_fovy))
        info.r = np.eye(3).reshape(-1).tolist()
        k = info.k
        info.p = [k[0], 0., k[2], 0., 0., k[4], k[5], 0., 0., 0., 1., 0.]
        self._emit("camera/color/camera_info", info)
        self._emit("camera/depth/camera_info", info)
        self._emit("perception/can_visible", self._types["Bool"](data=bool(obs["detection_valid"])))
        if "camera_position" in obs:
            # MuJoCo camera looks down -Z with +Y up. ROS optical is +Z
            # forward with +Y down, a 180 degree rotation around camera X.
            rotation = np.asarray(obs["camera_rotation"]).reshape(3, 3) @ np.diag([1., -1., -1.])
            self._camera_transform = self._transform(stamp, "world", "camera_optical_frame",
                                                     obs["camera_position"], rotation_quaternion(rotation))
            self._tf.sendTransform(self._camera_transform)

    def status(self):
        with self._lock:
            return {"enabled": self.node is not None and not self._stop.is_set(),
                    "namespace": self.namespace, "command_topic": self.namespace + "/joint_trajectory",
                    "joint_count": len(self.joint_names), "counts": self._counts.copy(), "error": self._error}

    def close(self):
        self._stop.set()
        if self._thread is not None:
            self._thread.join(timeout=2)
        with self._lock:
            if self._executor is not None:
                self._executor.shutdown(timeout_sec=1)
            if self.node is not None:
                self.node.destroy_node()
                self.node = None
            if self._context is not None and self._context.ok():
                self._context.shutdown()

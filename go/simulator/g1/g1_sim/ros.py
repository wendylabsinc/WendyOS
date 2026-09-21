"""Standard ROS 2 observations from a coherently sampled MuJoCo world.

Device mode uses wall timestamps. Frozen physics produces no fresh messages.
Odometry is a simple velocity integrator with documented bias/covariance;
exact pose is exposed separately as /simulation/ground_truth.
"""

import math
import os
import threading
import time

import numpy as np
import rclpy
from builtin_interfaces.msg import Time
from geometry_msgs.msg import PoseStamped, TransformStamped
from nav_msgs.msg import Odometry
from rclpy.executors import SingleThreadedExecutor
from rclpy.node import Node
from rclpy.qos import DurabilityPolicy, QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import Imu, JointState
from std_msgs.msg import UInt64
from tf2_ros import StaticTransformBroadcaster, TransformBroadcaster

from .sensors import IMU_POSITION, LIDAR_POSITION, CAMERA_POSITION, PhysicsSampler
from .observations import SnapshotQueue, load_snapshot
from .slow_sensors import SlowObservations


def vector(target, values):
    target.x, target.y, target.z = map(float, values)


def quaternion(target, wxyz):
    target.w, target.x, target.y, target.z = map(float, wxyz)


def covariance(size, diagonals):
    result = [0.0] * (size * size)
    for index, value in enumerate(diagonals):
        result[index * size + index] = value
    return result


class RobotObservations(Node):
    def __init__(self, runtime):
        super().__init__("wendy_g1_observations")
        self.runtime = runtime
        self.sampler = PhysicsSampler(runtime.sim)
        self.qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.RELIABLE)
        self.imu_pub = self.create_publisher(Imu, "/imu/data", self.qos)
        self.lidar_imu_pub = self.create_publisher(Imu, "/utlidar/imu", self.qos)
        self.joint_pub = self.create_publisher(JointState, "/joint_states", self.qos)
        self.odom_pub = self.create_publisher(Odometry, "/odom", self.qos)
        self.truth_pub = self.create_publisher(PoseStamped, "/simulation/ground_truth", self.qos)
        self.epoch_pub = self.create_publisher(UInt64, "/simulation/epoch", QoSProfile(
            depth=1, durability=DurabilityPolicy.TRANSIENT_LOCAL))
        self.tf = TransformBroadcaster(self)
        self.static_tf = StaticTransformBroadcaster(self)
        self.last_sample = None
        self.next_imu = self.next_joint = 0.0
        self.position = np.zeros(3)
        self.yaw_bias = 0.0
        self.last_time = None
        self.samples = {"imu": 0, "joints": 0, "odom": 0, "scan": 0}
        self.last_published_monotonic = None
        self.native_state = self.native_commands = None
        if os.environ.get("G1_NATIVE", "0") == "1":
            from .native_state import NativeState
            from .native_commands import NativeCommands
            self.native_state = NativeState(self, runtime)
            self.native_commands = NativeCommands(self, runtime)
            runtime.ros_commands.native_handler = self.native_commands.receive
        # Publishers serialize synchronously. Reuse owned message instances on
        # this worker instead of rebuilding fixed fields at every sensor tick.
        self.imu = Imu()
        self.imu.header.frame_id = "imu_link"
        self.imu.orientation_covariance = covariance(3, [0.0025] * 3)
        self.imu.angular_velocity_covariance = covariance(3, [0.0004] * 3)
        self.imu.linear_acceleration_covariance = covariance(3, [0.04] * 3)
        self.joint = JointState()
        self.joint.name = list(runtime.sim.joint_names)
        self.odom = Odometry()
        self.odom.header.frame_id, self.odom.child_frame_id = "odom", "base_link"
        self.odom.pose.covariance = covariance(6, [0.01, 0.01, 0.02, 0.0025, 0.0025, 0.005])
        self.odom.twist.covariance = covariance(6, [0.01, 0.01, 0.02, 0.0025, 0.0025, 0.005])
        self.odom_tf = TransformStamped()
        self.truth = PoseStamped()
        self.truth.header.frame_id = "simulation_world"
        self.velocity_bias = np.array([0.001, -0.0005, 0.0])
        self.publish_mounts()
        # Consume genuinely captured physics states, once each. A bounded batch
        # absorbs scheduler jitter while leaving time for command responses.
        self.timer = self.create_timer(0.002, self.sample)

    def publish_mounts(self):
        transforms = []
        for name, position in (("imu_link", IMU_POSITION), ("lidar_link", LIDAR_POSITION),
                               ("camera_link", CAMERA_POSITION)):
            tf = TransformStamped()
            tf.header.stamp = self.get_clock().now().to_msg()
            tf.header.frame_id = "base_link"
            tf.child_frame_id = name
            vector(tf.transform.translation, position)
            tf.transform.rotation.w = 1.0
            transforms.append(tf)
        optical = TransformStamped()
        optical.header.stamp = transforms[0].header.stamp
        optical.header.frame_id = "camera_link"
        optical.child_frame_id = "camera_optical_frame"
        # optical X right=-bodyY, optical Y down=-bodyZ, optical Z forward=bodyX.
        quaternion(optical.transform.rotation, [0.5, -0.5, 0.5, -0.5])
        transforms.append(optical)
        self.static_tf.sendTransform(transforms)

    def sample(self):
        bridge = getattr(self.runtime, "ros_bridge", None)
        snapshots = getattr(bridge, "snapshots", None)
        for _ in range(4 if snapshots is not None else 1):
            snapshot = snapshots.take() if snapshots is not None else None
            if snapshots is not None and snapshot is None:
                return
            with self.runtime.observation_lock:
                self._sample(snapshot)

    def _sample(self, snapshot=None):
        if snapshot is None:
            if not self.sampler.capture(self.runtime):
                return
        else:
            with self.runtime.lock:
                if (snapshot.epoch != self.runtime.sim.epoch or
                        snapshot.generation != self.runtime.observation_generation or
                        self.runtime.sim.mode in {"paused", "fault"}):
                    return
            load_snapshot(self.sampler, snapshot)
        state = self.sampler.state()
        identity = (state["epoch"], state["time"])
        if identity == self.last_sample:
            return
        if self.last_sample is None or identity[0] != self.last_sample[0]:
            self.position[:] = state["position"]
            self.yaw_bias = 0.0
            self.last_time = state["time"]
            self.next_imu = self.next_joint = 0.0
            self.epoch_pub.publish(UInt64(data=state["epoch"]))
        self.last_sample = identity
        sec, nanosec = divmod(state["wall_timestamp_ns"], 1_000_000_000)
        stamp = Time(sec=sec, nanosec=nanosec)
        dt = state["time"] - self.last_time
        self.last_time = state["time"]
        rotation = self.sampler.data.xmat[self.runtime.sim.body_id].reshape(3, 3)
        body_velocity = state["linear_velocity_body"] + self.velocity_bias
        self.position += rotation @ body_velocity * dt
        self.yaw_bias += 0.0002 * dt
        if self.native_state:
            self.native_state.publish(self.sampler, state, stamp, self.position,
                                      state["mode"], state["control_mode"])
        if state["time"] + 1e-9 >= self.next_imu:
            self.publish_imu(state, stamp)
            self.next_imu = (math.floor((state["time"] + 1e-9) / 0.005) + 1) * 0.005
        if state["time"] + 1e-9 >= self.next_joint:
            self.publish_motion(state, stamp, body_velocity)
            self.next_joint = (math.floor((state["time"] + 1e-9) / 0.02) + 1) * 0.02
        self.last_published_monotonic = time.monotonic()

    def publish_imu(self, state, stamp):
        imu = self.imu
        imu.header.stamp = stamp
        quaternion(imu.orientation, state["quaternion_wxyz"])
        vector(imu.angular_velocity, state["angular_velocity_body"])
        vector(imu.linear_acceleration, state["specific_force_body"])
        self.imu_pub.publish(imu)
        # This alias represents the same virtual IMU, not a second physical site.
        self.lidar_imu_pub.publish(imu)
        self.samples["imu"] += 1

    def publish_motion(self, state, stamp, body_velocity):
        joint = self.joint
        joint.header.stamp = stamp
        joint.position = state["joint_position"].tolist()
        joint.velocity = state["joint_velocity"].tolist()
        joint.effort = state["joint_effort"].tolist()
        self.joint_pub.publish(joint)
        self.samples["joints"] += 1
        odom = self.odom
        odom.header.stamp = stamp
        vector(odom.pose.pose.position, self.position)
        w, x, y, z = state["quaternion_wxyz"]
        c, s = math.cos(self.yaw_bias / 2), math.sin(self.yaw_bias / 2)
        quaternion(odom.pose.pose.orientation, [c*w-s*z, c*x-s*y, c*y+s*x, c*z+s*w])
        vector(odom.twist.twist.linear, body_velocity)
        vector(odom.twist.twist.angular, state["angular_velocity_body"] + [0, 0, 0.0002])
        self.odom_pub.publish(odom)
        self.samples["odom"] += 1
        tf = self.odom_tf
        tf.header = odom.header
        tf.child_frame_id = odom.child_frame_id
        tf.transform.translation.x = odom.pose.pose.position.x
        tf.transform.translation.y = odom.pose.pose.position.y
        tf.transform.translation.z = odom.pose.pose.position.z
        tf.transform.rotation = odom.pose.pose.orientation
        self.tf.sendTransform(tf)
        truth = self.truth
        truth.header.stamp = stamp
        vector(truth.pose.position, state["position"])
        quaternion(truth.pose.orientation, state["quaternion_wxyz"])
        self.truth_pub.publish(truth)


class ROSBridge:
    def __init__(self, runtime):
        self.runtime = runtime
        self.thread = None
        self.node = None
        self.slow_node = None
        self.slow_thread = None
        self.snapshots = SnapshotQueue(runtime.sim)
        self.stop_event = threading.Event()

    def start(self):
        self.thread = threading.Thread(target=self._run, daemon=True)
        self.thread.start()

    def _run(self):
        executor = None
        try:
            rclpy.init()
            self.node = RobotObservations(self.runtime)
            self.slow_node = SlowObservations(self.runtime, self.node.samples)
            self.slow_thread = threading.Thread(target=self._run_slow, daemon=True)
            self.slow_thread.start()
            executor = SingleThreadedExecutor()
            executor.add_node(self.node)
            while not self.runtime.stop_event.is_set() and not self.stop_event.is_set():
                executor.spin_once(timeout_sec=0.02)
        except Exception as exc:
            with self.runtime.lock:
                self.runtime.errors["ros"] = str(exc)
                self.runtime.sim.pause()
                self.runtime.sim.mode = "fault"
        finally:
            self.stop_event.set()
            if self.slow_thread is not None:
                self.slow_thread.join(timeout=5)
            if executor is not None:
                executor.shutdown()
            if self.node is not None:
                self.node.destroy_node()
            if rclpy.ok():
                rclpy.shutdown()

    def _run_slow(self):
        executor = SingleThreadedExecutor()
        executor.add_node(self.slow_node)
        try:
            while not self.runtime.stop_event.is_set() and not self.stop_event.is_set():
                executor.spin_once(timeout_sec=0.02)
        except Exception as exc:
            with self.runtime.lock:
                self.runtime.errors["ros"] = str(exc)
                self.runtime.sim.pause()
                self.runtime.sim.mode = "fault"
        finally:
            self.stop_event.set()
            executor.shutdown()
            self.slow_node.destroy_node()

    def close(self):
        self.stop_event.set()
        if self.thread is not None:
            self.thread.join(timeout=5)

    def status(self):
        from .runtime import timing_summary
        last_sample = self.node.last_published_monotonic if self.node else None
        return {"running": self.thread is not None and self.thread.is_alive() and self.node is not None,
                "fresh": last_sample is not None and time.monotonic() - last_sample < 1.0,
                "samples": self.node.samples.copy() if self.node else {},
                "snapshot_queue": self.snapshots.status(),
                "image_publish_ms": timing_summary(self.slow_node.image_publish_ms if self.slow_node else ()),
                "native_samples": self.node.native_state.samples.copy()
                if self.node and self.node.native_state else {}}

"""Standard ROS 2 messages. No dependency on a physical serial controller."""

import math
import json
import os
from pathlib import Path
import subprocess
from types import SimpleNamespace

import rclpy
from builtin_interfaces.msg import Time
from geometry_msgs.msg import TransformStamped
from nav_msgs.msg import Odometry
from rclpy.node import Node
from rclpy.qos import qos_profile_sensor_data
from sensor_msgs.msg import Imu, JointState, LaserScan, Image, CameraInfo
from .camera import CAMERA_OFFSET, WIDTH, HEIGHT, FX, FY, CX, CY
from tf2_ros import StaticTransformBroadcaster, TransformBroadcaster

from .simulation import LIDAR_OFFSET, SCAN_COUNT


def stamp(ns):
    return Time(sec=ns // 1_000_000_000, nanosec=ns % 1_000_000_000)


def rotation(q, yaw):
    q.z, q.w = math.sin(yaw / 2), math.cos(yaw / 2)


class Bridge:
    def __init__(self, runtime):
        rclpy.init()
        self.node = Node("rosmaster_r2_simulator")
        self.runtime = runtime
        # Humble rclpy discards MessageInfo, and its raw metadata omits the GID.
        # A small rclcpp receiver supplies the actual middleware identity.
        receiver = Path(__file__).parents[1] / "ros_ws/install/lib/r2_command_ingress/r2_command_ingress"
        self.ingress = subprocess.Popen([str(receiver)], stdin=subprocess.DEVNULL, stdout=subprocess.PIPE)
        os.set_blocking(self.ingress.stdout.fileno(), False)
        self.pending = b""
        self.odom = self.node.create_publisher(Odometry, "/odom", 10)
        self.imu = self.node.create_publisher(Imu, "/imu/data", qos_profile_sensor_data)
        self.joints = self.node.create_publisher(JointState, "/joint_states", 10)
        self.scan = self.node.create_publisher(LaserScan, "/scan", qos_profile_sensor_data)
        self.rgb = self.node.create_publisher(Image, "/camera/color/image_raw", qos_profile_sensor_data)
        self.depth = self.node.create_publisher(Image, "/camera/depth/image_raw", qos_profile_sensor_data)
        self.rgb_info = self.node.create_publisher(CameraInfo, "/camera/color/camera_info", qos_profile_sensor_data)
        self.depth_info = self.node.create_publisher(CameraInfo, "/camera/depth/camera_info", qos_profile_sensor_data)
        self.last_camera_ns = 0
        self.tf = TransformBroadcaster(self.node)
        self.static = StaticTransformBroadcaster(self.node)
        transforms = []
        for child, position in (("laser_frame", LIDAR_OFFSET), ("imu_link", (0., 0., .12))):
            transform = TransformStamped()
            transform.header.stamp = self.node.get_clock().now().to_msg()
            transform.header.frame_id, transform.child_frame_id = "base_link", child
            transform.transform.translation.x, transform.transform.translation.y, transform.transform.translation.z = position
            transform.transform.rotation.w = 1.
            transforms.append(transform)
        camera_tf = TransformStamped()
        camera_tf.header.stamp = self.node.get_clock().now().to_msg()
        camera_tf.header.frame_id, camera_tf.child_frame_id = "base_link", "camera_optical_frame"
        camera_tf.transform.translation.x, camera_tf.transform.translation.y, camera_tf.transform.translation.z = CAMERA_OFFSET
        camera_tf.transform.rotation.x, camera_tf.transform.rotation.y = -.5, .5
        camera_tf.transform.rotation.z, camera_tf.transform.rotation.w = -.5, .5
        transforms.append(camera_tf)
        self.static.sendTransform(transforms)

    def spin_once(self):
        if self.ingress.poll() is not None:
            raise RuntimeError("ROS command receiver exited; restart the simulator")
        try:
            chunk = os.read(self.ingress.stdout.fileno(), 65536)
        except BlockingIOError:
            chunk = b""
        self.pending += chunk
        if len(self.pending) > 65536:
            raise RuntimeError("ROS command receiver exceeded its buffer limit")
        while b"\n" in self.pending:
            line, self.pending = self.pending.split(b"\n", 1)
            command = json.loads(line)
            message = SimpleNamespace(linear=SimpleNamespace(x=command["vx"], y=command["vy"]),
                                      angular=SimpleNamespace(z=command["wz"]))
            info = SimpleNamespace(source_timestamp=command["source_timestamp"],
                                   publisher_gid=bytes.fromhex(command["publisher_gid"]))
            self.runtime.ros_command(message, info)
        rclpy.spin_once(self.node, timeout_sec=0)

    def publish(self, state, capture_ns, scan):
        captured = stamp(capture_ns)
        odom = Odometry()
        odom.header.stamp, odom.header.frame_id, odom.child_frame_id = captured, "odom", "base_link"
        odom.pose.pose.position.x, odom.pose.pose.position.y = state["x"], state["y"]
        rotation(odom.pose.pose.orientation, state["yaw"])
        odom.twist.twist.linear.x, odom.twist.twist.angular.z = state["speed"], state["yaw_rate"]
        # Ideal model observations, small declared covariance for ROS consumers.
        for index in (0, 7, 14, 21, 28, 35):
            odom.pose.covariance[index] = odom.twist.covariance[index] = .0001
        self.odom.publish(odom)
        transform = TransformStamped()
        transform.header, transform.child_frame_id = odom.header, "base_link"
        transform.transform.translation.x, transform.transform.translation.y = state["x"], state["y"]
        transform.transform.rotation = odom.pose.pose.orientation
        self.tf.sendTransform(transform)

        imu = Imu()
        imu.header.stamp, imu.header.frame_id = captured, "imu_link"
        rotation(imu.orientation, state["yaw"])
        imu.angular_velocity.z = state["yaw_rate"]
        imu.linear_acceleration.x = state["acceleration"]
        imu.linear_acceleration.y = state["lateral_acceleration"]
        imu.linear_acceleration.z = 9.80665
        for field in (imu.orientation_covariance, imu.angular_velocity_covariance, imu.linear_acceleration_covariance):
            for index in (0, 4, 8):
                field[index] = .0001
        self.imu.publish(imu)
        joints = JointState()
        joints.header.stamp = captured
        joints.name = ["front_left_wheel_joint", "front_right_wheel_joint", "rear_left_wheel_joint",
                       "rear_right_wheel_joint", "front_left_steering_joint", "front_right_steering_joint"]
        joints.position = state["wheel_positions"] + state["steering_angles"]
        self.joints.publish(joints)
        self.publish_camera()
        if scan is not None:
            message = LaserScan()
            message.header.stamp, message.header.frame_id = stamp(self.runtime.scan_ns), "laser_frame"
            message.angle_min = scan["angle_min"]
            message.angle_increment = scan["angle_increment"]
            message.angle_max = message.angle_min + (SCAN_COUNT - 1) * message.angle_increment
            message.range_min, message.range_max = scan["range_min"], scan["range_max"]
            message.scan_time = .1
            # All rays share one exposure; there is no rolling scan distortion.
            message.time_increment = 0.
            message.ranges = [value if value is not None else float("inf") for value in scan["ranges"]]
            self.scan.publish(message)

    def publish_camera(self):
        frame = self.runtime.camera_frame
        if not frame or frame["capture_ns"] == self.last_camera_ns:
            return
        self.last_camera_ns = frame["capture_ns"]
        captured = stamp(frame["capture_ns"])
        for publisher, key, encoding, step in ((self.rgb,"rgb","rgb8",WIDTH*3),
                                                 (self.depth,"depth_raw","16UC1",WIDTH*2)):
            message = Image()
            message.header.stamp, message.header.frame_id = captured, "camera_optical_frame"
            message.width, message.height, message.encoding, message.step = WIDTH, HEIGHT, encoding, step
            message.is_bigendian = 0
            message.data = frame[key]
            publisher.publish(message)
        info = CameraInfo()
        info.header.stamp, info.header.frame_id = captured, "camera_optical_frame"
        info.width, info.height = WIDTH, HEIGHT
        info.distortion_model, info.d = "plumb_bob", [0.]*5
        info.k = [FX,0.,CX,0.,FY,CY,0.,0.,1.]
        info.r = [1.,0.,0.,0.,1.,0.,0.,0.,1.]
        info.p = [FX,0.,CX,0.,0.,FY,CY,0.,0.,0.,1.,0.]
        self.rgb_info.publish(info)
        self.depth_info.publish(info)

    def close(self):
        self.ingress.terminate()
        try:
            self.ingress.wait(timeout=3)
        except subprocess.TimeoutExpired:
            self.ingress.kill()
            self.ingress.wait()
        self.ingress.stdout.close()
        self.node.destroy_node()
        rclpy.shutdown()

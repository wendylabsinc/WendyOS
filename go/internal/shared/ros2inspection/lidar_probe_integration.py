"""Explicit real ROS integration suite for the embedded LiDAR subscriber.

Run inside a cached ROS image, with no network access to any robot:
  docker run --rm --network none -e ROS_LOCALHOST_ONLY=1 \
    -v "$PWD/internal/shared/ros2inspection:/tests:ro" \
    ros:humble-ros-base python3 -B /tests/lidar_probe_integration.py

The fixture only publishes synthetic observations and transforms. No robot
motion topics, services, or hardware interfaces are used.
"""

import json
import math
import pathlib
import struct
import subprocess
import sys
import threading
import time
import unittest

import rclpy
from geometry_msgs.msg import TransformStamped
from rclpy.executors import SingleThreadedExecutor
from rclpy.qos import QoSProfile, ReliabilityPolicy
from sensor_msgs.msg import LaserScan, PointCloud2, PointField
from tf2_ros import StaticTransformBroadcaster, TransformBroadcaster


PROBE = pathlib.Path(__file__).with_name("lidar_probe.py").read_text()


class RealROSProbeTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init(args=[])
        cls.node = rclpy.create_node("synthetic_lidar_fixture", enable_rosout=False)
        qos = QoSProfile(depth=1, reliability=ReliabilityPolicy.BEST_EFFORT)
        cls.cloud_pub = cls.node.create_publisher(PointCloud2, "/probe_test/cloud", qos)
        cls.scan_pub = cls.node.create_publisher(LaserScan, "/probe_test/scan", qos)
        cls.dynamic_pub = cls.node.create_publisher(PointCloud2, "/probe_test/dynamic_cloud", qos)
        cls.static_tf = StaticTransformBroadcaster(cls.node)
        cls.dynamic_tf = TransformBroadcaster(cls.node)
        static = TransformStamped()
        static.header.stamp = cls.node.get_clock().now().to_msg()
        static.header.frame_id, static.child_frame_id = "probe_base", "probe_lidar"
        static.transform.translation.x = 0.2
        static.transform.translation.z = 0.5
        static.transform.rotation.w = 1.0
        cls.static_tf.sendTransform(static)
        cls.dynamic_stale_until = None
        cls.dynamic_stale_sent = 0
        cls.timer = cls.node.create_timer(0.05, cls.publish)
        cls.executor = SingleThreadedExecutor()
        cls.executor.add_node(cls.node)
        cls.thread = threading.Thread(target=cls.executor.spin, daemon=True)
        cls.thread.start()

    @classmethod
    def tearDownClass(cls):
        cls.executor.shutdown(timeout_sec=2)
        cls.thread.join(timeout=2)
        cls.node.destroy_node()
        rclpy.shutdown()

    @staticmethod
    def cloud(stamp, frame):
        msg = PointCloud2()
        msg.header.stamp, msg.header.frame_id = stamp, frame
        msg.width, msg.height, msg.point_step, msg.row_step = 3, 2, 16, 56
        msg.fields = [PointField(name="x", offset=4, datatype=PointField.FLOAT32, count=1),
                      PointField(name="y", offset=8, datatype=PointField.FLOAT32, count=1),
                      PointField(name="z", offset=12, datatype=PointField.FLOAT32, count=1)]
        points = [(1, 0, 0), (0, 2, 0), (-3, 0, 0), (0, -4, 0), (math.nan, 0, 0), (0, 0, 5)]
        data = bytearray(msg.row_step * msg.height)
        for index, point in enumerate(points):
            offset = (index // msg.width) * msg.row_step + (index % msg.width) * msg.point_step + 4
            struct.pack_into("<fff", data, offset, *point)
        msg.data = data
        return msg

    @classmethod
    def publish(cls):
        stamp = cls.node.get_clock().now().to_msg()
        cls.cloud_pub.publish(cls.cloud(stamp, "probe_lidar"))
        scan = LaserScan()
        scan.header.stamp, scan.header.frame_id = stamp, "probe_lidar"
        scan.angle_min, scan.angle_max, scan.angle_increment = -math.pi, math.pi/2, math.pi/2
        scan.range_min, scan.range_max = 0.1, 10.0
        scan.ranges = [1.0, 2.0, 3.0, math.inf]
        cls.scan_pub.publish(scan)
        dynamic = TransformStamped()
        dynamic.header.stamp = stamp
        dynamic.header.frame_id, dynamic.child_frame_id = "probe_base", "probe_dynamic_lidar"
        dynamic.transform.translation.x = 0.4
        dynamic.transform.rotation.w = 1.0
        cls.dynamic_tf.sendTransform(dynamic)
        msg = cls.cloud(stamp, "probe_dynamic_lidar")
        if cls.dynamic_pub.get_subscription_count() > 0:
            if cls.dynamic_stale_until is None:
                cls.dynamic_stale_until = time.monotonic() + 0.6
            if time.monotonic() < cls.dynamic_stale_until:
                # Intentional cloud older than the new listener's dynamic TF
                # cache. A probe pinned forever to its first cloud will fail.
                msg.header.stamp.sec -= 2
                cls.dynamic_stale_sent += 1
        cls.dynamic_pub.publish(msg)

    def observe(self, **kwargs):
        opts = {"topic": "/probe_test/cloud", "duration_seconds": 6, "count": 1, **kwargs}
        result = subprocess.run([sys.executable, "-B", "-c", PROBE, json.dumps(opts)],
                                capture_output=True, text=True, timeout=opts["duration_seconds"] + 8)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(result.stdout.strip(), result.stderr)
        summaries = [json.loads(line) for line in result.stdout.splitlines()]
        self.assertTrue(all(len(json.dumps(item).encode()) < 65536 for item in summaries))
        return summaries

    def test_best_effort_cloud_is_decoded_without_raw_payload(self):
        result, = self.observe()
        self.assertEqual(result["status"], "observed", result)
        self.assertEqual(result["total_points"], 6)
        self.assertEqual(result["included_points"], 4)
        self.assertEqual(result["sectors"][0]["min_distance_m"], 1)
        self.assertEqual(result["sectors"][2]["min_distance_m"], 2)
        self.assertEqual(result["source_frame"], "probe_lidar")
        self.assertEqual(result["source_freshness"], "unknown")
        self.assertEqual(result["subscription_qos"]["reliability"], "best_effort")
        self.assertNotIn("data", result)

    def test_laserscan_best_effort_and_missing_return(self):
        result, = self.observe(topic="/probe_test/scan", message_type="sensor_msgs/msg/LaserScan")
        self.assertEqual(result["status"], "observed", result)
        self.assertEqual(result["included_points"], 3)
        self.assertEqual(result["rejected_points"]["nonfinite"], 1)
        self.assertAlmostEqual(result["sectors"][0]["min_distance_m"], 3)
        self.assertIsNone(result["sectors"][2]["min_distance_m"])

    def test_static_tf_at_acquisition_stamp(self):
        result, = self.observe(target_frame="probe_base")
        self.assertEqual(result["status"], "observed", result)
        self.assertEqual(result["source_frame"], "probe_lidar")
        self.assertEqual(result["frame_id"], "probe_base")
        self.assertTrue(result["transform_applied"])
        self.assertAlmostEqual(result["sectors"][0]["min_distance_m"], 1.2)
        self.assertEqual(result["sectors"][0]["nearest_xyz_m"], [1.2, 0.0, 0.5])

    def test_dynamic_tf_retries_cloud_older_than_listener_history(self):
        result, = self.observe(topic="/probe_test/dynamic_cloud", target_frame="probe_base")
        self.assertGreater(self.dynamic_stale_sent, 0)
        self.assertEqual(result["status"], "observed", result)
        self.assertTrue(result["transform_applied"])
        self.assertEqual(result["source_frame"], "probe_dynamic_lidar")
        self.assertAlmostEqual(result["sectors"][0]["min_distance_m"], 1.4)
        self.assertLess(abs(result["source_age_seconds"]), 1)

    def test_missing_tf_returns_unknown_without_sensor_frame_fallback(self):
        result, = self.observe(target_frame="absent_target", duration_seconds=2)
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(result["error"]["code"], "transform_unavailable")
        self.assertNotIn("sectors", result)

    def test_no_messages_returns_unknown(self):
        result, = self.observe(topic="/probe_test/absent", duration_seconds=2)
        self.assertEqual(result["status"], "unknown")
        self.assertEqual(result["error"]["code"], "no_messages")


if __name__ == "__main__":
    unittest.main(verbosity=2)

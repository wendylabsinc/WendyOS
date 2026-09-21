"""Focused checks using generated ROS messages and the original SDK CRC."""

from array import array
import math
from pathlib import Path
import struct
import unittest

from sensor_msgs.msg import CameraInfo, Image, LaserScan, PointCloud2, PointField
from unitree_go.msg import LowState

from exercise import soak_interval
from full_sensors import FullSensorChecks, sdk_crc_oracle


class FullSensorTests(unittest.TestCase):
    def setUp(self):
        path = Path("/app/pinned_sdk_crc.py")
        if not path.exists():
            path = Path(__file__).parent / "vendor/pinned_sdk_crc.py"
        self.oracle = sdk_crc_oracle(path)
        self.checks = FullSensorChecks(self.oracle)

    def test_native_crc_uses_sdk_layout_and_detects_a_corrupted_packet(self):
        low = LowState()
        low.head, low.level_flag = [0xFE, 0xEF], 0xFF
        low.tick = 42
        low.motor_state[7].q, low.motor_state[7].dq = 0.5, -0.25
        low.imu_state.quaternion = [1.0, 0.0, 0.0, 0.0]
        low.crc = self.oracle(low)
        self.checks.observe("lowstate", low, 42_000_000)
        self.assertEqual(self.checks.crc_checked, 1)
        self.assertEqual(self.checks.errors, [])
        low.motor_state[7].q = 0.6
        for _ in range(10):
            self.checks.observe("lowstate", low, 42_000_000)
        self.assertEqual(self.checks.crc_checked, 1)
        self.assertIn("CRC", self.checks.errors[0])

    def test_full_payloads_and_matching_scan_cloud_are_checked(self):
        image = Image()
        image.header.frame_id = "camera_optical_frame"
        image.width, image.height, image.encoding, image.step = 640, 360, "rgb8", 1920
        image.data = array("B", bytes(640 * 360 * 3))
        info = CameraInfo()
        info.header.frame_id = image.header.frame_id
        info.width, info.height = image.width, image.height
        info.k = [300., 0., 319.5, 0., 300., 179.5, 0., 0., 1.]
        self.checks.observe("image", image, 1000)
        self.checks.observe("camera_info", info, 1000)
        scan = LaserScan()
        scan.angle_min, scan.angle_increment = 0., math.pi / 2
        scan.ranges = [1., math.inf, 2.]
        cloud = PointCloud2()
        cloud.header.frame_id = "utlidar_lidar"
        cloud.height, cloud.width, cloud.point_step, cloud.row_step = 1, 3, 12, 36
        cloud.fields = [PointField(name=name, offset=index*4, datatype=7, count=1)
                        for index, name in enumerate(("x", "y", "z"))]
        cloud.data = array("B", struct.pack("<9f", 1., 0., 0., -2., 0., 0., 1., 0., 0.5))
        self.checks.observe("scan", scan, 2000)
        self.checks.observe("cloud", cloud, 2000)
        self.assertEqual(self.checks.errors, [])
        self.assertEqual(self.checks.coherent_pairs()["horizontal_returns"], 2)
        cloud.data = array("B", struct.pack("<9f", 4., 0., 0., -2., 0., 0., 1., 0., 0.5))
        self.checks.observe("cloud", cloud, 2000)
        with self.assertRaisesRegex(ValueError, "disagrees"):
            self.checks.coherent_pairs()

    def test_incomplete_image_data_is_a_validation_failure(self):
        image = Image()
        image.header.frame_id = "camera_optical_frame"
        image.width, image.height, image.encoding, image.step = 640, 360, "rgb8", 1920
        image.data = array("B", b"short")
        self.checks.observe("image", image, 1000)
        self.assertIn("incomplete", self.checks.errors[0])

    def test_soak_rate_mapping_keeps_native_and_camera_counters_distinct(self):
        def record(at):
            return {"received_at": at, "time": at,
                    "metrics": {"wall_seconds": at, "policy_updates": at*50,
                                "camera_frames": at*15, "scene_states": 0, "physics_steps": at*500},
                    "probe": {"received": {"imu": at*200, "truth": at*50, "image": at*15,
                                            "camera_info": at*15, "cloud": at*10, "lowstate": at*500},
                              "command_count": at*20, "command_gaps": 0},
                    "published": {"imu": at*200, "odom": at*50, "camera": at*15, "cloud": at*10},
                    "native_published": {"lowstate": at*500}}
        measured = soak_interval(record(2), record(12))
        self.assertEqual(measured["received_hz"], measured["published_hz"])
        self.assertEqual(measured["received_hz"]["lowstate"], 500)
        self.assertEqual(measured["received_hz"]["image"], 15)
        self.assertEqual(measured["camera_fps"], 15)
        self.assertEqual(measured["command_hz"], 20)


if __name__ == "__main__":
    unittest.main()

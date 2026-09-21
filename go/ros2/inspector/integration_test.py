"""Synthetic native DDS checks. Run with --network none, never on a robot bus."""

import base64
import json
import os
import subprocess
import unittest

from rclpy.serialization import deserialize_message, serialize_message
from rosidl_runtime_py.utilities import get_message


AUDIO = ["ros2", "run", "wendy_ros2_inspection", "audio"]


class NativeInspector(unittest.TestCase):
    def test_native_type_support_loads_and_round_trips(self):
        # Each class loads compiled middleware typesupport, not only Python stubs.
        for name in ("unitree_go/msg/AudioData", "unitree_go/msg/LidarState",
                     "unitree_go/msg/HeightMap", "unitree_go/msg/LowState",
                     "unitree_go/msg/SportModeState", "unitree_go/msg/WirelessController",
                     "unitree_go/msg/UwbState", "unitree_api/msg/Request", "unitree_api/msg/Response"):
            with self.subTest(message_type=name):
                cls = get_message(name)
                message = cls()
                self.assertEqual(deserialize_message(serialize_message(message), cls), message)

    def test_audio_packet_between_cyclone_and_fastdds(self):
        payload = bytes(range(256))
        subscriber = subprocess.Popen(AUDIO + ["sample", "/wendy_test/audio", "--count", "1",
            "--duration", "20", "--include-data"], stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
            env={**os.environ, "RMW_IMPLEMENTATION": "rmw_fastrtps_cpp"})
        try:
            published = subprocess.run(AUDIO + ["publish", "/wendy_test/audio", "--time-frame", str(2**64-1),
                "--data-base64", base64.b64encode(payload).decode(), "--duration", "15"],
                env={**os.environ, "RMW_IMPLEMENTATION": "rmw_cyclonedds_cpp"},
                capture_output=True, text=True, timeout=25)
            self.assertEqual(published.returncode, 0, published.stderr)
            stdout, stderr = subscriber.communicate(timeout=25)
            self.assertEqual(subscriber.returncode, 0, stderr)
            packet = json.loads(stdout)
            self.assertEqual(packet["time_frame"], 2**64-1)
            self.assertEqual(base64.b64decode(packet["data_base64"]), payload)
        finally:
            if subscriber.poll() is None:
                subscriber.kill()
            subscriber.communicate()

    def test_missing_audio_is_a_failure(self):
        result = subprocess.run(AUDIO + ["sample", "/wendy_test/absent", "--duration", "1"],
            capture_output=True, text=True, timeout=10)
        self.assertEqual(result.returncode, 2, result.stderr)
        self.assertNotIn('"schema_version"', result.stdout)
        self.assertIn("audio sample timed out after 0/3 packets", result.stderr)


if __name__ == "__main__":
    unittest.main()

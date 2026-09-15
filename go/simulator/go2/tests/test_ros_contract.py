"""ROS lifecycle regressions using actual MuJoCo state and Humble publishers.

The observation tests run inside the ROS runtime image. They skip on development
hosts without rclpy; command admission remains testable without ROS installed.
"""

import http.client
from http.server import ThreadingHTTPServer
import json
import threading
import time
from types import SimpleNamespace
import unittest

from go2_sim.commands import ROSCommands
from go2_sim.runtime import Runtime
from go2_sim.server import Handler
from go2_sim.simulation import DEFAULT_ASSETS, Simulation

try:
    import rclpy
    from sensor_msgs.msg import Imu
except ImportError:
    rclpy = None


def wait_until(predicate, timeout=3.0):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return
        time.sleep(0.01)
    raise AssertionError("timed out waiting for the live ROS test runtime")


class CommandOrderingTests(unittest.TestCase):
    def test_duplicate_and_reordered_samples_cannot_replace_newer_target(self):
        clock = SimpleNamespace(now=1_000_000_000_000)
        sim = Simulation(monotonic=lambda: clock.now / 1e9)
        runtime = SimpleNamespace(
            sim=sim, ensure_running=lambda: None,
            command=lambda values, token: sim.command_velocity(*values, token),
        )
        commands = ROSCommands(runtime, "/unused", monotonic_ns=lambda: clock.now,
                               wall_ns=lambda: clock.now)

        def envelope(velocity):
            return {"kind": "twist", "publisher_gid": "a" * 48,
                    "received_ns": clock.now, "source_timestamp_ns": clock.now,
                    "velocity": velocity}

        self.assertFalse(commands.admit(envelope([0.0, 0.0, 0.0])))
        commands.grant("a" * 48)
        clock.now += 50_000_000
        older = envelope([0.3, 0.0, 0.0])
        clock.now += 50_000_000
        newer = envelope([-0.3, 0.0, 0.0])
        self.assertTrue(commands.admit(newer))
        for replay in (newer, older, {**older, "received_ns": clock.now + 1}):
            clock.now += 1
            self.assertFalse(commands.admit(replay))
            self.assertEqual(sim.command.tolist(), [-0.3, 0.0, 0.0])
            self.assertEqual(sim._last_received, newer["received_ns"] / 1e9)
            self.assertEqual(commands.sources["a" * 48]["last_received_ns"],
                             newer["received_ns"])
        self.assertEqual(commands.accepted, 1)


@unittest.skipIf(rclpy is None, "Humble rclpy is available in the ROS runtime image")
class ObservationLifecycleTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        rclpy.init()

    @classmethod
    def tearDownClass(cls):
        rclpy.shutdown()

    def setUp(self):
        from go2_sim.ros import RobotObservations
        from go2_sim.sensors import instrumented_model

        self.runtime = Runtime(render=False, simulation=Simulation(
            model=instrumented_model(DEFAULT_ASSETS)))
        self.runtime.start()
        self.node = RobotObservations(self.runtime)
        # This test controls the real sample call precisely, without a second
        # timer callback capturing another world while inspecting publication.
        self.node.destroy_timer(self.node.timer)
        from go2_sim.slow_sensors import SlowObservations
        self.slow_node = SlowObservations(self.runtime, self.node.samples)
        self.slow_node.destroy_timer(self.slow_node.timer)
        self.messages = []
        self.subscription = self.node.create_subscription(
            Imu, "/imu/data", self.messages.append, self.node.qos)
        self.request_entered = threading.Event()
        entered = self.request_entered

        class ObservedHandler(Handler):
            def do_POST(self):
                entered.set()
                super().do_POST()

        self.server = ThreadingHTTPServer(("127.0.0.1", 0), ObservedHandler)
        self.server.runtime = self.runtime
        self.serving = threading.Thread(target=self.server.serve_forever,
                                        kwargs={"poll_interval": 0.01}, daemon=True)
        self.serving.start()
        wait_until(lambda: self.runtime.sim.data.time > 0.02)
        wait_until(lambda: self.node.count_subscribers("/imu/data") > 0)

    def tearDown(self):
        self.server.shutdown()
        self.serving.join(timeout=2.0)
        self.server.server_close()
        self.node.destroy_node()
        self.slow_node.destroy_node()
        self.runtime.close()

    def request(self, path, body=None):
        connection = http.client.HTTPConnection("127.0.0.1", self.server.server_port,
                                                timeout=3.0)
        try:
            connection.request("POST", path, body=json.dumps({} if body is None else body),
                               headers={"Content-Type": "application/json"})
            response = connection.getresponse()
            return response.status, json.loads(response.read())
        finally:
            connection.close()

    def fenced_control(self, path):
        """Suspend real sensor conversion after capture, then issue real HTTP."""
        captured = threading.Event()
        release = threading.Event()
        completed = threading.Event()
        errors, response, sample_state = [], [], []
        original_state = self.node.sampler.state

        def delayed_state():
            state = original_state()
            sample_state.append(state)
            captured.set()
            if not release.wait(3.0):
                raise AssertionError("test did not release captured observation")
            return state

        def publish():
            try:
                self.node.sample()
            except Exception as exc:
                errors.append(exc)

        def control():
            try:
                response.append(self.request(path))
            except Exception as exc:
                errors.append(exc)
            finally:
                completed.set()

        self.node.sampler.state = delayed_state
        publisher = threading.Thread(target=publish, daemon=True)
        controller = threading.Thread(target=control, daemon=True)
        publisher.start()
        try:
            self.assertTrue(captured.wait(3.0))
            physics_time = self.runtime.sim.data.time
            controller.start()
            self.assertTrue(self.request_entered.wait(3.0))
            self.assertFalse(completed.wait(0.12),
                             "control returned while an old observation could still publish")
            self.assertGreater(self.runtime.sim.data.time, physics_time,
                               "sensor publication must not hold the physics lock")
        finally:
            release.set()
            publisher.join(timeout=3.0)
            if controller.ident is not None:
                controller.join(timeout=3.0)
            self.node.sampler.state = original_state
        self.assertFalse(publisher.is_alive())
        self.assertFalse(controller.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(response[0][0], 200)
        self.assertEqual(self.node.samples["imu"], 1)

        # Receive the actual DDS message. Its timestamp must describe capture,
        # not the delayed conversion/publication at least 120 ms afterwards.
        deadline = time.monotonic() + 3.0
        while not self.messages and time.monotonic() < deadline:
            rclpy.spin_once(self.node, timeout_sec=0.05)
        self.assertTrue(self.messages)
        stamp = self.messages[0].header.stamp
        self.assertEqual(stamp.sec * 1_000_000_000 + stamp.nanosec,
                         sample_state[0]["wall_timestamp_ns"])
        self.assertGreater(time.time_ns() - sample_state[0]["wall_timestamp_ns"],
                           100_000_000)
        return sample_state[0], response[0][1]

    def test_pause_waits_for_inflight_publication_then_emits_no_fresh_sample(self):
        self.fenced_control("/api/pause")
        samples = self.node.samples.copy()
        self.assertEqual(self.runtime.sim.mode, "paused")
        self.node.sample()
        self.assertEqual(self.node.samples, samples)

    def test_reset_waits_for_inflight_publication_then_next_sample_has_new_epoch(self):
        old, response = self.fenced_control("/api/reset")
        self.assertNotEqual(response["epoch"], old["epoch"])
        self.node.sample()
        self.assertEqual(self.node.last_sample[0], response["epoch"])
        self.assertEqual(self.node.samples["imu"], 2)

    def test_captured_fast_queue_state_cannot_cross_reset_or_pause_generation(self):
        from go2_sim.observations import SnapshotQueue
        queue = SnapshotQueue(self.runtime.sim)
        for path in ("/api/reset", "/api/pause"):
            with self.runtime.lock:
                queue.capture(self.runtime)
            old = queue.take()
            self.assertIsNotNone(old)
            self.assertEqual(self.request(path)[0], 200)
            before = self.node.samples.copy()
            with self.runtime.observation_lock:
                self.node._sample(old)
            self.assertEqual(self.node.samples, before)

    def test_ray_result_computed_during_reset_is_discarded_without_blocking_physics(self):
        captured, release = threading.Event(), threading.Event()
        original = self.slow_node.lidar.sample
        errors = []

        def delayed(sampler):
            result = original(sampler)
            captured.set()
            if not release.wait(3.0):
                raise AssertionError("test did not release lidar result")
            return result

        def sample():
            try:
                self.slow_node.sample()
            except Exception as exc:
                errors.append(exc)

        self.slow_node.lidar.sample = delayed
        worker = threading.Thread(target=sample, daemon=True)
        worker.start()
        try:
            self.assertTrue(captured.wait(3.0))
            self.assertEqual(self.request("/api/reset")[0], 200)
            release.set()
            worker.join(timeout=3.0)
        finally:
            release.set()
            worker.join(timeout=3.0)
            self.slow_node.lidar.sample = original
        self.assertFalse(worker.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(self.node.samples["scan"], 0)
        self.assertEqual(self.node.samples["cloud"], 0)
        self.slow_node.sample()
        self.assertEqual(self.node.samples["scan"], 1)

    def test_camera_frame_keeps_exposure_stamp_is_published_once_and_cannot_cross_pause(self):
        import numpy as np
        from go2_sim.camera import CAMERA_HEIGHT, CAMERA_WIDTH, CameraFrame
        frame = CameraFrame(self.runtime.sim.epoch, self.runtime.observation_generation,
                            0.123, 123_456_789_012,
                            np.zeros((CAMERA_HEIGHT, CAMERA_WIDTH, 3), dtype=np.uint8))
        self.runtime.camera_frame = frame
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)
        stamp = self.slow_node.image.header.stamp
        self.assertEqual(stamp.sec * 1_000_000_000 + stamp.nanosec, frame.wall_timestamp_ns)
        self.assertEqual(self.slow_node.info.header.stamp, stamp)
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)
        self.assertEqual(self.request("/api/pause")[0], 200)
        # Even an accidentally retained old frame cannot survive the fence.
        self.runtime.camera_frame = frame
        self.slow_node.last_camera = None
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)

    def test_runtime_lidar_fault_stops_real_ros_samples_while_physics_and_imu_continue(self):
        from sensor_msgs.msg import LaserScan, PointCloud2
        scans, clouds = [], []
        self.slow_node.create_subscription(LaserScan, "/scan", scans.append, self.node.qos)
        self.slow_node.create_subscription(PointCloud2, "/utlidar/cloud", clouds.append, self.node.qos)

        def pump(seconds):
            deadline = time.monotonic() + seconds
            while time.monotonic() < deadline:
                self.slow_node.sample()
                self.node.sample()
                rclpy.spin_once(self.slow_node, timeout_sec=0.001)
                rclpy.spin_once(self.node, timeout_sec=0.001)
                time.sleep(0.005)

        pump(0.4)
        self.assertGreaterEqual(len(scans), 2)
        self.assertGreaterEqual(len(clouds), 2)
        self.assertEqual(self.request("/api/sensors", {"lidar_enabled": False})[0], 200)
        pump(0.1)  # Drain messages already emitted before the acknowledged change.
        counts = (len(scans), len(clouds), len(self.messages))
        sim_time = self.runtime.sim.data.time
        source_stamp = scans[-1].header.stamp
        pump(0.4)
        self.assertEqual((len(scans), len(clouds)), counts[:2])
        self.assertEqual(scans[-1].header.stamp, source_stamp)
        self.assertGreater(self.runtime.sim.data.time - sim_time, 0.3)
        self.assertGreater(len(self.messages), counts[2] + 5)
        self.assertEqual(self.request("/api/sensors", {"lidar_enabled": True})[0], 200)
        pump(0.3)
        self.assertGreater(len(scans), counts[0])
        self.assertGreater(len(clouds), counts[1])
        self.assertNotEqual(scans[-1].header.stamp, source_stamp)

    def test_configured_dropout_shares_ros_scan_cloud_mask_and_reseeds_on_reset(self):
        import numpy as np
        self.assertEqual(self.request("/api/sensors", {"lidar_dropout": 0.5})[0], 200)
        self.slow_node.sample()
        scan = self.slow_node.scan
        cloud = self.slow_node.cloud
        self.assertEqual(scan.header.stamp, cloud.header.stamp)
        points = np.frombuffer(cloud.data, dtype="<f4").reshape(-1, 3)
        horizontal = points[np.abs(points[:, 2]) < 1e-6]
        finite = np.flatnonzero(np.isfinite(scan.ranges))
        self.assertGreater(len(finite), 0)
        self.assertLess(len(finite), 360)
        self.assertEqual(len(horizontal), len(finite))
        np.testing.assert_allclose(np.linalg.norm(horizontal, axis=1), np.asarray(scan.ranges)[finite], rtol=1e-6)
        first_mask = np.isfinite(scan.ranges)
        self.assertEqual(self.request("/api/reset")[0], 200)
        self.slow_node.sample()
        np.testing.assert_array_equal(np.isfinite(self.slow_node.scan.ranges), first_mask)
        self.assertEqual(self.request("/api/sensors", {"lidar_dropout": 1.0})[0], 200)
        self.slow_node.sample()
        self.assertTrue(np.isinf(self.slow_node.scan.ranges).all())
        self.assertEqual(self.slow_node.cloud.width, 0)
        self.assertEqual(len(self.slow_node.cloud.data), 0)

    def test_sensor_change_discards_an_inflight_old_dropout_result(self):
        captured, release = threading.Event(), threading.Event()
        original = self.slow_node.lidar.sample
        errors = []
        def delayed(sampler):
            result = original(sampler)
            captured.set()
            if not release.wait(3):
                raise AssertionError("test did not release the old lidar exposure")
            return result
        def sample():
            try:
                self.slow_node.sample()
            except Exception as error:
                errors.append(error)
        self.slow_node.lidar.sample = delayed
        worker = threading.Thread(target=sample, daemon=True)
        worker.start()
        try:
            self.assertTrue(captured.wait(3))
            self.assertEqual(self.request("/api/sensors", {"lidar_dropout": 1.0})[0], 200)
        finally:
            release.set()
            worker.join(timeout=3)
        self.assertFalse(worker.is_alive())
        self.assertEqual(errors, [])
        self.assertEqual(self.node.samples["scan"], 0)
        self.assertEqual(self.node.samples["cloud"], 0)
        self.slow_node.sample()
        self.assertEqual(self.node.samples["scan"], 1)
        self.assertEqual(self.slow_node.cloud.width, 0)

    def test_camera_sensor_fault_drops_retained_exposures_without_pausing_world(self):
        import numpy as np
        from go2_sim.camera import CAMERA_HEIGHT, CAMERA_WIDTH, CameraFrame
        def frame():
            return CameraFrame(self.runtime.sim.epoch, self.runtime.observation_generation,
                               float(self.runtime.sim.data.time), time.time_ns(),
                               np.zeros((CAMERA_HEIGHT, CAMERA_WIDTH, 3), dtype=np.uint8))
        old = frame()
        self.runtime.camera_frame = old
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)
        self.assertEqual(self.request("/api/sensors", {"camera_enabled": False})[0], 200)
        self.runtime.camera_frame = frame()  # Even an accidentally populated current exposure is gated.
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)
        self.assertNotEqual(self.runtime.sim.mode, "paused")
        self.assertEqual(self.request("/api/sensors", {"camera_enabled": True})[0], 200)
        self.runtime.camera_frame = old
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 1)
        self.runtime.camera_frame = frame()
        self.slow_node.publish_camera()
        self.assertEqual(self.node.samples["camera"], 2)


if __name__ == "__main__":
    unittest.main()

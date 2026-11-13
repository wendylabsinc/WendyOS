import time
import unittest
from types import SimpleNamespace

from r2_sim.runtime import Runtime


class RuntimeTests(unittest.TestCase):
    def setUp(self):
        self.runtime = Runtime()

    def test_watchdog_stops_disconnected_controller(self):
        r = self.runtime
        token = r.arm('browser')['token']
        r.command(dict(token=token, sequence=1, speed=1., steering=.1))
        r.tick(.1, r.deadline - .1)
        self.assertGreater(r.sim.speed, 0)
        r.tick(.01, r.deadline + .001)
        self.assertEqual(r.sim.speed, 0)
        self.assertEqual(r.sim.target_speed, 0)

    def test_reset_revokes_tokens_and_stale_commands(self):
        r = self.runtime
        token = r.arm('browser')['token']
        command = dict(token=token, sequence=1, speed=1., steering=0.)
        r.command(command)
        with self.assertRaises(ValueError):
            r.command(command)
        r.reset()
        self.assertEqual(r.epoch, 2)
        with self.assertRaises(ValueError):
            r.command({**command, 'sequence': 2})
        self.assertEqual(r.sim.x, 0)

    def test_paused_runtime_cannot_arm(self):
        r = self.runtime
        r.paused = True
        with self.assertRaises(ValueError):
            r.arm('browser')

    def test_ros_rejects_old_stamps_and_other_publishers(self):
        r = self.runtime
        message = SimpleNamespace(linear=SimpleNamespace(x=.5, y=0.), angular=SimpleNamespace(z=.1))
        info = SimpleNamespace(source_timestamp=r.armed_ns - 1, publisher_gid=[1]*16)
        r.ros_command(message, info)
        self.assertIsNone(r.owner)
        info.source_timestamp = time.time_ns()
        r.ros_command(message, info)
        self.assertEqual(r.owner, '01'*16)
        r.ros_command(SimpleNamespace(linear=SimpleNamespace(x=-.5, y=0.), angular=SimpleNamespace(z=0.)),
                      SimpleNamespace(source_timestamp=time.time_ns(), publisher_gid=[2]*16))
        self.assertEqual(r.sim.target_speed, .5)
        r.reset()
        info.source_timestamp = time.time_ns()
        r.ros_command(message, info)
        self.assertIsNone(r.owner)

    def test_browser_and_ros_cannot_drive_together(self):
        r = self.runtime
        r.arm('browser')
        message = SimpleNamespace(linear=SimpleNamespace(x=.5, y=0.), angular=SimpleNamespace(z=0.))
        r.ros_command(message, SimpleNamespace(source_timestamp=time.time_ns(), publisher_gid=[1]*16))
        self.assertEqual(r.sim.target_speed, 0)

    def test_runtime_reports_liveness_and_stops_cleanly(self):
        r = self.runtime
        self.assertFalse(r.status()['ready'])
        r.start()
        try:
            deadline = time.monotonic() + 3
            while not r.status()['ready'] and time.monotonic() < deadline:
                time.sleep(.01)
            self.assertTrue(r.status()['ready'])
        finally:
            r.close()
        self.assertFalse(r.status()['ready'])


if __name__ == '__main__':
    unittest.main()

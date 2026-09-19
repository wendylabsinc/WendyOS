"""Offline fault injection through the real supervisor, guard, and motor gate.

Only the final physical backend is a recorder. No ROS imports, sockets, SDKs,
robot connections, or physical commands are used by these tests.
"""

import math
from pathlib import Path
import tempfile
import unittest

from robot_navigation.guard import GuardConfig, VelocityGuard
from robot_navigation.motor import MotorConfig, MotorGate
from robot_navigation.runtime import NavigationRuntime, RuntimeConfig


class Clock:
    source = 100.0
    monotonic = 0.0

    def advance(self, seconds, source=True):
        self.monotonic += seconds
        if source:
            self.source += seconds


class RecordedMotor:
    def __init__(self):
        self.calls = []

    def move(self, linear, angular):
        self.calls.append(("move", linear, angular))

    def stop(self):
        self.calls.append(("stop",))


class NavigationBackend:
    def __init__(self, guard):
        self.guard = guard
        self.enabled = False
        self.goal_id = None
        self.speed = 0.1
        self.cancelled = []

    def send_goal(self, goal_id, pose, max_speed):
        self.goal_id, self.speed = goal_id, max_speed

    def cancel_goal(self, goal_id):
        self.cancelled.append(goal_id)

    def set_enabled(self, enabled):
        self.enabled = enabled
        if not enabled:
            self.guard.update_permit(False)

    def stop(self):
        self.enabled = False
        self.guard.update_permit(False)


class SafetyChainTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.db_path = str(Path(self.directory.name) / "goals.sqlite")
        self.clock = Clock()
        self.guard = VelocityGuard(GuardConfig(commissioned=True, reaction_seconds=0.75),
                                   source_clock=lambda: self.clock.source, clock=lambda: self.clock.monotonic)
        self.hardware = RecordedMotor()
        self.motor = MotorGate(MotorConfig(mode="twist", motion_enabled=True, watchdog_commissioned=True),
                               self.hardware, monotonic=lambda: self.clock.monotonic)
        self.backend = NavigationBackend(self.guard)
        self.runtime = self.new_runtime()
        self.addCleanup(lambda: self.runtime.close())
        self.addCleanup(lambda: self.motor.stop())
        self.step()

    def new_runtime(self):
        return NavigationRuntime(RuntimeConfig(motion_enabled=True, watchdog_commissioned=True,
                                               max_linear_speed=0.25, max_angular_speed=0.4),
                                 self.db_path, self.backend, source_clock=lambda: self.clock.source,
                                 monotonic_clock=lambda: self.clock.monotonic)

    def step(self, *, supervisor=True, sensors=True, guard=True, measured_speed=0, seconds=0.05):
        self.clock.advance(seconds)
        now = self.clock.source
        if sensors:
            self.guard.update_scan(stamp=now, frame_id="base_link", angle_min=-math.pi,
                                   angle_increment=math.pi/180, ranges=[4.0]*360, range_min=0.1, range_max=10)
            self.guard.update_odometry(stamp=now, frame_id="base_link", linear_speed=measured_speed, angular_speed=0)
        if supervisor:
            self.runtime.update_sensor("scan", self.guard.scan[0] if self.guard.scan else None,
                                       "base_link", healthy=self.guard.sensor_problem() is None)
            self.runtime.update_sensor("transforms", now, "map")
            self.runtime.update_navigation_pose(now, "map", 0, 0, 0)
            self.runtime.update_odometry(now, "odom", "base_link", 0, 0, 0, measured_speed, 0, 0.01, 0.01, "standing")
            state = self.guard.evaluate()
            self.runtime.update_guard(state["ready"], state["stop_latched"])
            self.runtime.update_navigation_available(True)
            self.runtime.tick()
            self.guard.update_permit(self.backend.enabled, self.backend.speed)
        # Nav2 may keep publishing after cancellation while its result is pending.
        if self.backend.goal_id:
            self.guard.update_command(self.backend.speed, 0, 0)
        if guard:
            state = self.guard.evaluate()
            self.motor.receive(state["linear_x"], state["angular_z"], now, now, "base_link")
        self.motor.tick()

    def start(self, lease_seconds=10):
        goal = self.runtime.navigate("request-1", {"x": 1, "y": 0, "yaw": 0, "frame_id": "map"}, 0.1,
                                     lease_seconds=lease_seconds)
        self.assertFalse(self.backend.enabled)
        self.runtime.backend_accepted(goal["goal_id"])
        self.step()
        self.assertEqual(("move", 0.1, 0.0), self.hardware.calls[-1])
        return goal

    def test_expired_supervision_lease_cancels_and_inhibits_real_command_chain(self):
        goal = self.start(lease_seconds=0.3)
        for _ in range(7):
            self.step()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("lease_expired", state["reason"])
        self.assertEqual("stopping", state["state"])
        self.assertIn(goal["goal_id"], self.backend.cancelled)
        self.assertFalse(self.backend.enabled)
        self.assertEqual(("stop",), self.hardware.calls[-1])
        self.assertFalse(state["stopped_confirmed"])
        # Even a stationary robot cannot start a new goal before Nav2 settles.
        for _ in range(12):
            self.step()
        self.assertEqual("stopping", self.runtime.status(goal["goal_id"])["state"])
        self.runtime.backend_result(goal["goal_id"], "cancelled")
        for _ in range(12):
            self.step()
        self.assertEqual("failed", self.runtime.status(goal["goal_id"])["state"])

    def test_supervisor_freeze_expires_guard_permit_despite_continued_nav_commands(self):
        self.start()
        for _ in range(7):
            self.step(supervisor=False, sensors=True)
        self.assertEqual("permit_expired", self.guard.evaluate()["reason"])
        self.assertEqual(("stop",), self.hardware.calls[-1])
        before = sum(call[0] == "move" for call in self.hardware.calls)
        for _ in range(6):
            self.step(supervisor=False, sensors=True)
        self.assertEqual(before, sum(call[0] == "move" for call in self.hardware.calls))

    def test_guard_and_ros_clock_freeze_are_caught_by_motor_monotonic_watchdog(self):
        self.start()
        self.clock.advance(0.21, source=False)
        self.motor.tick()
        self.assertEqual(("stop",), self.hardware.calls[-1])
        self.assertEqual("command_watchdog_expired", self.motor.status()["reason"])
        self.assertFalse(self.motor.status()["stopped_confirmed"])

    def test_sensor_loss_stops_chain_even_when_supervisor_and_commands_continue(self):
        self.start()
        for _ in range(7):
            self.step(sensors=False)
        self.assertFalse(self.guard.evaluate()["ready"])
        self.assertEqual(("stop",), self.hardware.calls[-1])
        self.assertFalse(self.backend.enabled)

    def test_restart_abandons_durable_goal_and_cannot_replay_motion(self):
        goal = self.start()
        self.runtime.close()
        self.runtime = self.new_runtime()
        # Repeated Nav2 output and an idempotent request retry cannot rearm.
        state = self.runtime.request_status("request-1")
        self.assertEqual("abandoned", state["state"])
        self.assertFalse(self.runtime.readiness()["ready"])
        retry = self.runtime.navigate("request-1", {"x": 1, "y": 0, "yaw": 0, "frame_id": "map"}, 0.1)
        self.assertEqual(goal["goal_id"], retry["goal_id"])
        self.assertEqual("abandoned", retry["state"])
        self.runtime.backend_accepted(goal["goal_id"])
        self.step()
        self.assertFalse(self.backend.enabled)
        self.assertEqual(("stop",), self.hardware.calls[-1])


if __name__ == "__main__":
    unittest.main()

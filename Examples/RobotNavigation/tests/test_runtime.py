import dataclasses
import json
import math
from pathlib import Path
import sqlite3
import sys
import tempfile
import threading
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from robot_navigation.runtime import NavigationRuntime, RuntimeConfig, SensorRequirement


class Clock:
    def __init__(self):
        self.source = 100.0
        self.mono = 0.0

    def advance(self, seconds):
        self.source += seconds
        self.mono += seconds


class Backend:
    def __init__(self):
        self.calls = []
        self.on_send = None

    def send_goal(self, goal_id, pose, max_speed):
        self.calls.append(("send", goal_id, pose, max_speed))
        if self.on_send:
            self.on_send(goal_id)

    def cancel_goal(self, goal_id):
        self.calls.append(("cancel", goal_id))

    def set_enabled(self, enabled):
        self.calls.append(("enable", enabled))

    def stop(self):
        self.calls.append(("stop",))


class RuntimeTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = str(Path(self.directory.name) / "goals.sqlite")
        self.clock, self.backend = Clock(), Backend()
        self.config = RuntimeConfig(motion_enabled=True, watchdog_commissioned=True)
        self.runtime = self.create()
        self.addCleanup(lambda: self.runtime.close())

    def create(self, config=None):
        return NavigationRuntime(config or self.config, self.path, self.backend,
                                 source_clock=lambda: self.clock.source,
                                 monotonic_clock=lambda: self.clock.mono)

    def odom(self, **kwargs):
        values = dict(stamp=self.clock.source, frame_id="odom", child_frame_id="base_link", x=0, y=0, yaw=0,
                      linear_speed=0, angular_speed=0, position_variance=0.01, yaw_variance=0.01, posture="standing")
        values.update(kwargs)
        self.runtime.update_odometry(**values)

    def feed(self):
        self.runtime.update_sensor("scan", self.clock.source, "base_link")
        self.runtime.update_sensor("transforms", self.clock.source, "map")
        self.runtime.update_navigation_pose(self.clock.source, "map", 0, 0, 0)
        self.odom()
        self.runtime.update_guard(True)
        self.runtime.update_navigation_available(True)

    def navigate(self, request_id="request-1", **kwargs):
        values = dict(request_id=request_id, pose={"x": 1, "y": 0, "yaw": 0, "frame_id": "map"}, max_speed=0.1)
        values.update(kwargs)
        return self.runtime.navigate(**values)

    def stopped(self):
        goal = self.runtime.status()["active_goal"]
        if goal:
            self.runtime.backend_settled(goal["goal_id"])
        for _ in range(4):
            self.clock.advance(0.2)
            self.odom()

    def test_disabled_defaults_and_restart_freshness(self):
        self.runtime.close()
        self.runtime = self.create(RuntimeConfig())
        self.feed()
        blockers = self.runtime.readiness()["blockers"]
        self.assertIn({"source": "configuration", "reason": "motion_disabled"}, blockers)
        self.assertIn({"source": "configuration", "reason": "watchdog_not_commissioned"}, blockers)
        with self.assertRaises(RuntimeError):
            self.navigate()
        self.assertFalse(any(call[0] == "send" for call in self.backend.calls))

    def test_invalid_sensor_feedback_is_not_replaced_by_last_good_value(self):
        for parameters, reason in (({"stamp": 99}, "stale_source"), ({"stamp": 101}, "future_timestamp"),
                                   ({"frame_id": "camera"}, "wrong_frame"), ({"healthy": False}, "unhealthy"),
                                   ({"coverage_ok": False}, "unhealthy"), ({"stamp": math.nan}, "invalid_timestamp")):
            with self.subTest(reason=reason):
                self.feed()
                values = dict(name="scan", stamp=self.clock.source, frame_id="base_link")
                values.update(parameters)
                self.runtime.update_sensor(**values)
                self.assertFalse(self.runtime.readiness()["ready"])

    def test_regressing_source_and_stale_monotonic_receipt_fail_closed(self):
        self.feed()
        self.clock.advance(0.1)
        self.runtime.update_sensor("scan", self.clock.source, "base_link")
        self.runtime.update_sensor("scan", self.clock.source - 0.01, "base_link")
        self.assertIn({"source": "scan", "reason": "regressing_timestamp"}, self.runtime.readiness()["blockers"])
        self.feed()
        self.clock.mono += 1  # ROS time is paused, but heartbeat freshness still expires.
        self.assertIn({"source": "scan", "reason": "stale_heartbeat"}, self.runtime.readiness()["blockers"])

    def test_duplicate_samples_do_not_refresh_receipts_and_diagnostics_explain(self):
        self.feed()
        self.clock.mono += 1
        self.feed()
        diagnostics = self.runtime.diagnostics()
        self.assertFalse(diagnostics["readiness"]["ready"])
        self.assertEqual(1, diagnostics["sensors"]["scan"]["receipt_age_seconds"])
        self.assertEqual("base_link", diagnostics["sensors"]["scan"]["frame_id"])
        self.assertTrue(diagnostics["sensors"]["scan"]["coverage_ok"])
        self.assertEqual("standing", diagnostics["odometry"]["posture"])
        self.assertNotIn("received", diagnostics["odometry"])

    def test_odometry_frames_covariance_posture_and_values(self):
        for parameters in ({"child_frame_id": "camera"}, {"frame_id": "map"}, {"position_variance": 1},
                           {"position_variance": -1}, {"yaw_variance": math.nan}, {"posture": "fallen"},
                           {"linear_speed": math.inf}, {"x": True}):
            with self.subTest(parameters=parameters):
                self.feed()
                self.odom(**parameters)
                self.assertFalse(self.runtime.readiness()["ready"])

    def test_goal_validation_and_distance_use_navigation_frame(self):
        self.feed()
        for parameters in ({"max_speed": 0.3}, {"max_speed": True}, {"max_speed": math.nan},
                           {"lease_seconds": 0}, {"lease_seconds": 31}, {"timeout_seconds": 601},
                           {"pose": {"x": 0, "y": 0, "yaw": 0, "frame_id": "odom"}},
                           {"pose": {"x": 11, "y": 0, "yaw": 0, "frame_id": "map"}},
                           {"pose": {"x": 0, "y": 0, "yaw": 4, "frame_id": "map"}}):
            with self.subTest(parameters=parameters), self.assertRaises(ValueError):
                self.navigate(**parameters)
        self.assertFalse(any(call[0] == "send" for call in self.backend.calls))

    def test_idempotency_and_one_active_goal(self):
        self.feed()
        first = self.navigate()
        self.assertEqual(first, self.navigate())
        with self.assertRaises(ValueError):
            self.navigate(max_speed=0.15)
        with self.assertRaises(RuntimeError):
            self.navigate("second")
        self.assertEqual(1, sum(call[0] == "send" for call in self.backend.calls))
        self.assertEqual("submitting", first["state"])
        self.runtime.backend_accepted(first["goal_id"])
        self.assertEqual("running", self.runtime.status(first["goal_id"])["state"])

    def test_backend_success_does_not_mean_stopped(self):
        self.feed()
        goal = self.navigate()
        self.runtime.backend_result(goal["goal_id"], "succeeded")
        self.odom(linear_speed=0.1)
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("stopping", state["state"])
        self.assertFalse(state["stopped_confirmed"])
        self.assertIn(("enable", False), self.backend.calls)
        self.stopped()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("succeeded", state["state"])
        self.assertTrue(state["stopped_confirmed"])

    def test_stopped_motion_without_backend_settlement_remains_inhibited(self):
        self.feed()
        goal = self.navigate()
        self.runtime.cancel(goal["goal_id"])
        for _ in range(4):
            self.clock.advance(0.2)
            self.odom()
        state = self.runtime.status(goal["goal_id"])
        self.assertFalse(state["measured_stopped"])
        self.assertFalse(state["backend_settled"])
        self.assertEqual("stopping", state["state"])
        with self.assertRaises(RuntimeError):
            self.navigate("next")
        self.runtime.backend_result(goal["goal_id"], "cancelled")
        self.assertEqual("stopping", self.runtime.status(goal["goal_id"])["state"])
        self.stopped()
        self.assertEqual("cancelled", self.runtime.status(goal["goal_id"])["state"])

    def test_late_acceptance_cannot_enable_cancelled_or_uncertain_goal(self):
        self.feed()
        goal = self.navigate()
        self.assertNotIn(("enable", True), self.backend.calls)
        self.runtime.backend_uncertain(goal["goal_id"], "action_transport_lost")
        self.runtime.backend_accepted(goal["goal_id"])
        self.assertNotIn(("enable", True), self.backend.calls)
        self.assertFalse(self.runtime.status(goal["goal_id"])["backend_settled"])

    def test_request_metadata_is_durable_and_part_of_idempotency(self):
        self.feed()
        self.assertIsNone(self.runtime.request_status("missing"))
        goal = self.navigate(metadata={"kind": "approach", "target": "person-1", "distance": 1.5})
        self.assertEqual(goal, self.runtime.request_status("request-1"))
        with self.assertRaises(ValueError):
            self.navigate(metadata={"kind": "different"})
        self.runtime.close()
        self.runtime = self.create()
        self.assertEqual(goal["metadata"], self.runtime.request_status("request-1")["metadata"])

    def test_duplicate_stamps_and_gaps_do_not_confirm_stop(self):
        self.feed()
        goal = self.navigate()
        self.runtime.cancel(goal["goal_id"])
        self.assertEqual("cancel_requested", self.runtime.status(goal["goal_id"])["state"])
        for _ in range(5):
            self.clock.mono += 0.2
            self.odom()
        self.assertFalse(self.runtime.status(goal["goal_id"])["stopped_confirmed"])
        self.clock.advance(5)
        self.odom()
        self.clock.advance(0.1)
        self.odom()
        self.assertFalse(self.runtime.status(goal["goal_id"])["stopped_confirmed"])
        self.stopped()
        self.assertEqual("cancelled", self.runtime.status(goal["goal_id"])["state"])

    def test_sensor_loss_and_guard_failure_stop_active_goal(self):
        for trigger in (lambda: self.runtime.update_sensor("scan", self.clock.source, "base_link", healthy=False),
                        lambda: self.runtime.update_guard(False),
                        lambda: self.runtime.update_guard(True, stop_latched=True),
                        lambda: self.runtime.update_navigation_available(False)):
            self.feed()
            goal = self.navigate(str(len(self.backend.calls)))
            trigger()
            state = self.runtime.status(goal["goal_id"])
            self.assertEqual("readiness_lost", state["reason"])
            self.assertEqual("stopping", state["state"])
            self.stopped()

    def test_lease_expiry_read_only_status_and_explicit_renew(self):
        self.feed()
        goal = self.navigate(lease_seconds=0.4)
        self.clock.advance(0.2)
        self.feed()
        self.assertEqual(goal["lease_deadline"], self.runtime.status(goal["goal_id"])["lease_deadline"])
        renewed = self.runtime.renew(goal["goal_id"])
        self.assertGreater(renewed["lease_deadline"], goal["lease_deadline"])
        self.clock.advance(0.4)
        self.runtime.tick()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("lease_expired", state["reason"])
        self.assertEqual(state["lease_deadline"], self.runtime.renew(goal["goal_id"])["lease_deadline"])
        self.assertIn(("cancel", goal["goal_id"]), self.backend.calls)

    def test_remaining_budgets_use_monotonic_time_and_stop_at_zero(self):
        self.feed()
        goal = self.navigate(lease_seconds=1, timeout_seconds=2)
        self.clock.advance(0.25)
        state = self.runtime.status(goal["goal_id"])
        self.assertAlmostEqual(0.75, state["lease_remaining_seconds"])
        self.assertAlmostEqual(1.75, state["timeout_remaining_seconds"])
        self.assertEqual(state, self.runtime.request_status("request-1"))
        self.runtime.cancel(goal["goal_id"])
        self.stopped()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual(0, state["lease_remaining_seconds"])
        self.assertEqual(0, state["timeout_remaining_seconds"])

    def test_timeout_is_independent_of_lease(self):
        self.feed()
        goal = self.navigate(timeout_seconds=0.2)
        self.clock.advance(0.2)
        self.runtime.tick()
        self.assertEqual("goal_timeout", self.runtime.status(goal["goal_id"])["reason"])

    def test_target_movement_and_loss_never_switch_identity(self):
        self.feed()
        self.runtime.update_target("person-1", self.clock.source, 2, 0, "map")
        goal = self.navigate(target_id="person-1")
        self.assertEqual(self.clock.source, goal["target"]["stamp"])
        self.runtime.update_target("person-1", self.clock.source, 3, 0, "map")
        self.assertEqual("target_moved", self.runtime.status(goal["goal_id"])["reason"])
        self.stopped()
        self.feed()
        self.runtime.update_target("person-1", self.clock.source, 2, 0, "map")
        goal = self.navigate("second", target_id="person-1")
        self.runtime.update_target("person-1", self.clock.source, 2, 0, "map", valid=False)
        self.assertEqual("target_lost", self.runtime.status(goal["goal_id"])["reason"])

    def test_target_stamp_race_rejects_goal_and_active_identity_survives_eviction(self):
        self.feed()
        stamp = self.clock.source
        self.runtime.update_target("chosen", stamp, 2, 0, "map")
        self.clock.advance(0.1)
        self.feed()
        self.runtime.update_target("chosen", self.clock.source, 2.1, 0, "map")
        with self.assertRaises(RuntimeError):
            self.navigate(target_id="chosen", expected_target_stamp=stamp)
        goal = self.navigate(target_id="chosen", expected_target_stamp=self.clock.source)
        for i in range(300):
            self.runtime.update_target(f"other-{i}", self.clock.source, 3, 0, "map")
        self.assertLessEqual(len(self.runtime._targets), 256)
        self.assertIn("chosen", self.runtime._targets)
        self.assertEqual("submitting", self.runtime.status(goal["goal_id"])["state"])

    def test_restart_abandons_and_never_resumes(self):
        self.feed()
        goal = self.navigate()
        self.runtime.close()
        self.backend.calls.clear()
        self.runtime = self.create()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("abandoned", state["state"])
        self.assertFalse(state["stopped_confirmed"])
        self.assertFalse(self.runtime.readiness()["ready"])
        self.assertEqual(state, self.navigate())  # A retry returns the durable outcome.
        self.assertEqual([("enable", False), ("stop",)], self.backend.calls)
        self.runtime.backend_accepted(goal["goal_id"])
        self.runtime.backend_result(goal["goal_id"], "succeeded")
        self.assertEqual("abandoned", self.runtime.status(goal["goal_id"])["state"])

    def test_bounded_history_and_payload_not_exposed(self):
        self.runtime.close()
        self.runtime = self.create(dataclasses.replace(self.config, max_history=3))
        for i in range(8):
            self.feed()
            goal = self.navigate(f"request-{i}")
            self.assertNotIn("payload", goal)
            self.runtime.cancel(goal["goal_id"])
            self.stopped()
        with sqlite3.connect(self.path) as db:
            self.assertLessEqual(db.execute("SELECT count(*) FROM goals").fetchone()[0], 3)

    def test_backend_callbacks_run_outside_state_lock_and_can_reenter(self):
        self.feed()
        def send(goal_id):
            result = []
            thread = threading.Thread(target=lambda: result.append(self.runtime.status(goal_id)))
            thread.start()
            thread.join(timeout=1)
            self.assertFalse(thread.is_alive(), "backend invoked while state lock was held")
            self.assertTrue(result)
            self.runtime.backend_accepted(goal_id)
        self.backend.on_send = send
        goal = self.navigate()
        self.assertEqual("running", self.runtime.status(goal["goal_id"])["state"])

    def test_backend_send_failure_inhibits_and_stops(self):
        self.feed()
        def fail(_):
            raise OSError("backend unavailable")
        self.backend.on_send = fail
        goal = self.navigate()
        state = self.runtime.status(goal["goal_id"])
        self.assertEqual("backend_command_failed", state["reason"])
        self.assertEqual("stopping", state["state"])
        self.assertEqual(("stop",), self.backend.calls[-1])

    def test_database_failure_cannot_prevent_stop_commands(self):
        self.feed()
        goal = self.navigate()
        original_save = self.runtime._save
        def disk_full(_):
            raise sqlite3.OperationalError("database or disk is full")
        self.runtime._save = disk_full
        try:
            state = self.runtime.cancel(goal["goal_id"])
            self.assertTrue(state["persistence_error"])
            self.assertEqual("cancel_requested", state["state"])
            self.assertIn(("cancel", goal["goal_id"]), self.backend.calls)
            self.assertEqual(("stop",), self.backend.calls[-1])
        finally:
            self.runtime._save = original_save

    def test_uncertain_backend_does_not_write_database_on_every_odometry_sample(self):
        self.feed()
        goal = self.navigate()
        self.runtime.cancel(goal["goal_id"])
        original_save, saves = self.runtime._save, []
        def counted_save(record):
            saves.append(record["state"])
            original_save(record)
        self.runtime._save = counted_save
        try:
            for _ in range(50):
                self.clock.advance(0.02)
                self.odom()
            self.assertEqual(["stopping"], saves)
            self.assertEqual("stopping", self.runtime.status(goal["goal_id"])["state"])
        finally:
            self.runtime._save = original_save

    def test_concurrent_duplicate_requests_send_only_once(self):
        self.feed()
        results = []
        threads = [threading.Thread(target=lambda: results.append(self.navigate())) for _ in range(12)]
        for thread in threads:
            thread.start()
        for thread in threads:
            thread.join(timeout=2)
            self.assertFalse(thread.is_alive())
        self.assertEqual(12, len(results))
        self.assertEqual(1, len({result["goal_id"] for result in results}))
        self.assertEqual(1, sum(call[0] == "send" for call in self.backend.calls))


if __name__ == "__main__":
    unittest.main()

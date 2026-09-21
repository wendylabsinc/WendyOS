import math
import unittest
from robot_navigation.guard import GuardConfig, VelocityGuard
from robot_navigation.guard_node import parse_permit, parse_stop, planar_origin


class GuardTests(unittest.TestCase):
    def setUp(self):
        self.now = 100.0
        self.guard = VelocityGuard(GuardConfig(commissioned=True), source_clock=lambda: self.now, clock=lambda: self.now)
        self.feed()

    def feed(self, distance=4):
        self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi,
                               angle_increment=math.pi / 180, ranges=[distance] * 360,
                               range_min=0.1, range_max=10)
        self.guard.update_odometry(stamp=self.now, frame_id="base_link", linear_speed=0, angular_speed=0)
        self.guard.update_permit(True)
        self.guard.update_command(0.2, 0, 0)

    def test_motion_requires_fresh_permit_command_and_sensors(self):
        self.assertEqual(self.guard.evaluate()["linear_x"], 0.2)
        self.now += 0.21
        self.assertEqual(self.guard.evaluate()["reason"], "command_expired")
        self.guard.update_command(0.2, 0, 0)
        self.now += 0.11
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)

    def test_obstacle_stops_before_footprint_contact(self):
        self.now += 0.01
        self.feed(0.9)
        self.assertEqual(self.guard.evaluate()["reason"], "obstacle_in_stopping_envelope")
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)

    def test_missing_coverage_and_invalid_ranges_are_unknown(self):
        for ranges in ([5] * 180, [math.nan] * 360, [math.inf] * 360, [0] * 360):
            self.now += 0.01
            self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi,
                                   angle_increment=math.pi/180, ranges=ranges, range_min=0.1, range_max=10)
            self.assertEqual(self.guard.evaluate()["reason"], "scan_unknown")

    def test_stop_is_latched_and_reenable_cannot_replay_command(self):
        self.guard.stop()
        self.guard.update_permit(True)
        self.guard.update_command(0.2, 0, 0)
        self.assertEqual(self.guard.evaluate()["reason"], "stop_latched")
        self.guard.reset_local()
        self.guard.update_permit(True)
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)

    def test_replayed_and_future_sensor_stamps_are_rejected(self):
        self.feed()
        self.assertFalse(self.guard.evaluate()["ready"])
        self.now += 0.01
        self.feed()
        self.now -= 0.1
        self.assertFalse(self.guard.evaluate()["ready"])

    def test_goal_speed_and_nonfinite_commands(self):
        self.guard.update_permit(True, 0.1)
        self.assertEqual(self.guard.evaluate()["reason"], "goal_speed_exceeded")
        self.guard.update_command(math.nan, 0, 0)
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)

    def test_reaction_budget_covers_each_watchdog(self):
        for overrides in ({"reaction_seconds": 0.1}, {"sensor_timeout": 0.5}, {"permit_timeout": 0.5}, {"command_timeout": 0.5}):
            with self.subTest(overrides=overrides), self.assertRaises(ValueError):
                GuardConfig(**overrides)

    def test_readiness_is_independent_of_permit_but_not_obstacles(self):
        self.guard.update_permit(False)
        self.assertTrue(self.guard.evaluate()["ready"])
        self.now += 0.01
        self.feed(0.9)
        self.guard.update_permit(False)
        self.assertFalse(self.guard.evaluate()["ready"])
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)

    def test_highwater_survives_invalid_samples(self):
        self.now += 0.1
        self.guard.update_odometry(stamp=self.now, frame_id="wrong", linear_speed=0, angular_speed=0)
        self.guard.update_odometry(stamp=self.now - 0.05, frame_id="base_link", linear_speed=0, angular_speed=0)
        self.assertIsNone(self.guard.odom)
        self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi, angle_increment=math.pi/180,
                               ranges=[math.nan]*360, range_min=0.1, range_max=10)
        self.guard.update_scan(stamp=self.now - 0.05, frame_id="base_link", angle_min=-math.pi, angle_increment=math.pi/180,
                               ranges=[4]*360, range_min=0.1, range_max=10)
        self.assertIsNone(self.guard.scan)

    def test_old_scan_motion_expands_envelope_in_every_direction(self):
        # Behind the command direction still matters: a stale scan may have
        # been acquired before the body rotated or moved.
        self.now += 0.01
        ranges = [4] * 360
        ranges[0] = 0.97
        self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi, angle_increment=math.pi/180,
                               ranges=ranges, range_min=0.1, range_max=10)
        self.assertTrue(self.guard.evaluate()["ready"])
        self.now += 0.2
        self.guard.update_command(0.2, 0, 0)
        result = self.guard.evaluate()
        self.assertFalse(result["ready"])
        self.assertEqual(result["reason"], "obstacle_in_stopping_envelope")

    def test_measured_overspeed_blocks_readiness_without_commands(self):
        self.now += 0.01
        self.guard.update_odometry(stamp=self.now, frame_id="base_link", linear_speed=0.4, angular_speed=0)
        self.guard.update_permit(False)
        result = self.guard.evaluate()
        self.assertFalse(result["ready"])
        self.assertEqual(result["reason"], "measured_speed_exceeded")

    def test_ros_boundary_parsers_reject_ambiguous_permits_and_stop_resets(self):
        self.assertEqual((True, 0.2), parse_permit('{"enabled":true,"max_speed":0.2}'))
        self.assertFalse(parse_stop('{"latch":false}'))
        self.assertTrue(parse_stop('{"latch":true}'))
        for text in ('{"enabled":"true","max_speed":0.2}', '{"enabled":true,"max_speed":NaN}',
                     '{"enabled":true,"max_speed":true}', '{"enabled":true}', '[]', 'x'*1025):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_permit(text)
        for text in ('{"latch":0}', '{"reset":true}', '[]'):
            with self.subTest(text=text), self.assertRaises(ValueError):
                parse_stop(text)

    def test_scan_transform_rejects_tilt_and_nonunit_quaternions(self):
        self.assertEqual((1, 2, 0), planar_origin((1, 2, 0.3), (0, 0, 0, 1)))
        for quaternion in ((0, 0, 0, 2), (math.nan, 0, 0, 1), (math.sin(0.1), 0, 0, math.cos(0.1))):
            with self.subTest(quaternion=quaternion), self.assertRaises(ValueError):
                planar_origin((0, 0, 0), quaternion)

    def test_disabled_permit_cannot_cache_commands_for_next_goal(self):
        self.guard.update_permit(False)
        self.guard.update_command(0.2, 0, 0)
        self.guard.update_permit(True)
        self.assertEqual(self.guard.evaluate()["linear_x"], 0)
        self.guard.update_command(0.2, 0, 0)
        self.assertEqual(self.guard.evaluate()["linear_x"], 0.2)

    def test_coarse_full_circle_and_blind_zone_outside_body_are_unknown(self):
        self.now += 0.01
        self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi,
                               angle_increment=math.pi/4, ranges=[4]*8, range_min=0.1, range_max=10)
        self.assertFalse(self.guard.evaluate()["ready"])
        self.now += 0.01
        self.guard.update_scan(stamp=self.now, frame_id="base_link", angle_min=-math.pi,
                               angle_increment=math.pi/180, ranges=[4]*360, range_min=0.2,
                               range_max=10, origin=(0.4, 0, 0))
        self.assertFalse(self.guard.evaluate()["ready"])

    def test_zero_current_speed_does_not_erase_possible_motion_since_scan(self):
        self.now += 0.01
        self.feed(0.84)
        self.guard.update_command(0, 0, 0)
        self.assertTrue(self.guard.evaluate()["ready"])
        self.now += 0.2
        self.guard.update_command(0, 0, 0)
        self.assertFalse(self.guard.evaluate()["ready"])


if __name__ == "__main__":
    unittest.main()

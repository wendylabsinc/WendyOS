"""Stationary scheduling regression with the production admission gate.

This models bounded timer jitter and the next scan's delivery, not ROS runtime
latency. The runtime checks still have to verify actual AMCL publication gaps.
"""
import ast
from pathlib import Path
import unittest

from sim.patrol_localization import PassiveUpdates, health


def stationary_period():
    """Read the wired wall timer without importing unavailable ROS packages."""
    source = Path(__file__).with_name('patrol_nav.py').read_text()
    matches = []
    for node in ast.walk(ast.parse(source)):
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and node.func.attr == 'create_timer' and len(node.args) >= 2
                and isinstance(node.args[1], ast.Attribute)
                and node.args[1].attr == 'update_stationary'):
            matches.append(ast.literal_eval(node.args[0]))
    if len(matches) != 1:
        raise AssertionError('expected one stationary observation wall timer')
    return matches[0]


def request(gate, now, **changes):
    values = dict(now=now, stamp=now - 1., sensor_time=now - .02,
                  sensors_ready=True, scan_valid=True, speed=.01,
                  angular_speed=.01, command=(0., 0., 0.),
                  scope=('office', '1', 'held-goal'))
    values.update(changes)
    return gate.begin(**values)


def scheduled_requests(period):
    gate = PassiveUpdates()
    admitted = []
    for tick in range(int(30. / period)):
        # Ten milliseconds of callback jitter can miss an exact 2s boundary.
        now = 10. + tick * period + (.01 if tick % 3 == 0 else 0.)
        token = request(gate, now)
        if token is not None:
            admitted.append(now)
            gate.finish(token)
    return admitted


class PassiveScheduleTests(unittest.TestCase):
    def test_existing_one_second_poll_can_starve_pose_freshness(self):
        requests = scheduled_requests(1.)
        self.assertAlmostEqual(requests[1] - requests[0], 3.)
        # A service response only schedules work. The next scan plus dispatch
        # delay is represented here by two bounded 50ms and 120ms delays.
        first_pose = requests[0] + .05
        next_pose = requests[1] + .12
        diagnosis = health(now=next_pose - .001,
                           sensor_time=next_pose - .02,
                           pose_time=first_pose, costmap_time=next_pose - .1,
                           policy_ready=True, sensors_ready=True,
                           scan_valid=True, pose=dict(x=0., y=0., yaw=0.),
                           covariance=[.001] * 36, navigation_ready=True,
                           planner_ready=True, passive=PassiveUpdates())
        self.assertIn('pose_stale', diagnosis['reasons'])

    def test_wired_poll_keeps_margin_without_requesting_before_two_seconds(self):
        requests = scheduled_requests(stationary_period())
        gaps = [right - left for left, right in zip(requests, requests[1:])]
        self.assertGreater(len(gaps), 5)
        self.assertGreaterEqual(min(gaps), 2.)
        # Leave margin for capture and delivery under the strict 3s pose age.
        self.assertLessEqual(max(gaps) + .12, 2.4)

    def test_faster_poll_keeps_pending_request_exclusive(self):
        gate = PassiveUpdates()
        token = request(gate, 10.)
        self.assertIsNotNone(token)
        for tick in range(1, 80):
            self.assertIsNone(request(gate, 10. + tick * .1))
        self.assertEqual(gate.attempts, 1)
        gate.finish(token)
        self.assertIsNotNone(request(gate, 18.))

    def test_faster_poll_preserves_motion_and_observation_rejections(self):
        for change in (dict(speed=.08), dict(angular_speed=.1),
                       dict(command=(.1, 0., 0.)), dict(scan_valid=False),
                       dict(sensors_ready=False), dict(sensor_time=1.)):
            gate = PassiveUpdates()
            for tick in range(50):
                self.assertIsNone(request(gate, 10. + tick * .1, **change))
            self.assertEqual(gate.attempts, 0)
        gate = PassiveUpdates()
        token = request(gate, 10., stamp=5.)
        gate.finish(token)
        for stamp in (5., 4.):
            self.assertIsNone(request(gate, 15., stamp=stamp))


if __name__ == '__main__':
    unittest.main()

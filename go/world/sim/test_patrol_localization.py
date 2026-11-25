import math
import json
import threading
import unittest

from sim.patrol_localization import NonOverlappingCall, PassiveUpdates, health, status_measurements


class LocalizationTests(unittest.TestCase):
    def snapshot(self, **changes):
        values = dict(now=10., sensor_time=9.8, pose_time=9., costmap_time=9.,
                      policy_ready=True, sensors_ready=True, scan_valid=True,
                      pose=dict(x=1., y=2., yaw=.3), covariance=[0.] * 36,
                      navigation_ready=True, planner_ready=True, passive=PassiveUpdates())
        values.update(changes)
        return health(**values)

    def attempt(self, gate, **changes):
        values = dict(now=10., stamp=3., sensor_time=9.8, sensors_ready=True,
                      scan_valid=True, speed=.01, angular_speed=.01,
                      command=(0., 0., 0.), scope=('office', '1', 'held-goal'))
        values.update(changes)
        return gate.begin(**values)

    def test_ready_keeps_existing_strict_thresholds(self):
        self.assertTrue(self.snapshot()['ready'])
        for field, value, reason in [('sensor_time', 9.39, 'sensor_stale'),
                                     ('pose_time', 7., 'pose_stale'),
                                     ('costmap_time', 7., 'costmap_stale')]:
            result = self.snapshot(**{field: value})
            self.assertFalse(result['ready'])
            self.assertIn(reason, result['reasons'])
        for i in (0, 7, 35):
            covariance = [0.] * 36
            covariance[i] = .0225
            self.assertIn('covariance_high', self.snapshot(covariance=covariance)['reasons'])
            covariance[i] = .022499
            self.assertTrue(self.snapshot(covariance=covariance)['ready'])

    def test_invalid_clocks_poses_and_covariance_fail_closed(self):
        for value in (0., -1., 11., math.nan, math.inf):
            self.assertIsNone(self.snapshot(pose_time=value)['pose_age_s'])
            self.assertFalse(self.snapshot(pose_time=value)['ready'])
        for pose in (None, {}, dict(x=math.nan, y=0., yaw=0.)):
            self.assertIn('pose_invalid', self.snapshot(pose=pose)['reasons'])
        for covariance in ([], [0.] * 8, [math.nan] * 36, [math.inf] * 36, [-.001] * 36):
            self.assertIn('covariance_invalid', self.snapshot(covariance=covariance)['reasons'])
        for field, reason in [('policy_ready', 'policy_unconfirmed'), ('sensors_ready', 'sensors_unready'),
                              ('scan_valid', 'scan_invalid'), ('navigation_ready', 'navigation_unavailable'),
                              ('planner_ready', 'planner_unavailable')]:
            self.assertIn(reason, self.snapshot(**{field: False})['reasons'])

    def test_passive_requires_stationary_fresh_distinct_observations(self):
        for changes in (dict(speed=.08), dict(angular_speed=.1), dict(speed=math.nan),
                        dict(speed=-1), dict(command=(0., 0., .1)), dict(command=(math.nan, 0., 0.)),
                        dict(sensor_time=9.), dict(sensor_time=11.), dict(stamp=math.inf),
                        dict(scan_valid=False), dict(sensors_ready=False)):
            self.assertIsNone(self.attempt(PassiveUpdates(), **changes), changes)
        gate = PassiveUpdates()
        first = self.attempt(gate)
        self.assertIsNotNone(first)
        self.assertIsNone(self.attempt(gate, now=13., sensor_time=12.8, stamp=4.))
        gate.finish(first)
        self.assertIsNone(self.attempt(gate, now=11., sensor_time=10.8, stamp=4.))
        for stamp in (3., 2.):
            self.assertIsNone(self.attempt(gate, now=13., sensor_time=12.8, stamp=stamp))
        self.assertIsNone(self.attempt(gate, now=9., sensor_time=8.8, stamp=4.))
        second = self.attempt(gate, now=12., sensor_time=11.8, stamp=4., scope=('office', '2', 'new-goal'))
        self.assertIsNotNone(second)
        gate.finish(first)  # A late completion cannot clear a newer request.
        self.assertEqual(gate.pending, second)
        gate.finish(second)
        self.assertIsNone(gate.pending)
        result = self.snapshot(passive=gate, now=12., sensor_time=11.8, pose_time=11., costmap_time=11.)
        self.assertTrue(result['ready'])
        self.assertFalse(result['motion_resumed'])
        self.assertEqual(result['passive']['attempts'], 2)
        self.assertEqual(result['passive']['last_scan_stamp'], 4.)
        self.assertEqual(result['passive']['last_attempt_age_s'], 0.)

    def test_invalid_raw_measurements_do_not_hide_health_json(self):
        for value in (math.nan, math.inf, -math.inf):
            raw_pose = dict(x=value, y=0., yaw=0.)
            raw_covariance = [0.] * 36
            raw_covariance[0] = value
            diagnosis = self.snapshot(pose=raw_pose, covariance=raw_covariance)
            pose, covariance = status_measurements(raw_pose, raw_covariance)
            payload = json.loads(json.dumps(dict(ready=diagnosis['ready'], pose=pose,
                covariance=covariance, localization_health=diagnosis), allow_nan=False))
            self.assertIsNone(payload['pose'])
            self.assertEqual(payload['covariance'], [])
            self.assertIn('pose_invalid', payload['localization_health']['reasons'])
            self.assertIn('covariance_invalid', payload['localization_health']['reasons'])
            self.assertFalse(payload['ready'])

    def test_drive_callback_skips_overlap_and_releases_after_failure(self):
        gate = NonOverlappingCall()
        entered, release = threading.Event(), threading.Event()
        calls = []
        def slow_dispatch():
            calls.append('stopped command sent')
            entered.set()
            release.wait(2.)
            calls.append('stopped command acknowledged')
        worker = threading.Thread(target=lambda: gate.run(slow_dispatch))
        worker.start()
        try:
            self.assertTrue(entered.wait(1.))
            self.assertFalse(gate.run(lambda: calls.append('moving command sent')))
            self.assertEqual(calls, ['stopped command sent'])
        finally:
            release.set()
            worker.join(2.)
        self.assertFalse(worker.is_alive())
        self.assertTrue(gate.run(lambda: calls.append('moving command sent')))
        self.assertEqual(calls, ['stopped command sent', 'stopped command acknowledged', 'moving command sent'])
        def failed_dispatch():
            raise RuntimeError('request failed')
        with self.assertRaises(RuntimeError):
            gate.run(failed_dispatch)
        self.assertTrue(gate.run(lambda: None))


if __name__ == '__main__':
    unittest.main()

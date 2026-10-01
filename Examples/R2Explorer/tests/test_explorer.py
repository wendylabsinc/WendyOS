import array
import math
import sys
import unittest
from unittest.mock import Mock

from app import Explorer, check_fresh, check_status, front_depth
from planner import Planner


class SensorTests(unittest.TestCase):
    def test_hardware_is_rejected_even_if_healthy(self):
        with self.assertRaisesRegex(ValueError, 'simulator'):
            check_status({'ready': True, 'healthy': True, 'robot_kind': 'rosmaster-r2'})

    def test_stale_or_future_sensor_stamp_is_rejected(self):
        check_fresh(1_000_000_000, 1_100_000_000)
        for stamp in (0, 2_000_000_000):
            with self.assertRaises(ValueError):
                check_fresh(stamp, 1_100_000_000)

    def test_depth_unknown_and_obstacle_are_distinct(self):
        calibration = {'width': 80, 'height': 60, 'encoding': '16UC1', 'depth_unit': 'mm'}
        self.assertIsNone(front_depth(bytes(80*60*2), calibration))
        raw = array.array('H', [250]*(80*60))
        if sys.byteorder != 'little':
            raw.byteswap()
        self.assertEqual(front_depth(raw.tobytes(), calibration), .25)
        with self.assertRaises(ValueError):
            front_depth(b'\x00', calibration)


class ControlTests(unittest.TestCase):
    def test_stop_does_not_rearm_after_owner_revoked(self):
        explorer = Explorer('http://127.0.0.1:8890')
        explorer.running, explorer.token = True, 'old-session'
        explorer.sim.request = Mock(side_effect=RuntimeError('revoked'))
        explorer.stop()
        self.assertFalse(explorer.running)
        self.assertIsNone(explorer.token)
        self.assertEqual(explorer.sim.request.call_count, 1)
        path, body = explorer.sim.request.call_args.args
        self.assertEqual(path, '/api/app/command')
        self.assertTrue(body['stop'])

    def test_world_reset_stops_before_any_command(self):
        explorer = Explorer('http://127.0.0.1:8890')
        explorer.epoch = 1
        explorer.sim.request = Mock(return_value={'simulation': True, 'robot_kind': 'rosmaster-r2',
                                                  'ready': True, 'healthy': True, 'epoch': 2,
                                                  'control_mode': 'app'})
        with self.assertRaisesRegex(ValueError, 'control changed'):
            explorer.step()
        self.assertEqual(explorer.sim.request.call_count, 1)


class PlannerTests(unittest.TestCase):
    def setUp(self):
        self.pose = dict(x=0., y=0., yaw=0., speed=0., steering=0.)
        self.scan = {'origin': [.13, 0., .28], 'yaw': 0., 'angle_min': -math.pi,
                     'angle_increment': math.tau/360, 'ranges': [3.]*360}

    def test_open_room_drives_and_records_observations(self):
        planner = Planner()
        speed, _, _ = planner.command(self.pose, self.scan)
        self.assertGreater(speed, 0)
        self.assertGreater(len(planner.free), 50)
        self.assertGreater(len(planner.occupied), 20)
        self.assertEqual(len(planner.visits), 1)

    def test_close_depth_blocks_forward_motion(self):
        speed, _, _ = Planner().command(self.pose, self.scan, depth_front=.2)
        self.assertLessEqual(speed, 0)

    def test_surrounded_car_stays_stopped(self):
        self.scan['ranges'] = [.15]*360
        speed, _, _ = Planner().command(self.pose, self.scan)
        self.assertEqual(speed, 0)


if __name__ == '__main__':
    unittest.main()

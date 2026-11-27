"""Functional tests for full-scene composition, physics and sensing isolation."""
import json
import math
import os
from pathlib import Path
import unittest
import xml.etree.ElementTree as ET

import numpy as np
from sim.patrol_physics import PatrolPhysics, LegOdometry, IMUAttitude, compose_office, command_values

SOURCE = os.environ.get('PATROL_SOURCE', '/source')
ASSETS = os.environ.get('PATROL_ASSETS', '/assets')
OFFICE = Path(__file__).resolve().parents[1] / 'examples/collaborative-office/office.xml'


class Commands(unittest.TestCase):
    def test_speed_contract(self):
        for v in (0., .5, .55):
            self.assertEqual(command_values({'vx': v}), [v, 0., 0.])
        for body in ({'vx': .2}, {'vx': .56}, {'vx': -.55}, {'vx': float('nan')}, {'vx': True}, {'vy': .1}, {'wz': .71}):
            with self.assertRaises(ValueError): command_values(body)

    def test_imu_attitude_uses_gravity_and_gyro_only(self):
        estimator = IMUAttitude()
        roll, pitch = .12, -.08
        gravity = [-math.sin(pitch)*9.81, math.sin(roll)*math.cos(pitch)*9.81,
                   math.cos(roll)*math.cos(pitch)*9.81]
        for _ in range(4000): estimator.update(gravity, [0., 0., 0.], .002)
        self.assertAlmostEqual(estimator.roll, roll, places=4)
        self.assertAlmostEqual(estimator.pitch, pitch, places=4)
        # With gravity unavailable, the measured gyro still propagates attitude.
        before = estimator.roll
        estimator.update([0., 0., 0.], [.5, 0., 0.], .02)
        self.assertAlmostEqual(estimator.roll-before, .01)
        held = (estimator.roll, estimator.pitch)
        for _ in range(500):
            estimator.update([2., 0., 9.6], [0., 0., 0.], .002, gravity_correction=False)
        self.assertEqual((estimator.roll, estimator.pitch), held)

    def test_odometry_no_pose_input(self):
        odom = LegOdometry()
        for _ in range(100): odom.update([1., 0., 0.], [0., 0., 0.], .01)
        self.assertAlmostEqual(odom.x, 1.005, places=4)
        self.assertAlmostEqual(odom.yaw, .0002)
        odom.update(None, [0., 0., 0.], .01)
        self.assertFalse(odom.valid)
        self.assertEqual(odom.vx, 0)


@unittest.skipUnless(Path(ASSETS).is_dir() and Path(SOURCE).is_dir(), 'pinned external Go2 bundle required')
class Physics(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.rt = PatrolPhysics(SOURCE, ASSETS, OFFICE, 'patrol-tests')
        for _ in range(1600): cls.rt.step()

    def test_full_office(self):
        before = ET.parse(OFFICE).getroot()
        after = ET.fromstring(compose_office(OFFICE, ASSETS))
        car_names = {g.get('name') for body in before.findall('worldbody/body') if body.get('name', '').startswith('robot-') for g in body.iter('geom')}
        original = {g.get('name'): g for g in before.iter('geom') if g.get('name') not in car_names}
        compiled = {g.get('name'): g for g in after.iter('geom')}
        for name, geom in original.items():
            self.assertIn(name, compiled)
            for attr in ('size', 'type', 'contype', 'conaffinity', 'pos', 'quat', 'fromto'):
                self.assertEqual(geom.get(attr), compiled[name].get(attr), (name, attr))
        self.assertEqual(self.rt.model.nq, 19)
        self.assertEqual(self.rt.model.nu, 12)
        self.assertGreater(self.rt.model.ngeom, 500)
        self.assertIsNone(after.find("worldbody/body[@name='inspection-puck']"))

    def test_ready_and_ground_truth_isolation(self):
        sensors = self.rt.sensors()
        self.assertTrue(sensors['ready'])
        self.assertNotIn('position', sensors)
        self.assertNotIn('ground_truth', sensors)
        self.assertLess(abs(sensors['odom']['x']), .1)
        self.assertLess(self.rt.state()['position'][0], -5.9)
        self.assertEqual(len(sensors['scan']['ranges']), 360)
        self.assertEqual(sensors['transforms'][0]['translation'], [.28, 0., .08])

    def test_scan_odometry_and_attitude_share_capture_time(self):
        before = self.rt.sensors()
        old_x, old_roll = self.rt.odom.x, self.rt.imu.roll
        try:
            # New odometry/attitude must not be mislabeled with the earlier
            # scan stamp while physics progresses between raycasts.
            self.rt.odom.x += .55*.099
            self.rt.imu.roll += .05
            after = self.rt.sensors()
            self.assertEqual(before['stamp'], after['stamp'])
            self.assertEqual(before['odom'], after['odom'])
            self.assertEqual(before['imu'], after['imu'])
        finally:
            self.rt.odom.x, self.rt.imu.roll = old_x, old_roll

    def test_map_and_mesh(self):
        m = self.rt._map
        self.assertEqual(len(m['data']), m['width'] * m['height'])
        x, y = int((-6-m['origin'][0])/m['resolution']), int((0-m['origin'][1])/m['resolution'])
        self.assertEqual(m['data'][y*m['width']+x], 0)
        self.assertTrue(any(g['type'] == 'mesh' and g['vertices'] and g['faces'] for g in self.rt._geometry['geoms']))
        self.assertEqual(len(self.rt.state()['joints']['q']), 12)
        self.assertTrue(any(self.rt.state()['foot_contacts']))

    def test_synthetic_capture(self):
        capture = self.rt.capture()
        self.assertEqual({s['kind'] for s in capture['samples']}, {'camera', 'lidar', 'gas', 'audio'})
        self.assertEqual(len(capture['samples']), 5)
        self.assertEqual(len(capture['media']), 3)
        for s in capture['samples']:
            self.assertEqual(s['run_id'], 'patrol-tests')
            if s['kind'] in ('camera', 'audio'):
                self.assertIn(s['evidence']['sha256'], capture['media'])
            else:
                self.assertNotIn('evidence', s)
        mobile = next(s for s in capture['samples'] if s['sensor_id'] == 'camera-b')
        pos = self.rt.data.cam_xpos[self.rt.model.camera('camera-b').id]
        np.testing.assert_allclose(mobile['position_m'], [pos[0], pos[2], -pos[1]])

    def test_lidar_loss_not_ready(self):
        self.rt.lidar_loss_until = self.rt.data.time + 1
        self.assertFalse(self.rt.sensors()['ready'])
        self.assertIsNone(self.rt.sensors()['scan'])
        with self.assertRaises(ValueError): self.rt.capture()
        self.rt.lidar_loss_until = 0

    def test_z_torque_walk_and_watchdog(self):
        # Independent physics instance. Virtual time tests the controller's real
        # deadline branch without delaying the suite or changing its dynamics.
        rt = PatrolPhysics(SOURCE, ASSETS, OFFICE, 'patrol-watchdog')
        rt.sim._clock = lambda: float(rt.data.time)
        rt.sim._last_clock = 0.
        for _ in range(1500): rt.step()
        start = rt.data.qpos[:3].copy()
        previous = start.copy()
        joint_min, joint_max = rt.data.qpos[rt.sim.qaddr].copy(), rt.data.qpos[rt.sim.qaddr].copy()
        for tick in range(2000):
            if tick % 50 == 0: rt.command({'vx': .55})
            rt.step()
            self.assertLess(np.linalg.norm(rt.data.qpos[:3] - previous), .01)
            previous = rt.data.qpos[:3].copy()
            joint_min = np.minimum(joint_min, rt.data.qpos[rt.sim.qaddr])
            joint_max = np.maximum(joint_max, rt.data.qpos[rt.sim.qaddr])
        self.assertGreater(np.linalg.norm(rt.data.qpos[:2] - start[:2]), 1.)
        self.assertGreater(float(np.max(joint_max - joint_min)), .3)
        for _ in range(1000): rt.step()  # No commands: executor disappeared.
        np.testing.assert_array_equal(rt.sim.command, [0., 0., 0.])
        self.assertEqual(rt.sim.mode, 'standing')
        self.assertGreater(rt.data.qpos[2], .25)
        rt.capture_worker.shutdown()

    @classmethod
    def tearDownClass(cls):
        if cls.rt.renderer: cls.rt.capture_worker.submit(cls.rt.renderer.close).result()
        cls.rt.capture_worker.shutdown()

if __name__ == '__main__': unittest.main()

import math
import unittest

import numpy as np

from r2_sim.camera import CAMERA_OFFSET, HEIGHT, WIDTH, render, jpeg
from r2_sim.simulation import Simulation


class CameraTests(unittest.TestCase):
    def test_depth_matches_obstacle_distance_in_millimetres(self):
        sim = Simulation()
        rgb, depth, preview = render(sim.state(), sim.obstacles)
        expected = (2.5 - .8/2 - CAMERA_OFFSET[0]) * 1000
        self.assertAlmostEqual(int(depth[HEIGHT//2, WIDTH//2]), expected, delta=2)
        self.assertEqual(depth.dtype, np.dtype('<u2'))
        self.assertEqual(rgb.shape, (HEIGHT, WIDTH, 3))
        self.assertEqual(preview.shape, rgb.shape)
        self.assertTrue(jpeg(rgb).startswith(b'\xff\xd8'))

    def test_motion_changes_rgb_and_depth(self):
        sim = Simulation()
        before = render(sim.state(), sim.obstacles)
        sim.x = .5
        after = render(sim.state(), sim.obstacles)
        self.assertAlmostEqual(int(before[1][120,160])-int(after[1][120,160]), 500, delta=2)
        self.assertFalse(np.array_equal(before[0], after[0]))
        sim.yaw = math.pi
        behind = render(sim.state(), sim.obstacles)
        self.assertEqual(behind[1][120,160], 0, 'far wall is beyond the sensor range')

    def test_nearer_obstacle_occludes_farther_obstacle(self):
        sim = Simulation()
        near = dict(x=1., y=0., width=.2, depth=.5, height=.5)
        _, depth, _ = render(sim.state(), [*sim.obstacles, near])
        self.assertAlmostEqual(int(depth[120,160]), (1.-.1-CAMERA_OFFSET[0])*1000, delta=2)

    def test_optical_axes_ground_is_below_center(self):
        rgb, depth, _ = render(Simulation().state(), [])
        self.assertEqual(depth[0,WIDTH//2], 0)
        self.assertGreater(depth[-1,WIDTH//2], 0)
        self.assertFalse(np.array_equal(rgb[0],rgb[-1]))

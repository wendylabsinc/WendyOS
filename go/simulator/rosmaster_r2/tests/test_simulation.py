import math
import unittest

from r2_sim.simulation import Simulation


class KinematicsTests(unittest.TestCase):
    def test_forward_and_reverse_have_no_lateral_motion(self):
        for speed in (.5, -.5):
            sim = Simulation(obstacles=[])
            sim.drive(speed, 0.)
            for _ in range(100):
                sim.step(.01)
            self.assertGreater(sim.x * speed, 0)
            self.assertEqual(sim.y, 0)
            self.assertEqual(sim.yaw, 0)

    def test_constant_steering_follows_expected_circle(self):
        for speed in (.4, -.4):
            for steering in (.3, -.3):
                sim = Simulation(obstacles=[])
                sim.drive(speed, steering)
                sim.speed, sim.steering = speed, steering
                for _ in range(100):
                    sim.step(.01)
                radius = sim.geometry.wheelbase / math.tan(steering)
                yaw = speed / radius
                self.assertAlmostEqual(sim.yaw, yaw, places=8)
                self.assertAlmostEqual(sim.x, radius * math.sin(yaw), places=8)
                self.assertAlmostEqual(sim.y, radius * (1 - math.cos(yaw)), places=8)

    def test_front_wheels_share_turn_center_and_inside_wheel_is_slower(self):
        sim = Simulation()
        sim.speed, sim.steering = .5, .3
        angles, speeds = sim.wheel_state()
        radius = sim.geometry.wheelbase / math.tan(sim.steering)
        self.assertAlmostEqual(sim.geometry.wheelbase / math.tan(angles[0]) + sim.geometry.track / 2, radius)
        self.assertAlmostEqual(sim.geometry.wheelbase / math.tan(angles[1]) - sim.geometry.track / 2, radius)
        self.assertGreater(angles[0], angles[1])
        self.assertLess(speeds[0], speeds[1])
        self.assertLess(speeds[2], speeds[3])

    def test_twist_cannot_strafe_or_turn_in_place(self):
        sim = Simulation()
        with self.assertRaises(ValueError):
            sim.twist(1, .1, 0)
        sim.twist(0, 0, 1)
        sim.step(.1)
        self.assertEqual((sim.x, sim.y, sim.yaw), (0, 0, 0))
        sim.twist(-.5, 0, .4)
        self.assertLess(sim.target_steering, 0)

    def test_limits_and_invalid_inputs(self):
        sim = Simulation()
        sim.drive(100, 100)
        self.assertEqual(sim.target_speed, sim.geometry.max_speed)
        self.assertEqual(sim.target_steering, sim.geometry.max_steering)
        sim.step(.1)
        self.assertAlmostEqual(sim.speed, .12)
        self.assertAlmostEqual(sim.steering, math.radians(9))
        for value in (float('nan'), float('inf'), True, '1', None):
            with self.assertRaises(ValueError):
                sim.drive(value, 0)

    def test_obstacle_stops_car_and_allows_reverse(self):
        sim = Simulation()
        sim.drive(1.8, 0)
        for _ in range(400):
            sim.step(.01)
        self.assertTrue(sim.collision)
        self.assertEqual(sim.speed, 0)
        self.assertLess(sim.x + sim.geometry.wheelbase / 2 + sim.geometry.length / 2, 2.1)
        stopped_x = sim.x
        sim.drive(-.5, 0)
        for _ in range(100):
            sim.step(.01)
        self.assertFalse(sim.collision)
        self.assertLess(sim.x, stopped_x)

    def test_rotated_footprint_and_room_boundary(self):
        sim = Simulation(obstacles=[])
        self.assertTrue(sim.intersects(4.9, 0, 0))
        self.assertTrue(sim.intersects(0, 4.9, math.pi/2))
        self.assertFalse(sim.intersects(0, 0, math.pi/4))

    def test_lidar_uses_obstacle_geometry_and_sensor_origin(self):
        sim = Simulation()
        scan = sim.scan()
        self.assertEqual(len(scan['ranges']), 360)
        self.assertAlmostEqual(scan['ranges'][180], 2.1 - .13)
        self.assertAlmostEqual(scan['ranges'][0], 5.13)
        sim.x = .5
        self.assertAlmostEqual(sim.scan()['ranges'][180], 2.1 - .63)


if __name__ == '__main__':
    unittest.main()

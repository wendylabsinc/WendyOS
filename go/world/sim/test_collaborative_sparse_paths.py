"""Sparse-route consumer checks, independent of planner search resolution.

These use the actual Python follower with ideal kinematic integration. They do
not establish MuJoCo contact clearance or physical stopping distance.
"""
import math
import unittest
from collections import deque
from unittest.mock import Mock

from sim.collaborative_runtime import (
    MAX_SPEED, RobotLink, SceneRuntime, SimulatedMesh, follow_path,
)


def link():
    return RobotLink({"id": "atlas", "start": [0., 0.]})


def diagonal_command():
    return {"velocity": [.33, .44], "path": [[0., 0.], [6., 8.], [8., 8.]],
            "job_id": "inspection", "state": "moving"}


class SparseFollowerTests(unittest.TestCase):
    def test_long_diagonal_tracks_line_and_stops_without_dense_waypoints(self):
        path = [[0., 0.], [6., 8.]]
        position, cursor = [0., 0.], 0
        previous_distance = math.dist(position, path[-1])
        max_deviation = 0.
        for _ in range(2000):  # At most 40 ideal simulation seconds.
            velocity, cursor = follow_path(position, path, cursor)
            self.assertLessEqual(math.hypot(*velocity), MAX_SPEED + 1e-12)
            max_deviation = max(max_deviation, abs(8 * position[0] - 6 * position[1]) / 10)
            if velocity == [0., 0.]:
                break
            position = [p + v * .02 for p, v in zip(position, velocity)]
            current_distance = math.dist(position, path[-1])
            self.assertLess(current_distance, previous_distance)
            previous_distance = current_distance
        else:
            self.fail("Long diagonal did not finish within the bounded interval")
        self.assertEqual(cursor, len(path))
        self.assertLess(math.dist(position, path[-1]), .075)
        self.assertLess(max_deviation, 1e-10)
        self.assertEqual(path, [[0., 0.], [6., 8.]])

    def test_corner_is_not_skipped_while_next_segment_is_closer_to_goal(self):
        path = [[0., 0.], [3., 4.], [3., 10.]]
        # The next target is far north. A goal-directed shortcut here would
        # cut the accepted diagonal before its corner.
        for position in ([1.5, 2.], [2.94, 3.92]):
            velocity, cursor = follow_path(position, path, 1)
            self.assertEqual(cursor, 1)
            self.assertGreater(velocity[0], 0)
            self.assertAlmostEqual(velocity[0] / velocity[1], .75)
        velocity, cursor = follow_path([3., 4.], path, 1)
        self.assertEqual(cursor, 2)
        self.assertEqual(velocity[0], 0)
        self.assertGreater(velocity[1], 0)

    def test_disconnect_midsegment_keeps_progress_and_terminal_stop(self):
        worker = link()
        original = diagonal_command()
        self.assertTrue(worker.accept(original, 1., 20))
        self.assertEqual(worker.velocity([0., 0.], 1.1, True, True), [.33, .44])
        self.assertEqual(worker.cursor, 1)
        self.assertEqual(worker.velocity([3., 4.], 1.2, True, True), [.33, .44])
        worker.online = False
        self.assertFalse(worker.accept({"velocity": [-.55, 0.], "path": [[-4., 4.]],
                                        "job_id": "replacement"}, 2., 21))
        # A disconnected worker may use its accepted route beyond the online
        # command TTL while the caller's mission/health conditions still hold.
        velocity = worker.velocity([3., 4.], 100., True, True)
        self.assertEqual(worker.cursor, 1)
        self.assertGreater(velocity[0], 0)
        self.assertGreater(velocity[1], 0)
        self.assertAlmostEqual(velocity[0] / velocity[1], .75)
        self.assertEqual(worker.path, original["path"])
        self.assertEqual(worker.job_id, "inspection")
        self.assertEqual(worker.velocity([6., 8.], 101., True, True), [.55, 0.])
        self.assertEqual(worker.cursor, 2)
        for position in ([8., 8.], [8.1, 8.], [7.9, 8.]):
            self.assertEqual(worker.velocity(position, 102., True, True), [0., 0.])
            self.assertEqual(worker.cursor, 3)
            self.assertEqual(worker.state, "awaiting_reconnect")

    def test_reconnection_requires_post_restore_command_before_motion(self):
        runtime = SceneRuntime.__new__(SceneRuntime)
        worker = link()
        runtime.links = {"atlas": worker}
        runtime.mesh = SimulatedMesh(["atlas"])
        runtime._routes = {"atlas": runtime.mesh.route("atlas")}
        runtime.data, runtime.events, runtime.sequence = Mock(time=1.), deque(), 20
        self.assertTrue(worker.accept(diagonal_command(), 1., 20))
        worker.velocity([0., 0.], 1.1, True, True)
        runtime.toggle_edge("atlas--hub")
        self.assertFalse(worker.online)
        self.assertGreater(worker.velocity([3., 4.], 2., True, True)[0], 0)
        runtime.sequence = 30
        runtime.toggle_edge("atlas--hub")
        self.assertTrue(worker.online)
        self.assertEqual(worker.velocity([3., 4.], 3., True, True), [0., 0.])
        self.assertFalse(worker.accept(diagonal_command(), 3., 30))
        self.assertEqual(worker.velocity([3., 4.], 3.1, True, True), [0., 0.])
        resumed = {"velocity": [.33, .44], "path": [[6., 8.], [8., 8.]],
                   "job_id": "inspection", "state": "moving"}
        self.assertTrue(worker.accept(resumed, 3.2, 31))
        self.assertEqual(worker.velocity([3., 4.], 3.3, True, True), [.33, .44])
        self.assertEqual(worker.path[0], [6., 8.])

    def test_sparse_cached_route_keeps_all_existing_motion_gates(self):
        worker = link()
        worker.accept(diagonal_command(), 1., 20)
        self.assertEqual(worker.velocity([3., 4.], 1.500001, True, True), [0., 0.])
        worker.online = False
        for active, healthy, available in ((False, True, True), (True, False, True), (True, True, False)):
            worker.available = available
            self.assertEqual(worker.velocity([3., 4.], 10., active, healthy), [0., 0.])
        fresh = link()
        fresh.online = False
        self.assertEqual(fresh.velocity([3., 4.], 10., True, True), [0., 0.])


if __name__ == "__main__":
    unittest.main()

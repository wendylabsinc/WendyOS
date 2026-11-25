"""Communication gates, cached autonomy and ray-map merge invariants."""
import unittest
from collections import deque
from unittest.mock import Mock

from sim.collaborative_runtime import CoordinatorPipe, RobotLink, SceneRuntime, SimulatedMesh, VoxelMap, follow_path


def robot():
    return RobotLink({"id": "atlas", "start": [0., 0.]})


class RuntimeGateTests(unittest.TestCase):
    def test_far_robot_edge_is_down_but_local_relay_remains_available(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"],
            {"hub": [0, 0], "atlas": [-4, 0], "echo": [4, 0], "iris": [0, 3]})
        edge = mesh.edges["atlas--echo"]
        self.assertTrue(edge["enabled"])
        self.assertEqual(edge["distance_m"], 8.)
        self.assertFalse(edge["up"])
        self.assertEqual(edge["reason"], "out_of_range")
        mesh.toggle("atlas--hub")
        self.assertEqual(mesh.deliver("atlas", "hub", "telemetry"), ["atlas", "iris", "hub"])
        self.assertEqual(edge["packets"], 0)

    def test_radio_range_restores_enabled_links_but_never_overrides_manual_disable(self):
        positions = {"hub": [0, 0], "atlas": [-4, 0], "echo": [4, 0], "iris": [0, 3]}
        mesh = SimulatedMesh(["atlas", "iris", "echo"], positions)
        edge = mesh.edges["atlas--echo"]
        mesh.toggle("atlas--echo")  # Disable, even though currently out of range.
        positions["echo"] = [1, 0]
        mesh.set_positions(positions)
        self.assertTrue(edge["in_range"])
        self.assertFalse(edge["up"])
        self.assertEqual(edge["reason"], "disabled")
        mesh.toggle("atlas--echo")
        self.assertTrue(edge["up"])
        positions["echo"] = [4, 0]
        mesh.set_positions(positions)
        self.assertFalse(edge["up"])
        mesh.toggle("atlas--echo"); mesh.toggle("atlas--echo")
        self.assertTrue(edge["enabled"])
        self.assertFalse(edge["up"])  # Clicking cannot force a far-away radio up.

    def test_range_break_gates_commands_telemetry_and_map_then_fences_restore(self):
        positions = {"hub": [0, 0], "atlas": [-4, 0], "echo": [4, 0], "iris": [0, 3]}
        runtime = SceneRuntime.__new__(SceneRuntime)
        runtime.links = {n: RobotLink({"id": n, "start": positions[n]}) for n in ("atlas", "iris", "echo")}
        runtime.mesh = SimulatedMesh(list(runtime.links), positions)
        runtime._routes = {n: runtime.mesh.route(n) for n in runtime.links}
        runtime.sequence = 5
        runtime._pose = lambda n: (positions[n] + [.24], [0., 0.])
        link = runtime.links["atlas"]
        positions["atlas"] = [-20, 0]
        runtime._update_radio_positions(); runtime._refresh_routes()
        self.assertFalse(link.online)
        self.assertEqual(link.telemetry([-20, 0], [0, 0])["position"], [-4, 0])
        command = {"velocity": [.3, 0], "path": [[1, 0]], "job_id": "inspect"}
        self.assertFalse(link.accept(command, 1., 6))
        runtime.mesh.maps["atlas"].observe([-20, 1, .5], "atlas", 1.)
        runtime.mesh.gossip()
        self.assertEqual(len(runtime.mesh.maps["hub"].cells), 0)
        positions["atlas"] = [-4, 0]
        runtime.sequence = 9
        runtime._update_radio_positions(); runtime._refresh_routes()
        self.assertTrue(link.online)
        self.assertFalse(link.accept(command, 2., 9))
        self.assertTrue(link.accept(command, 2., 10))

    def test_internet_starts_off_and_never_changes_local_routes_or_gossip(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"])
        self.assertFalse(mesh.snapshot()["internet"]["enabled"])
        self.assertFalse(mesh.snapshot()["internet"]["up"])
        routes = mesh.snapshot()["routes"]
        mesh.maps["atlas"].observe([1, 1, .5], "atlas", 2.)
        mesh.gossip()
        self.assertEqual(len(mesh.maps["hub"].cells), 1)
        mesh.toggle("hub--internet")
        self.assertEqual(mesh.snapshot()["routes"], routes)
        mesh.toggle("hub--internet")
        self.assertEqual(mesh.snapshot()["routes"], routes)
        self.assertEqual(mesh.internet["packets"], 0)
        with self.assertRaises(ValueError):
            mesh.route("atlas", "internet")

    def test_radio_positions_refuse_missing_or_nonfinite_nodes(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"])
        with self.assertRaises(ValueError):
            mesh.set_positions({"atlas": [0, 0]})
        with self.assertRaises(ValueError):
            mesh.set_positions({"hub": [0, 0], "atlas": [0, float("nan")], "iris": [1, 0], "echo": [2, 0]})

    def test_direct_link_loss_routes_telemetry_over_peers_and_counts_each_hop(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"])
        mesh.toggle("atlas--hub")
        route = mesh.deliver("atlas", "hub", "telemetry")
        self.assertEqual(route, ["atlas", "echo", "hub"])
        self.assertEqual(mesh.edges["atlas--echo"]["telemetry_packets"], 1)
        self.assertEqual(mesh.edges["echo--hub"]["telemetry_packets"], 1)
        self.assertEqual(mesh.edges["atlas--hub"]["packets"], 0)
        mesh.toggle("atlas--echo")
        self.assertEqual(mesh.route("atlas"), ["atlas", "iris", "hub"])
        mesh.toggle("atlas--iris")
        before = sum(e["packets"] for e in mesh.edges.values())
        self.assertEqual(mesh.deliver("atlas", "hub", "telemetry"), [])
        self.assertEqual(sum(e["packets"] for e in mesh.edges.values()), before)

    def test_peer_component_gossips_while_hub_isolated_then_reconverges(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"])
        for robot_id in ("atlas", "iris", "echo"):
            mesh.toggle(mesh.edge_id(robot_id, "hub"))
        mesh.maps["atlas"].observe([1, 1, .5], "atlas", 2.)
        mesh.maps["iris"].observe([2, 1, .5], "iris", 3.)
        mesh.gossip()
        self.assertEqual(len(mesh.maps["hub"].cells), 0)
        self.assertEqual(len(mesh.maps["echo"].cells), 2)
        self.assertEqual(mesh.maps["atlas"].snapshot(), mesh.maps["iris"].snapshot())
        mesh.toggle("echo--hub")
        mesh.gossip()
        self.assertEqual(mesh.maps["hub"].snapshot(), mesh.maps["echo"].snapshot())
        self.assertEqual([(v["source"], v["last_seen_s"]) for v in mesh.maps["hub"].snapshot()],
                         [("atlas", 2.), ("iris", 3.)])

    def test_gossip_crosses_only_one_link_per_round(self):
        mesh = SimulatedMesh(["atlas", "iris", "echo"])
        for edge in mesh.edges.values():
            edge["up"] = edge["id"] in ("atlas--echo", "echo--hub")
        mesh.maps["atlas"].observe([1, 1, .5], "atlas", 4.)
        mesh.gossip()
        self.assertEqual(len(mesh.maps["echo"].cells), 1)
        self.assertEqual(len(mesh.maps["hub"].cells), 0)
        mesh.gossip()
        self.assertEqual(mesh.maps["hub"].snapshot()[0]["last_seen_s"], 4.)

    def test_equal_time_and_capacity_merge_converges_in_reversed_order(self):
        observations = [([0, 0, 0], "atlas", 1.), ([0, 0, 0], "iris", 1.),
                        ([1, 0, 0], "atlas", 2.), ([2, 0, 0], "echo", 2.)]
        maps = [VoxelMap(2), VoxelMap(2)]
        for target, values in zip(maps, (observations, list(reversed(observations)))):
            for point, source, stamp in values:
                target.observe(point, source, stamp)
        self.assertEqual(maps[0].snapshot(), maps[1].snapshot())
        self.assertFalse(maps[0].observe([0, 0, 0], "iris", 1.))
        a, b = VoxelMap(), VoxelMap()
        a.observe([0, 0, 0], "atlas", 1.)
        b.observe([0, 0, 0], "iris", 1.)
        a.merge(b); b.merge(a)
        self.assertEqual(a.snapshot(), b.snapshot())
        self.assertEqual(a.snapshot()[0]["source"], "iris")

    def test_reconnection_fences_old_commands_and_restores_fresh_telemetry(self):
        runtime = SceneRuntime.__new__(SceneRuntime)
        runtime.links = {n: RobotLink({"id": n, "start": [0., 0.]}) for n in ("atlas", "iris", "echo")}
        runtime.mesh = SimulatedMesh(list(runtime.links))
        runtime._routes = {n: runtime.mesh.route(n) for n in runtime.links}
        runtime.data, runtime.events, runtime.sequence = Mock(time=1.), deque(), 5
        link = runtime.links["atlas"]
        command = {"velocity": [.3, 0], "path": [[1, 0]], "job_id": "inspect"}
        link.accept(command, 1., 5)
        runtime.toggle_link("atlas")
        self.assertTrue(link.online)  # Direct link is down, mesh route is usable.
        runtime.toggle_edge("atlas--echo"); runtime.toggle_edge("atlas--iris")
        self.assertFalse(link.online)
        link.telemetry([2., 0.], [.3, 0.])
        self.assertEqual(link.delivered["position"], [0., 0.])
        runtime.sequence = 9
        runtime.toggle_edge("atlas--echo")
        self.assertTrue(link.online)
        self.assertFalse(link.accept(command, 2., 9))
        self.assertEqual(link.velocity([0, 0], 2., True, True), [0., 0.])
        self.assertEqual(link.telemetry([2., 0.], [0., 0.])["position"], [2., 0.])
        self.assertTrue(link.accept(command, 2., 10))
        self.assertEqual(link.velocity([0, 0], 2., True, True), [.3, 0])

    def test_pause_resume_does_not_accept_pre_release_inflight_command(self):
        runtime = SceneRuntime.__new__(SceneRuntime)
        link = robot()
        runtime.links = {"atlas": link}
        runtime.data, runtime.events, runtime.sequence = Mock(time=1.), deque(), 5
        runtime._brake = Mock()
        command = {"velocity": [.3, 0], "path": [[1, 0]], "job_id": "old-inspection"}
        link.accept(command, 1., 5)
        runtime.toggle_available("atlas")
        runtime.sequence = 8
        runtime.toggle_available("atlas")
        self.assertFalse(link.accept(command, 2., 5))
        self.assertFalse(link.accept(command, 2., 8))
        self.assertEqual(link.velocity([0, 0], 2., True, True), [0., 0.])
        self.assertTrue(link.accept({**command, "job_id": "new-inspection"}, 2., 9))
        self.assertEqual(link.job_id, "new-inspection")

    def test_mission_deadline_starts_on_authorization_and_cannot_be_extended(self):
        runtime = SceneRuntime.__new__(SceneRuntime)
        runtime.data = Mock(time=700.)
        runtime.stopped, runtime.fault = False, None
        runtime.authorization_started_s = None
        runtime.events = deque()
        self.assertTrue(runtime.start_mission())
        self.assertFalse(runtime._expired())
        runtime.data.time = 800.
        self.assertTrue(runtime.start_mission())
        self.assertEqual(runtime.authorization_started_s, 700.)
        runtime.data.time = 1300.
        self.assertTrue(runtime._expired())
        self.assertFalse(runtime.start_mission())

    def test_crashed_coordinator_stdin_does_not_interrupt_cleanup(self):
        pipe = CoordinatorPipe.__new__(CoordinatorPipe)
        pipe.done, pipe.thread, pipe.process = Mock(), Mock(), Mock()
        pipe.process.poll.return_value = 1
        pipe.process.stdin.close.side_effect = BrokenPipeError()
        pipe.close()
        pipe.done.set.assert_called_once()
        pipe.thread.join.assert_called_once_with(timeout=1)
        pipe.process.stdout.close.assert_called_once()

    def test_cut_withholds_new_telemetry_and_pause_until_restore(self):
        link = robot()
        first = link.telemetry([1., 2.], [.2, 0.])
        link.online = False
        link.available = False
        cut = link.telemetry([2., 2.], [0., 0.])
        self.assertEqual(cut["position"], first["position"])
        self.assertEqual(cut["velocity"], first["velocity"])
        self.assertTrue(cut["available"])
        self.assertFalse(cut["online"])
        link.online = True
        self.assertFalse(link.telemetry([2., 2.], [0., 0.])["available"])
        self.assertEqual(link.stats["telemetry_dropped"], 1)

    def test_offline_commands_are_dropped_but_cached_route_continues(self):
        link = robot()
        link.accept({"velocity": [.3, 0], "path": [[0, 0], [1, 0]], "job_id": "inspect", "state": "moving"}, 5.)
        link.velocity([0, 0], 5.1, True, True)
        link.online = False
        link.accept({"velocity": [0, -.5], "path": [[0, -5]], "job_id": "wrong"}, 7.)
        self.assertEqual(link.job_id, "inspect")
        self.assertGreater(link.velocity([.2, 0], 8., True, True)[0], 0)
        self.assertEqual(link.velocity([1, 0], 9., True, True), [0., 0.])
        self.assertEqual(link.state, "awaiting_reconnect")
        self.assertEqual(link.stats["commands_dropped"], 1)

    def test_watchdog_authority_and_local_pause_override_cached_motion(self):
        link = robot()
        link.accept({"velocity": [.3, 0], "path": [[1, 0]], "job_id": "inspect"}, 5.)
        self.assertEqual(link.velocity([0, 0], 6., True, True), [0., 0.])
        link.online = False
        for active, healthy in ((False, True), (True, False)):
            self.assertEqual(link.velocity([0, 0], 6., active, healthy), [0., 0.])
        link.available = False
        self.assertEqual(link.velocity([0, 0], 6., True, True), [0., 0.])

    def test_new_process_has_no_authorized_path_to_resume(self):
        link = robot()
        link.online = False
        self.assertEqual(link.velocity([0, 0], 8., True, True), [0., 0.])
        self.assertEqual(link.job_id, "")

    def test_partitioned_map_is_buffered_then_merges_without_reaging(self):
        link, shared = robot(), VoxelMap()
        link.local_map.observe([1.1, 2.1, .6], "atlas", 1.)
        link.publish_map(shared)
        link.online = False
        link.local_map.observe([3.1, 2.1, .6], "atlas", 4.)
        link.publish_map(shared)
        self.assertEqual(len(shared.cells), 1)
        link.online = True
        link.publish_map(shared)
        self.assertEqual(len(shared.cells), 2)
        self.assertEqual(sorted(x["last_seen_s"] for x in shared.cells.values()), [1., 4.])
        self.assertEqual(shared.merge(link.local_map), 0)

    def test_voxels_are_bounded_and_older_duplicates_do_not_replace(self):
        voxels = VoxelMap(2)
        voxels.observe([0, 0, 0], "a", 2.)
        self.assertFalse(voxels.observe([0, 0, 0], "b", 1.))
        voxels.observe([1, 0, 0], "a", 3.)
        voxels.observe([2, 0, 0], "b", 4.)
        self.assertEqual(len(voxels.cells), 2)
        self.assertEqual([x["last_seen_s"] for x in voxels.cells.values()], [3., 4.])
        with self.assertRaises(ValueError):
            voxels.observe([float("nan"), 0, 0], "a", 3.)

    def test_offline_path_does_not_skip_a_distant_waypoint(self):
        velocity, cursor = follow_path([0, 0], [[0, 0], [0, 1], [1, 1]])
        self.assertEqual(cursor, 1)
        self.assertEqual(velocity[0], 0)
        self.assertGreater(velocity[1], 0)


if __name__ == "__main__":
    unittest.main()

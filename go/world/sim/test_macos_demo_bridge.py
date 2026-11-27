"""Headless native-view bridge checks; no window, device or network control."""
import json
import math
import os
from pathlib import Path
import queue
import tempfile
import threading
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from sim.macos_demo_bridge import (ACTIONS, Bridge, COMMAND_SCHEMA, MAX_COMMAND_BYTES,
    MAX_JSON_BYTES, SessionFiles, geometry, json_bytes, run)

try:
    import mujoco
except ImportError:
    mujoco = None


class LocalPipe:
    """Only coordinator I/O is replaced; MuJoCo/mesh/runtime remain actual."""
    def __init__(self, *args):
        self.requests = queue.Queue(1)
        self.process = SimpleNamespace(poll=lambda: None)
        self.error = None
        self.closed = False

    def submit(self, request):
        return False

    def poll(self):
        return []

    def close(self):
        self.closed = True


def write_command(directory, number, action="start", target="", **extra):
    value = dict(schema=COMMAND_SCHEMA, run_id="bridge-test", sequence=number,
                 action=action, target=target)
    value.update(extra)
    path = directory / "commands" / f"{number:06d}.json"
    path.write_bytes(json_bytes(value))
    path.chmod(0o600)
    return path


class FileTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory(prefix="world-bridge-")
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.files = SessionFiles(self.directory, "bridge-test")
        self.addCleanup(self.files.close)

    def test_session_marker_refuses_restart_and_preexisting_commands(self):
        with self.assertRaises(FileExistsError):
            SessionFiles(self.directory, "bridge-test")
        with tempfile.TemporaryDirectory() as other:
            path = Path(other)
            (path / "commands").mkdir(mode=0o700)
            with self.assertRaises(FileExistsError):
                SessionFiles(path, "fresh")
            self.assertTrue((path / "bridge-session.json").exists())

    def test_private_owner_paths_and_symlinks_are_refused(self):
        outside = self.directory / "outside.json"
        outside.write_text("{\"unchanged\":true}")
        outside.chmod(0o600)
        (self.directory / "commands" / "000001.json").symlink_to(outside)
        with self.assertRaises(OSError):
            self.files.command(1)
        (self.directory / "state.json").symlink_to(outside)
        with self.assertRaises(ValueError):
            self.files.write("state.json", {}, replace=True)
        self.assertEqual(json.loads(outside.read_bytes()), {"unchanged": True})
        with tempfile.TemporaryDirectory() as other:
            os.chmod(other, 0o755)
            with self.assertRaises(ValueError):
                SessionFiles(other, "test")
        link = self.directory / "directory-link"
        link.symlink_to(self.directory, target_is_directory=True)
        with self.assertRaises(OSError):
            SessionFiles(link, "test")

    def test_command_bounds_duplicate_keys_and_nonfinite_values(self):
        path = self.directory / "commands" / "000001.json"
        for raw in (b"x" * (MAX_COMMAND_BYTES + 1), b'{"a":1,"a":2}', b'{"a":NaN}', b'\xff'):
            with self.subTest(raw_length=len(raw)):
                path.write_bytes(raw)
                path.chmod(0o600)
                with self.assertRaises((ValueError, UnicodeError)):
                    self.files.command(1)
        path.write_bytes(b"{}")
        path.chmod(0o644)
        with self.assertRaises(ValueError):
            self.files.command(1)

    def test_atomic_state_replacement_is_bounded_and_complete(self):
        self.files.write("state.json", {"sequence": 0, "data": "x" * 5000})
        failures = []
        stopped = threading.Event()

        def read():
            while not stopped.is_set():
                try:
                    value = json.loads((self.directory / "state.json").read_bytes())
                    if value["data"] != "x" * 5000:
                        failures.append("partial value")
                except Exception as error:
                    failures.append(type(error).__name__)

        thread = threading.Thread(target=read)
        thread.start()
        try:
            for sequence in range(1, 51):
                self.files.write("state.json", {"sequence": sequence, "data": "x" * 5000}, replace=True)
        finally:
            stopped.set()
            thread.join(timeout=2)
        self.assertFalse(thread.is_alive())
        self.assertEqual(failures, [])
        with self.assertRaises(ValueError):
            self.files.write("state.json", {"data": "x" * MAX_JSON_BYTES}, replace=True)
        self.assertEqual(json.loads((self.directory / "state.json").read_bytes())["sequence"], 50)
        self.assertFalse(any(p.name.startswith(".bridge-") for p in self.directory.iterdir()))


@unittest.skipUnless(mujoco is not None, "requires pinned MuJoCo 4 environment")
class GeometryTests(unittest.TestCase):
    def model_runtime(self):
        model = mujoco.MjModel.from_xml_string('''<mujoco>
          <asset><material name="paint" rgba=".1 .2 .3 .4" emission=".2"/></asset>
          <worldbody>
            <geom type="plane" size="5 4 .1" rgba="0 0 0 0"/>
            <geom type="box" name="cabinet" size="1 2 3" pos="2 3 4" euler="0 0 90" material="paint"/>
            <geom type="sphere" size=".5" pos="4 0 0"/>
            <geom type="ellipsoid" size=".1 .2 .3" pos="5 0 0"/>
            <geom type="cylinder" size=".1 .2" pos="6 0 0"/>
            <geom type="capsule" size=".2 .3" pos="7 0 0"/>
            <body name="robot" pos="1 1 1"><joint type="slide" axis="1 0 0"/>
              <geom type="sphere" size=".1"/><body name="sensor" pos=".2 0 .3"><geom type="box" size=".1 .1 .1"/></body>
            </body>
          </worldbody></mujoco>''')
        data = mujoco.MjData(model)
        mujoco.mj_forward(model, data)
        robot = mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_BODY, "robot")
        return SimpleNamespace(model=model, data=data, mj=mujoco, run_id="bridge-test",
            config={"model": {"sha256": "a" * 64}}, bindings={"atlas": {"body": robot}})

    def test_compiled_shapes_world_rotation_material_and_robot_descendants(self):
        runtime = self.model_runtime()
        result, dynamic = geometry(runtime)
        self.assertEqual(result["coordinate_frame"]["up"], "Z")
        self.assertEqual({g["type"] for g in result["geoms"]},
                         {"plane", "box", "sphere", "ellipsoid", "cylinder", "capsule"})
        cabinet = next(g for g in result["geoms"] if g["name"] == "cabinet")
        self.assertEqual(cabinet["size"], [1., 2., 3.])
        self.assertEqual(cabinet["position"], [2., 3., 4.])
        self.assertAlmostEqual(cabinet["rotation"][1], -1.)
        for actual, expected in zip(cabinet["rgba"], [.1, .2, .3, .4]):
            self.assertAlmostEqual(actual, expected)
        self.assertAlmostEqual(cabinet["material"]["emission"], .2)
        self.assertEqual(cabinet["material_name"], "paint")
        self.assertEqual(len(dynamic), 2)
        self.assertTrue(all(result["geoms"][i]["robot_id"] == "atlas" for i in dynamic))
        before = result["geoms"][dynamic[-1]]["position"]
        runtime.data.qpos[0] = .75
        mujoco.mj_forward(runtime.model, runtime.data)
        after, _ = geometry(runtime)
        self.assertAlmostEqual(after["geoms"][dynamic[-1]]["position"][0] - before[0], .75)
        self.assertEqual(after["geoms"][1], cabinet)

    def test_unsupported_geometry_and_nonfinite_state_fail_closed(self):
        runtime = self.model_runtime()
        runtime.model.geom_type[0] = mujoco.mjtGeom.mjGEOM_MESH
        with self.assertRaises(ValueError):
            geometry(runtime)
        runtime.model.geom_type[0] = mujoco.mjtGeom.mjGEOM_PLANE
        runtime.data.geom_xpos[0, 0] = math.nan
        with self.assertRaises(ValueError):
            geometry(runtime)


@unittest.skipUnless(mujoco is not None, "requires pinned MuJoCo 4 environment")
class RuntimeBridgeTests(unittest.TestCase):
    def setUp(self):
        from sim.collaborative_runtime import SceneRuntime
        self.tmp = tempfile.TemporaryDirectory(prefix="world-bridge-")
        self.addCleanup(self.tmp.cleanup)
        self.directory = Path(self.tmp.name)
        self.scene = Path(__file__).resolve().parents[1] / "examples/collaborative-office/scene.json"
        self.pipe_patch = patch("sim.collaborative_runtime.CoordinatorPipe", LocalPipe)
        self.pipe_patch.start()
        self.addCleanup(self.pipe_patch.stop)
        self.runtime = SceneRuntime(self.scene, "unused", "bridge-test")
        self.addCleanup(self.runtime.close)
        self.files = SessionFiles(self.directory, "bridge-test")
        self.addCleanup(self.files.close)
        self.bridge = Bridge(self.runtime, self.files)

    def receipt(self, sequence):
        return json.loads((self.directory / "receipts" / f"{sequence:06d}.json").read_bytes())

    def test_start_explicit_order_stop_latch_and_no_replay(self):
        self.assertFalse(self.runtime.authorized)
        write_command(self.directory, 2, "stop")
        self.bridge.commands()
        self.assertEqual(self.bridge.command_sequence, 0)
        write_command(self.directory, 1)
        self.bridge.commands()
        self.assertTrue(self.receipt(1)["accepted"])
        self.assertTrue(self.receipt(2)["accepted"])
        self.assertTrue(self.runtime.stopped)
        self.assertFalse(self.runtime.authorized)
        write_command(self.directory, 3)
        self.bridge.commands()
        self.assertEqual(self.receipt(3)["error"], "start_refused")
        self.assertFalse(self.runtime.authorized)
        self.bridge.commands()
        self.assertEqual(self.bridge.command_sequence, 3)

    def test_commands_are_exact_and_rejections_have_no_effect(self):
        for i, change in enumerate(({"run_id": "another-run"}, {"sequence": True},
                                    {"surprise": 1}, {"target": "atlas"}, {"action": []}), 1):
            write_command(self.directory, i, **change)
            self.bridge.commands()
            self.assertFalse(self.receipt(i)["accepted"])
            self.assertFalse(self.runtime.authorized)
        write_command(self.directory, 6, "toggle_available", "absent")
        self.bridge.commands()
        self.assertEqual(self.receipt(6)["error"], "unknown_target")
        self.assertTrue(all(link.available for link in self.runtime.links.values()))

    def test_selected_replica_does_not_mutate_map_or_mesh(self):
        self.runtime.mesh.maps["atlas"].observe([1., 2., 3.], "atlas", 4.)
        before = self.runtime.mesh.snapshot()
        write_command(self.directory, 1, "select_map", "atlas")
        self.bridge.commands()
        state = self.bridge.export()
        self.assertEqual(state["snapshot"]["map_node"], "atlas")
        self.assertEqual(state["snapshot"]["voxels"][0]["source"], "atlas")
        self.assertEqual(self.runtime.mesh.snapshot(), before)
        self.assertEqual(self.runtime.mesh.maps["hub"].snapshot(), [])
        self.assertFalse(self.runtime.authorized)

    def test_mesh_toggle_uses_real_range_gate_and_robot_pause_brakes(self):
        far = next(edge for edge in self.runtime.mesh.edges.values() if not edge["in_range"])
        write_command(self.directory, 1, "toggle_edge", far["id"])
        write_command(self.directory, 2, "toggle_edge", far["id"])
        write_command(self.directory, 3, "toggle_available", "atlas")
        self.runtime.data.qvel[:] = .4
        self.bridge.commands()
        self.assertTrue(far["enabled"])
        self.assertFalse(far["up"])
        self.assertFalse(self.runtime.links["atlas"].available)
        self.assertEqual(self.runtime.data.qvel[self.runtime.bindings["atlas"]["dofs"]].tolist(), [0., 0.])

    def test_invalid_file_is_consumed_once_and_mutation_exception_stops(self):
        path = self.directory / "commands" / "000001.json"
        path.write_bytes(b"{")
        path.chmod(0o600)
        self.bridge.commands()
        self.assertEqual(self.receipt(1)["error"], "invalid_command")
        write_command(self.directory, 2, "toggle_available", "atlas")
        with patch.object(self.runtime, "toggle_available", side_effect=RuntimeError("unknown")):
            with self.assertRaises(RuntimeError):
                self.bridge.commands()
        self.assertEqual(self.receipt(2)["error"], "unknown_outcome")
        self.assertTrue(self.runtime.stopped)
        self.assertFalse(self.runtime.authorized)

    def test_owned_loop_duration_stop_and_failure_close_coordinator(self):
        from sim.collaborative_runtime import SceneRuntime
        for reason in ("duration", "stop_file", "runtime_fault"):
            with self.subTest(reason=reason), tempfile.TemporaryDirectory() as output:
                target = Path(output)
                instances = []

                def factory(*args):
                    instance = SceneRuntime(*args)
                    instances.append(instance)
                    if reason == "stop_file":
                        (target / "stop").touch(mode=0o600)
                    if reason == "runtime_fault":
                        instance.fault = "test fault"
                    return instance

                code = run(self.scene, "unused", target, "bridge-test", .04, runtime_factory=factory)
                self.assertEqual(code, 1 if reason == "runtime_fault" else 0)
                self.assertTrue(instances[0].closed)
                self.assertTrue(instances[0].transport.closed)
                result = json.loads((target / "bridge-result.json").read_bytes())
                state = json.loads((target / "final-state.json").read_bytes())
                self.assertEqual(result["exit_reason"], reason)
                self.assertTrue(result["coordinator_closed"])
                self.assertEqual(state["worker_status"], "closed")
                self.assertFalse(state["snapshot"]["authorized"])
                self.assertTrue(state["snapshot"]["stopped"])
                with self.assertRaises(FileExistsError):
                    run(self.scene, "unused", target, "bridge-test", .01, runtime_factory=factory)


if __name__ == "__main__":
    unittest.main()

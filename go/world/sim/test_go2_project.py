"""Private static-project conversion; native cases require the bounded Linux worker."""
import copy
import hashlib
import importlib.util
import json
import math
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from sim import go2_project as converter

ROOT = Path(__file__).resolve().parents[1]
EXAMPLES = ROOT / "examples" / "go2-worlds"
NATIVE = sys.platform.startswith("linux") and importlib.util.find_spec("mujoco") is not None


class ContractTests(unittest.TestCase):
    def test_selection_is_explicit_unique_and_bounded(self):
        for names in ([], ["x", "x"], ["x\n"], ["a" * 257], list(map(str, range(65)))):
            with self.subTest(names=names[:2]):
                with self.assertRaises(converter.ConversionError):
                    converter.request("world.json", names, "run")
        for run in ("", "../run", "r\n", "r" * 129):
            with self.assertRaises(converter.ConversionError):
                converter.request("world.json", ["x"], run)
        self.assertEqual(converter.request("world.json", ["b", "a"], "run")["names"], ["a", "b"])

    def test_unsupported_host_never_imports_native(self):
        with mock.patch.object(converter.sys, "platform", "darwin"), mock.patch.object(converter.subprocess, "Popen") as popen:
            with self.assertRaisesRegex(converter.ConversionError, "Linux"):
                converter.convert("world.json", ["x"], "run")
            popen.assert_not_called()

    def test_timeout_kills_and_reaps_without_unbounded_capture(self):
        child = mock.Mock(pid=12345)
        child.wait.side_effect = [subprocess.TimeoutExpired("worker", 15), None]
        child.poll.return_value = None
        with mock.patch.object(converter.sys, "platform", "linux"), mock.patch.object(converter.subprocess, "Popen", return_value=child) as popen, mock.patch.object(converter.os, "killpg") as kill:
            with self.assertRaisesRegex(converter.ConversionError, "wall deadline"):
                converter.convert("world.json", ["x"], "run")
            self.assertEqual(child.wait.call_count, 2)
            kill.assert_called_once_with(12345, converter.signal.SIGKILL)
            kwargs = popen.call_args.kwargs
            self.assertTrue(kwargs["start_new_session"])
            self.assertNotEqual(kwargs["stdout"], subprocess.PIPE)
            self.assertEqual(set(kwargs["env"]), {"PATH", "LANG", "OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS"})
            self.assertEqual(popen.call_args.args[0][1], "-I")
            self.assertEqual(kwargs["cwd"], str(ROOT))

    def test_parent_interruption_also_reaps(self):
        child = mock.Mock(pid=12345)
        child.wait.side_effect = [KeyboardInterrupt(), None]
        child.poll.return_value = None
        with mock.patch.object(converter.sys, "platform", "linux"), mock.patch.object(converter.subprocess, "Popen", return_value=child), mock.patch.object(converter.os, "killpg") as kill:
            with self.assertRaises(KeyboardInterrupt):
                converter.convert("world.json", ["x"], "run")
            self.assertEqual(child.wait.call_count, 2)
            kill.assert_called_once()

    def test_output_creation_never_overwrites_or_follows_existing_target(self):
        result = {"scene": {"version": 1}, "report": {"schema": converter.SCHEMA}}
        with tempfile.TemporaryDirectory() as tmp:
            dest = Path(tmp).resolve() / "out"
            converter.install(result, dest)
            original = (dest / "project/scene.json").read_bytes()
            self.assertEqual((dest / "project/scene.json").stat().st_mode & 0o777, 0o600)
            self.assertEqual((dest / "conversion.json").stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                converter.install(result, dest)
            self.assertEqual((dest / "project/scene.json").read_bytes(), original)
            link = Path(tmp).resolve() / "link"
            link.symlink_to(dest, target_is_directory=True)
            with self.assertRaises(FileExistsError):
                converter.install(result, link)
            with self.assertRaisesRegex(converter.ConversionError, "symlink"):
                converter.install(result, link / "nested")

    def test_examples_have_valid_closed_digests(self):
        from sim.run import load_package
        for scene in ("corridor", "islands"):
            loaded = load_package(EXAMPLES / scene / "world.json")
            self.assertEqual(loaded["provenance"], "synthetic")
            self.assertEqual(loaded["_assets"], {})
            self.assertEqual(loaded["_digest"], converter.digest((EXAMPLES / scene / "world.json").read_bytes()))


@unittest.skipUnless(NATIVE, "requires Linux bounded worker and pinned MuJoCo; run in go2-project image")
class NativeConversionTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.package = Path(self.temp.name).resolve() / "source"
        shutil.copytree(EXAMPLES / "corridor", self.package)

    def convert(self, names=None):
        return converter.convert(self.package / "world.json", names or ["corridor_wall_a", "corridor_wall_b"], "conversion-test")

    def replace_xml(self, old, new):
        p = self.package / "scene.xml"
        raw = p.read_text().replace(old, new).encode()
        p.write_bytes(raw)
        manifest_path = self.package / "world.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["mujoco"]["sha256"] = hashlib.sha256(raw).hexdigest()
        manifest_path.write_text(json.dumps(manifest))

    def test_compiled_body_transform_stays_engine_z_up(self):
        result = self.convert()
        a = result["scene"]["geoms"][0]
        angle = math.radians(15)
        for actual, expected in zip(a["position"], [-3 + 1.6 * math.sin(angle), -1.6 * math.cos(angle), 1]):
            self.assertAlmostEqual(actual, expected, places=10)
        for actual, expected in zip(a["quaternion"], [math.cos(angle / 2), 0, 0, math.sin(angle / 2)]):
            self.assertAlmostEqual(actual, expected, places=10)
        report = result["report"]
        self.assertEqual(report["compiler"]["version"], "4.0.0")
        self.assertEqual(report["source"]["model_to_local_position"], ["x", "z", "-y"])
        self.assertEqual(report["target"]["coordinates"]["up"], "Z")
        self.assertEqual(report["source"]["local_frame"]["up"], "Y")
        self.assertEqual(report["source"]["run_id"], "conversion-test")
        self.assertEqual(report["source"]["frame_revision"], "corridor-frame-v1")

    def test_two_scenes_exact_snapshot_digest_and_explicit_losses(self):
        revisions = set()
        for scene in ("corridor", "islands"):
            folder = EXAMPLES / scene
            names = json.loads((folder / "selection.json").read_text())["geom_names"]
            result = converter.convert(folder / "world.json", names, "conversion-test")
            raw = converter.encoded(result["scene"])
            report = result["report"]
            revisions.add(report["target"]["project_revision"])
            self.assertEqual(report["target"]["project_revision"], converter.digest(b"scene.json\0" + raw + b"\0"))
            self.assertEqual(report["target"]["files"], [{"path": "scene.json", "size": len(raw), "sha256": converter.digest(raw)}])
            self.assertEqual({v["source_geom_name"] for v in report["selection"]}, set(names))
            self.assertEqual({v["name"] for v in report["exclusions"]["geoms"]}, {"source_floor", "source_robot"})
            self.assertEqual(report["exclusions"]["actuators"][0]["name"], "source_drive")
            self.assertEqual(report["exclusions"]["sensors"][0]["name"], "source_position")
            self.assertEqual(report["exclusions"]["cameras"][0]["name"], "source_camera")
            self.assertEqual(len(report["selected_property_differences"]), len(names))
            self.assertFalse(report["operational"])
            self.assertIn("sandbox", " ".join(report["differences"]))
            self.assertLess(len(converter.encoded(result)), converter.MAX_RESULT)
        self.assertEqual(len(revisions), 2)

    def test_plane_and_dynamic_geometry_cannot_be_flattened(self):
        for name, reason in (("source_floor", "unsupported"), ("source_robot", "moving"), ("missing", "not found")):
            with self.subTest(name=name), self.assertRaisesRegex(converter.ConversionError, reason):
                self.convert([name])

    def test_joint_ancestor_and_mocap_ancestor_refused(self):
        for body in ('<body name="parent"><joint name="free" type="free"/><geom type="sphere" size=".1" mass="1"/>',
                     '<body name="parent" mocap="true">'):
            with self.subTest(body=body):
                original = (self.package / "scene.xml").read_text()
                self.replace_xml('<body name="corridor_mount"', body + '<body name="corridor_mount"')
                self.replace_xml('</body><body name="source_puck"', '</body></body><body name="source_puck"')
                with self.assertRaisesRegex(converter.ConversionError, "moving or mocap"):
                    self.convert()
                p = self.package / "scene.xml"
                self.replace_xml(p.read_text(), original)

    def test_spawn_and_size_refusal(self):
        self.replace_xml('pos="-3 0 0"', 'pos="0 0 0"')
        with self.assertRaisesRegex(converter.ConversionError, "spawn"):
            self.convert()
        self.replace_xml('pos="0 0 0"', 'pos="-3 0 0"')
        self.replace_xml('size="1.5 .12 1"', 'size="4.1 .12 1"')
        with self.assertRaisesRegex(converter.ConversionError, "bounds"):
            self.convert()

    def test_tamper_and_wrong_frame_never_compile_into_result(self):
        path = self.package / "scene.xml"
        path.write_bytes(path.read_bytes() + b" ")
        with self.assertRaisesRegex(converter.ConversionError, "digest"):
            self.convert()
        manifest_path = self.package / "world.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["local_frame"]["up"] = "Z"
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(converter.ConversionError, "frame"):
            self.convert()

    def test_selected_mesh_rejected_even_with_verified_asset(self):
        mesh = b"v 0 0 0\nv 1 0 0\nv 0 1 0\nv 0 0 1\nf 1 3 2\nf 1 2 4\nf 1 4 3\nf 2 3 4\n"
        (self.package / "mesh.obj").write_bytes(mesh)
        self.replace_xml('<worldbody>', '<asset><mesh name="test_mesh" file="mesh.obj"/></asset><worldbody>')
        self.replace_xml('type="box"', 'type="mesh" mesh="test_mesh"')
        manifest_path = self.package / "world.json"
        manifest = json.loads(manifest_path.read_text())
        manifest["assets"] = [{"path": "mesh.obj", "size": len(mesh), "sha256": converter.digest(mesh)}]
        manifest_path.write_text(json.dumps(manifest))
        with self.assertRaisesRegex(converter.ConversionError, "unsupported"):
            self.convert()

    def test_repeat_conversion_is_byte_stable_and_output_install_is_exact(self):
        first, second = self.convert(), self.convert(["corridor_wall_b", "corridor_wall_a"])
        self.assertEqual(converter.encoded(first), converter.encoded(second))
        output = Path(self.temp.name).resolve() / "converted"
        converter.install(first, output)
        self.assertEqual((output / "project/scene.json").read_bytes(), converter.encoded(first["scene"]))
        self.assertEqual((output / "conversion.json").read_bytes(), converter.encoded(first["report"]))
        self.assertEqual({p.name for p in (output / "project").iterdir()}, {"scene.json"})


if __name__ == "__main__":
    unittest.main()

"""Closed manifest inventory and verified in-memory MuJoCo import tests."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import tempfile
import unittest

from sim.run import PackageError, compile_model, load_package, observations

FIXTURE = Path(__file__).resolve().parents[1] / "examples" / "asset-office"


class _AssetFixture(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name) / "package"
        shutil.copytree(FIXTURE, self.root)
        self.manifest = json.loads((self.root / "world.json").read_text())

    def load(self):
        (self.root / "world.json").write_text(json.dumps(self.manifest))
        return load_package(self.root / "world.json")

    def replace(self, path, value):
        data = value.encode() if isinstance(value, str) else value
        (self.root / path).parent.mkdir(parents=True, exist_ok=True)
        (self.root / path).write_bytes(data)
        digest = hashlib.sha256(data).hexdigest()
        if path == self.manifest["mujoco"]["path"]:
            self.manifest["mujoco"]["sha256"] = digest
        else:
            self.manifest["assets"] = [a for a in self.manifest["assets"] if a["path"] != path]
            self.manifest["assets"].append({"path": path, "sha256": digest, "size": len(data)})


class AssetPackageTests(_AssetFixture):
    def test_inventory_and_import_report(self):
        package = self.load()
        self.assertEqual(package["_import_report"]["verified_files"], 3)
        self.assertEqual(package["_import_report"]["included_files"], ["models/parts/room.xml"])
        self.assertEqual(package["_import_report"]["unused_declared_assets"], [])
        self.assertNotIn("<include", package["_model_xml"])
        self.assertNotIn("meshdir", package["_model_xml"])
        self.assertEqual(list(package["_assets"]), ["wendy_asset_0.obj"])

    def test_missing_corrupt_and_wrong_size_assets(self):
        path = self.root / "models/meshes/desk.obj"
        original = path.read_bytes()
        path.write_bytes(original + b"# changed\n")
        with self.assertRaisesRegex(PackageError, "size"):
            self.load()
        path.write_bytes(bytes(len(original)))
        with self.assertRaisesRegex(PackageError, "digest"):
            self.load()
        path.unlink()
        with self.assertRaisesRegex(PackageError, "unavailable"):
            self.load()

    def test_undeclared_dependency_and_include_cycles(self):
        original = copy.deepcopy(self.manifest)
        self.manifest["assets"].pop()
        with self.assertRaisesRegex(PackageError, "undeclared"):
            self.load()
        self.manifest = original
        self.replace("models/parts/room.xml", '<mujocoinclude><include file="parts/room.xml"/></mujocoinclude>')
        with self.assertRaisesRegex(PackageError, "cyclic"):
            self.load()

    def test_paths_collisions_and_symlink_directories(self):
        original = copy.deepcopy(self.manifest)
        for path in ("../secret", "a/../secret", "/etc/passwd", "a\\b", "https:thing", ".hidden", "CON.obj", "world.json", "models/office.xml/extra", "Models/Office.xml", "Models/different.xml", "a" * 129, "/".join(["a"] * 33)):
            with self.subTest(path=path):
                self.manifest = copy.deepcopy(original)
                self.manifest["assets"][0]["path"] = path
                with self.assertRaises(PackageError):
                    self.load()
        self.manifest = original
        directory = self.root / "models/meshes"
        saved = self.root / "outside"
        directory.rename(saved)
        directory.symlink_to(saved, target_is_directory=True)
        with self.assertRaisesRegex(PackageError, "symlink/path escapes"):
            self.load()

    def test_nonregular_assets_do_not_block(self):
        path = self.root / "models/meshes/desk.obj"
        path.unlink()
        os.mkfifo(path)
        with self.assertRaisesRegex(PackageError, "regular file"):
            self.load()

    def test_plugins_entities_and_unsupported_model_refs_fail_closed(self):
        for fragment in ('<extension><plugin plugin="secret"/></extension>', '<asset><model file="other.xml"/></asset>', '<compiler strippath="true"/>', '<!DOCTYPE x [<!ENTITY x "expanded">]>'):
            with self.subTest(fragment=fragment):
                self.replace("models/office.xml", '<mujoco>' + fragment + '</mujoco>')
                with self.assertRaises(PackageError):
                    self.load()

    def test_encoded_xml_cannot_bypass_entity_rejection(self):
        xml = '<?xml version="1.0" encoding="utf-16"?><!DOCTYPE mujoco [<!ENTITY custom "resolved">]><mujoco model="&custom;"/>'
        for encoding in ("utf-16", "utf-16-le", "utf-16-be", "utf-32"):
            with self.subTest(encoding=encoding):
                self.replace("models/office.xml", xml.encode(encoding))
                with self.assertRaises(PackageError):
                    self.load()

    def test_directory_attributes_resolve_from_main_model(self):
        model = (self.root / "models/office.xml").read_text()
        self.replace("models/office.xml", model.replace('meshdir="meshes"', 'assetdir="unused" meshdir="meshes/"'))
        package = self.load()
        self.assertEqual(package["_import_report"]["asset_bindings"][0]["path"], "models/meshes/desk.obj")
        self.replace("models/office.xml", model.replace('meshdir="meshes"', 'meshdir="../outside"'))
        with self.assertRaises(PackageError):
            self.load()

    def test_duplicate_json_and_null_assets_rejected(self):
        raw = (self.root / "world.json").read_text()
        (self.root / "world.json").write_text(raw.replace('"schema_version":', '"assets": [], "schema_version":', 1))
        with self.assertRaisesRegex(PackageError, "duplicate"):
            load_package(self.root / "world.json")
        (self.root / "world.json").write_text(raw.replace('"schema_version":', '"extra": 1e309, "schema_version":', 1))
        with self.assertRaisesRegex(PackageError, "non-finite"):
            load_package(self.root / "world.json")
        self.manifest["assets"] = None
        with self.assertRaises(PackageError):
            self.load()


@unittest.skipUnless(importlib.util.find_spec("mujoco"), "requires MuJoCo")
class AssetPhysicsTests(_AssetFixture):
    def compile_pair(self):
        import mujoco
        import numpy as np
        package = self.load()
        direct = mujoco.MjModel.from_xml_path(str(self.root / self.manifest["mujoco"]["path"]))
        verified = compile_model(package, mujoco)
        for name in ("nbody", "ngeom", "nmesh", "nhfield", "nskin", "nq", "nu", "ntex"):
            self.assertEqual(getattr(direct, name), getattr(verified, name), name)
        for name in ("body_pos", "body_quat", "body_mass", "geom_pos", "geom_size", "mesh_vert", "mesh_face", "hfield_data", "skin_vert", "skin_face", "actuator_ctrlrange", "actuator_gear"):
            np.testing.assert_array_equal(getattr(direct, name), getattr(verified, name), err_msg=name)
        direct_data, verified_data = mujoco.MjData(direct), mujoco.MjData(verified)
        direct_data.ctrl[:] = verified_data.ctrl[:] = self.manifest["mujoco"]["control"]
        for _ in range(100):
            mujoco.mj_step(direct, direct_data)
            mujoco.mj_step(verified, verified_data)
        np.testing.assert_array_equal(direct_data.qpos, verified_data.qpos)
        np.testing.assert_array_equal(direct_data.qvel, verified_data.qvel)
        return package, verified

    def test_import_matches_native_compiler_and_trajectory(self):
        self.compile_pair()

    def test_height_field_and_skin_directory_parity(self):
        import struct
        hfield = struct.pack("<ii4f", 2, 2, 0, 0.1, 0.2, 0.3)
        skin = struct.pack("<4i9f3i", 3, 0, 1, 1, 0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 1, 2)
        skin += struct.pack("<40s7fi3i3f", b"inspection-puck", 0, 0, 0, 1, 0, 0, 0, 3, 0, 1, 2, 1, 1, 1)
        self.replace("models/meshes/floor.bin", hfield)
        self.replace("models/meshes/cloth.skn", skin)
        main = (self.root / "models/office.xml").read_text()
        self.replace("models/office.xml", main.replace('meshdir="meshes"', 'assetdir="meshes"'))
        room = (self.root / "models/parts/room.xml").read_text()
        room = room.replace('<asset>', '<asset><hfield name="terrain" file="floor.bin" size="1 1 .1 .1"/>')
        room = room.replace('</mujocoinclude>', '<deformable><skin name="cloth" file="cloth.skn"/></deformable></mujocoinclude>')
        self.replace("models/parts/room.xml", room)
        package, model = self.compile_pair()
        self.assertEqual((model.nhfield, model.nskin), (1, 1))
        self.assertEqual({b["path"] for b in package["_import_report"]["asset_bindings"]}, {"models/meshes/desk.obj", "models/meshes/floor.bin", "models/meshes/cloth.skn"})
        # Without assetdir the native parser falls back to the include's
        # directory for skin/hfield, even when meshdir contains these files.
        self.replace("models/office.xml", main)
        self.replace("models/parts/floor.bin", hfield)
        self.replace("models/parts/cloth.skn", skin)
        package, _ = self.compile_pair()
        self.assertEqual({b["path"] for b in package["_import_report"]["asset_bindings"]}, {"models/meshes/desk.obj", "models/parts/floor.bin", "models/parts/cloth.skn"})

    def test_nested_include_and_mesh_fallback_match_native(self):
        room = (self.root / "models/parts/room.xml").read_text()
        self.replace("models/parts/room.xml", '<mujocoinclude><include file="nested/inner.xml"/></mujocoinclude>')
        self.replace("models/parts/nested/inner.xml", room.replace('file="desk.obj"', 'file="local.obj"'))
        self.replace("models/parts/nested/local.obj", (self.root / "models/meshes/desk.obj").read_bytes())
        package, _ = self.compile_pair()
        self.assertEqual(package["_import_report"]["asset_bindings"][0]["path"], "models/parts/nested/local.obj")

    def test_repeated_asset_references_share_vfs_bytes(self):
        room = (self.root / "models/parts/room.xml").read_text().replace('<asset>', '<asset><mesh name="second" file="desk.obj"/>')
        self.replace("models/parts/room.xml", room)
        package, model = self.compile_pair()
        self.assertEqual(model.nmesh, 2)
        self.assertEqual(len(package["_assets"]), 1)
        self.assertEqual(len(package["_import_report"]["asset_bindings"]), 2)

    def test_compiles_and_runs_after_original_files_are_removed(self):
        package = self.load()
        shutil.rmtree(self.root)
        result = list(observations(package, steps=100, every=10, seed=7, run_id="asset-test"))
        self.assertEqual(len(result), 20)
        self.assertGreater(result[-2]["value"]["position_m"][0], result[0]["value"]["position_m"][0])
        self.assertEqual(result[0]["provenance"]["package_sha256"], package["_digest"])

    def test_duplicate_basenames_are_unambiguous_in_vfs(self):
        import mujoco
        mesh = (self.root / "models/meshes/desk.obj").read_bytes()
        self.replace("models/meshes/one/shape.obj", mesh)
        self.replace("models/meshes/two/shape.obj", mesh)
        room = (self.root / "models/parts/room.xml").read_text()
        room = room.replace('<asset>', '<asset><mesh name="second" file="two/shape.obj"/>').replace('file="desk.obj"', 'file="one/shape.obj"')
        self.replace("models/parts/room.xml", room)
        package = self.load()
        model = compile_model(package, mujoco)
        self.assertEqual(model.nmesh, 2)

    def test_texture_directory_and_verified_png_compile(self):
        import struct
        import zlib
        import mujoco
        def chunk(kind, data):
            return struct.pack(">I", len(data)) + kind + data + struct.pack(">I", zlib.crc32(kind + data))
        png = bytes([137, 80, 78, 71, 13, 10, 26, 10])
        png += chunk(b"IHDR", struct.pack(">IIBBBBB", 1, 1, 8, 2, 0, 0, 0))
        png += chunk(b"IDAT", zlib.compress(bytes([0, 255, 64, 0]))) + chunk(b"IEND", b"")
        self.replace("models/textures/orange.png", png)
        main = (self.root / "models/office.xml").read_text().replace('meshdir="meshes"', 'assetdir="wrong" meshdir="meshes" texturedir="textures"')
        self.replace("models/office.xml", main)
        room = (self.root / "models/parts/room.xml").read_text().replace('<asset>', '<asset><texture name="orange" type="2d" file="orange.png"/>')
        self.replace("models/parts/room.xml", room)
        package = self.load()
        model = compile_model(package, mujoco)
        self.assertEqual(model.ntex, 1)

    def test_implicit_asset_name_survives_aliasing(self):
        import mujoco
        room = (self.root / "models/parts/room.xml").read_text().replace('name="desk-mesh" ', '').replace('mesh="desk-mesh"', 'mesh="desk"')
        self.replace("models/parts/room.xml", room)
        package = self.load()
        model = compile_model(package, mujoco)
        self.assertGreaterEqual(mujoco.mj_name2id(model, mujoco.mjtObj.mjOBJ_MESH, "desk"), 0)


if __name__ == "__main__":
    unittest.main()

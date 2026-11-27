import copy
import hashlib
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

from sim.run import PackageError, load_package, observations


FIXTURE = Path(__file__).resolve().parents[1] / "examples" / "office"


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.manifest = json.loads((FIXTURE / "world.json").read_text())
        (self.root / "office.xml").write_bytes((FIXTURE / "office.xml").read_bytes())

    def load(self):
        path = self.root / "world.json"
        path.write_text(json.dumps(self.manifest))
        return load_package(path)

    def test_valid_fixture(self):
        package = self.load()
        self.assertEqual(package["provenance"], "synthetic")
        self.assertEqual(len(package["_digest"]), 64)

    def test_escape_paths(self):
        for path in ("../office.xml", "/tmp/office.xml"):
            with self.subTest(path=path):
                self.manifest["mujoco"]["path"] = path
                with self.assertRaisesRegex(PackageError, "escapes"):
                    self.load()

    def test_symlink_escape(self):
        (self.root / "outside.xml").symlink_to(FIXTURE / "office.xml")
        self.manifest["mujoco"]["path"] = "outside.xml"
        with self.assertRaisesRegex(PackageError, "escapes"):
            self.load()

    def test_digest_tampering(self):
        (self.root / "office.xml").write_text("<mujoco/>")
        with self.assertRaisesRegex(PackageError, "digest"):
            self.load()

    def test_includes_files_and_plugins_rejected_even_with_valid_digest(self):
        for xml in ('<mujoco><include file="../secret.xml"/></mujoco>', '<mujoco><asset><mesh file="secret.stl"/></asset></mujoco>', '<mujoco><extension><plugin plugin="custom"/></extension></mujoco>'):
            with self.subTest(xml=xml):
                (self.root / "office.xml").write_text(xml)
                self.manifest["mujoco"]["sha256"] = hashlib.sha256(xml.encode()).hexdigest()
                with self.assertRaisesRegex(PackageError, "not supported"):
                    self.load()

    def test_geography_and_gas_reject_invalid_numbers(self):
        original = copy.deepcopy(self.manifest)
        cases = [("geographic_anchor", "latitude_deg", 91), ("geographic_anchor", "longitude_deg", float("nan")), ("gas_model", "sigma_m", 0), ("gas_model", "noise_stddev_ppm", -1), ("gas_model", "peak_ppm", True)]
        for section, field, value in cases:
            with self.subTest(field=field):
                self.manifest = copy.deepcopy(original)
                self.manifest[section][field] = value
                with self.assertRaises(PackageError):
                    self.load()

    def test_frames_and_provenance_cannot_be_implicit(self):
        original = copy.deepcopy(self.manifest)
        for mutate in (lambda p: p.pop("frame_revision"), lambda p: p.update(provenance="measured"), lambda p: p["local_frame"].update(up="Z"), lambda p: p["mujoco"].update(to_local_position=["x", "y", "z"])):
            self.manifest = copy.deepcopy(original)
            mutate(self.manifest)
            with self.assertRaises(PackageError):
                self.load()

    def test_unknown_schema_rejected(self):
        self.manifest["schema_version"] = "wendy.world.package.v100"
        with self.assertRaisesRegex(PackageError, "unsupported"):
            self.load()

    def test_invalid_run_parameters_rejected_before_mujoco(self):
        package = self.load()
        for kwargs in ({"steps": 0}, {"steps": 1_000_001}, {"steps": True}, {"every": 0}, {"seed": -1}, {"run_id": "../bad"}):
            with self.subTest(kwargs=kwargs), self.assertRaises(PackageError):
                list(observations(package, **kwargs))


@unittest.skipUnless(importlib.util.find_spec("mujoco"), "install sim/requirements.txt to run MuJoCo checks")
class MujocoTests(unittest.TestCase):
    def test_repeatable_real_physics_and_explicit_provenance(self):
        package = load_package(FIXTURE / "world.json")
        first = list(observations(package, steps=100, seed=7, run_id="test-run"))
        second = list(observations(package, steps=100, seed=7, run_id="test-run"))
        self.assertEqual(first, second)
        self.assertEqual(len(first), 20)
        self.assertEqual(first[-1]["timing"]["sample_ns"], 1_000_000_000)
        self.assertGreater(first[-2]["value"]["position_m"][0], first[0]["value"]["position_m"][0])
        self.assertAlmostEqual(first[0]["value"]["position_m"][1], 0.2)
        for item in first:
            self.assertEqual(item["provenance"]["mode"], "simulated")
            self.assertEqual(item["timing"]["clock_domain"], "simulation:test-run")
            self.assertNotIn("sample_unix_ns", item["timing"])
            json.dumps(item, allow_nan=False)

    def test_seed_changes_gas_noise_without_changing_pose(self):
        package = load_package(FIXTURE / "world.json")
        a = list(observations(package, steps=10, seed=1))
        b = list(observations(package, steps=10, seed=2))
        self.assertEqual(a[0]["value"], b[0]["value"])
        self.assertNotEqual(a[1]["value"]["concentration"], b[1]["value"]["concentration"])

    def test_final_partial_interval_is_emitted(self):
        result = list(observations(load_package(FIXTURE / "world.json"), steps=11, every=10))
        self.assertEqual(len(result), 4)
        self.assertEqual(result[-1]["timing"]["sample_ns"], 110_000_000)

    def test_missing_model_bindings_are_rejected(self):
        package = load_package(FIXTURE / "world.json")
        package["mujoco"]["body"] = "missing-body"
        with self.assertRaisesRegex(PackageError, "does not exist"):
            list(observations(package))


if __name__ == "__main__":
    unittest.main()

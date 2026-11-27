"""Generic model admission, native execution and bounded publication regressions."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest
from unittest import mock

from sim import package_runner as runner
from sim.run import PackageError, load_model_package, load_package

ROOT = Path(__file__).resolve().parents[1]
NATIVE4 = False
if importlib.util.find_spec("mujoco") is not None:
    import mujoco
    NATIVE4 = mujoco.__version__ == "4.0.0"

PASSIVE = '<mujoco><worldbody><body name="ball" pos="0 0 1"><freejoint/><geom type="sphere" size=".1"/></body></worldbody></mujoco>'
MULTI = '''<mujoco><option timestep=".002" gravity="0 0 0"/><worldbody>
<body name="one"><joint name="x" type="slide" axis="1 0 0"/><geom type="sphere" size=".1"/>
<body name="two"><joint name="y" type="slide" axis="0 1 0"/><geom type="sphere" size=".1"/></body></body></worldbody>
<actuator><motor name="mx" joint="x" ctrllimited="true" ctrlrange="-1 1"/><motor name="my" joint="y" ctrllimited="true" ctrlrange="-2 2"/></actuator>
<sensor><jointpos name="xpos" joint="x"/><jointvel name="yvel" joint="y"/></sensor></mujoco>'''


def make_package(folder, xml=PASSIVE):
    original = json.loads((ROOT / "examples/asset-office/world.json").read_text())
    original["schema_version"] = "wendy.world.package.v1"
    original["mujoco"] = {"path": "model.xml", "sha256": hashlib.sha256(xml.encode()).hexdigest(), "to_local_position": ["x", "z", "-y"]}
    original.pop("assets", None); original.pop("gas_model", None)
    folder.mkdir(exist_ok=True)
    (folder / "model.xml").write_text(xml)
    (folder / "world.json").write_bytes(runner.canonical_bytes(original))
    return folder / "world.json"


def request(path):
    return {"schema_version": runner.REQUEST_SCHEMA, "package_sha256": hashlib.sha256(path.read_bytes()).hexdigest(),
            "run_id": "generic-test", "steps": 20, "sample_every": 5, "body_names": [], "sensor_names": [],
            "initial_state": "model-default", "control": {"mode": "zero"}, "resource_policy": runner.POLICY}


class PackageRunnerTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.folder = Path(self.tmp.name)
        self.path = make_package(self.folder / "package")
        self.request = request(self.path)

    def test_passive_generic_load_and_legacy_adapter_refusal(self):
        package = load_model_package(self.path)
        self.assertNotIn("gas_model", package)
        self.assertNotIn("actuator", package["mujoco"])
        with self.assertRaisesRegex(PackageError, "explicit package runner"):
            load_package(self.path)
        legacy = load_package(ROOT / "examples/asset-office/world.json")
        self.assertEqual(legacy["mujoco"]["control"], .5)

    def test_manifest_strict_version_and_optional_metadata(self):
        raw = self.path.read_bytes(); manifest = json.loads(raw)
        for change in ({"extra": 1}, {"gas_model": {}}, {"schema_version": "wendy.world.package.v2"}, {"assets": None}):
            with self.subTest(change=change):
                self.path.write_bytes(runner.encoded({**manifest, **change}))
                with self.assertRaises(PackageError): load_model_package(self.path)
        manifest["fidelity"] = None
        self.path.write_bytes(runner.encoded(manifest))
        self.assertIsNone(load_model_package(self.path)["fidelity"])

    def test_request_bounds_duplicates_types_and_owned_copy(self):
        validated = runner.validate_request(self.request)
        validated["body_names"].append("ball")
        self.assertEqual(self.request["body_names"], [])
        variants = [{"steps": True}, {"sample_every": 0}, {"steps": 10001}, {"steps": 1000, "sample_every": 1},
                    {"body_names": ["a", "a"]}, {"sensor_names": ["x"] * 33}, {"initial_state": "keyframe"},
                    {"resource_policy": "unlimited"}, {"package_sha256": "A" * 64}, {"run_id": "../../escape"},
                    {"worker": "/bin/sh"}, {"control": {"mode": "zero", "values": []}},
                    {"control": {"mode": "fixed", "values": [{"actuator": "x", "value": True}]}},
                    {"control": {"mode": "fixed", "values": [{"actuator": "x", "value": float("nan")}]}},
                    {"control": {"mode": "fixed", "values": [{"actuator": "x", "value": 0}] * 2}}]
        for change in variants:
            with self.subTest(change=change), self.assertRaises(runner.RunnerError):
                runner.validate_request({**self.request, **change})
        raw = runner.encoded(self.request)
        self.assertEqual(runner.validate_request(raw), self.request)
        with self.assertRaises(runner.RunnerError): runner.validate_request(raw[:-2] + b',"steps":2}\n')

    @unittest.skipUnless(NATIVE4, "requires MuJoCo 4.0.0")
    def test_native_passive_default_quaternion_and_removed_source(self):
        from sim.package_worker import execute
        package = load_model_package(self.path)
        shutil.rmtree(self.path.parent)
        self.request["body_names"] = ["ball"]
        result = execute(package, self.request, mujoco)
        runner.validate_result(result, self.request)
        self.assertEqual(result["initial_state"]["qpos"], [0., 0., 1., 1., 0., 0., 0.])
        self.assertEqual(result["model"]["nu"], 0)
        self.assertLess(result["samples"][-1]["qpos"][2], 1)
        self.assertEqual(result["samples"][-1]["bodies"][0]["position_local_m"], [0., result["samples"][-1]["qpos"][2], -0.])

    @unittest.skipUnless(NATIVE4, "requires MuJoCo 4.0.0")
    def test_fixed_multijoint_matches_native_trajectory_and_sensors(self):
        from sim.package_worker import execute
        path = make_package(self.folder / "multi", MULTI)
        selected = request(path)
        selected.update(body_names=["one", "two"], sensor_names=["xpos", "yvel"],
                        control={"mode": "fixed", "values": [{"actuator": "mx", "value": .5}, {"actuator": "my", "value": -.25}]})
        result = execute(load_model_package(path), selected, mujoco)
        runner.validate_result(result, selected)
        model = mujoco.MjModel.from_xml_string(MULTI); data = mujoco.MjData(model)
        data.ctrl[:] = [.5, -.25]
        for step in range(1, 21):
            mujoco.mj_step(model, data)
            if step % 5 == 0:
                mujoco.mj_forward(model, data)
                sample = result["samples"][step // 5 - 1]
                self.assertEqual(sample["qpos"], list(data.qpos))
                self.assertEqual(sample["qvel"], list(data.qvel))
                self.assertEqual([s["values"][0] for s in sample["sensors"]], list(data.sensordata))
        self.assertNotEqual(result["samples"][-1]["qpos"], [0., 0.])

    @unittest.skipUnless(NATIVE4, "requires MuJoCo 4.0.0")
    def test_unknown_and_out_of_range_compiled_controls(self):
        from sim.package_worker import execute
        path = make_package(self.folder / "multi", MULTI)
        for name, value in (("missing", 0), ("mx", 2)):
            selected = request(path); selected["control"] = {"mode": "fixed", "values": [{"actuator": name, "value": value}]}
            with self.assertRaises(runner.RunnerError): execute(load_model_package(path), selected, mujoco)
        path = make_package(self.folder / "positive", MULTI.replace('ctrlrange="-2 2"', 'ctrlrange="1 2"'))
        with self.assertRaisesRegex(runner.RunnerError, "compiled actuator range"):
            execute(load_model_package(path), request(path), mujoco)

    @unittest.skipUnless(NATIVE4, "requires MuJoCo 4.0.0")
    def test_report_is_exclusive_and_failure_preserves_existing_bytes(self):
        from sim.package_worker import execute
        result = execute(load_model_package(self.path), self.request, mujoco)
        destination = self.folder / "report.json"
        runner.install_report(result, destination)
        original = destination.read_bytes()
        with self.assertRaises(runner.RunnerError): runner.install_report(result, destination)
        self.assertEqual(destination.read_bytes(), original)
        broken = copy.deepcopy(result); broken["samples"][0] = {}
        broken["samples_sha256"] = runner.digest(broken["samples"])
        with self.assertRaises(runner.RunnerError): runner.install_report(broken, self.folder / "bad.json")
        self.assertFalse((self.folder / "bad.json").exists())
        link = self.folder / "link.json"; link.symlink_to(destination)
        with self.assertRaises(runner.RunnerError): runner.install_report(result, link)
        self.assertEqual(destination.read_bytes(), original)
        self.assertEqual(list(self.folder.glob('.world-run-*')), [])

    @unittest.skipUnless(sys.platform.startswith("linux") and NATIVE4, "requires Linux and MuJoCo 4.0.0")
    def test_actual_worker_binds_package_and_refuses_stale_digest(self):
        result = runner.run_package(self.path, self.request)
        self.assertEqual(result["summary"]["completed_steps"], 20)
        self.assertEqual(result["runtime"]["mujoco_version"], "4.0.0")
        changed = dict(self.request, package_sha256="0" * 64)
        with self.assertRaises(runner.RunnerError) as failure: runner.run_package(self.path, changed)
        self.assertEqual(failure.exception.stage, "structural_validation")
        self.assertEqual(failure.exception.code, "package_mismatch")

    @unittest.skipUnless(sys.platform.startswith("linux"), "requires Linux resource limits")
    def test_preimport_limits_and_actual_memory_output_enforcement(self):
        script = f'''import sys,json,resource
sys.path.insert(0,{str(ROOT)!r})
from sim.package_worker import install_limits
assert "mujoco" not in sys.modules and "numpy" not in sys.modules
install_limits()
assert resource.getrlimit(resource.RLIMIT_AS)==(2<<30,2<<30)
assert resource.getrlimit(resource.RLIMIT_CPU)==(10,10)
assert resource.getrlimit(resource.RLIMIT_FSIZE)==(2<<20,2<<20)
assert resource.getrlimit(resource.RLIMIT_CORE)==(0,0)
try: bytearray(3<<30)
except MemoryError: pass
else: raise AssertionError("memory limit was not enforced")
try:
    with open("large", "wb") as output: output.write(b"x"*(3<<20))
except OSError: pass
else: raise AssertionError("output limit was not enforced")
print("limits enforced")
'''
        process = subprocess.run([sys.executable, "-I", "-c", script], cwd=self.folder, capture_output=True, timeout=5)
        self.assertEqual(process.returncode, 0, process.stderr)
        self.assertEqual((self.folder / "large").stat().st_size, 2 << 20)

    @unittest.skipUnless(sys.platform.startswith("linux"), "requires Linux process group behavior")
    def test_parent_timeout_reaps_worker_and_refuses_output_flood(self):
        real_popen = subprocess.Popen
        spawned = []
        def replace_with(script):
            def spawn(args, **kwargs):
                process = real_popen([sys.executable, "-I", "-c", script], **kwargs)
                spawned.append(process)
                return process
            return spawn
        with mock.patch.object(runner.subprocess, "Popen", side_effect=replace_with("import time; time.sleep(5)")), mock.patch.dict(runner.LIMITS, wall_seconds=.1):
            with self.assertRaises(runner.RunnerError) as failure: runner._run_child({})
            self.assertEqual(failure.exception.code, "wall_limit")
        self.assertIsNotNone(spawned[-1].poll())
        with mock.patch.object(runner.subprocess, "Popen", side_effect=replace_with("import sys; sys.stdout.buffer.write(b'x'*(3<<20))")):
            with self.assertRaises(runner.RunnerError) as failure: runner._run_child({})
            self.assertEqual(failure.exception.code, "output_limit")


if __name__ == "__main__":
    unittest.main()

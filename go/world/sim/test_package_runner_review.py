"""Independent boundary checks. Native execution cases require explicit Linux setup."""
import copy
import hashlib
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from sim import package_runner as runner
from sim.run import PackageError, load_model_package


def review_manifest(model):
    return {
        "schema_version": "wendy.world.package.v1", "site_id": "review-site",
        "package_revision": "review-package", "frame_revision": "review-frame", "provenance": "synthetic",
        "geographic_anchor": {"latitude_deg": 52.0, "longitude_deg": 5.0, "altitude_m": 0,
                              "altitude_datum": "WGS84-ellipsoid", "placement": "illustrative"},
        "local_frame": {"units": "metres", "handedness": "right", "up": "Y", "axes": ["east", "up", "south"]},
        "mujoco": {"path": "models/passive.xml", "sha256": hashlib.sha256(model).hexdigest(),
                   "to_local_position": ["x", "z", "-y"]},
    }


def review_request(package_digest="a" * 64):
    return {"schema_version": runner.REQUEST_SCHEMA, "package_sha256": package_digest, "run_id": "review-run",
            "steps": 10, "sample_every": 5, "body_names": [], "sensor_names": [],
            "initial_state": "model-default", "control": {"mode": "zero"}, "resource_policy": runner.POLICY}


class GenericEnvelopeReview(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.model = b'<mujoco model="passive"><worldbody><geom type="sphere" size="0.1"/></worldbody></mujoco>'
        (self.root / "models").mkdir()
        (self.root / "models/passive.xml").write_bytes(self.model)
        self.manifest = review_manifest(self.model)

    def load(self, manifest=None, raw=None):
        if raw is None:
            raw = json.dumps(manifest if manifest is not None else self.manifest, ensure_ascii=True).encode()
        (self.root / "world.json").write_bytes(raw)
        return load_model_package(self.root / "world.json")

    def test_passive_package_keeps_exact_identity_without_fixture_bindings(self):
        raw = b" \n" + json.dumps(self.manifest).encode()
        package = self.load(raw=raw)
        self.assertEqual(package["_digest"], hashlib.sha256(raw).hexdigest())
        self.assertNotIn("gas_model", package)
        self.assertNotIn("body", package["mujoco"])
        self.assertNotIn("actuator", package["mujoco"])
        self.assertEqual(package["_assets"], {})
        self.assertIn('<geom type="sphere"', package["_model_xml"])

    def test_unicode_and_numeric_agree_with_go_v1_boundary(self):
        for name, value, valid in [
            ("exact UTF8 bytes", "é" * 128, True), ("over UTF8 bytes", "é" * 129, False),
            ("NBSP", "site\u00a0", False), ("Unicode space", "\u2000site", False),
            ("zero width is not space", "site\u200b", True),
        ]:
            with self.subTest(name=name):
                document = copy.deepcopy(self.manifest)
                document["site_id"] = value
                if valid:
                    self.load(document)
                else:
                    with self.assertRaises(PackageError):
                        self.load(document)
        for value, valid in [(r'"\ud800"', False), (r'"\udc00"', False), (r'"\udc00\ud800"', False),
                             (r'"\ud83d\ude80"', True), (r'"\\ud800"', True),
                             ('{"nested":1e309}', False), ('{"nested":' + '9' * 400 + '}', False),
                             ('{"nested":1,"nested":2}', False)]:
            with self.subTest(value=value[:40]):
                raw = json.dumps(self.manifest).encode()[:-1] + b',"scenario":' + value.encode() + b'}'
                if valid:
                    self.load(raw=raw)
                else:
                    with self.assertRaises(PackageError):
                        self.load(raw=raw)
        raw = json.dumps(self.manifest).encode()[:-1] + b',"description":"bad\xff"}'
        with self.assertRaises(PackageError):
            self.load(raw=raw)

    def test_runtime_overrides_are_not_package_metadata(self):
        for field in ("gas_model", "live_sensors", "controller", "entrypoint", "command", "credentials", "hardware_bindings"):
            with self.subTest(field=field):
                document = copy.deepcopy(self.manifest)
                document[field] = {"enabled": True}
                with self.assertRaises(PackageError):
                    self.load(document)
        for field in ("body", "actuator", "control", "control_mode", "plugin", "script"):
            with self.subTest(field=field):
                document = copy.deepcopy(self.manifest)
                document["mujoco"][field] = "run-me"
                with self.assertRaises(PackageError):
                    self.load(document)

    def test_symlink_and_undeclared_resource_cannot_be_loaded(self):
        path = self.root / "models/passive.xml"
        path.unlink()
        outside = self.root / "outside.xml"
        outside.write_bytes(self.model)
        path.symlink_to(outside)
        with self.assertRaises(PackageError):
            self.load()
        path.unlink()
        external = b'<mujoco><asset><mesh file="../../outside.obj"/></asset></mujoco>'
        path.write_bytes(external)
        document = review_manifest(external)
        with self.assertRaises(PackageError):
            self.load(document)


class GenericRequestReview(unittest.TestCase):
    def test_request_is_owned_and_cannot_choose_a_worker_or_hardware(self):
        original = review_request()
        original["body_names"] = ["free body"]
        original["control"] = {"mode": "fixed", "values": [{"actuator": "motor", "value": .25}]}
        copied = runner.validate_request(original)
        original["body_names"].append("other")
        original["control"]["values"][0]["value"] = .9
        self.assertEqual(copied["body_names"], ["free body"])
        self.assertEqual(copied["control"]["values"][0]["value"], .25)
        for key in ("worker", "executable", "script", "callback", "device", "credentials", "output", "network", "seed"):
            with self.subTest(key=key):
                request = review_request()
                request[key] = "/tmp/foreign-worker"
                with self.assertRaises(runner.RunnerError):
                    runner.validate_request(request)

    def test_exact_request_types_and_sampling_bounds(self):
        for field, value in [("steps", True), ("steps", 1.0), ("steps", 0), ("steps", 10001),
                             ("sample_every", True), ("sample_every", 0), ("sample_every", 11),
                             ("body_names", None), ("body_names", ["duplicate", "duplicate"]),
                             ("sensor_names", ["line\nbreak"]), ("sensor_names", ["é"]),
                             ("initial_state", "zero"), ("resource_policy", "unrestricted"),
                             ("package_sha256", "A" * 64), ("run_id", "../other")]:
            with self.subTest(field=field, value=value):
                request = review_request()
                request[field] = value
                with self.assertRaises(runner.RunnerError):
                    runner.validate_request(request)
        request = review_request()
        request.update(steps=257, sample_every=1)
        with self.assertRaises(runner.RunnerError):
            runner.validate_request(request)
        request.update(steps=256)
        self.assertEqual(runner.validate_request(request)["steps"], 256)

    def test_explicit_control_never_accepts_aliases_or_nonfinite_values(self):
        controls = [None, {"mode": "zero", "values": []}, {"mode": "fixed", "values": []},
                    {"mode": "callback", "script": "arbitrary"},
                    {"mode": "fixed", "values": [{"actuator": "motor", "value": 0, "class": "safe"}]},
                    {"mode": "fixed", "values": [{"actuator": "motor", "value": 0}] * 2}]
        controls += [{"mode": "fixed", "values": [{"actuator": "motor", "value": value}]}
                     for value in (True, float("nan"), float("inf"), 1e9 + 1)]
        for control in controls:
            with self.subTest(control=repr(control)):
                request = review_request()
                request["control"] = control
                with self.assertRaises(runner.RunnerError):
                    runner.validate_request(request)


class GenericWorkerLifecycleReview(unittest.TestCase):
    def test_deadline_kills_and_reaps_the_process_group(self):
        class Child:
            pid = 54321
            returncode = None
            waited = False
            def poll(self):
                return None
            def wait(self):
                self.waited = True
                self.returncode = -9
                return self.returncode
        child = Child()
        with patch.object(runner.subprocess, "Popen", return_value=child) as spawn, \
             patch.object(runner.os, "killpg") as kill, \
             patch.object(runner.time, "monotonic", side_effect=[0, runner.LIMITS["wall_seconds"]]):
            with self.assertRaises(runner.RunnerError) as caught:
                runner._run_child({"package": "/unused", "request": review_request()})
        self.assertEqual(caught.exception.code, "wall_limit")
        self.assertTrue(spawn.call_args.kwargs["start_new_session"])
        self.assertNotIn("shell", spawn.call_args.kwargs)
        self.assertIn("-I", spawn.call_args.args[0])
        for name in ("OPENBLAS_NUM_THREADS", "OMP_NUM_THREADS", "MKL_NUM_THREADS"):
            self.assertEqual(spawn.call_args.kwargs["env"][name], "1")
        kill.assert_called_once_with(child.pid, runner.signal.SIGKILL)
        self.assertTrue(child.waited)

    def test_exited_worker_with_oversized_output_cannot_escape_cleanup(self):
        class Child:
            pid = 54322
            returncode = 0
            waited = False
            def poll(self):
                return 0
            def wait(self):
                self.waited = True
                return 0
        child = Child()
        def spawn(*args, **kwargs):
            kwargs["stdout"].write(b"x" * (runner.MAX_RESULT + 1))
            kwargs["stdout"].flush()
            return child
        with patch.object(runner.subprocess, "Popen", side_effect=spawn), patch.object(runner.os, "killpg") as kill:
            with self.assertRaises(runner.RunnerError) as caught:
                runner._run_child({"package": "/unused", "request": review_request()})
        self.assertEqual(caught.exception.code, "output_limit")
        kill.assert_called_once_with(child.pid, runner.signal.SIGKILL)
        self.assertTrue(child.waited)


def review_report():
    """Constructed validator input only; this fixture is not a native run receipt."""
    request = review_request()
    request.update(body_names=["body"], sensor_names=["joint", "position"],
                   control={"mode": "fixed", "values": [{"actuator": "motor", "value": .25}]})
    initial = {"qpos": [0., 0., 1., 1., 0., 0., 0.], "qvel": [0.] * 6, "ctrl": [.25, 0.], "act": [0.]}
    sensors = [{"name": "joint", "type": 9, "dimension": 1}, {"name": "position", "type": 26, "dimension": 3}]
    samples = [{"step": step, "simulation_time_s": step * .002, **copy.deepcopy(initial),
                "bodies": [{"name": "body", "position_mujoco_m": [1., 2., 3.], "position_local_m": [1., 3., -2.]}],
                "sensors": [{**s, "values": [0.] * s["dimension"]} for s in sensors]}
               for step in (5, 10)]
    return {"schema_version": runner.RESULT_SCHEMA, "request": request, "request_sha256": runner.digest(request),
            "package": {"sha256": request["package_sha256"], "schema_version": "wendy.world.package.v1",
                        "site_id": "review-site", "package_revision": "review-package", "frame_revision": "review-frame"},
            "validation": {"inventory_verified": True, "native_compiled": True, "simulation_completed": True},
            "runtime": {"mujoco_version": "4.0.0", "python_version": "3.12.9", "resource_policy": runner.POLICY,
                        "limits": dict(runner.LIMITS), "seed": None,
                        "isolation": "Linux process resource limits only; network and filesystem isolation are not attested here"},
            "model": {"nq": 7, "nv": 6, "nu": 2, "na": 1, "nbody": 2, "ngeom": 1, "njnt": 1,
                      "nsensor": 2, "nsensordata": 4, "timestep_s": .002, "actuator_names": ["motor", None],
                      "selected_sensors": sensors}, "initial_state": initial, "samples": samples,
            "summary": {"completed_steps": 10, "sample_count": 2, "final_simulation_time_s": .02},
            "samples_sha256": runner.digest(samples), "source_sha256": runner.source_hashes(),
            "import_report": {"schema_version": "wendy.mujoco.import.v1"},
            "limitations": ["constructed validator test input, not execution evidence"]}


class GenericResultReview(unittest.TestCase):
    def test_complete_validator_fixture_is_accepted(self):
        report = review_report()
        self.assertIs(runner.validate_result(report, report["request"]), report)

    def test_rehashed_state_does_not_hide_wrong_types_shapes_or_coordinates(self):
        mutations = [
            ("empty sample", lambda r: r["samples"].__setitem__(0, {})),
            ("boolean step", lambda r: r["samples"][0].__setitem__("step", True)),
            ("skipped step", lambda r: r["samples"][0].__setitem__("step", 4)),
            ("backward time", lambda r: r["samples"][1].__setitem__("simulation_time_s", .001)),
            ("wrong time domain", lambda r: r["samples"][0].__setitem__("simulation_time_s", 1794000000.)),
            ("truncated vector", lambda r: r["samples"][0]["qpos"].pop()),
            ("boolean numeric", lambda r: r["samples"][0]["qvel"].__setitem__(0, False)),
            ("different control", lambda r: r["samples"][0]["ctrl"].__setitem__(1, 1.)),
            ("aliased body", lambda r: r["samples"][0]["bodies"][0].__setitem__("name", "another")),
            ("wrong local frame", lambda r: r["samples"][0]["bodies"][0]["position_local_m"].__setitem__(2, 2.)),
            ("unrequested sensor", lambda r: r["samples"][0]["sensors"][0].__setitem__("name", "other")),
            ("changed sensor dimension", lambda r: r["samples"][0]["sensors"][0].__setitem__("dimension", 2)),
            ("missing sensor values", lambda r: r["samples"][0]["sensors"][1]["values"].pop()),
            ("invented units", lambda r: r["samples"][0]["sensors"][0].__setitem__("units", "ppm")),
        ]
        for label, mutate in mutations:
            with self.subTest(label=label):
                report = review_report()
                mutate(report)
                report["samples_sha256"] = runner.digest(report["samples"])
                with self.assertRaises(runner.RunnerError):
                    runner.validate_result(report, report["request"])

    def test_exact_request_source_runtime_and_completion_are_required(self):
        mutations = [
            ("request bool alias", lambda r: r["request"].__setitem__("steps", True)),
            ("source change", lambda r: r["source_sha256"].__setitem__("sim/run.py", "b" * 64)),
            ("source omitted", lambda r: r["source_sha256"].pop("sim/package_worker.py")),
            ("package change", lambda r: r["package"].__setitem__("sha256", "b" * 64)),
            ("old runtime", lambda r: r["runtime"].__setitem__("mujoco_version", "3.3.7")),
            ("invented sandbox", lambda r: r["runtime"].__setitem__("isolation", "network isolated")),
            ("limit bool alias", lambda r: r["runtime"]["limits"].__setitem__("core_bytes", False)),
            ("completion int alias", lambda r: r["validation"].__setitem__("simulation_completed", 1)),
            ("dimensions bool alias", lambda r: r["model"].__setitem__("na", True)),
            ("missing final sample", lambda r: r["samples"].pop()),
            ("unknown root", lambda r: r.__setitem__("physical_authority", True)),
        ]
        for label, mutate in mutations:
            with self.subTest(label=label):
                report = review_report()
                expected = copy.deepcopy(report["request"])
                mutate(report)
                with self.assertRaises(runner.RunnerError):
                    runner.validate_result(report, expected)

    def test_rehashed_nonfinite_and_over_budget_vectors_refused(self):
        for bad in (float("nan"), float("inf"), 10 ** 400):
            with self.subTest(bad=str(bad)[:40]):
                report = review_report()
                report["initial_state"]["qpos"][0] = bad
                with self.assertRaises(runner.RunnerError):
                    runner.validate_result(report, report["request"])
        report = review_report()
        report["model"]["nq"] = 16384
        report["request"].update(steps=4, sample_every=1)
        report["request_sha256"] = runner.digest(report["request"])
        with self.assertRaisesRegex(runner.RunnerError, "scalar budget"):
            runner.validate_result(report, report["request"])

    def test_selected_sensor_cardinality_cannot_exceed_native_arrays(self):
        for dimension, value in (("nsensor", 1), ("nsensordata", 3)):
            with self.subTest(dimension=dimension):
                report = review_report()
                report["model"][dimension] = value
                with self.assertRaises(runner.RunnerError):
                    runner.validate_result(report, report["request"])

    def test_failure_never_replaces_or_publishes_a_report(self):
        with tempfile.TemporaryDirectory() as folder:
            destination = Path(folder) / "result.json"
            report = review_report()
            runner.install_report(report, destination)
            original = destination.read_bytes()
            self.assertEqual(destination.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(runner.RunnerError):
                runner.install_report(report, destination)
            self.assertEqual(destination.read_bytes(), original)
            symlink = Path(folder) / "symlink.json"
            symlink.symlink_to(destination)
            with self.assertRaises(runner.RunnerError):
                runner.install_report(report, symlink)
            report["summary"]["completed_steps"] = 9
            with self.assertRaises(runner.RunnerError):
                runner.install_report(report, Path(folder) / "failed.json")
            self.assertFalse((Path(folder) / "failed.json").exists())
            self.assertEqual(destination.read_bytes(), original)
            self.assertEqual(list(Path(folder).glob(".world-run-*")), [])


if __name__ == "__main__":
    unittest.main()

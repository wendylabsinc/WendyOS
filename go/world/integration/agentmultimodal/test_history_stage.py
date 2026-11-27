"""History staging tests use synthetic files and fake image metadata only."""
import copy
import hashlib
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
sys.path.insert(0, str(HERE))
import history
import history_stage as h
import stage
from test_stage import Docker, RUNTIME, SCENE


def elf(path, suffix=b""):
    data = bytearray(64)
    data[:6] = b"\x7fELF\x02\x01"
    data[18:20] = (183).to_bytes(2, "little")
    path.write_bytes(data + suffix)
    path.chmod(0o755)
    return path


class HistoryStageTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory(prefix="world-history-stage-")
        self.addCleanup(temporary.cleanup)
        self.root = Path(temporary.name).resolve()
        self.root.chmod(0o700)
        self.staging = self.root / "staging"
        self.staging.mkdir(mode=0o700)
        docker = Docker()
        base = self.staging / "base"
        stage.export_rootfs(SCENE, base, run=docker.run, staging=self.staging)
        binaries = {name: elf(self.root / name, name.encode()) for name in stage.BINARIES}
        self.baseline = self.staging / "baseline"
        stage.prepare(base, self.baseline, RUNTIME, binaries, "history-test", 1767225600000000000,
                      7, run=docker.run, staging=self.staging)
        self.reader = elf(self.root / "proof-reader", b"pinned independent reader")
        self.output = self.staging / "history"
        self.pins = {
            "reader_sha256": stage.digest_file(self.reader),
            "baseline_plan_sha256": stage.digest_file(self.baseline / "plan.json"),
            "baseline_inventory_sha256": stage.digest_file(self.baseline / "files.json"),
        }

    def prepare(self, output=None, **pins):
        return h.prepare(self.baseline, output or self.output, self.reader,
                         staging=self.staging, **{**self.pins, **pins})

    def verify(self, **pins):
        return h.validate_bundle(self.output, self.baseline, **{**self.pins, **pins})

    def save(self, relative, value):
        path = self.output / relative
        path.write_bytes(stage.json_bytes(value))

    def refresh_inventory(self):
        files = stage.inventory(self.output)
        files.pop("files.json")
        self.save("files.json", {"version": 1, "files": files})

    def tamper_manifest(self, phase, mutate):
        path = h.MANIFESTS[phase]
        manifest = json.loads((self.output / path).read_bytes())
        mutate(manifest)
        self.save(path, manifest)
        plan = json.loads((self.output / "plan.json").read_bytes())
        index = h.PHASES.index(phase)
        plan["phases"][index]["manifest_sha256"] = stage.digest_file(self.output / path)
        self.save("plan.json", plan)
        self.refresh_inventory()

    def test_additive_bundle_preserves_exact_baseline_and_executes_nothing(self):
        original = stage.inventory(self.baseline)
        original_project = (self.baseline / "project/wendy.json").read_bytes()
        # Preparation and verification must not invoke Docker, a shell or the
        # provided binaries, even for their version metadata.
        with patch("subprocess.run", side_effect=AssertionError("unexpected process")), \
                patch("subprocess.Popen", side_effect=AssertionError("unexpected process")):
            plan = self.prepare()
            self.assertEqual(self.verify(), plan)
        self.assertEqual(stage.inventory(self.baseline), original)
        self.assertEqual((self.baseline / "project/wendy.json").read_bytes(), original_project)
        self.assertEqual(set(stage.SERVICES), set(json.loads(original_project)["services"]))
        self.assertFalse(plan["deployment_verified"])
        self.assertEqual(plan["scene_lifetime_seconds"], 600)
        self.assertEqual(plan["baseline"]["project_sha256"], hashlib.sha256(original_project).hexdigest())
        self.assertEqual(set(p.relative_to(self.output).as_posix() for p in self.output.rglob("*")
                             if p.is_file()), set(h.FILES) | {"files.json"})
        self.assertEqual((self.output / h.CONTEXT / "app").read_bytes(), self.reader.read_bytes())
        self.assertNotIn("retain_for_ms", json.dumps(plan))
        self.assertEqual(h.READER, history.READER)
        self.assertEqual(h.PHASES, history.PHASES)

    def test_exact_grants_phase_volumes_and_manifest_transition_pins(self):
        plan = self.prepare()
        original = json.loads((self.baseline / "project/wendy.json").read_bytes())
        previous = stage.digest_file(self.baseline / "project/wendy.json")
        all_volumes = set()
        reader_definitions = []
        original_volumes = {ent["name"] for service in original["services"].values()
                            for ent in service["entitlements"] if ent["type"] == "persist"}
        for phase in plan["phases"]:
            raw = (self.output / phase["manifest"]).read_bytes()
            manifest = json.loads(raw)
            self.assertEqual(set(manifest["services"]), set(stage.SERVICES) | {h.READER})
            for name in stage.SERVICES:
                self.assertEqual(manifest["services"][name], original["services"][name])
            self.assertEqual({k: v for k, v in manifest.items() if k != "services"},
                             {k: v for k, v in original.items() if k != "services"})
            reader = manifest["services"][h.READER]
            self.assertEqual(set(reader), {"context", "entitlements"})
            world = reader["entitlements"][2]
            self.assertEqual(set(world), {"type", "subscribe"})
            self.assertEqual(world["type"], "world")
            self.assertEqual([(s["service"], s["componentType"], s["stream"])
                              for s in world["subscribe"]], [
                ("camera-a", stage.CAPTURE, "camera-a"),
                ("sensorlink-bridge", stage.CAPTURE, "camera-b"),
                ("gas", stage.CAPTURE, "gas-1"),
                ("audio", stage.CAPTURE, "audio-1"),
                ("fusion", stage.RESULT, "hazard")])
            self.assertTrue(all(s["app"] == stage.APP for s in world["subscribe"]))
            volumes = phase["volumes"]
            self.assertEqual(volumes, h.phase_volumes("history-test", phase["name"]))
            self.assertFalse(set(volumes.values()) & (all_volumes | original_volumes))
            all_volumes.update(volumes.values())
            self.assertEqual(phase["expected_previous_manifest_sha256"], previous)
            previous = hashlib.sha256(raw).hexdigest()
            self.assertEqual(previous, phase["manifest_sha256"])
            reader = copy.deepcopy(reader)
            for entitlement in reader["entitlements"][:2]:
                entitlement.pop("name")
            reader_definitions.append(reader)
        self.assertEqual(reader_definitions[0], reader_definitions[1])
        dockerfile = (self.output / h.CONTEXT / "Dockerfile").read_text()
        self.assertIn('"--start-file", "/config/start"', dockerfile)
        self.assertIn('"--duration", "300s"', dockerfile)
        self.assertIn('"--config", "/config/config.json"', dockerfile)
        self.assertIn("FROM scratch", dockerfile)
        self.assertNotIn("ENV", dockerfile)

    def test_wrong_external_pins_refuse_before_output_creation(self):
        for name in self.pins:
            with self.subTest(pin=name), self.assertRaises(stage.Refused):
                self.prepare(**{name: "0" * 64})
            self.assertFalse(self.output.exists())
        with self.assertRaises(stage.Refused):
            self.prepare(reader_sha256=self.pins["reader_sha256"].upper())
        self.assertFalse(self.output.exists())

    def test_baseline_mutation_and_extra_files_refused_before_and_after_prepare(self):
        self.prepare()
        extra = self.baseline / "project/private-config.json"
        extra.write_text('{"unapproved":"must not be copied"}')
        with self.assertRaises(stage.Refused):
            self.verify()
        with self.assertRaises(stage.Refused):
            self.prepare(self.staging / "other")
        self.assertFalse((self.staging / "other").exists())
        extra.unlink()
        cli = self.baseline / "tools/cli"
        cli.write_bytes(cli.read_bytes() + b"changed")
        with self.assertRaises(stage.Refused):
            self.verify()

    def test_changed_source_definition_refused_even_with_rehashed_baseline(self):
        project = self.baseline / "project/wendy.json"
        value = json.loads(project.read_bytes())
        value["services"]["scene"]["env"] = {"UNAPPROVED": "1"}
        project.write_bytes(stage.json_bytes(value))
        files = stage.inventory(self.baseline)
        files.pop("files.json")
        inventory = self.baseline / "files.json"
        inventory.write_bytes(stage.json_bytes({"version": 1, "files": files}))
        with self.assertRaises(stage.Refused):
            self.prepare(baseline_inventory_sha256=stage.digest_file(inventory))
        self.assertFalse(self.output.exists())

    def test_rehashed_manifest_cannot_add_authority_or_change_a_baseline_service(self):
        mutations = {
            "publish": lambda m: m["services"][h.READER]["entitlements"][2].update(
                publish=[{"componentType": stage.RESULT, "stream": "hazard"}]),
            "wildcard": lambda m: m["services"][h.READER]["entitlements"][2]["subscribe"][0].update(stream="*"),
            "wrong_source": lambda m: m["services"][h.READER]["entitlements"][2]["subscribe"][0].update(service="other"),
            "changed_scene": lambda m: m["services"]["scene"].update(context="services/other"),
            "depends_on": lambda m: m["services"][h.READER].update(dependsOn=["scene"]),
        }
        for name, mutate in mutations.items():
            with self.subTest(mutation=name):
                self.output = self.staging / name
                self.prepare()
                self.tamper_manifest("before_restart", mutate)
                with self.assertRaises(stage.Refused):
                    self.verify()

    def test_rehashed_second_phase_cannot_reuse_first_volumes_or_wrong_chain(self):
        self.prepare()
        first = json.loads((self.output / h.MANIFESTS["before_restart"]).read_bytes())
        self.tamper_manifest("after_restart", lambda m: m["services"].update(
            {h.READER: first["services"][h.READER]}))
        with self.assertRaises(stage.Refused):
            self.verify()
        self.output = self.staging / "wrong-chain"
        self.prepare()
        plan = json.loads((self.output / "plan.json").read_bytes())
        plan["phases"][1]["expected_previous_manifest_sha256"] = plan["baseline"]["project_sha256"]
        self.save("plan.json", plan)
        self.refresh_inventory()
        with self.assertRaises(stage.Refused):
            self.verify()

    def test_bundled_reader_runner_or_launcher_changes_are_refused(self):
        for name in ("app", "app-runner", "Dockerfile"):
            with self.subTest(file=name):
                self.output = self.staging / ("changed-" + name.lower())
                self.prepare()
                path = self.output / h.CONTEXT / name
                path.write_bytes(path.read_bytes() + b"changed")
                self.refresh_inventory()
                with self.assertRaises(stage.Refused):
                    self.verify()

    def test_reader_changed_during_prepare_leaves_incomplete_nonreusable_output(self):
        original = h._binary

        def changing_reader(path, destination=None):
            if Path(path) == self.reader and destination is not None:
                self.reader.write_bytes(self.reader.read_bytes() + b"changed after pin")
            return original(path, destination)

        with patch.object(h, "_binary", side_effect=changing_reader), self.assertRaises(stage.Refused):
            self.prepare()
        self.assertTrue(self.output.exists())
        self.assertFalse((self.output / "files.json").exists())
        with self.assertRaises(stage.Refused):
            self.verify()
        with self.assertRaises(stage.Refused):
            self.prepare(reader_sha256=stage.digest_file(self.reader))

    def test_duplicate_unknown_and_wrong_type_json_fields_refused(self):
        for filename in ("plan.json", "files.json", h.MANIFESTS["before_restart"]):
            with self.subTest(file=filename):
                self.output = self.staging / ("duplicate-" + Path(filename).name.replace(".", "-"))
                self.prepare()
                path = self.output / filename
                raw = path.read_bytes()
                key = "appId" if filename.endswith("wendy.json") else "version"
                value = json.loads(raw)[key]
                path.write_bytes(b"{" + json.dumps(key).encode() + b":" + json.dumps(value).encode() + b"," + raw[1:])
                if filename != "files.json":
                    self.refresh_inventory()
                with self.assertRaises(stage.Refused):
                    self.verify()
        for key, value in (("unknown", "field"), ("version", True)):
            self.output = self.staging / ("plan-" + key)
            self.prepare()
            plan = json.loads((self.output / "plan.json").read_bytes())
            plan[key] = value
            self.save("plan.json", plan)
            self.refresh_inventory()
            with self.subTest(field=key), self.assertRaises(stage.Refused):
                self.verify()
        self.output = self.staging / "inventory-bool"
        self.prepare()
        value = json.loads((self.output / "files.json").read_bytes())
        value["version"] = True
        self.save("files.json", value)
        with self.assertRaises(stage.Refused):
            self.verify()

    def test_extra_files_directories_modes_and_symlinks_refused(self):
        for kind in ("file", "directory", "symlink", "mode"):
            with self.subTest(kind=kind):
                self.output = self.staging / kind
                self.prepare()
                if kind == "file":
                    (self.output / "scene-read-token").write_text("unapproved")
                elif kind == "directory":
                    (self.output / "unlisted").mkdir()
                elif kind == "symlink":
                    path = self.output / h.CONTEXT / "app"
                    path.unlink()
                    path.symlink_to(self.reader)
                else:
                    (self.output / "plan.json").chmod(0o620)
                with self.assertRaises((stage.Refused, OSError)):
                    self.verify()

    def test_output_reuse_escape_missing_parent_and_writable_parent_refused(self):
        self.prepare()
        with self.assertRaises(stage.Refused):
            self.prepare()
        for output in (self.root / "outside", self.staging / "missing" / "nested",
                       self.baseline / "history"):
            with self.subTest(path=output), self.assertRaises(stage.Refused):
                self.prepare(output)
            self.assertFalse(output.exists())
        parent = self.staging / "writable"
        parent.mkdir(mode=0o700)
        parent.chmod(0o722)
        with self.assertRaises(stage.Refused):
            self.prepare(parent / "history")
        parent.chmod(0o700)
        link = self.staging / "link"
        link.symlink_to(parent, target_is_directory=True)
        with self.assertRaises(stage.Refused):
            self.prepare(link / "history")

    def test_input_symlink_wrong_architecture_and_oversize_binary_refused(self):
        original = self.reader
        link = self.root / "reader-link"
        link.symlink_to(original)
        self.reader = link
        with self.assertRaises(OSError):
            self.prepare()
        parent = self.root / "linked-input"
        parent.symlink_to(self.root, target_is_directory=True)
        self.reader = parent / original.name
        with self.assertRaises(OSError):
            self.prepare()
        self.reader = original
        data = bytearray(original.read_bytes())
        data[18:20] = (62).to_bytes(2, "little")
        original.write_bytes(data)
        with self.assertRaises(stage.Refused):
            self.prepare(reader_sha256=stage.digest_file(original))
        with original.open("wb") as target:
            target.truncate(h.MAX_BINARY + 1)
        with self.assertRaises(stage.Refused):
            self.prepare()
        self.assertFalse(self.output.exists())


if __name__ == "__main__":
    unittest.main()

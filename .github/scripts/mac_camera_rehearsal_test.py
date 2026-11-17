#!/usr/bin/env python3
"""Offline fixtures only: no camera, Keychain, native inventory or signature tool."""
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest.mock import patch

PATH = Path(__file__).resolve().parents[1] / "regression-coverage/manual/mac-camera-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("mac_camera_rehearsal", PATH)
GEN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GEN)


def git_output(command, **_):
    if command == ["git", "rev-parse", "HEAD"]:
        return "a" * 40 + "\n"
    if command == ["git", "status", "--porcelain"]:
        return ""
    raise AssertionError(f"native command during generation: {command}")


class MacCameraRehearsalTests(unittest.TestCase):
    def inventory(self, root):
        bundle = root / "Synthetic.app"
        (bundle / "Contents/_CodeSignature").mkdir(parents=True)
        (bundle / "Contents/Info.plist").write_text("synthetic plist; not a signed app")
        (bundle / "Contents/_CodeSignature/CodeResources").write_text("synthetic seal")
        (bundle / "Contents/Frameworks/Fixture.framework/Versions/A").mkdir(parents=True)
        (bundle / "Contents/Frameworks/Fixture.framework/Versions/A/lib").write_text("synthetic framework")
        (bundle / "Contents/Frameworks/Fixture.framework/Versions/Current").symlink_to("A")
        cli = root / "synthetic-cli"
        cli.write_text("#!/bin/sh\nprintf 'synthetic observation'\n")
        cli.chmod(0o700)
        decoder = root / "synthetic-decoder"
        decoder.write_text("#!/bin/sh\nprintf 'synthetic decoder output'\n")
        decoder.chmod(0o700)
        profiles, records = {}, {}
        for name in GEN.PROFILES:
            profile = root / name
            profile.mkdir()
            (profile / "config.json").write_text('{"synthetic":true}')
            profiles[name] = str(profile)
        for name in GEN.RECORDS:
            record = root / (name + ".txt")
            record.write_text("SYNTHETIC OFFLINE RECORD. No physical acceptance.")
            records[name] = str(record)
        raw = {"candidate_source_commit": "a" * 40, "bundle": str(bundle), "cli_binary": str(cli),
               "decoder_binary": str(decoder), "captured_at": "2026-11-12T12:00:00Z", "owner": "fixture owner",
               "mac": {"serial": "SYNTHETIC", "hardware_uuid": "SYNTHETIC-UUID", "hostname": "fixture-mac",
                       "os_version": "synthetic", "architecture": "arm64"},
               "camera": {"unique_id": "synthetic-native-id", "name": "Synthetic", "physical_label": "fixture", "id": 1},
               "endpoint": "127.0.0.1:50051", "records": records, "profiles": profiles, "actor_checks": False}
        inventory = root / "inventory.json"
        inventory.write_text(json.dumps(raw))
        return inventory, raw

    def generate(self, inventory, output):
        with patch.object(GEN.subprocess, "check_output", side_effect=git_output), \
             patch.object(GEN.subprocess, "Popen", side_effect=AssertionError("generation started a process")):
            return GEN.generate(inventory, output)

    def test_offline_generation_pins_inputs_and_never_claims_acceptance(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            plan = self.generate(inventory, root / "plan")
            GEN.verify(plan)
            self.assertEqual(plan["hardware_execution"], "not_run")
            self.assertEqual(plan["cleanup"]["status"], "not_run")
            self.assertEqual(len(plan["cases"]), 10)
            self.assertTrue(all(c["status"] == "not_run" for c in plan["cases"]))
            commands = json.loads((root / "plan/commands.json").read_text())
            for command in commands.values():
                self.assertEqual(command["argv"][0], str(Path(raw["cli_binary"]).resolve()))
                self.assertEqual(command["argv"][command["argv"].index("--device") + 1], raw["endpoint"])
                self.assertNotIn("--raw", command["argv"])
                if command["decode"]:
                    self.assertIn("--stdout", command["argv"])
                    self.assertIn("--non-interactive", command["argv"])
            self.assertEqual((root / "plan").stat().st_mode & 0o777, 0o700)
            self.assertTrue(all(p.stat().st_mode & 0o777 == 0o600 for p in (root / "plan").iterdir()))
            procedure = (root / "plan/procedure.txt").read_text()
            self.assertIn("ALL CASES ARE NOT RUN", procedure)
            self.assertIn("not by itself\nAVFoundation teardown", procedure)
            self.assertIn("keep that case not_run", procedure)

    def test_changed_records_bundle_config_and_commands_refuse_execution(self):
        for target in ("inventory", "record", "bundle", "profile", "commands", "cli"):
            with self.subTest(target=target), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                inventory, raw = self.inventory(root)
                plan = self.generate(inventory, root / "plan")
                paths = {"inventory": inventory, "record": Path(raw["records"]["permissions"]),
                         "bundle": Path(raw["bundle"]) / "Contents/Info.plist",
                         "profile": Path(raw["profiles"]["operator"]) / "config.json",
                         "commands": root / "plan/commands.json", "cli": Path(raw["cli_binary"])}
                paths[target].write_text("changed")
                with self.assertRaises(ValueError):
                    GEN.verify(plan)

    def test_profile_separation_and_native_exact_target_validation(self):
        mutations = [lambda r: r.update(endpoint="discovered.local:50051"),
                     lambda r: r.update(endpoint="127.0.0.1:0"),
                     lambda r: r["camera"].update(id=True),
                     lambda r: r["mac"].update(architecture="linux"),
                     lambda r: r["mac"].update(hostname="host\ncommand"),
                     lambda r: r.update(candidate_source_commit="short"),
                     lambda r: r.update(actor_checks="true"),
                     lambda r: r.update(captured_at="2026-11-12T12:00:00"),
                     lambda r: r["profiles"].update(anonymous=r["profiles"]["operator"])]
        for mutate in mutations:
            with self.subTest(mutate=mutate), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                inventory, raw = self.inventory(root)
                mutate(raw)
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    self.generate(inventory, root / "plan")
                self.assertFalse((root / "plan").exists())

    def test_keychain_reference_cannot_escape_disposable_profile(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            (Path(raw["profiles"]["operator"]) / "config.json").write_text('{"privateKey":"keychain:v1:test"}')
            with self.assertRaisesRegex(ValueError, "Keychain"):
                self.generate(inventory, root / "plan")

    def test_framework_link_cannot_escape_bundle_and_link_changes_invalidate(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            plan = self.generate(inventory, root / "plan")
            link = Path(raw["bundle"]) / "Contents/Frameworks/Fixture.framework/Versions/Current"
            link.unlink()
            link.symlink_to(root)
            with self.assertRaisesRegex(ValueError, "escapes"):
                GEN.verify(plan)

    def test_wrong_host_does_not_start_any_capture(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, _ = self.inventory(root)
            self.generate(inventory, root / "plan")
            with patch.object(GEN.platform, "node", return_value="wrong-host"), \
                 patch.object(GEN.subprocess, "Popen", side_effect=AssertionError("should refuse first")):
                with self.assertRaisesRegex(ValueError, "inventoried"):
                    GEN.observe(root / "plan", "operator-capture-8", root / "observation")

    def test_real_owned_child_observation_is_never_a_hardware_pass(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, _ = self.inventory(root)
            self.generate(inventory, root / "plan")
            with patch.object(GEN.platform, "node", return_value="fixture-mac"), \
                 patch.object(GEN.platform, "system", return_value="Darwin"):
                GEN.observe(root / "plan", "operator-capture-8", root / "observation")
            result = json.loads((root / "observation/observation.json").read_text())
            self.assertEqual(result["status"], "needs_human_review")
            self.assertEqual(result["returncode"], 0)
            self.assertEqual(result["decoder"]["returncode"], 0)
            self.assertFalse(result["scheduled_cancel"])
            self.assertEqual((root / "observation/capture.bin").read_text(), "synthetic observation")
            plan = json.loads((root / "plan/evidence.json").read_text())
            self.assertTrue(all(c["status"] == "not_run" for c in plan["cases"]))

    def test_idle_real_child_is_cancelled_and_reaped(self):
        with tempfile.TemporaryDirectory() as temp:
            with (Path(temp) / "out").open("wb") as out:
                result = GEN.bounded_process([sys.executable, "-c", "import time; time.sleep(60)"],
                                             os.environ.copy(), out, out, 0.15, interrupt=True)
            self.assertTrue(result["scheduled_cancel"])
            self.assertFalse(result["forced_termination"])
            self.assertIsNotNone(result["returncode"])
            with self.assertRaises(ChildProcessError):
                os.waitpid(result["pid"], os.WNOHANG)

    def test_stubborn_real_child_is_escalated_and_reaped(self):
        with tempfile.TemporaryDirectory() as temp:
            code = "import signal,time; signal.signal(signal.SIGINT,signal.SIG_IGN); signal.signal(signal.SIGTERM,signal.SIG_IGN); time.sleep(60)"
            before = time.monotonic()
            with (Path(temp) / "out").open("wb") as out:
                result = GEN.bounded_process([sys.executable, "-c", code], os.environ.copy(), out, out, 0.25, True)
            self.assertTrue(result["forced_termination"])
            self.assertEqual(result["returncode"], -signal.SIGKILL)
            self.assertLess(time.monotonic() - before, 9)
            with self.assertRaises(ChildProcessError):
                os.waitpid(result["pid"], os.WNOHANG)

    def test_helper_sigterm_drains_owned_capture_before_exiting(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            Path(raw["cli_binary"]).write_text(
                f"#!{sys.executable}\nimport os,time\nprint(os.getpid(),flush=True)\ntime.sleep(60)\n")
            self.generate(inventory, root / "plan")
            code = ("import importlib.util,pathlib; "
                    "s=importlib.util.spec_from_file_location('fixture', " + repr(str(PATH)) + "); "
                    "m=importlib.util.module_from_spec(s); s.loader.exec_module(m); "
                    "m.platform.system=lambda:'Darwin'; m.platform.node=lambda:'fixture-mac'; "
                    "m.observe(pathlib.Path(" + repr(str(root / "plan")) + "), 'operator-capture-45', pathlib.Path(" +
                    repr(str(root / "observation")) + "))")
            helper = subprocess.Popen([sys.executable, "-c", code], stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
            try:
                deadline = time.monotonic() + 5
                capture = root / "observation/capture.bin"
                while (not capture.exists() or not capture.stat().st_size) and time.monotonic() < deadline:
                    time.sleep(0.02)
                self.assertTrue(capture.exists() and capture.stat().st_size, "synthetic child never started")
                child_pid = int(capture.read_text().strip())
                helper.terminate()
                self.assertEqual(helper.wait(timeout=8), 143)
                with self.assertRaises(ProcessLookupError):
                    os.kill(child_pid, 0)
                self.assertFalse((root / "observation/observation.json").exists())
            finally:
                if helper.poll() is None:
                    helper.terminate()
                    helper.wait(timeout=8)
                helper.stderr.close()

    def test_real_child_file_growth_is_bounded(self):
        with tempfile.TemporaryDirectory() as temp:
            path = Path(temp) / "out"
            with path.open("wb") as out, (Path(temp) / "err").open("wb") as err:
                result = GEN.bounded_process([sys.executable, "-c",
                    "import os; block=b'x'*1048576\nwhile True: os.write(1,block)"],
                    os.environ.copy(), out, err, 3)
            self.assertLessEqual(path.stat().st_size, 64 * 1024 * 1024)
            self.assertNotEqual(result["returncode"], 0)
            self.assertFalse(result["timed_out"])


if __name__ == "__main__":
    unittest.main()

#!/usr/bin/env python3
"""Inert record/CLI fixtures only. Never open cameras, microphones or Agent state."""
import importlib.util
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT = Path(__file__).resolve().parents[1] / "regression-coverage/manual/linux-media-hardware-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("linux_media_rehearsal", SCRIPT)
GEN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GEN)


def sources(source="audio:fixture-card", alias="audio:12", backend="alsa"):
    domain = "ALSA" if backend == "alsa" else "PIPEWIRE"
    return {"sources": [{"id": source, "kind": "audio", "healthy": True,
                         "clock_domain": domain + "_CAPTURE/AGENT_RECEIPT", "aliases": [alias]}]}


class LinuxMediaHardwareRehearsalTests(unittest.TestCase):
    def inventory(self, root):
        def record(name, value):
            path = root / name
            path.write_text(json.dumps(value))
            return str(path)
        cli = root / "inert cli with spaces"
        cli.write_text(f"#!{sys.executable}\nimport sys\nsys.stdout.buffer.write(b'\\x00INERT-MEDIA\\xff')\nprint('inert diagnostic', file=sys.stderr)\n")
        cli.chmod(0o700)
        # This string exercises shell quoting without any executed expansion.
        stable = "by-id:fixture;$(touch never-created)'camera"
        raw = {"cli_binary": str(cli), "agent_binary": record("agent", "INERT"),
               "agent_source_commit": "a" * 40,
               "target": {"selector": "fixture.invalid:50051", "serial": "SYNTHETIC", "owner": "inert test",
                          "board": "SYNTHETIC", "kernel": "SYNTHETIC", "encoder_driver": "qcom-iris"},
               "camera": {"stable_id": stable, "node": "/dev/video7", "physical_label": "NO CAMERA"},
               "microphone": {"source_id": "audio:fixture-card", "backend": "alsa", "physical_label": "NO MICROPHONE"},
               "records": {"camera_list": record("cameras.json", [{"stable_id": stable}]),
                           "sources": record("sources.json", sources())}}
        for name in ("device_info", "installed_binary", "udev", "recovery"):
            raw["records"][name] = record(name + ".json", {"synthetic": "not physical evidence"})
        return record("inventory.json", raw), raw

    def test_generation_is_offline_and_preserves_pending_physical_cases(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            with patch.object(subprocess, "Popen", side_effect=AssertionError("generation must not execute anything")):
                plan = GEN.generate(inventory, root / "plan")
            self.assertEqual(plan["hardware_execution"], "not_run")
            self.assertTrue(all(c["status"] == "not_run" for c in plan["cases"]))
            self.assertEqual(plan["audit_entries"], ["1739", "1954", "2542"])
            self.assertTrue(all(len(a["sha256"]) == 64 for a in plan["source_files"]))
            commands = json.loads((root / "plan/commands.json").read_text())
            for argv in commands.values():
                self.assertEqual(argv[:3], [str(Path(raw["cli_binary"]).resolve()), "--device", raw["target"]["selector"]])
            self.assertIn(raw["microphone"]["source_id"], commands["microphone_after"])
            self.assertEqual(commands["stop_owned_episode_only"][-2:], ["data", "stop"])
            for name in ("capture-camera.sh", "console-capture.sh"):
                subprocess.run(["sh", "-n", str(root / "plan" / name)], check=True)
            with self.assertRaises(FileExistsError):
                GEN.generate(inventory, root / "plan")

    def test_real_inert_cli_capture_separates_media_and_diagnostics(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, _ = self.inventory(root)
            GEN.generate(inventory, root / "plan")
            subprocess.run(["sh", str(root / "plan/capture-camera.sh"), "baseline"], check=True, timeout=12)
            self.assertEqual((root / "plan/baseline.video").read_bytes(), b"\x00INERT-MEDIA\xff")
            self.assertIn("inert diagnostic", (root / "plan/baseline.stderr").read_text())
            self.assertEqual((root / "plan/baseline.exit").read_text(), "0\n")
            self.assertFalse((root / "plan/never-created").exists())
            second = subprocess.run(["sh", str(root / "plan/capture-camera.sh"), "baseline"], capture_output=True, timeout=5)
            self.assertNotEqual(second.returncode, 0)
            self.assertEqual((root / "plan/baseline.video").read_bytes(), b"\x00INERT-MEDIA\xff")
            self.assertEqual(json.loads((root / "plan/evidence.json").read_text())["hardware_execution"], "not_run")

    def test_real_json_comparison_requires_actual_alias_change_and_same_backend(self):
        for backend in ("alsa", "pipewire"):
            before, after = sources(backend=backend), sources(alias="audio:37", backend=backend)
            result = GEN.check_renumber(before, after, "audio:fixture-card", backend)
            self.assertEqual(result["before_alias"], "audio:12")
            self.assertEqual(result["after_alias"], "audio:37")
            self.assertEqual(result["physical_identity_and_pcm"], "requires_human_review")
            with self.assertRaisesRegex(ValueError, "no observed numeric renumbering"):
                GEN.check_renumber(before, before, "audio:fixture-card", backend)
            with self.assertRaises(ValueError):
                GEN.check_renumber(before, sources(alias="audio:37", backend="pipewire" if backend == "alsa" else "alsa"), "audio:fixture-card", backend)
            with self.assertRaises(ValueError):
                GEN.check_renumber(before, sources(source="audio:other-physical-mic"), "audio:fixture-card", backend)
            duplicated = {"sources": after["sources"] * 2}
            with self.assertRaises(ValueError):
                GEN.check_renumber(before, duplicated, "audio:fixture-card", backend)

    def test_generator_rejects_numeric_source_and_ambiguous_camera(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.inventory(root)
            raw["microphone"]["source_id"] = "audio:12"
            Path(inventory).write_text(json.dumps(raw))
            with self.assertRaisesRegex(ValueError, "stable microphone"):
                GEN.generate(inventory, root / "numeric")
            raw["microphone"]["source_id"] = "audio:fixture-card"
            Path(inventory).write_text(json.dumps(raw))
            Path(raw["records"]["camera_list"]).write_text(json.dumps([{"stable_id": raw["camera"]["stable_id"]}] * 2))
            with self.assertRaisesRegex(ValueError, "unique"):
                GEN.generate(inventory, root / "ambiguous")


if __name__ == "__main__":
    unittest.main()

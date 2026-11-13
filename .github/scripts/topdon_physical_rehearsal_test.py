#!/usr/bin/env python3
"""Offline plan generation only. All inventory is explicitly synthetic."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

PATH = Path(__file__).resolve().parents[1] / "regression-coverage/manual/topdon-physical-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("topdon_physical_plan", PATH)
GEN = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GEN)


class TopdonPhysicalPlanTests(unittest.TestCase):
    def inventory(self, root):
        def record(name, data):
            path = root / name
            path.write_text(json.dumps(data))
            return str(path)
        cli = root / "inert-cli"
        cli.write_text("#!/bin/sh\nexit 99\n")
        cli.chmod(0o700)
        camera = {"stable_id": "by-id:SYNTHETIC", "serial": "SYNTHETIC", "usb_port": "SYNTHETIC", "bcd_device": "0409"}
        data = {"cli_binary": str(cli), "agent_binary": record("agent", "NOT AN AGENT"), "agent_source_commit": "a" * 40,
                "threshold_celsius": 30, "board": {"selector": "fixture.invalid:50051", "serial": "SYNTHETIC", "owner": "inert", "architecture": "arm64"},
                "camera": camera, "records": {}}
        for key in ("device_info", "installed_binary", "reference_calibration", "reference_setup", "recovery"):
            data["records"][key] = record(key, {"synthetic": "NOT PHYSICAL EVIDENCE"})
        data["records"]["camera_list"] = record("camera.json", [{"stable_id": camera["stable_id"], "thermal": {
            "adapter": "topdon-tc001-hik-v1", "capture_width": 96, "capture_height": 200}}])
        data["records"]["usb_descriptor"] = record("usb.json", {"idVendor": "2bdf", "idProduct": "0102", "bcdDevice": "0409", "serial": "SYNTHETIC", "port": "SYNTHETIC"})
        return record("inventory.json", data), data

    def test_generation_never_executes_and_cannot_claim_physical_success(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, data = self.inventory(root)
            with patch.object(subprocess, "Popen", side_effect=AssertionError("generator executed a command")):
                result = GEN.generate(inventory, root / "plan")
            self.assertEqual(result["hardware_execution"], "not_run")
            self.assertTrue(all(c["status"] == "not_run" for c in result["cases"]))
            app = json.loads((root / "plan/app/wendy.json").read_text())
            self.assertEqual(app["entitlements"], [{"type": "admin"}])
            self.assertEqual(app["env"]["THERMAL_NOTIFY_DELIVERY"], "monitor-only")
            self.assertEqual(app["env"]["THERMAL_CAMERA_STABLE_ID"], data["camera"]["stable_id"])
            self.assertNotEqual(app["appId"], "sh.wendy.examples.thermalnotify")
            self.assertTrue((root / "plan/app/Tests/ThermalNotifyTests/ThermalNotifyTests.swift").is_file())
            commands = json.loads((root / "plan/commands.json").read_text())
            self.assertNotIn("--detach", commands["monitor_only_attached_from_app_directory"])
            self.assertIn("300", commands["monitor_only_attached_from_app_directory"])
            reference = json.loads((root / "plan/reference.template.json").read_text())
            self.assertEqual(reference["samples"], [])
            self.assertIsNone(reference["reference_serial"])
            with self.assertRaises(FileExistsError):
                GEN.generate(inventory, root / "plan")

    def test_different_revision_geometry_and_nan_threshold_are_rejected(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, data = self.inventory(root)
            data["camera"]["bcd_device"] = "9999"
            Path(inventory).write_text(json.dumps(data))
            with self.assertRaisesRegex(ValueError, "0409"):
                GEN.generate(inventory, root / "wrong-firmware")
            data["camera"]["bcd_device"] = "0409"
            data["threshold_celsius"] = float("nan")
            Path(inventory).write_text(json.dumps(data))
            with self.assertRaisesRegex(ValueError, "finite threshold"):
                GEN.generate(inventory, root / "invalid-threshold")
            data["threshold_celsius"] = 30
            Path(inventory).write_text(json.dumps(data))
            Path(data["records"]["camera_list"]).write_text(json.dumps([{"stable_id": data["camera"]["stable_id"], "thermal": {
                "adapter": "topdon-tc001-hik-v1", "capture_width": 256, "capture_height": 392}}]))
            with self.assertRaisesRegex(ValueError, "96x200"):
                GEN.generate(inventory, root / "wrong-mode")


if __name__ == "__main__":
    unittest.main()

"""Preparation and isolation checks only; no Wi-Fi association is performed."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "regression-coverage/manual/orin-wpa3-rehearsal.py"
SPEC = importlib.util.spec_from_file_location("wpa3_rehearsal", SCRIPT)
REHEARSAL = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(REHEARSAL)


class WPA3RehearsalTests(unittest.TestCase):
    def fixture(self, root):
        cli = root / "cli"
        marker = root / "unexpected-device-operation"
        cli.write_text("#!/bin/sh\ntouch " + str(marker) + "\nexit 97\n")
        cli.chmod(0o755)
        artifact = root / "artifact"
        artifact.write_text("nonexecuted fixture artifact")
        raw = {"cli_binary": str(cli), "agent_binary": str(artifact), "agent_source_commit": "a" * 40,
               "recovery_record": str(artifact), "ap_config_record": str(artifact), "boards": []}
        for ordinal, kind in enumerate(sorted(REHEARSAL.KINDS)):
            info = root / (kind + ".json")
            info.write_text(json.dumps({"deviceType": kind, "os": "wendyos", "cpuArchitecture": "arm64",
                                        "version": "fixture", "osVersion": "fixture-os", "jetpackVersion": "fixture-jp"}))
            raw["boards"].append({"selector": "192.0.2." + str(ordinal + 1), "serial": "fixture-" + str(ordinal),
                                  "owner": "fixture operator", "device_info": str(info), "interface": "wlan0",
                                  "station_mac": "02:00:00:00:00:0" + str(ordinal),
                                  "wpa3_ssid": "lab SAE; literal", "mixed_ssid": "lab mixed",
                                  "gateway": "192.0.2.254", "wpa3_bssid": "02:01:02:03:04:05",
                                  "mixed_bssid": "02:01:02:03:04:06"})
        return raw, marker

    def test_prepares_both_radios_without_execution_or_false_passes(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            raw, marker = self.fixture(root)
            inventory = root / "inventory.json"
            inventory.write_text(json.dumps(raw))
            result = REHEARSAL.generate(inventory, root / "output")
            self.assertFalse(marker.exists())
            self.assertEqual(result["hardware_execution"], "not_run")
            self.assertEqual(len(result["cases"]), 14)
            self.assertEqual({case["status"] for case in result["cases"]}, {"not_run"})
            commands = json.loads((root / "output/commands.json").read_text())
            for board in raw["boards"]:
                kind = json.loads(Path(board["device_info"]).read_text())["deviceType"]
                command = commands[kind]["connect-sae"]
                self.assertEqual(command[-1], "lab SAE; literal")
                self.assertNotIn("--password", command)
                self.assertNotIn("--allow-unverified-device", command)
                snapshot = root / "output" / (kind + "-snapshot.sh")
                subprocess.run(["sh", "-n", str(snapshot)], check=True, timeout=5)
                self.assertNotIn("--show-secrets", snapshot.read_text())
                self.assertIn("ping -I", snapshot.read_text())
            self.assertIn("fresh association/reassociation", (root / "output/procedure.txt").read_text())
            with self.assertRaises(FileExistsError):
                REHEARSAL.generate(inventory, root / "output")

    def test_rejects_incomplete_or_ambiguous_physical_inventory(self):
        for fault in ("missing-board", "duplicate-serial", "invalid-interface", "missing-ap", "same-ssid", "bad-source"):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temporary:
                root = Path(temporary)
                raw, marker = self.fixture(root)
                if fault == "missing-board": raw["boards"].pop()
                elif fault == "duplicate-serial": raw["boards"][1]["serial"] = raw["boards"][0]["serial"]
                elif fault == "invalid-interface": raw["boards"][0]["interface"] = "../../user"
                elif fault == "missing-ap": raw["boards"][0]["mixed_bssid"] = "unknown"
                elif fault == "same-ssid": raw["boards"][0]["mixed_ssid"] = raw["boards"][0]["wpa3_ssid"]
                else: raw["agent_source_commit"] = "main"
                inventory = root / "inventory.json"
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError):
                    REHEARSAL.generate(inventory, root / "output")
                self.assertFalse(marker.exists())
                self.assertFalse((root / "output").exists())


if __name__ == "__main__":
    unittest.main()

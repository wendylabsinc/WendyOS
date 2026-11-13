#!/usr/bin/env python3
import importlib.util
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "discovery", Path(__file__).with_name("integration-discover.py")
)
discovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(discovery)


class DiscoveryTests(unittest.TestCase):
    def test_selects_renamed_devices_and_all_required_families(self):
        devices = [("new-pi.local", "raspberry-pi"),
                   ("new-thor.local", "jetson"),
                   ("ci-iq8.local", "qualcomm")]
        self.assertEqual(discovery.select(devices, True), [d[0] for d in devices])

    def test_required_families_take_priority_over_matrix_limit(self):
        devices = [(f"other-{i}.local", None) for i in range(20)]
        devices += [(f"{family}.local", family) for family in discovery.REQUIRED]
        selected = discovery.select(devices, True)
        self.assertEqual(len(selected), 20)
        for family in discovery.REQUIRED:
            self.assertIn(f"{family}.local", selected)

    def test_missing_platform_fails_gate_but_allows_discovery_only(self):
        devices = [("thor.local", "jetson")]
        with self.assertRaisesRegex(ValueError, "raspberry-pi, qualcomm"):
            discovery.select(devices, True)
        self.assertEqual(discovery.select(devices, False), ["thor.local"])
        with self.assertRaises(ValueError):
            discovery.select([], True)

    def test_candidates_validate_and_deduplicate_advertisements(self):
        devices = [{"hostname": host, "isWendyDevice": True} for host in
                   ["New-Pi.local", "new-pi.local", "--help", "a;whoami", "a\nb"]]
        devices.append({"hostname": "not-wendy.local", "isWendyDevice": False})
        self.assertEqual(discovery.candidates({"lanDevices": devices}), ["new-pi.local"])
        self.assertEqual(discovery.candidates({"lanDevices": None}), [])

    def test_platform_names(self):
        for kind, family in [("raspberry-pi-3", "raspberry-pi"),
                             ("raspberry-pi-5", "raspberry-pi"),
                             ("jetson-agx-thor", "jetson"),
                             ("jetson-orin-nano", "jetson"),
                             ("dragonwing-iq-8275", "qualcomm"),
                             ("dragonwing-iq-9075", "qualcomm"),
                             ("qualcomm-iq8", "qualcomm"), ("x86", None)]:
            self.assertEqual(discovery.platform(kind), family)

    def test_public_info_does_not_prove_authorization(self):
        with patch.object(discovery, "cli_json", side_effect=[
            {"deviceType": "jetson-agx-thor"},
            subprocess.CalledProcessError(1, "wendy"),
        ]) as cli:
            self.assertIsNone(discovery.probe("wendy", "thor.local"))
            self.assertEqual(cli.call_count, 2)
            self.assertEqual(cli.call_args.args,
                             ("wendy", "device", "apps", "list", "--device", "thor.local"))

    def test_probe_uses_live_hardware_type(self):
        with patch.object(discovery, "cli_json", side_effect=[
            {"deviceType": "dragonwing-iq-8275"}, [],
        ]):
            self.assertEqual(discovery.probe("wendy", "renamed.local"),
                             ("renamed.local", "qualcomm"))

    def test_timed_out_device_is_skipped(self):
        with patch.object(discovery, "cli_json",
                          side_effect=subprocess.TimeoutExpired("wendy", 45)):
            self.assertIsNone(discovery.probe("wendy", "offline.local"))


if __name__ == "__main__":
    unittest.main()

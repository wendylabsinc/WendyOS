#!/usr/bin/env python3
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch


spec = importlib.util.spec_from_file_location(
    "discovery", Path(__file__).with_name("integration-discover.py")
)
discovery = importlib.util.module_from_spec(spec)
spec.loader.exec_module(discovery)


class DiscoveryTests(unittest.TestCase):
    def discover_process(self, devices, required=True):
        with tempfile.TemporaryDirectory() as directory:
            cli = Path(directory) / "wendy-fixture"
            cli.write_text('''#!/usr/bin/env python3
import json, os, sys
devices = json.loads(os.environ["FIXTURE_DEVICES"])
args = sys.argv[1:]
if args[0] == "discover":
    print(json.dumps({"lanDevices": [{"hostname": name, "isWendyDevice": True} for name in devices]}))
elif args[:2] == ["device", "info"]:
    device = devices[args[args.index("--device") + 1]]
    print(json.dumps({"deviceType": device["type"]}))
elif args[:3] == ["device", "apps", "list"]:
    device = devices[args[args.index("--device") + 1]]
    if not device.get("authorized", True): sys.exit(1)
    print("[]")
else:
    sys.exit("unexpected operation: " + repr(args))
''')
            cli.chmod(0o755)
            args = [os.sys.executable, str(Path(__file__).with_name("integration-discover.py")), "--wendy", str(cli)]
            if required:
                args.append("--require-platforms")
            return subprocess.run(args, env=dict(os.environ, FIXTURE_DEVICES=json.dumps(devices)),
                                  capture_output=True, text=True, timeout=15)

    def test_cli_gate_accepts_renamed_thor_and_refuses_missing_or_inaccessible_families(self):
        devices = {"renamed-pi.local": {"type": "raspberry-pi-5"},
                   "renamed-thor.local": {"type": "jetson-agx-thor"},
                   "renamed-iq.local": {"type": "dragonwing-iq-8275"}}
        result = self.discover_process(devices)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout), list(devices))
        for state in ("missing", "unauthorized"):
            with self.subTest(state=state):
                incomplete = dict(devices)
                if state == "missing":
                    del incomplete["renamed-thor.local"]
                else:
                    incomplete["renamed-thor.local"] = {"type": "jetson-agx-thor", "authorized": False}
                failed = self.discover_process(incomplete)
                self.assertEqual(failed.returncode, 1, failed.stderr)
                self.assertEqual(failed.stdout, "")
                self.assertIn("no accessible device for required platform(s): jetson", failed.stderr)
                discovery_only = self.discover_process(incomplete, required=False)
                self.assertEqual(discovery_only.returncode, 0, discovery_only.stderr)
                self.assertEqual(json.loads(discovery_only.stdout), ["renamed-pi.local", "renamed-iq.local"])

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

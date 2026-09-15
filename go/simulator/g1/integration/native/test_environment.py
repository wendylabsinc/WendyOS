"""Managed-VM admission remains strict without initializing ROS or DDS."""

import unittest
from unittest.mock import patch

from common import SimulatorAPI, require_test_environment


def identity(**changes):
    return {"simulation": True, "robot": "g1", "profile_version": 1, "robot_kind": "g1",
            "vm_name": "g1-sim", "dds_isolation": "udp-rtps-loopback", "clock_mode": "device",
            "policy_bundle": "g1-29dof-velocity-v0-4960b847-v1", "source_digest": "sha256:" + "a" * 64,
            **changes}


class EnvironmentTests(unittest.TestCase):
    def api(self, **kwargs):
        api = SimulatorAPI("http://127.0.0.1:8890", **kwargs)
        api._request = lambda _: (200, identity())
        return api

    def test_default_still_requires_an_isolated_namespace(self):
        with patch("common.require_isolated_network", side_effect=AssertionError("not isolated")) as guard:
            with self.assertRaisesRegex(AssertionError, "not isolated"):
                require_test_environment(self.api())
            guard.assert_called_once()

    def test_named_managed_vm_pins_the_first_valid_source(self):
        api = self.api(vm_name="g1-sim")
        with patch("common.require_isolated_network") as guard:
            require_test_environment(api)
            guard.assert_not_called()
        self.assertEqual(api.source_digest, "sha256:" + "a" * 64)
        api._request = lambda _: (200, identity(source_digest="sha256:" + "b" * 64))
        with self.assertRaisesRegex(AssertionError, "source changed"):
            api.status()

    def test_missing_mismatched_or_unmanaged_identity_is_rejected(self):
        for changes in ({"vm_name": "another-vm"}, {"dds_isolation": "unmanaged"},
                        {"robot_kind": "go2"}, {"robot": "go2"}, {"source_digest": ""}, {"source_digest": None},
                        {"simulation": False}, {"profile_version": True}, {"clock_mode": "simulation"}):
            with self.subTest(changes=changes):
                api = self.api(vm_name="g1-sim")
                api._request = lambda _, changes=changes: (200, identity(**changes))
                with self.assertRaises(AssertionError):
                    require_test_environment(api)

    def test_explicit_source_pin_and_loopback_url_are_enforced(self):
        api = self.api(vm_name="g1-sim", source_digest="sha256:" + "b" * 64)
        with self.assertRaisesRegex(AssertionError, "requested pin"):
            api.status()
        for url in ("http://192.168.123.161:8890", "http://127.0.0.1:8890/redirect", "https://127.0.0.1"):
            with self.assertRaises(AssertionError):
                SimulatorAPI(url, vm_name="g1-sim")


if __name__ == "__main__":
    unittest.main()

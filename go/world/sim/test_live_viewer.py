"""Read-only viewer regressions: foreign poses cannot become a live scene."""
import copy
import unittest
from unittest.mock import patch
from sim.live_viewer import validate_snapshot, NoRedirect, main


class ViewerValidationTests(unittest.TestCase):
    def test_cocoa_launch_argument_reaches_viewer_without_broad_argument_bypass(self):
        args = ["--ready", "ready.json", "--package", "world.json"]
        with patch("sim.live_viewer.run") as run:
            main(args + ["-ApplePersistenceIgnoreState", "YES"])
            run.assert_called_once()
        for extra in (["-ApplePersistenceIgnoreState", "NO"], ["--unknown"]):
            with patch("sim.live_viewer.run") as run, patch("sys.stderr"), self.assertRaises(SystemExit):
                main(args + extra)
            run.assert_not_called()

    def setUp(self):
        self.ready = {"run_id": "demo"}
        self.package = {"site_id": "synthetic-office", "frame_revision": "office-frame-v1", "_digest": "abc"}
        self.snapshot = {"schema_version": "wendy.sim.live.v1", "run_id": "demo", "boot_id": "boot", "sequence": 4,
            "state": {"synthetic": True, "site_id": "synthetic-office", "boot_id": "boot",
                "frame": {"id": "office-floor", "revision": "office-frame-v1"},
                "provenance": {"mode": "simulated", "package_sha256": "abc"},
                "position_m": [-1.2, .2, 0], "velocity_mps": [.1, 0, 0], "simulation_time_s": 2.5}}

    def check(self, value, previous=None):
        return validate_snapshot(value, self.ready, self.package, previous)

    def test_copied_pose_and_duplicate_do_not_advance_time(self):
        first = self.check(self.snapshot)
        self.assertEqual(first, self.check(self.snapshot, first))
        self.assertEqual(first["position"], [-1.2, .2, 0])

    def test_foreign_identity_and_provenance_refused(self):
        mutations = [lambda x: x.update(run_id="other"), lambda x: x["state"].update(synthetic=False),
            lambda x: x["state"]["provenance"].update(mode="measured"),
            lambda x: x["state"]["provenance"].update(package_sha256="different"),
            lambda x: x["state"]["frame"].update(revision="other")]
        for mutation in mutations:
            value = copy.deepcopy(self.snapshot)
            mutation(value)
            with self.subTest(value=value), self.assertRaises(ValueError):
                self.check(value)

    def test_restart_and_clock_regression_refused(self):
        previous = self.check(self.snapshot)
        for key, value in (("seconds", 3), ("sequence", 5), ("boot_id", "different")):
            earlier = {**previous, key: value}
            with self.subTest(key=key), self.assertRaises(ValueError):
                self.check(self.snapshot, earlier)

    def test_invalid_pose_refused(self):
        for position in ([float("nan"), .2, 0], [True, .2, 0], [3, .2, 0], [0, .3, 0], [0, .2, 1], [0]):
            value = copy.deepcopy(self.snapshot)
            value["state"]["position_m"] = position
            with self.subTest(position=position), self.assertRaises(ValueError):
                self.check(value)

    def test_redirect_cannot_forward_sensor_token(self):
        with self.assertRaises(ValueError):
            NoRedirect().redirect_request(None, None, 302, "redirect", {}, "https://example.org")


if __name__ == "__main__":
    unittest.main()

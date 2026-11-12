#!/usr/bin/env python3
"""Portable safety checks; booted systemd guest evidence is supplied by hosted CI."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name("run-installed-linux-package-guests.py")
SPEC = importlib.util.spec_from_file_location("package_guests", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class GuestRunnerTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="wendy-guest-runner-test-")
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        with mock.patch.dict(os.environ, {"RUNNER_TEMP": str(self.root)}):
            self.guest = MODULE.Guests(self.root, "amd64", 30)

    def test_real_command_retains_both_channels_and_reports_failure(self):
        log = self.root / "command.log"
        self.guest.command([sys.executable, "-c", "import sys; print('out'); print('err', file=sys.stderr)"], log=log)
        self.assertIn("out\n", log.read_text())
        self.assertIn("err\n", log.read_text())
        with self.assertRaisesRegex(RuntimeError, "intended failure"):
            self.guest.command([sys.executable, "-c", "import sys; print('intended failure'); sys.exit(3)"], log=log)
        self.assertIn("intended failure", log.read_text())

    def test_real_command_timeout_and_overall_deadline_are_bounded(self):
        start = time.monotonic()
        with self.assertRaises(subprocess.TimeoutExpired):
            self.guest.command([sys.executable, "-c", "import time; time.sleep(30)"], timeout=0.1)
        self.assertLess(time.monotonic() - start, 5)
        self.guest.deadline = time.monotonic() - 1
        marker = self.root / "must-not-exist"
        with self.assertRaisesRegex(RuntimeError, "deadline"):
            self.guest.command([sys.executable, "-c", f"open({str(marker)!r},'w').close()"])
        self.assertFalse(marker.exists())

    def test_unknown_systemd_unit_is_never_stopped(self):
        response = subprocess.CompletedProcess([], 0, stdout="LoadState=loaded\nDescription=another workflow\n", stderr="")
        with mock.patch.object(self.guest, "captured", return_value=response), mock.patch.object(self.guest, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "unknown ownership"):
                self.guest.stop_unit("wendy-pkg-0123456789abcdef-case.service")
            command.assert_not_called()

    def test_owned_unit_is_tracked_before_a_failed_launch(self):
        name = "wendy-pkg-0123456789abcdef-case"
        with mock.patch.object(self.guest, "command", side_effect=subprocess.TimeoutExpired("systemd-run", 30)) as command:
            with self.assertRaises(subprocess.TimeoutExpired):
                self.guest.start_unit(name, ["/bin/true"], seconds=20, wait=False, log=self.root / "unit.log")
        self.assertIn(name + ".service", self.guest.units)
        args = command.call_args.args[0]
        self.assertIn("--property=RuntimeMaxSec=20s", args)
        self.assertIn("--property=KillMode=control-group", args)
        self.assertIn("--description=" + self.guest.description, args)

    def test_unknown_docker_container_is_never_removed(self):
        record = [{"Config": {"Labels": {"wendy.package-fixture": "someone-else"}}}]
        response = subprocess.CompletedProcess([], 0, stdout=json.dumps(record), stderr="")
        with mock.patch.object(self.guest, "captured", return_value=response), mock.patch.object(self.guest, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "unknown ownership"):
                self.guest.remove_container("wendy-pkg-export")
            command.assert_not_called()

    def test_mounted_root_or_unowned_directory_is_never_deleted(self):
        root = self.guest.work / "guest"
        root.mkdir()
        marker = root / "retain"
        marker.write_text("evidence")
        mounts = {"filesystems": [{"target": "/", "children": [{"target": str(root / "evidence")}]}]}
        response = subprocess.CompletedProcess([], 0, stdout=json.dumps(mounts), stderr="")
        with mock.patch.object(self.guest, "captured", return_value=response):
            with self.assertRaisesRegex(RuntimeError, "still-mounted"):
                self.guest.remove_root(root)
            with self.assertRaisesRegex(RuntimeError, "outside owned"):
                self.guest.remove_root(self.root)
        self.assertEqual(marker.read_text(), "evidence")
        with mock.patch.object(self.guest, "captured", return_value=subprocess.CompletedProcess([], 0, stdout='{"filesystems": []}', stderr="")):
            self.guest.remove_root(root)
        self.assertFalse(root.exists())

    def test_selects_exact_native_artifacts_and_rejects_ambiguity(self):
        for name in ("wendy_1_amd64.deb", "wendy-agent_1_amd64.deb", "wendy-1-1.x86_64.rpm", "wendy-agent-1-1.x86_64.rpm",
                     "wendy_1_arm64.deb", "wendy-agent_1_arm64.deb"):
            (self.root / name).write_bytes(b"fixture")
        selected = MODULE.select_packages(self.root, "amd64")
        self.assertEqual(selected["rpm"]["cli"].name, "wendy-1-1.x86_64.rpm")
        with self.assertRaisesRegex(RuntimeError, "exactly one native"):
            MODULE.select_packages(self.root, "arm64")
        (self.root / "wendy_2_amd64.deb").write_bytes(b"other build")
        with self.assertRaisesRegex(RuntimeError, "exactly one native"):
            MODULE.select_packages(self.root, "amd64")

    def test_non_hosted_execution_is_rejected_before_any_subprocess(self):
        with mock.patch.object(MODULE.sys, "platform", "linux"), mock.patch.object(MODULE.os, "geteuid", return_value=0):
            with mock.patch.dict(os.environ, {"GITHUB_ACTIONS": "false"}), mock.patch.object(subprocess, "run") as run:
                with self.assertRaisesRegex(RuntimeError, "outside a GitHub-hosted"):
                    MODULE.hosted_guard()
                run.assert_not_called()

    def test_generated_dependency_scripts_have_valid_bash_syntax(self):
        for spec in MODULE.ROOTS:
            dependencies = ["containerd, xdg-dbus-proxy, dbus, systemd"] if spec["format"] == "deb" else ["containerd", "libc.so.6()(64bit)", "bash >= 5"]
            script = self.root / (spec["name"] + ".sh")
            script.write_text(MODULE.provision_script(spec, dependencies))
            subprocess.run(["bash", "-n", str(script)], check=True, capture_output=True)


if __name__ == "__main__":
    unittest.main()

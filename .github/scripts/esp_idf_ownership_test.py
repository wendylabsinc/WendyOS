#!/usr/bin/env python3
"""Portable cleanup safety checks; actual ESP-IDF execution requires Linux CI."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest import mock

SCRIPT = Path(__file__).with_name("test-esp-idf-ownership.py")
SPEC = importlib.util.spec_from_file_location("idf_ownership", SCRIPT)
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


class CleanupTests(unittest.TestCase):
    def test_only_private_workspace_container_is_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory).resolve() / "owned"
            work.mkdir()
            calls = []

            def checked(*args, **kwargs):
                calls.append(args)
                return "wendy-idf-owned\nwendy-idf-other\n" if args[1] == "ps" else ""

            def captured(*args, **kwargs):
                source = work / "tmp/project" if args[-1] == "wendy-idf-owned" else work.parent / "other"
                record = [{"Mounts": [{"Source": str(source)}], "Config": {"Image": MODULE.IMAGE}}]
                return subprocess.CompletedProcess(args, 0, json.dumps(record), "")

            report = {"cleanup_errors": []}
            with mock.patch.object(MODULE, "checked", side_effect=checked), mock.patch.object(MODULE, "captured", side_effect=captured):
                MODULE.cleanup(work, False, report)
            self.assertEqual(report["cleanup_errors"], [])
            self.assertFalse(work.exists())
            self.assertEqual([call for call in calls if call[1] == "rm"], [("docker", "rm", "--force", "wendy-idf-owned")])

    def test_unknown_shared_mount_retains_workspace_without_stopping_container(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory).resolve()
            record = [{"Mounts": [{"Source": str(work / "project")}, {"Source": "/unrelated"}], "Config": {"Image": MODULE.IMAGE}}]
            result = subprocess.CompletedProcess([], 0, json.dumps(record), "")
            report = {"cleanup_errors": []}
            with mock.patch.object(MODULE, "checked", return_value="wendy-idf-unknown\n") as checked, mock.patch.object(MODULE, "captured", return_value=result):
                MODULE.cleanup(work, False, report)
            self.assertEqual(checked.call_count, 1)
            self.assertTrue(work.exists())
            self.assertIn("unexpected container", report["cleanup_errors"][0])

    def test_inspect_failure_is_not_mistaken_for_absent_container(self):
        with tempfile.TemporaryDirectory() as directory:
            work = Path(directory).resolve()
            result = subprocess.CompletedProcess([], 1, "", "Docker daemon unavailable")
            report = {"cleanup_errors": []}
            with mock.patch.object(MODULE, "checked", return_value="wendy-idf-unknown\n"), mock.patch.object(MODULE, "captured", return_value=result):
                MODULE.cleanup(work, False, report)
            self.assertTrue(work.exists())
            self.assertIn("cannot inspect", report["cleanup_errors"][0])


if __name__ == "__main__":
    unittest.main()

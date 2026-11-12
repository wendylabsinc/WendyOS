#!/usr/bin/env python3
"""Offline launcher checks. No ALSA, PipeWire, account or kernel operations."""

import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("test-sensorlink-audio-loopback.sh")


class KernelFixtureLauncherTests(unittest.TestCase):
    def test_non_hosted_invocation_refuses_before_setup(self):
        env = dict(os.environ, GITHUB_ACTIONS="", RUNNER_ENVIRONMENT="")
        result = subprocess.run(["bash", str(SCRIPT)], env=env, capture_output=True, text=True, timeout=5)
        self.assertEqual(result.returncode, 2)
        self.assertIn("requires the disposable GitHub-hosted Linux runner", result.stderr)

    def test_completed_or_failed_test_reaps_its_abandoned_child(self):
        launcher = SCRIPT.read_text().split("<<'PY_CAPTURE'\n", 1)[1].split("\nPY_CAPTURE", 1)[0]
        for code in (0, 17):
            with self.subTest(code=code), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                launch = root / "launcher.py"
                launch.write_text(launcher)
                child_pid = root / "child.pid"
                # This test binary deliberately exits without waiting for its
                # child, like a panic that bypasses aplay/arecord cleanup.
                fake_test = root / "test-binary"
                fake_test.write_text(
                    f"#!{sys.executable}\n"
                    "import os, subprocess, sys\n"
                    "child = subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(60)'])\n"
                    "with open(os.environ['FIXTURE_CHILD_PID'], 'w') as out: out.write(str(child.pid))\n"
                    f"sys.exit({code})\n"
                )
                fake_test.chmod(0o700)
                pid = None
                try:
                    result = subprocess.run([sys.executable, str(launch), str(fake_test), "offline"],
                                            env=dict(os.environ, FIXTURE_CHILD_PID=str(child_pid)),
                                            capture_output=True, text=True, timeout=10)
                    self.assertEqual(result.returncode, code, result.stderr)
                    pid = int(child_pid.read_text())
                    # A reparented zombie may briefly await init; it cannot
                    # retain audio descriptors or keep the fixture alive.
                    state = subprocess.run(["ps", "-o", "stat=", "-p", str(pid)], capture_output=True, text=True, timeout=2)
                    self.assertTrue(not state.stdout.strip() or state.stdout.strip().startswith("Z"), state.stdout)
                finally:
                    if pid is None and child_pid.exists():
                        pid = int(child_pid.read_text())
                    if pid is not None:
                        try:
                            os.kill(pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass


if __name__ == "__main__":
    unittest.main()

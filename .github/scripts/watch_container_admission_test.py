"""Refuse accidental local kernel setup before invoking any mutation command."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("test-watch-container.sh")


class WatchContainerAdmissionTests(unittest.TestCase):
    def test_nonhosted_invocation_cannot_reach_kernel_or_cleanup_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / "unexpected-command"
            for command in ("unshare", "mount", "containerd", "ctr", "ip", "rm", "timeout"):
                target = root / command
                target.write_text('#!/bin/sh\nprintf invoked > "$WATCH_MARKER"\nexit 99\n')
                target.chmod(0o755)
            environment = dict(os.environ, GITHUB_ACTIONS="false", RUNNER_ENVIRONMENT="self-hosted",
                               PATH=str(root)+os.pathsep+os.environ.get("PATH", ""), WATCH_MARKER=str(marker))
            for case in ("normal", "multiapp-normal", "early-readiness", "ignored-service-env", "retained-isolation"):
                with self.subTest(case=case):
                    result = subprocess.run(["bash", str(SCRIPT), "/absent/test", "/absent/prepared", case],
                                            env=environment, capture_output=True, text=True, timeout=5)
                    self.assertEqual(result.returncode, 2, result.stderr)
                    self.assertIn("disposable hosted Linux", result.stderr)
                    self.assertFalse(marker.exists())


if __name__ == "__main__":
    unittest.main()

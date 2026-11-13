"""Exercise runner cleanup with a fake app quit command; never launch the runner."""

import os
from pathlib import Path
import subprocess
import tempfile
import unittest


class ManagedAgentCleanupTests(unittest.TestCase):
    def test_cache_fixture_rejects_socket_override_before_building(self):
        runner = Path(__file__).with_name("E2ETest.sh")
        with tempfile.TemporaryDirectory() as directory:
            result = subprocess.run([
                "bash", str(runner), "--output-dir", directory,
                "--device-cache-fixture", str(Path(directory) / "fixture.json"),
                "--device-address", "cache-test.local", "--no-managed-agent",
            ], env={**os.environ, "WENDY_AGENT_SOCKET": "/tmp/unrelated-agent.sock"},
                capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 64, result.stderr)
            self.assertIn("unset WENDY_AGENT_SOCKET", result.stderr)
            self.assertNotIn("Building", result.stdout)

    def test_unmanaged_macos_cleanup_never_quits_an_app_or_opens_a_log(self):
        source = Path(__file__).with_name("E2ETest.sh").read_text()
        function = "stop_managed_agent() {" + source.split("stop_managed_agent() {", 1)[1].split("\n}\n", 1)[0] + "\n}"
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            quit_script = root / "swift/Scripts/Quit.sh"
            quit_script.parent.mkdir(parents=True)
            quit_script.write_text('#!/bin/bash\ntouch "$REPO_DIR/quit-called"\n')
            quit_script.chmod(0o700)
            env = {**os.environ, "REPO_DIR": directory, "RUN_DIR": str(root / "run")}
            script = function + '\nmanaged_agent_is_macos() { return 0; }\nstop_managed_agent\n'
            for logs_exist in [False, True]:
                if logs_exist:
                    (root / "run/managed-agent").mkdir(parents=True)
                result = subprocess.run(["bash", "-eu", "-c", script], env={**env, "MANAGED_AGENT": "false"}, capture_output=True, text=True, timeout=5)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertEqual(result.stderr, "")
                self.assertFalse((root / "quit-called").exists())
                self.assertFalse((root / "run/managed-agent/quit.log").exists())
            # Positive control: only the fake command runs for owned agents.
            result = subprocess.run(["bash", "-eu", "-c", script], env={**env, "MANAGED_AGENT": "true"}, capture_output=True, text=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertTrue((root / "quit-called").exists())


if __name__ == "__main__":
    unittest.main()

"""Portable refusal checks; these never create a network namespace."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("test-rtps-multicast-isolation.sh")


class RTPSNamespaceGuardTests(unittest.TestCase):
    def test_local_and_self_hosted_invocations_refuse_before_mutation(self):
        for actions, environment, mode in (("false", "github-hosted", None), ("true", "self-hosted", "--namespace"), ("true", "self-hosted", "--inside")):
            with self.subTest(actions=actions, environment=environment, mode=mode), tempfile.TemporaryDirectory() as directory:
                output = Path(directory) / "must-not-be-created"
                env = os.environ.copy()
                env.update(GITHUB_ACTIONS=actions, RUNNER_ENVIRONMENT=environment)
                command = ["bash", str(SCRIPT), *([mode] if mode else []), str(output)]
                result = subprocess.run(command, env=env, text=True, capture_output=True, timeout=3)
                self.assertEqual(result.returncode, 2)
                self.assertIn("disposable GitHub-hosted Linux runner", result.stderr)
                self.assertFalse(output.exists())


if __name__ == "__main__":
    unittest.main()

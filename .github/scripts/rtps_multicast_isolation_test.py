"""Portable refusal checks; these never create a network namespace."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("test-rtps-multicast-isolation.sh")


class RTPSNamespaceGuardTests(unittest.TestCase):
    def test_inside_requires_distinct_namespaces_before_root_check(self):
        for kind in ('mnt', 'net', 'pid'):
            with self.subTest(kind=kind), tempfile.TemporaryDirectory() as directory:
                env = dict(os.environ, GITHUB_ACTIONS='true', RUNNER_ENVIRONMENT='github-hosted')
                real_namespaces = Path('/proc/self/ns/mnt').exists()
                if not real_namespaces:
                    # Exercise the shell guard on macOS too. Linux uses real
                    # namespace IDs; this does not claim namespace isolation.
                    for name, body in [('uname', 'echo Linux'), ('readlink', 'echo fixture-namespace')]:
                        tool = Path(directory) / name
                        tool.write_text('#!/bin/sh\n' + body + '\n'); tool.chmod(0o755)
                    env['PATH'] = directory + os.pathsep + env['PATH']
                for other in ('mnt', 'net', 'pid'):
                    env['WENDY_RTPS_PARENT_' + other] = (os.readlink('/proc/self/ns/' + other) if real_namespaces else 'fixture-namespace') if other == kind else 'different'
                result = subprocess.run(['bash', str(SCRIPT), '--inside', '/not-executed'],
                                        env=env, text=True, capture_output=True, timeout=3)
                self.assertEqual(result.returncode, 2)
                self.assertIn('requires a distinct ' + kind + ' namespace', result.stderr)

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

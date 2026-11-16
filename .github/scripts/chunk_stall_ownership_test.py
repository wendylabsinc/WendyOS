#!/usr/bin/env python3
"""Offline safety admission checks; never invokes sudo or changes users."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('test-chunk-stall-ownership.sh')


class OwnershipAdmissionTests(unittest.TestCase):
    def test_local_environment_refuses_before_creating_paths_or_running_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / 'unexpected-operation'
            for name in ('sudo', 'go', 'mkdir', 'mktemp'):
                tool = root / name
                tool.write_text(f'#!/bin/sh\ntouch "{marker}"\nexit 97\n')
                tool.chmod(0o700)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'], GITHUB_ACTIONS='', RUNNER_ENVIRONMENT='')
            result = subprocess.run(['bash', str(SCRIPT), str(root / 'output')], env=env, capture_output=True, text=True, timeout=3)
            self.assertEqual(result.returncode, 2)
            self.assertIn('disposable GitHub-hosted Linux runner', result.stderr)
            self.assertFalse(marker.exists())
            self.assertFalse((root / 'output').exists())

    def test_non_hosted_runner_is_rejected_even_with_actions_flag(self):
        result = subprocess.run(['bash', str(SCRIPT), '/unused'],
            env=dict(os.environ, GITHUB_ACTIONS='true', RUNNER_ENVIRONMENT='self-hosted'),
            capture_output=True, text=True, timeout=3)
        self.assertEqual(result.returncode, 2)
        self.assertIn('disposable GitHub-hosted Linux runner', result.stderr)


if __name__ == '__main__':
    unittest.main()

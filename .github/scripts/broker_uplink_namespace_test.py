#!/usr/bin/env python3
"""Check fixture admission without accessing privileged host networking."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('test-broker-uplink-namespace.sh')

class BrokerUplinkAdmissionTests(unittest.TestCase):
    def test_local_machine_refuses_all_entry_points(self):
        for args in (['output'], ['--namespace', '/unused'], ['--inside', '/unused']):
            with self.subTest(args=args):
                result = subprocess.run(['bash', str(SCRIPT), *args],
                    env=dict(os.environ, GITHUB_ACTIONS='', RUNNER_ENVIRONMENT=''),
                    capture_output=True, text=True, timeout=3)
                self.assertEqual(result.returncode, 2)
                self.assertIn('disposable GitHub-hosted Linux runner', result.stderr)

    def test_inside_refuses_shared_namespaces_before_mutations(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            marker = root / 'unexpected-mutation'
            for name, body in {
                'uname': 'echo Linux',
                'readlink': 'echo shared-parent-namespace',
                **{name: f'touch "{marker}"; exit 97' for name in ('mount', 'ip', 'sysctl')},
            }.items():
                path = root / name
                path.write_text('#!/bin/sh\n' + body + '\n')
                path.chmod(0o700)
            env = dict(os.environ, PATH=str(root)+os.pathsep+os.environ['PATH'],
                       GITHUB_ACTIONS='true', RUNNER_ENVIRONMENT='github-hosted',
                       WENDY_UPLINK_PARENT_mnt='shared-parent-namespace',
                       WENDY_UPLINK_PARENT_net='shared-parent-namespace',
                       WENDY_UPLINK_PARENT_pid='shared-parent-namespace')
            result = subprocess.run(['bash', str(SCRIPT), '--inside', '/unused'],
                                    env=env, capture_output=True, text=True, timeout=3)
            self.assertEqual(result.returncode, 2)
            # Names the namespace guard: the EUID refusal alone exits 2 silently.
            self.assertIn('namespaces shared with its parent', result.stderr)
            self.assertFalse(marker.exists())

if __name__ == '__main__':
    unittest.main()

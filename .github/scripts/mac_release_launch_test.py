"""Exercise release launch observation and cleanup without launching a real app."""

import json
import os
from pathlib import Path
import shlex
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'swift/Scripts/SmokeTestMacReleaseLaunch.sh'

TOOL = r'''
import json, os, sys
from pathlib import Path
state_path = Path(os.environ['SMOKE_FIXTURE_STATE'])
state = json.loads(state_path.read_text())
tool, *args = sys.argv[1:]
state['calls'].append([tool, *args])
code = 0
if tool == 'xcrun':
    assert args[0] == 'swiftc'
    target = Path(args[args.index('-o') + 1])
    target.write_text(os.environ['SMOKE_FIXTURE_OBSERVER'])
    target.chmod(0o755)
elif tool == 'observer':
    action, candidate = args
    assert candidate == state['candidate']
    if action == 'quit':
        state['started'] = False
    elif action == 'exclusive':
        code = int(state['foreign'])
    elif action == 'running':
        present = False
        if state['started']:
            present = state['samples'][min(state['index'], len(state['samples']) - 1)]
            state['index'] += 1
        code = int(not present)
    elif action != 'diagnose':
        raise AssertionError(action)
elif tool == 'open':
    assert args == ['-n', '-g', state['candidate']]
    state['started'] = True
elif tool not in ('sleep', 'log'):
    raise AssertionError('Unexpected native tool: ' + tool)
state_path.write_text(json.dumps(state))
sys.exit(code)
'''


class MacReleaseLaunchTests(unittest.TestCase):
    def run_launch(self, samples, foreign=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            state_path = root / 'state.json'
            candidate = str(root / 'Candidate App.app')
            state_path.write_text(json.dumps(dict(candidate=candidate, samples=samples, foreign=foreign,
                                                  started=False, index=0, calls=[])))
            driver = root / 'tool.py'
            driver.write_text(TOOL)
            prefix = '#!/bin/sh\nexec ' + shlex.quote(sys.executable) + ' ' + shlex.quote(str(driver)) + ' '
            for tool in ('xcrun', 'open', 'sleep', 'log', 'osascript'):
                path = root / tool
                path.write_text(prefix + tool + ' "$@"\n')
                path.chmod(0o755)
            observer = prefix + 'observer "$@"\n'
            env = dict(os.environ, PATH=str(root) + os.pathsep + os.environ['PATH'],
                       SMOKE_FIXTURE_STATE=str(state_path), SMOKE_FIXTURE_OBSERVER=observer)
            result = subprocess.run(['bash', str(SCRIPT), candidate], env=env,
                                    capture_output=True, text=True, timeout=15)
            return result, json.loads(state_path.read_text())

    def test_launchd_handoff_can_temporarily_remove_the_first_instance(self):
        result, state = self.run_launch([True, False, False, True, True, True, True])
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(state['index'], 7)
        self.assertFalse(state['started'], 'Successful launch must still clean up the candidate')

    def test_never_started_and_early_exit_are_failures(self):
        for samples in ([False], [True, False]):
            with self.subTest(samples=samples):
                result, state = self.run_launch(samples)
                self.assertNotEqual(result.returncode, 0)
                self.assertIn('did not remain running', result.stderr)
                self.assertEqual(state['index'], 60)

    def test_another_bundle_cannot_satisfy_launch_or_be_quit(self):
        result, state = self.run_launch([True], foreign=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn('Another WendyAgentMac bundle is running', result.stderr)
        self.assertTrue(state['foreign'])
        self.assertFalse(any(call[0] == 'open' for call in state['calls']))
        self.assertFalse(any(call[0] == 'osascript' for call in state['calls']))


if __name__ == '__main__':
    unittest.main()

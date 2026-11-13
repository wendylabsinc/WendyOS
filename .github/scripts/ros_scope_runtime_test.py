#!/usr/bin/env python3
"""Exercise the hosted wrapper's conservative cleanup on real private files."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('test-ros-scope-runtime.sh')
CLEANUP = 'cleanup() {\n'+SCRIPT.read_text().split('  cleanup() {\n', 1)[1].split('\n  }\n  trap cleanup EXIT', 1)[0]+'\n}\n'


class ROSScopeCleanupTests(unittest.TestCase):
    def exercise(self, interrupted, drained):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            work = root/'owned-work'
            work.mkdir()
            (work/'keep').write_text('fixture data')
            neighbor = root/'unrelated'
            neighbor.write_text('untouched')
            shell = ('set -euo pipefail\nprepared="$1"; work="$1/owned-work"; case_name=normal\n'
                     f'namespace_started=1; namespace_drained={drained}; interrupted={interrupted}\n'
                     + CLEANUP + 'cleanup\n')
            result = subprocess.run(['bash', '-c', shell, 'cleanup-check', str(root)],
                                    capture_output=True, text=True, timeout=5, env=os.environ)
            retained = bool(interrupted or not drained)
            self.assertEqual(result.returncode, 1 if retained else 0, result.stdout+result.stderr)
            self.assertEqual(work.exists(), retained)
            self.assertEqual(neighbor.read_text(), 'untouched')
            evidence = json.loads((root/'normal-cleanup.json').read_text())
            self.assertEqual(evidence['status'], 'retained-uncertain' if retained else 'removed')
            if retained:
                self.assertEqual((work/'keep').read_text(), 'fixture data')

    def test_unknown_namespace_drain_retains_owned_work(self):
        self.exercise(interrupted=0, drained=0)

    def test_direct_interrupt_retains_owned_work(self):
        self.exercise(interrupted=1, drained=1)

    @unittest.skipUnless(shutil.which('findmnt'), 'normal cleanup requires Linux findmnt')
    def test_reaped_namespace_removes_only_owned_work(self):
        self.exercise(interrupted=0, drained=1)


if __name__ == '__main__':
    unittest.main()

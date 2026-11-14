#!/usr/bin/env python3
"""Exercise the hosted wrapper's conservative cleanup on real private files."""
import json
import hashlib
import sys
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


class ROSScopePreparedSourceTests(unittest.TestCase):
    def check(self, changed=None):
        # Execute the exact wrapper admission, without starting a namespace,
        # image, ROS process or privileged command.
        code = SCRIPT.read_text().split("<<'VERIFY'\n", 1)[1].split("\nVERIFY", 1)[0]
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            prepared, source = root/'prepared', root/'source'
            (prepared/'context').mkdir(parents=True)
            source.mkdir()
            staged = prepared/'context'/'production-observe.py'
            tracked = source/'observe.py'
            archive = prepared/'ros-image.tar'
            for path, data in [(staged, b'exact staged source'), (tracked, b'exact tracked source'),
                               (archive, b'inert image identity test; not an OCI image')]:
                path.write_bytes(data)
            sha = lambda path: hashlib.sha256(path.read_bytes()).hexdigest()
            (prepared/'manifest.json').write_text(json.dumps({
                'status': 'prepared', 'cleanup_errors': [],
                'staged_sha256': {staged.name: sha(staged)},
                'source_sha256': {tracked.name: sha(tracked)},
                'archive_sha256': sha(archive)}))
            if changed:
                {'staged': staged, 'source': tracked, 'archive': archive}[changed].write_bytes(b'changed')
            result = subprocess.run([sys.executable, '-', str(prepared), str(source)],
                                    input=code, capture_output=True, text=True, timeout=5, cwd=root)
            self.assertEqual(result.returncode == 0, changed is None, result.stderr)
            if changed:
                self.assertIn({'staged': 'staged runtime source changed',
                               'source': 'prepared fixture differs from current source',
                               'archive': 'ROS image archive changed'}[changed], result.stderr)

    def test_admits_bound_inputs_from_an_unrelated_working_directory(self):
        self.check()

    def test_rejects_each_changed_runtime_input_before_namespace_start(self):
        for changed in ('staged', 'source', 'archive'):
            with self.subTest(changed=changed):
                self.check(changed)


if __name__ == '__main__':
    unittest.main()

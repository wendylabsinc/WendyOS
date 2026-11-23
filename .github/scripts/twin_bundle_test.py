"""Verify Apple builds consume complete, integrity-checked Linux bundles."""
import gzip
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

from ci_execution_test import workflow

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / 'go/macos/Scripts/bundle-twin.py'


class TwinBundleTests(unittest.TestCase):
    def test_native_linux_builds_supply_both_apple_platforms(self):
        jobs = workflow('go-tests.yml')['jobs']
        build = jobs['twin-linux-bundles']
        self.assertEqual({entry['arch'] for entry in build['strategy']['matrix']['include']}, {'arm64', 'amd64'})
        apple = jobs['native-app-regressions']
        self.assertEqual(apple['needs'], 'twin-linux-bundles')
        self.assertIn('WENDY_TWIN_BUNDLE_DIR', apple['env'])
        download = next(step for step in apple['steps'] if step.get('name') == 'Download Linux bundles built from this commit')
        self.assertEqual(download['with']['pattern'], 'twin-linux-bundle-*')
        self.assertNotIn('run-id', download['with'])
        self.assertNotIn('repository', download['with'])

    def test_prebuilt_bundle_integrity_and_missing_architecture(self):
        for fault in (None, 'digest', 'diffID', 'size', 'architecture', 'missing'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                source = root / 'source'
                source.mkdir()
                for arch in ('arm64', 'amd64'):
                    raw = ('fixture-' + arch).encode()
                    layer = gzip.compress(raw, mtime=0)
                    digest = 'sha256:' + hashlib.sha256(layer).hexdigest()
                    diff_id = 'sha256:' + hashlib.sha256(raw).hexdigest()
                    metadata = {'digest': digest, 'diffID': diff_id, 'size': len(layer),
                                'imageConfig': {'architecture': arch, 'os': 'linux',
                                                'rootfs': {'diff_ids': [diff_id]}}}
                    if arch == 'amd64':
                        if fault == 'missing':
                            continue
                        if fault == 'architecture':
                            metadata['imageConfig']['architecture'] = 'arm64'
                        elif fault:
                            metadata[fault] = 0 if fault == 'size' else 'sha256:wrong'
                    (source / (arch + '.tar.gz')).write_bytes(layer)
                    (source / (arch + '.json')).write_text(json.dumps(metadata))
                env = {key: value for key, value in os.environ.items()
                       if key not in ('WENDY_TWIN_BUILD_ARCH', 'WENDY_TWIN_BUNDLE_DIR')}
                result = subprocess.run([sys.executable, str(SCRIPT)],
                                        env=dict(env, WENDY_TWIN_BUNDLE_DIR=str(source),
                                                 TARGET_BUILD_DIR=str(root / 'app'),
                                                 UNLOCALIZED_RESOURCES_FOLDER_PATH='Resources'),
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(result.returncode == 0, fault is None, result.stderr)
                if fault is None:
                    for original in source.iterdir():
                        copied = root / 'app/Resources/DigitalTwin' / original.name
                        self.assertEqual(copied.read_bytes(), original.read_bytes())


if __name__ == '__main__':
    unittest.main()

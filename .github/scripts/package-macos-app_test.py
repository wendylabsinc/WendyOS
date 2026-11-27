#!/usr/bin/env python3
"""Exercise release gating with fake Apple services, without signing credentials."""
import hashlib
import os
from pathlib import Path
import subprocess
import tempfile
import unittest
import zipfile


ROOT = Path(__file__).resolve().parents[2]
FAKE_TOOL = r'''#!/usr/bin/env python3
import datetime, json, os, pathlib, plistlib, shutil, sys, zipfile
name = pathlib.Path(sys.argv[0]).name
args = sys.argv[1:]
if name == 'security':
    bundle = 'dev.wendy.desktop'
    if args[-1].endswith('network.profile'):
        bundle += '.network'
    sys.stdout.buffer.write(plistlib.dumps({
        'ExpirationDate': datetime.datetime(2099, 1, 1),
        'TeamIdentifier': ['TESTTEAM'],
        'Entitlements': {
            'com.apple.application-identifier': 'TESTTEAM.' + bundle,
            'com.apple.developer.networking.networkextension': ['packet-tunnel-provider-systemextension'],
            'com.apple.developer.system-extension.install': True,
            'com.apple.developer.aps-environment': 'production',
        },
    }))
elif name == 'ditto':
    source, destination = map(pathlib.Path, args[-2:])
    if '-c' in args:
        with zipfile.ZipFile(destination, 'w') as archive:
            for path in source.rglob('*'):
                if path.is_file():
                    archive.write(path, path.relative_to(source.parent))
    else:
        shutil.copytree(source, destination)
elif name == 'xcrun':
    if args[:2] == ['notarytool', 'submit']:
        if os.environ.get('MOCK_STATUS') == 'malformed':
            print('not JSON')
        else:
            print(json.dumps({'id': 'test-submission', 'status': os.environ.get('MOCK_STATUS', 'Accepted')}))
        sys.exit(int(os.environ.get('MOCK_SUBMIT_EXIT', '0')))
    elif args[:2] == ['notarytool', 'log']:
        pathlib.Path(args[-1]).write_text('{}')
    elif args[:2] == ['stapler', 'staple']:
        if os.environ.get('MOCK_FAIL') == 'staple':
            sys.exit(1)
        (pathlib.Path(args[-1]) / 'ticket').write_text('stapled')
    elif args[:2] == ['stapler', 'validate'] and os.environ.get('MOCK_FAIL') == 'validate':
        sys.exit(1)
elif name == 'spctl' and os.environ.get('MOCK_FAIL') == 'gatekeeper':
    sys.exit(1)
elif name == 'codesign' and os.environ.get('MOCK_FAIL') == 'codesign':
    sys.exit(1)
'''


class PackageTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        binaries = self.work / 'bin'
        binaries.mkdir()
        driver = binaries / 'driver'
        driver.write_text(FAKE_TOOL)
        driver.chmod(0o755)
        for name in ('security', 'ditto', 'xcrun', 'spctl', 'codesign'):
            (binaries / name).symlink_to(driver)
        self.source = self.work / 'source.app'
        (self.source / 'Contents/Library/SystemExtensions/dev.wendy.desktop.network.systemextension/Contents').mkdir(parents=True)
        (self.source / 'payload').write_text('release build')
        for name in ('app.profile', 'network.profile'):
            (self.work / name).touch()
        self.env = {
            **os.environ,
            'PATH': str(binaries) + os.pathsep + os.environ['PATH'],
            'RUNNER_TEMP': str(self.work),
            'WENDY_SIGNING_IDENTITY': 'test-identity',
            'KEYCHAIN_PATH': str(self.work / 'keychain'),
            'NOTARY_PROFILE': 'test-notary',
            'APP_PROVISIONING_PROFILE_PATH': str(self.work / 'app.profile'),
            'NET_PROXY_PROVISIONING_PROFILE_PATH': str(self.work / 'network.profile'),
        }
        self.env.pop('WENDY_BUILD_NUMBER', None)
        self.output = self.work / 'release'

    def package(self, **env):
        return subprocess.run(
            ['bash', str(ROOT / '.github/scripts/package-macos-app.sh'),
             str(self.source), str(self.output), '1.2.3'],
            env={**self.env, **env}, capture_output=True, text=True,
        )

    def test_archive_contains_stapled_app_and_matching_checksum(self):
        result = self.package()
        self.assertEqual(result.returncode, 0, result.stderr)
        archive = self.output / 'wendy-macos-arm64-1.2.3.zip'
        with zipfile.ZipFile(archive) as contents:
            self.assertEqual(contents.read('WendyDesktop.app/ticket'), b'stapled')
            self.assertEqual(contents.read('WendyDesktop.app/payload'), b'release build')
        checksum = archive.with_suffix('.zip.sha256').read_text().split()
        self.assertEqual(checksum, [hashlib.sha256(archive.read_bytes()).hexdigest(), archive.name])
        self.assertFalse((self.source / 'ticket').exists())
        self.assertEqual(list(self.work.glob('macos-app-package.*')), [])

    def test_failed_checks_never_produce_a_release_archive(self):
        cases = [
            {'MOCK_STATUS': 'Invalid'}, {'MOCK_STATUS': 'In Progress'},
            {'MOCK_STATUS': 'malformed'}, {'MOCK_SUBMIT_EXIT': '1'},
            *({'MOCK_FAIL': check} for check in ('codesign', 'staple', 'validate', 'gatekeeper')),
        ]
        for index, env in enumerate(cases):
            with self.subTest(env=env):
                self.output = self.work / f'release-{index}'
                result = self.package(**env)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(list(self.output.glob('*.zip')), [])
                self.assertEqual(list(self.output.glob('*.sha256')), [])
                self.assertEqual(list(self.work.glob('macos-app-package.*')), [])


if __name__ == '__main__':
    unittest.main()

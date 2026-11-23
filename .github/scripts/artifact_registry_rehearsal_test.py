"""Offline exact-artifact admission/receipt checks; never contacts GCP or a VM."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / '.github/regression-coverage/manual/artifact-registry-rehearsal.py'
spec = importlib.util.spec_from_file_location('registry_plan', SCRIPT)
PLAN = importlib.util.module_from_spec(spec)
spec.loader.exec_module(PLAN)


class ArtifactRegistryPlans(unittest.TestCase):
    def fixture(self, root):
        record = root / 'record.txt'; record.write_text('synthetic offline evidence, never actual GCP acceptance')
        raw = {'trial': 'a' * 32, 'source_commit': 'b' * 40,
               'release_run': 'https://github.com/wendylabsinc/WendyOS/actions/runs/123',
               'target': {'mode': 'reserved', 'project': 'synthetic-project', 'location': 'us-central1',
                          'apt': 'wendy-audit-' + 'a' * 16 + '-apt', 'yum': 'wendy-audit-' + 'a' * 16 + '-yum'},
               'records': {name: str(record) for name in ('authorization', 'release_provenance', 'repository_identity',
                                                         'guest_reservation', 'cleanup')},
               'apt_keyring': str(record), 'apt_keyring_sha256': PLAN.sha(record), 'packages': [], 'guests': {}}
        for fmt in ('deb', 'rpm'):
            for arch in ('amd64', 'arm64'):
                for name in ('wendy', 'wendy-agent'):
                    package = root / f'{name}-{arch}.{fmt}'
                    package.write_bytes((b'!<arch>\n' if fmt == 'deb' else bytes.fromhex('edabeedb0300')) + name.encode() + arch.encode())
                    item = {'format': fmt, 'architecture': arch, 'name': name, 'version': '1.2.3' if fmt == 'deb' else '1.2.3-1',
                            'path': str(package), 'sha256': PLAN.sha(package)}
                    normal = root / f'{name}-{arch}-{fmt}-normal.json'
                    normal.write_text(json.dumps({'status': 'passed', 'case': 'normal', 'manifest': {'format': fmt,
                        'packages': {'candidate' if name == 'wendy-agent' else 'cli': {'name': name, 'sha256': item['sha256'],
                            'version': item['version'], 'elf_architecture': arch, 'binary_sha256': 'c' * 64}}}}))
                    item['native_result'] = str(normal)
                    raw['packages'].append(item)
                raw['guests'][fmt + '-' + arch] = {
                    'instance_id': str(len(raw['guests']) + 1), 'project': 'synthetic-project', 'zone': 'us-central1-a',
                    'boot_id': '00000000-0000-4000-8000-000000000001', 'os_release_sha256': 'd' * 64,
                    'client_sha256': 'e' * 64, 'metadata_tool_sha256': 'f' * 64}
        inventory = root / 'inventory.json'; inventory.write_text(json.dumps(raw))
        return inventory, raw

    def test_complete_offline_preparation_binds_each_native_result_and_upload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, raw = self.fixture(root)
            with patch.object(PLAN.subprocess, 'run') as spawn, patch.object(PLAN.urllib.request, 'build_opener') as network:
                plan = PLAN.prepare(inventory, root / 'plan')
            spawn.assert_not_called(); network.assert_not_called()
            self.assertEqual(plan['external_execution'], 'not_run')
            self.assertTrue(all(case['status'] == 'not_run' for case in plan['cases']))
            commands = json.loads((root / 'plan/commands.json').read_text())
            uploads = [argv for name, argv in commands.items() if name.startswith('upload-')]
            self.assertEqual(len(uploads), 8)
            for item, argv in zip(plan['packages'], uploads):
                self.assertIn('--source=' + item['path'], argv)
                self.assertIn('--project=synthetic-project', argv)
                self.assertIn('600', argv)
            for name, expected in json.loads((root / 'plan/prepared-files.json').read_text()).items():
                self.assertEqual(PLAN.sha(root / 'plan' / name), expected)

    def test_production_release_plan_never_emits_manual_upload(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, raw = self.fixture(root)
            raw['target'].update(mode='authorized-release', apt='wendy-apt', yum='wendy-yum')
            inventory.write_text(json.dumps(raw)); PLAN.prepare(inventory, root / 'plan')
            commands = json.loads((root / 'plan/commands.json').read_text())
            self.assertFalse(any(name.startswith('upload-') for name in commands))
            self.assertEqual(len(commands), 4)

    def test_wrong_bytes_format_native_proof_identity_and_credentials_refused(self):
        for fault in ('hash', 'rpm-lead', 'native-proof', 'missing-arm', 'duplicate-vm', 'production-alias', 'secret'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = Path(temp); inventory, raw = self.fixture(root)
                if fault == 'hash': raw['packages'][0]['sha256'] = '0' * 64
                if fault == 'rpm-lead':
                    item = next(p for p in raw['packages'] if p['format'] == 'rpm')
                    Path(item['path']).write_bytes(bytes.fromhex('edabeedb0400') + b'wrong')
                    item['sha256'] = PLAN.sha(item['path'])
                if fault == 'native-proof':
                    path = Path(raw['packages'][0]['native_result']); proof = json.loads(path.read_text())
                    proof['manifest']['packages']['cli']['sha256'] = '0' * 64; path.write_text(json.dumps(proof))
                if fault == 'missing-arm': raw['packages'].pop()
                if fault == 'duplicate-vm': raw['guests']['rpm-arm64']['instance_id'] = '1'
                if fault == 'production-alias': raw['target']['apt'] = 'wendy-apt'
                if fault == 'secret': raw['access_token'] = 'must-not-copy'
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError): PLAN.prepare(inventory, root / 'plan')
                self.assertFalse((root / 'plan').exists())

    def test_wrong_guest_confirmation_refuses_before_network_or_spawn(self):
        with patch.object(PLAN.subprocess, 'run') as spawn, patch.object(PLAN, 'metadata_value') as network:
            with self.assertRaisesRegex(ValueError, 'confirmation'):
                PLAN.admit({'trial': 'a' * 32}, 'deb-amd64', 'foreign')
            spawn.assert_not_called(); network.assert_not_called()

    def test_changed_prepared_file_refuses_before_guest_admission(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); inventory, _ = self.fixture(root)
            PLAN.prepare(inventory, root / 'plan')
            with (root / 'plan/plan.json').open('a') as out: out.write(' ')
            with patch.object(PLAN, 'admit') as admit, self.assertRaisesRegex(ValueError, 'changed'):
                PLAN.consume(root / 'plan/plan.json', 'deb-amd64', 'a' * 32, root / 'result')
            admit.assert_not_called()

    def test_actual_downloaded_files_must_match_hash_and_native_metadata(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); downloads = root / 'downloads'; downloads.mkdir()
            path = downloads / 'wendy.deb'; path.write_bytes(b'controlled package bytes')
            package = {'name': 'wendy', 'format': 'deb', 'architecture': 'amd64', 'version': '1.2.3', 'sha256': PLAN.sha(path)}
            # Real owned subprocess, controlled metadata peer; not a package-manager acceptance claim.
            tool = root / 'metadata-tool'
            tool.write_text('#!/bin/sh\nprintf "wendy\\n1.2.3\\namd64\\n"\n'); tool.chmod(0o755)
            result = PLAN.verify_downloads([package], downloads, str(tool))
            self.assertEqual(result[0]['sha256'], package['sha256'])
            tool.write_text('#!/bin/sh\nprintf "wendy\\n1.2.2\\namd64\\n"\n')
            with self.assertRaisesRegex(ValueError, 'metadata'): PLAN.verify_downloads([package], downloads, str(tool))
            path.write_bytes(b'other repository or changed bytes')
            with patch.object(PLAN.subprocess, 'run') as spawn, self.assertRaisesRegex(ValueError, 'bytes differ'):
                PLAN.verify_downloads([package], downloads, str(tool))
            spawn.assert_not_called()

    def test_real_dpkg_package_bytes_and_version_are_consumed(self):
        tool = shutil.which('dpkg-deb')
        self.assertIsNotNone(tool, 'existing Linux offline row requires dpkg-deb; no package installation is performed')
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp); package = root / 'package'; (package / 'DEBIAN').mkdir(parents=True)
            (package / 'DEBIAN/control').write_text('Package: wendy\nVersion: 1.2.3\nArchitecture: amd64\n'
                'Maintainer: Offline fixture <fixture@example.invalid>\nDescription: Controlled bytes, not a Wendy executable\n')
            (package / 'fixture.txt').write_text('actual local DEB archive, never installed')
            downloads = root / 'downloads'; downloads.mkdir()
            dest = downloads / 'candidate.deb'
            subprocess.run([tool, '--root-owner-group', '--build', str(package), str(dest)],
                           capture_output=True, timeout=15, check=True)
            item = {'format': 'deb', 'architecture': 'amd64', 'name': 'wendy', 'version': '1.2.3', 'sha256': PLAN.sha(dest)}
            self.assertEqual(PLAN.verify_downloads([item], downloads, tool)[0]['version'], '1.2.3')
            item['version'] = '1.2.2'
            with self.assertRaisesRegex(ValueError, 'metadata'):
                PLAN.verify_downloads([item], downloads, tool)


if __name__ == '__main__':
    unittest.main()

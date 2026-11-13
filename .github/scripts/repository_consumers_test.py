#!/usr/bin/env python3
"""Offline admission/ownership checks, not external repository acceptance."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

HERE = Path(__file__).resolve().parent

def module(name, file):
    spec = importlib.util.spec_from_file_location(name, HERE / file)
    value = importlib.util.module_from_spec(spec); spec.loader.exec_module(value)
    return value

HOST = module('repository_publish', 'publish-linux-repositories.py')
GUEST = module('repository_consume', 'consume-linux-repository.py')


def packages():
    return [dict(format=fmt, name=name, architecture=arch,
                 version='1.2.3' if fmt == 'deb' else '1.2.3-1',
                 filename=f'{name}-1.2.3.{arch}.{fmt}', size=10, sha256=(str(i) * 64))
            for i, (fmt, name, arch) in enumerate((fmt, name, arch) for fmt in ('deb', 'rpm')
                for name in ('wendy', 'wendy-agent') for arch in ('amd64', 'arm64'))]


class RepositoryAdmission(unittest.TestCase):
    def test_native_inventory_requires_all_pairs_and_exact_release_version(self):
        values = packages(); HOST.validate_packages(values, '1.2.3')
        for bad in (values[:-1], values[:-1] + [values[0]],
                    [dict(p, version='1.2.4') if i == 0 else p for i, p in enumerate(values)]):
            with self.subTest(bad=bad), self.assertRaises(RuntimeError):
                HOST.validate_packages(bad, '1.2.3')

    def test_upload_exit_and_hash_are_required_for_every_native_candidate(self):
        values = packages()
        receipts = [dict(p, exit_code=0) for p in values]
        HOST.validate_uploads(values, receipts)
        for bad in (receipts[:-1], [dict(r, exit_code=1) if i == 0 else r for i, r in enumerate(receipts)],
                    [dict(r, sha256='f' * 64) if i == 0 else r for i, r in enumerate(receipts)]):
            with self.subTest(bad=bad), self.assertRaisesRegex(RuntimeError, 'UPLOAD_RECEIPTS'):
                HOST.validate_uploads(values, bad)

    def test_long_lived_key_and_local_credential_sources_are_refused(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'credential.json'
            valid = {'type': 'external_account', 'credential_source': {'url': 'https://example.invalid/token'}}
            path.write_text(json.dumps(valid)); self.assertEqual(HOST.validate_wif(path), valid)
            for bad in ({'type': 'service_account', 'private_key': 'synthetic'},
                        {'type': 'external_account', 'credential_source': {'file': '/private/user/token'}},
                        {'type': 'external_account', 'credential_source': {'url': 'http://example.invalid/token'}}):
                path.write_text(json.dumps(bad))
                with self.assertRaises(RuntimeError):
                    HOST.validate_wif(path)
            path.write_text(json.dumps(valid))
            link = path.parent / 'link'; link.symlink_to(path)
            with self.assertRaises(RuntimeError):
                HOST.validate_wif(link)

    def test_actual_download_bytes_not_filename_qualify_receipt(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            path = root / 'wendy_1.2.3_amd64.deb'; path.write_bytes(b'candidate payload')
            def observed(file, fmt):
                return dict(name='wendy', version='1.2.3', architecture='amd64', format=fmt,
                            size=file.stat().st_size, sha256=GUEST.sha(file))
            expected = [observed(path, 'deb')]
            with patch.object(GUEST, 'metadata', side_effect=observed):
                GUEST.verify_downloads(root, expected)
                path.write_bytes(b'different payload')
                with self.assertRaisesRegex(RuntimeError, 'REPOSITORY_DOWNLOAD_HASH'):
                    GUEST.verify_downloads(root, expected)
                path.unlink(); path.symlink_to('/dev/null')
                with self.assertRaisesRegex(RuntimeError, 'REPOSITORY_DOWNLOAD_SET'):
                    GUEST.verify_downloads(root, expected)

    def test_rpm_v6_refused_before_native_query(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / 'candidate.rpm'; path.write_bytes(bytes.fromhex('edabeedb0400'))
            with patch.object(GUEST, 'command') as command, self.assertRaisesRegex(RuntimeError, 'REPOSITORY_RPM_V4_LEAD'):
                GUEST.metadata(path, 'rpm')
            command.assert_not_called()

    def test_cleanup_uses_verified_container_id(self):
        calls = []
        def execute(argv, **kwargs):
            calls.append(argv)
            if argv[1] == 'inspect':
                return subprocess.CompletedProcess(argv, 0, json.dumps([{'Id': 'a' * 64,
                    'Config': {'Labels': {'wendy.repository-owner': 'nonce'}}}]), '')
            return subprocess.CompletedProcess(argv, 0, '', '')
        with patch.object(HOST.subprocess, 'run', side_effect=execute):
            HOST.drain_container('owned-name', 'nonce', {})
        self.assertEqual(calls[1], ['docker', 'rm', '--force', 'a' * 64])
        self.assertEqual(calls[2][-1], 'id=' + 'a' * 64)

    def test_cleanup_refuses_foreign_container_and_unproven_drain(self):
        def execute(argv, **kwargs):
            if argv[1] == 'inspect':
                return subprocess.CompletedProcess(argv, 0, json.dumps([{'Id': 'a' * 64,
                    'Config': {'Labels': {'wendy.repository-owner': 'another'}}}]), '')
            self.fail('foreign container must not receive removal')
        with patch.object(HOST.subprocess, 'run', side_effect=execute), self.assertRaisesRegex(RuntimeError, 'ownership'):
            HOST.drain_container('owned-name', 'nonce', {})
        values = [subprocess.CompletedProcess([], 0, json.dumps([{'Id': 'a' * 64,
                  'Config': {'Labels': {'wendy.repository-owner': 'nonce'}}}]), ''),
                  subprocess.CompletedProcess([], 0, '', ''), subprocess.CompletedProcess([], 0, 'a' * 64, '')]
        with patch.object(HOST.subprocess, 'run', side_effect=values), self.assertRaisesRegex(RuntimeError, 'remains'):
            HOST.drain_container('owned-name', 'nonce', {})


if __name__ == '__main__':
    unittest.main()

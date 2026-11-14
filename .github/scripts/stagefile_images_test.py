#!/usr/bin/env python3
"""Offline checks for the real-image gate; these do not claim image execution."""
import hashlib
import importlib.util
import io
import json
import os
import signal
from pathlib import Path
import subprocess
import sys
import tarfile
import tempfile
import threading
import time
import unittest
from unittest.mock import patch


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


images = load('stagefile_images', 'test-stagefile-images.py')
native = load('native_make', 'test-native-make-version.py')


def add(archive, path, content):
    entry = tarfile.TarInfo(path); entry.size = len(content)
    archive.addfile(entry, io.BytesIO(content))


class StagefileImagesTests(unittest.TestCase):
    def test_guardian_bounds_timeout_and_interrupted_command(self):
        if os.name == 'nt':
            self.skipTest('owned process groups require POSIX')
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); cli = root/'cli'; cli.write_bytes(b'fixture')
            with patch.object(images.shutil, 'which', return_value='/unused/docker'):
                harness = images.Harness(cli, root)
            # A TERM-resistant real subprocess must stop writing after the
            # helper deadline. No Docker process or real build is involved.
            activity = root/'activity'
            harness.env['STAGEFILE_GUARDIAN_FIXTURE'] = 'child-env'
            child = ('import os,pathlib,signal,sys,time; '
                     'signal.signal(signal.SIGTERM, signal.SIG_IGN); '
                     'print(os.getcwd(), os.environ["STAGEFILE_GUARDIAN_FIXTURE"], flush=True); '
                     'print("child-stderr", file=sys.stderr, flush=True); '
                     'p=pathlib.Path('+repr(str(activity))+'); '
                     '\nwhile True:\n p.write_text(str(time.monotonic())); time.sleep(.02)\n')
            started = time.monotonic()
            code, output = harness.run([sys.executable, '-c', child], 'timeout', cwd=root, timeout=2, check=False)
            self.assertEqual(code, 124, output)
            self.assertIn(str(root.resolve())+' child-env', output)
            self.assertIn('child-stderr', output)
            self.assertLess(time.monotonic()-started, 10)
            before = activity.read_text(); time.sleep(.1)
            self.assertEqual(activity.read_text(), before)
            # Interrupt only after the command has started. The in-process
            # guardian handler consumes TERM and performs its own final kill.
            interrupted_activity = root/'interrupted-activity'
            child = child.replace(repr(str(activity)), repr(str(interrupted_activity)))
            ready = threading.Event()
            cancelled = threading.Event()
            def interrupt():
                deadline = time.monotonic()+3
                while not cancelled.is_set() and not interrupted_activity.exists() and time.monotonic() < deadline:
                    time.sleep(.02)
                if interrupted_activity.exists():
                    ready.set()
                if not cancelled.is_set():
                    os.kill(os.getpid(), signal.SIGTERM)
            # A broken runner returning before readiness must fail this test,
            # not let a delayed test signal terminate the test runner itself.
            previous = signal.signal(signal.SIGTERM, lambda signum, frame: None)
            interrupter = threading.Thread(target=interrupt)
            interrupter.start()
            try:
                with self.assertRaises(InterruptedError):
                    harness.run([sys.executable, '-c', child], 'interrupted', timeout=8)
            finally:
                cancelled.set()
                interrupter.join(timeout=4)
                signal.signal(signal.SIGTERM, previous)
            self.assertTrue(ready.is_set(), 'command must start before interrupting it')
            before = interrupted_activity.read_text(); time.sleep(.1)
            self.assertEqual(interrupted_activity.read_text(), before)

    def test_local_package_build_creates_fresh_installed_nonce(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            source = images.package(root, '1.0')
            nonces = []
            # Same source, different target directories, no index and no build
            # requirements: pip really invokes the stdlib-only PEP 517 backend.
            for run in range(2):
                target = root/f'installed-{run}'
                result = subprocess.run([sys.executable, '-m', 'pip', 'install', '--no-index', '--no-deps',
                                         '--no-compile', '--target', str(target), str(source)],
                                        capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
                result = subprocess.run([sys.executable, '-I', '-c',
                                         'import sys; sys.path.insert(0, '+repr(str(target))+'); '
                                         'import stagefixture; print(stagefixture.VERSION); print(stagefixture.BUILD_NONCE)'],
                                        capture_output=True, text=True, timeout=10, check=True)
                version, nonce = result.stdout.strip().splitlines()
                self.assertEqual(version, '1.0')
                self.assertRegex(nonce, '^[0-9a-f]{32}$')
                nonces.append(nonce)
            self.assertNotEqual(*nonces, 'uncached pip builds must produce distinct installed markers')

    def test_wheel_installs_and_imports_without_an_index(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for version in ('1.0', '2.0'):
                wheel = root/images.wheel(root, version)
                target = root/version
                result = subprocess.run([sys.executable, '-m', 'pip', 'install', '--no-index', '--no-deps',
                                         '--no-compile', '--target', str(target), str(wheel)],
                                        capture_output=True, text=True, timeout=60)
                self.assertEqual(result.returncode, 0, result.stdout+result.stderr)
                result = subprocess.run([sys.executable, '-I', '-c',
                                         'import sys; sys.path.insert(0, '+repr(str(target))+'); '
                                         'import stagefixture; print(stagefixture.VERSION)'],
                                        capture_output=True, text=True, timeout=10, check=True)
                self.assertEqual(result.stdout.strip(), version)

    def test_saved_layer_requires_installed_bytes_and_matches_image_layers(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory)/'image.tar'
            def save(content, include=True):
                layer = io.BytesIO()
                with tarfile.open(fileobj=layer, mode='w') as archive:
                    add(archive, images.MODULE if include else 'wheels/fixture.whl', content)
                with tarfile.open(path, 'w') as archive:
                    add(archive, 'manifest.json', json.dumps([{'Layers': ['layer.tar']}]).encode())
                    add(archive, 'layer.tar', layer.getvalue())
            save(b"VERSION = '1.0'\n")
            self.assertEqual(images.installed_layer(path, ['sha256:layer'], '1.0'), 'sha256:layer')
            with self.assertRaisesRegex(AssertionError, 'disagree'):
                images.installed_layer(path, [], '1.0')
            with self.assertRaisesRegex(AssertionError, 'unexpected contents'):
                images.installed_layer(path, ['sha256:layer'], '2.0')
            save(b"VERSION = '1.0'\n", include=False)
            with self.assertRaisesRegex(AssertionError, 'one installed wheel'):
                images.installed_layer(path, ['sha256:layer'], '1.0')

    def test_backend_and_cache_assertions_reject_unrelated_success(self):
        images.assert_backend([['buildx', 'build', '--load']], 'dockerfile')
        images.assert_backend([['load', '--input', 'image.tar']], 'llb')
        for calls in ([], [['buildx', 'build']], [['load'], ['buildx', 'build']]):
            with self.assertRaises(AssertionError):
                images.assert_backend(calls, 'llb')
        images.assert_cached_pip('#8 RUN pip install --root /overlay /wheels/stagefixture.whl\n#8 CACHED\n')
        for output in ('#8 RUN pip install stagefixture\n#9 CACHED\n',
                       '#8 RUN pip install stagefixture\n#8 DONE 0.1s\n', '#8 CACHED\n'):
            with self.assertRaisesRegex(AssertionError, 'cached pip'):
                images.assert_cached_pip(output)

    def test_native_retention_preserves_bytes_mode_and_refuses_replacement(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); source = root/'wendy'; destination = root/'retained'
            source.write_bytes(b'exact tested executable'); source.chmod(0o751)
            digest = native.retain_binary(source, destination)
            self.assertEqual(digest, hashlib.sha256(source.read_bytes()).hexdigest())
            self.assertEqual(destination.stat().st_mode & 0o777, 0o751)
            source.write_bytes(b'later rebuild')
            self.assertEqual(destination.read_bytes(), b'exact tested executable')
            with self.assertRaises(FileExistsError):
                native.retain_binary(source, destination)
            self.assertEqual(destination.read_bytes(), b'exact tested executable')

    def test_cli_cannot_change_or_escape_the_owned_builder(self):
        with patch.dict(os.environ, {'BUILDX_BUILDER': 'fixture-oci'}):
            images.guard_docker_call(['buildx', 'build', '--load'], 'fixture-oci')
            images.guard_docker_call(['exec', '-i', 'buildx_buildkit_fixture-oci0', 'buildctl', 'dial-stdio'], 'fixture-oci')
            for argv in (['buildx', 'create', '--name', 'unrelated'], ['buildx', 'rm', 'fixture-oci'],
                         ['buildx', 'use', 'fixture-oci'], ['buildx', 'prune'],
                         ['buildx', 'build', '--builder=unrelated'],
                         ['buildx', 'build', '--builder', 'unrelated'],
                         ['exec', '-i', 'unrelated', 'buildctl', 'dial-stdio']):
                with self.subTest(argv=argv), self.assertRaises(RuntimeError):
                    images.guard_docker_call(argv, 'fixture-oci')

    def test_partial_builder_creation_is_cleaned_and_cleanup_errors_fail(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); cli = root/'cli'; cli.write_bytes(b'fixture')
            with patch.object(images.shutil, 'which', return_value='/fixture/docker'):
                harness = images.Harness(cli, root)
            calls = []
            def run(argv, label, **kwargs):
                calls.append(argv)
                if label == 'docker-info':
                    return 0, json.dumps({'Architecture': 'amd64', 'OSType': 'linux'})
                if label == 'check-builder-absent':
                    return 1, 'no builder found'
                if label == 'create-builder':
                    raise RuntimeError('create failed after registering the builder')
                return 0, ''
            with patch.object(harness, 'run', side_effect=run):
                with self.assertRaisesRegex(RuntimeError, 'create failed'):
                    harness.execute()
                harness.cleanup()
            self.assertEqual(calls[-1], ['/fixture/docker', 'buildx', 'rm', '--force', harness.builder])
            harness.images = ['fixture-owned:latest']
            harness.builder_created = False
            with patch.object(harness, 'run', return_value=(1, 'permission denied')):
                with self.assertRaisesRegex(RuntimeError, 'cleanup failed'):
                    harness.cleanup()
            self.assertIn('permission denied', (root/'evidence.json').read_text())

    def test_live_gate_refuses_local_daemon_before_creating_output(self):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory)/'must-not-exist'
            env = dict(os.environ, GITHUB_ACTIONS='false')
            result = subprocess.run([sys.executable, str(Path(images.__file__)), '/missing/cli',
                                     '--output', str(output)], env=env, capture_output=True, text=True, timeout=10)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn('do not create a privileged local BuildKit daemon', result.stderr)
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()

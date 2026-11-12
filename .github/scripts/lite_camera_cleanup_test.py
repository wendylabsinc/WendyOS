#!/usr/bin/env python3
"""Offline ownership/ABI checks; no camera nodes or device ioctls are used."""

import importlib.util
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

PATH = Path(__file__).with_name('cleanup-owned-lite-camera.py')
SPEC = importlib.util.spec_from_file_location('camera_cleanup', PATH)
CLEANUP = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(CLEANUP)
LABEL = 'ci-lite-0123456789ab:camera'


class CleanupTests(unittest.TestCase):
    def test_local_cli_refuses(self):
        result = subprocess.run([sys.executable, str(PATH), '--label', LABEL],
                                env=dict(os.environ, GITHUB_ACTIONS=''), capture_output=True, text=True, timeout=3)
        self.assertEqual(result.returncode, 2)
        self.assertIn('requires the disposable hosted Linux runner', result.stderr)

    def test_absent_node_and_invalid_label_never_open_control(self):
        with tempfile.TemporaryDirectory() as directory, patch.object(CLEANUP.os, 'open', side_effect=AssertionError('opened control')):
            CLEANUP.cleanup(LABEL, Path(directory), Path(directory))
            with self.assertRaises(ValueError):
                CLEANUP.cleanup('unrelated camera', Path(directory), Path(directory))

    def test_exact_ownership_and_integer_abi(self):
        for scenario in ('owned', 'wrong-label', 'wrong-version', 'regular-file', 'replacement'):
            with self.subTest(scenario=scenario), tempfile.TemporaryDirectory() as directory:
                root = Path(directory)
                dev, sysfs = root / 'dev', root / 'sys'
                dev.mkdir()
                node = dev / 'video96'
                node.write_bytes(b'ordinary offline fixture, not a device')
                name = sysfs / 'class/video4linux/video96/name'
                name.parent.mkdir(parents=True)
                name.write_text(LABEL if scenario != 'wrong-label' else 'someone else')
                version = sysfs / 'module/v4l2loopback/version'
                version.parent.mkdir(parents=True)
                version.write_text('0.15.4' if scenario != 'wrong-version' else '0.14.0')
                actual = Path.lstat
                reads = 0

                def fake_lstat(path):
                    nonlocal reads
                    value = actual(path)
                    if path != node or scenario == 'regular-file':
                        return value
                    reads += 1
                    fields = list(value)
                    fields[0] = stat.S_IFCHR | 0o600
                    if scenario == 'replacement' and reads > 1:
                        fields[1] += 1
                    return os.stat_result(fields)

                def remove(fd, request, argument):
                    self.assertEqual((fd, request, argument), (101, 0x40047E02, 96))
                    self.assertIsInstance(argument, int)
                    node.unlink()

                with patch.object(Path, 'lstat', fake_lstat), patch.object(CLEANUP.os, 'open', return_value=101) as opened, \
                        patch.object(CLEANUP.os, 'close') as closed, patch.object(CLEANUP.fcntl, 'ioctl', side_effect=remove) as ioctl:
                    if scenario == 'owned':
                        CLEANUP.cleanup(LABEL, dev, sysfs)
                        ioctl.assert_called_once()
                        closed.assert_called_once_with(101)
                        self.assertFalse(node.exists())
                    else:
                        with self.assertRaises(RuntimeError):
                            CLEANUP.cleanup(LABEL, dev, sysfs)
                        ioctl.assert_not_called()
                        self.assertTrue(node.exists())
                        if scenario != 'replacement':
                            opened.assert_not_called()


if __name__ == '__main__':
    unittest.main()

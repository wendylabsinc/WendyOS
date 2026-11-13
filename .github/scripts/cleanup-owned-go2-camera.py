#!/usr/bin/env python3
"""Remove only the reserved, uniquely labelled CI Go2 loopback camera."""

import argparse
import fcntl
import os
from pathlib import Path
import re
import stat
import sys
import time


def cleanup(label, dev=Path('/dev'), sysfs=Path('/sys')):
    if not re.fullmatch(r'ci-go2-[0-9a-f]{12}', label):
        raise ValueError('expected this invocation\'s unique Go2 camera label')
    node = dev / 'video128'
    try:
        before = node.lstat()
    except FileNotFoundError:
        return
    if not stat.S_ISCHR(before.st_mode):
        raise RuntimeError('refusing a non-character or symlink camera node')
    if (sysfs / 'module/v4l2loopback/version').read_text().strip() != '0.15.4':
        raise RuntimeError('unexpected loopback control ABI')
    name = sysfs / 'class/video4linux/video128/name'
    if name.read_text().strip() != label:
        raise RuntimeError('refusing to remove another camera label')
    control = os.open(dev / 'v4l2loopback', os.O_RDWR | os.O_CLOEXEC)
    try:
        current = node.lstat()
        if (current.st_ino, current.st_rdev) != (before.st_ino, before.st_rdev) or name.read_text().strip() != label:
            raise RuntimeError('reserved camera changed before cleanup')
        # 0.15.4 ABI, identical to ipcam.removeLoopbackNode: REMOVE receives
        # the node NUMBER as its ioctl argument, never a pointer to that number.
        fcntl.ioctl(control, 0x40047E02, 128)
    finally:
        os.close(control)
    deadline = time.monotonic() + 2
    while node.exists():
        if time.monotonic() >= deadline:
            raise RuntimeError('owned camera node remained after removal')
        time.sleep(0.02)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--label', required=True)
    args = parser.parse_args()
    if (os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted'
            or not sys.platform.startswith('linux') or os.geteuid() != 0):
        parser.error('requires the disposable hosted Linux runner')
    cleanup(args.label)


if __name__ == '__main__':
    main()

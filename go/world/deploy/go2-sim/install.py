"""One-shot private simulation configuration installer. Never starts workers."""
import argparse
import fcntl
import os
from pathlib import Path
import shutil

from boot import BootError, create_durable, emit, real_directory, sync_directory
from run_bundle import APP, checked_release, marker, private_snapshot, read_bundle, validate_snapshot


def install(source, volume, bundle_sha, release_id, image_digest, validate=validate_snapshot, app=APP):
    checked_release(release_id, app)
    cfg, captured = read_bundle(source, bundle_sha, release_id, image_digest)
    snapshot = private_snapshot(captured)
    try:
        if not validate(snapshot, bundle_sha, release_id, image_digest, True):
            raise BootError('commission_validation_refused')
        info = real_directory(volume)
        if info.st_uid != os.geteuid() or info.st_mode & 0o022 or set(os.listdir(volume)):
            raise BootError('volume_not_empty_or_private')
        # O_EXCL and the retained lock file make every incomplete installation
        # terminal. No retry overwrites partial state, even before commit.
        lock = os.open(volume / '.install.lock', os.O_RDWR | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            os.fsync(lock)
            sync_directory(volume)
            if set(os.listdir(volume)) != {'.install.lock'}:
                raise BootError('volume_changed')
            commissioned = volume / 'commission'
            os.mkdir(commissioned, 0o700)
            sync_directory(volume)
            for name in sorted(captured):
                create_durable(commissioned / name, captured[name])
            sync_directory(commissioned)
            # This last durable file is the only installation completion fence.
            create_durable(volume / 'installed.json', marker(bundle_sha, release_id, image_digest))
            return cfg
        finally:
            os.close(lock)
    finally:
        shutil.rmtree(snapshot)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--bundle-sha256', required=True)
    parser.add_argument('--release-id', required=True)
    parser.add_argument('--image-digest', required=True)
    args = parser.parse_args()
    try:
        os.umask(0o077)
        if os.geteuid() != 0:
            raise BootError('installer_requires_root')
        install(Path('/opt/world/commission-input'), Path('/data'), args.bundle_sha256, args.release_id, args.image_digest)
        emit('configuration_installed')
        return 0
    except (BootError, OSError, ValueError):
        emit('installation_refused')
        return 1


if __name__ == '__main__':
    raise SystemExit(main())

"""Reusable image boot. Commissioning bytes never remain live volume inputs."""
import hashlib
import os
from pathlib import Path
import shutil
import signal
import threading

import boot
from run_bundle import APP, checked_release, load_installed, private_snapshot, selectors, validate_snapshot


def identity(environment):
    if (environment.get('WENDY_APP_ID') != 'com.wendy.world.go2'
            or environment.get('WENDY_APP_GROUP') != 'com.wendy.world.go2'
            or environment.get('WENDY_HOSTNAME') != 'mission.local'):
        raise boot.BootError('wendy_service_configuration_mismatch')
    bundle = environment.get('WORLD_GO2_BUNDLE_SHA256')
    release = environment.get('WORLD_GO2_RELEASE_ID')
    image = environment.get('WORLD_GO2_IMAGE_DIGEST')
    selectors(bundle, release, image)
    return bundle, release, image


def main():
    snapshot = None
    lock = None
    try:
        os.umask(0o077)
        bundle, release, image = identity(os.environ)
        checked_release(release)
        cfg, captured = load_installed(Path('/data'), bundle, release, image)
        snapshot = private_snapshot(captured, change_owner=True)
        if not validate_snapshot(snapshot, bundle, release, image, False):
            raise boot.BootError('run_integrity_refused')
        fence = hashlib.sha256((release + '\n' + image + '\n' + bundle + '\n').encode()).hexdigest()
        lock = boot.prepare_state(fence)
        boot.drop_privileges()
        closing = threading.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, lambda *_: closing.set())
        # Validation reads only the captured private snapshot. A later change
        # to the writable configuration volume cannot alter child inputs.
        return boot.supervise(cfg, fence, closing, config_dir=snapshot,
                              can_launch=lambda: validate_snapshot(snapshot, bundle, release, image, True))
    except (boot.BootError, OSError, ValueError):
        boot.emit('boot_refused')
        return 1
    finally:
        if lock is not None:
            os.close(lock)
        if snapshot is not None:
            shutil.rmtree(snapshot)


if __name__ == '__main__':
    raise SystemExit(main())

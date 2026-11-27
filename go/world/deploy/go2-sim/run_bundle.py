"""Fixed-code simulation bundle reader. No bundle supplies executable paths."""
import hashlib
import os
from pathlib import Path
import re
import stat
import subprocess
import tempfile

from boot import BootError, create_durable, json_read, read_regular, real_directory, sync_directory, UID

APP = Path('/opt/world')
BUNDLE_FILES = {'node.json', 'executor.json', 'binding.json', 'alignment.json', 'profile.json', 'bridge-token'}
CODE_FILES = {'worldd', 'world-go2-executor', 'world-go2-lab', 'boot.py', 'release_boot.py',
              'run_bundle.py', 'install.py', 'seal_release.py', 'mission-entrypoint.sh',
              'release-entrypoint.sh', 'cyclonedds.xml', 'sim/go2_bridge/__init__.py',
              'sim/go2_bridge/__main__.py', 'sim/go2_bridge/core.py',
              'sim/go2_bridge/protocol.py', 'sim/go2_bridge/transport.py',
              'sim/go2_bridge/native/CMakeLists.txt', 'sim/go2_bridge/native/sensors.cpp',
         'sim/__init__.py',
         'sim/go2_cloud.py',
         'sim/go2_cloud_observer.py',
         'sim/go2_cloud_recording.py',
         'sim/go2_cloud_stream.py',
         'sim/go2_cloud_capture.py',
         'sim/go2_cloud_shadow.py',
         'sim/go2_cloud_live.py',
         'sim/go2_cloud_native/CMakeLists.txt',
         'sim/go2_cloud_native/cloud.cpp'}


def sha(raw):
    return hashlib.sha256(raw).hexdigest()


def selectors(bundle_sha, release_id, image_digest):
    if (not all(isinstance(x, str) and re.fullmatch('[0-9a-f]{64}', x) for x in (bundle_sha, release_id))
            or not isinstance(image_digest, str) or not re.fullmatch('sha256:[0-9a-f]{64}', image_digest)):
        raise BootError('invalid_public_selector')


def exact_file(raw, expected, limit):
    if (not isinstance(expected, dict) or set(expected) != {'sha256', 'bytes'}
            or type(expected['bytes']) is not int or not 0 <= expected['bytes'] <= limit
            or not isinstance(expected['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', expected['sha256'])
            or len(raw) != expected['bytes'] or sha(raw) != expected['sha256']):
        raise BootError('file_digest_mismatch')


def checked_release(release_id, app=APP):
    raw = read_regular(app / 'release.json')
    cfg = json_read(raw)
    if (sha(raw) != release_id or not isinstance(cfg, dict) or set(cfg) != {'schema', 'domain', 'arch', 'files'}
            or cfg['schema'] != 'wendy.world.go2-release.v1' or cfg['domain'] != 'simulation'
            or cfg['arch'] != 'arm64' or not isinstance(cfg['files'], dict) or set(cfg['files']) != CODE_FILES):
        raise BootError('release_mismatch')
    for name, expected in cfg['files'].items():
        exact_file(read_regular(app / name, 128 << 20), expected, 128 << 20)
    native = json_read(read_regular(app / 'native-digest.json', 1024))
    if (not isinstance(native, dict) or set(native) != {'schema', 'release_id', 'helper', 'cloud_helper'}
            or native['schema'] != 'wendy.world.go2-native-build.v2' or native['release_id'] != release_id):
        raise BootError('native_manifest_mismatch')
    exact_file(read_regular(app / 'world-go2-sensors', 16 << 20), native['helper'], 16 << 20)
    exact_file(read_regular(app / 'world-go2-cloud-live', 16 << 20), native['cloud_helper'], 16 << 20)


def read_bundle(directory, bundle_sha, release_id, image_digest):
    selectors(bundle_sha, release_id, image_digest)
    real_directory(directory)
    if set(os.listdir(directory)) != BUNDLE_FILES | {'run.json'}:
        raise BootError('unexpected_bundle_files')
    raw = read_regular(directory / 'run.json')
    cfg = json_read(raw)
    if (sha(raw) != bundle_sha or not isinstance(cfg, dict)
            or cfg.get('schema') != 'wendy.world.go2-run.v1' or cfg.get('domain') != 'simulation'
            or cfg.get('release_id') != release_id or cfg.get('image_digest') != image_digest
            or cfg.get('app_id') != 'com.wendy.world.go2' or cfg.get('service') != 'mission'
            or not isinstance(cfg.get('files'), dict) or set(cfg['files']) != BUNDLE_FILES):
        raise BootError('bundle_mismatch')
    captured = {'run.json': raw}
    for name in sorted(BUNDLE_FILES):
        info = os.lstat(directory / name)
        if info.st_mode & 0o077:
            raise BootError('bundle_not_private')
        captured[name] = read_regular(directory / name)
        exact_file(captured[name], cfg['files'][name], 65536)
    return cfg, captured


def private_snapshot(captured, parent=None, change_owner=False):
    # Only exact bytes already verified against the selected manifest enter
    # this process-private filesystem. The writable volume is never reloaded.
    directory = Path(tempfile.mkdtemp(prefix='world-go2-', dir=parent))
    try:
        if set(captured) != BUNDLE_FILES | {'run.json'}:
            raise BootError('snapshot_inventory')
        for name in sorted(captured):
            create_durable(directory / name, captured[name])
            if change_owner:
                os.chown(directory / name, UID, UID, follow_symlinks=False)
        if change_owner:
            os.chown(directory, UID, UID, follow_symlinks=False)
        return directory
    except BaseException:
        # There is no motion or durable launch here. Retain no ambiguous
        # snapshot for a later process to discover or reuse.
        import shutil
        shutil.rmtree(directory)
        raise


def validate_snapshot(directory, bundle_sha, release_id, image_digest, current, app=APP):
    argv = [str(app / 'world-go2-lab'), 'validate-run', '--directory', str(directory),
            '--bundle-sha256', bundle_sha, '--release-id', release_id, '--image-digest', image_digest]
    if current:
        argv.append('--current')
    try:
        # Fixed executable and bounded static output only; no input path/code
        # from the bundle, raw exceptions, credentials or key material in logs.
        result = subprocess.run(argv, stdin=subprocess.DEVNULL, stdout=subprocess.DEVNULL,
                                stderr=subprocess.DEVNULL, timeout=5, check=False)
        return result.returncode == 0
    except (OSError, subprocess.TimeoutExpired):
        return False


def marker(bundle_sha, release_id, image_digest):
    from json import dumps
    return (dumps({'schema': 'wendy.world.go2-installed.v1', 'bundle_sha256': bundle_sha,
                   'release_id': release_id, 'image_digest': image_digest}, separators=(',', ':')) + '\n').encode()


def load_installed(volume, bundle_sha, release_id, image_digest):
    selectors(bundle_sha, release_id, image_digest)
    real_directory(volume)
    if read_regular(volume / 'installed.json', 512) != marker(bundle_sha, release_id, image_digest):
        raise BootError('installation_marker_mismatch')
    return read_bundle(volume / 'commission', bundle_sha, release_id, image_digest)

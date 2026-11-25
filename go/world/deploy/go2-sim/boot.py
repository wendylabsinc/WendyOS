"""Private simulation app boot. One durable launch attempt; no worker restart."""
import fcntl
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import socket
import stat
import subprocess
import sys
import threading
import time

APP = Path('/opt/world')
DATA = Path('/data/world-go2')
UID = 65532
MAX_CONFIG = 65536
FILES = {'worldd', 'world-go2-executor', 'node.json', 'executor.json',
         'binding.json', 'alignment.json', 'profile.json', 'bridge-token',
         'boot.py', 'mission-entrypoint.sh', 'cyclonedds.xml',
         'sim/go2_bridge/__init__.py', 'sim/go2_bridge/__main__.py',
         'sim/go2_bridge/core.py', 'sim/go2_bridge/protocol.py',
         'sim/go2_bridge/transport.py', 'sim/go2_bridge/native/CMakeLists.txt',
         'sim/go2_bridge/native/sensors.cpp',
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


class BootError(Exception):
    pass


def emit(stage):
    # Static status only. Never log configuration, bearer token or credentials.
    print(json.dumps({'schema': 'wendy.world.go2-boot-status.v1',
                      'domain': 'simulation', 'stage': stage}), flush=True)


def unique_pairs(items):
    result = {}
    for key, value in items:
        if key in result:
            raise BootError('duplicate_json_field')
        result[key] = value
    return result


def read_regular(path, maximum=MAX_CONFIG):
    try:
        before = os.lstat(path)
        if not stat.S_ISREG(before.st_mode) or before.st_size > maximum:
            raise BootError('invalid_regular_file')
        fd = os.open(path, os.O_RDONLY | os.O_NONBLOCK | os.O_NOFOLLOW)
        with os.fdopen(fd, 'rb') as stream:
            after = os.fstat(stream.fileno())
            if (not stat.S_ISREG(after.st_mode) or
                    (before.st_ino, before.st_dev) != (after.st_ino, after.st_dev)):
                raise BootError('file_changed')
            data = stream.read(maximum + 1)
        if len(data) > maximum:
            raise BootError('file_too_large')
        return data
    except FileNotFoundError:
        raise
    except BootError:
        raise
    except OSError:
        raise BootError('file_read_failed') from None


def json_read(raw):
    try:
        return json.loads(raw, object_pairs_hook=unique_pairs,
                          parse_constant=lambda _: (_ for _ in ()).throw(BootError('invalid_json_number')))
    except (ValueError, UnicodeError, RecursionError):
        raise BootError('invalid_json') from None


def checked_config(app=APP):
    raw = read_regular(app / 'boot.json')
    cfg = json_read(raw)
    if (not isinstance(cfg, dict) or set(cfg) != {'schema', 'run_id', 'task_id', 'gateway_port', 'bridge_port', 'files'}
            or cfg['schema'] != 'wendy.world.go2-boot.v1'
            or not isinstance(cfg['run_id'], str) or not re.fullmatch(r'[a-z0-9][a-z0-9-]{0,31}', cfg['run_id'])
            or not isinstance(cfg['task_id'], str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.:-]{0,127}', cfg['task_id'])
            or any(type(cfg[k]) is not int or not 1024 <= cfg[k] <= 65535 or cfg[k] == 8890 for k in ('gateway_port', 'bridge_port'))
            or cfg['gateway_port'] == cfg['bridge_port']
            or not isinstance(cfg['files'], dict) or set(cfg['files']) != FILES):
        raise BootError('invalid_boot_configuration')
    for name, expected in cfg['files'].items():
        if (not isinstance(expected, dict) or set(expected) != {'bytes', 'sha256'}
                or type(expected['bytes']) is not int or not 0 <= expected['bytes'] <= 128 << 20
                or not isinstance(expected['sha256'], str) or not re.fullmatch('[0-9a-f]{64}', expected['sha256'])):
            raise BootError('invalid_file_digest')
        observed = read_regular(app / name, 128 << 20)
        if len(observed) != expected['bytes'] or hashlib.sha256(observed).hexdigest() != expected['sha256']:
            raise BootError('staged_file_changed')
    return cfg, hashlib.sha256(raw).hexdigest()


def sync_directory(path):
    fd = os.open(path, os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def create_durable(path, raw):
    # A partial file on error remains a refusal on next boot, never a retry.
    fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    with os.fdopen(fd, 'wb') as stream:
        stream.write(raw)
        stream.flush()
        os.fsync(stream.fileno())
    sync_directory(path.parent)


def real_directory(path):
    info = os.lstat(path)
    if not stat.S_ISDIR(info.st_mode):
        raise BootError('invalid_state_directory')
    return info


def prepare_state(config_hash, data=DATA, change_owner=True):
    # /data is this app's unique persisted volume. Never walk or chown retained
    # records, siblings or parent directories; only this owned child and lock.
    real_directory(data.parent)
    try:
        os.mkdir(data, 0o700)
    except FileExistsError:
        pass
    info = real_directory(data)
    # The child entry itself must survive power loss, not only files beneath it.
    # Repeat on existing directories too before trusting the launch marker.
    sync_directory(data.parent)
    if info.st_uid not in (0, UID) or info.st_mode & 0o077:
        raise BootError('state_permissions')
    lock = os.open(data / 'boot.lock', os.O_RDWR | os.O_CREAT | os.O_NOFOLLOW | os.O_NONBLOCK, 0o600)
    try:
        lock_info = os.fstat(lock)
        if not stat.S_ISREG(lock_info.st_mode) or lock_info.st_mode & 0o077:
            raise BootError('invalid_boot_lock')
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        expected = (config_hash + '\n').encode('ascii')
        fence = data / 'configuration.sha256'
        try:
            if read_regular(fence, 65) != expected:
                raise BootError('persistent_configuration_changed')
        except FileNotFoundError:
            # Existing state without its initialization fence is ambiguous.
            if set(p.name for p in data.iterdir()) != {'boot.lock'}:
                raise BootError('unfenced_existing_state')
            create_durable(fence, expected)
        if change_owner:
            if os.geteuid() != 0:
                raise BootError('root_initialization_required')
            # The mounted parent may be 0700. Grant traversal without changing
            # its owner or exposing the 0700 child and its 0600 state files.
            parent = real_directory(data.parent)
            if parent.st_uid != 0 or parent.st_mode & 0o022:
                raise BootError('invalid_volume_permissions')
            os.chmod(data.parent, stat.S_IMODE(parent.st_mode) | 0o111)
            os.chown(data, UID, UID, follow_symlinks=False)
            os.fchown(lock, UID, UID)
            os.chown(fence, UID, UID, follow_symlinks=False)
        return lock
    except BaseException:
        os.close(lock)
        raise


def drop_privileges():
    os.setgroups([])
    os.setgid(UID)
    os.setuid(UID)
    if os.geteuid() != UID or os.getegid() != UID:
        raise BootError('privilege_drop_failed')


def claim_launch(config_hash, data=DATA):
    path = data / 'launch-once.json'
    marker = json.dumps({'schema': 'wendy.world.go2-launch.v1', 'configuration_sha256': config_hash},
                        separators=(',', ':')).encode() + b'\n'
    try:
        create_durable(path, marker)
        return True
    except FileExistsError:
        if read_regular(path, 512) != marker:
            raise BootError('invalid_launch_marker')
        return False


def wait_port(child, port, timeout, closing):
    until = time.monotonic() + timeout
    while not closing.is_set() and time.monotonic() < until:
        if child.poll() is not None:
            return False
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=.1):
                return child.poll() is None and not closing.is_set()
        except OSError:
            closing.wait(.05)
    return False


def stop_child(child):
    if child is None:
        return
    # The bridge owns a native sensor subprocess. Signal its process group too;
    # no descendant may survive a fault or become a later motion session.
    try:
        os.killpg(child.pid, signal.SIGTERM)
    except ProcessLookupError:
        pass
    try:
        child.wait(timeout=2)
    except subprocess.TimeoutExpired:
        pass
    # The main child may exit while a helper ignores TERM. Always make the
    # bounded final group-kill attempt, even after a successful main-child wait.
    try:
        os.killpg(child.pid, signal.SIGKILL)
    except ProcessLookupError:
        pass
    child.wait(timeout=2)


def start(argv):
    return subprocess.Popen(argv, cwd=APP, start_new_session=True, close_fds=True)


def supervise(cfg, config_hash, closing, data=DATA, spawn=start, ready=wait_port, stop=stop_child,
              config_dir=APP, can_launch=lambda: True):
    gateway = bridge = executor = None
    try:
        gateway = spawn([str(APP / 'worldd'), '-config', str(config_dir / 'node.json')])
        if not ready(gateway, cfg['gateway_port'], 15, closing):
            raise BootError('gateway_start_failed')
        emit('gateway_ready')
        if not claim_launch(config_hash, data):
            emit('restart_reporting_only')
        elif closing.is_set():
            emit('launch_consumed_cancelled')
        elif not can_launch():
            emit('motion_configuration_refused_reporting_only')
        elif closing.is_set():
            emit('launch_consumed_cancelled')
        else:
            # Marker is durable BEFORE either motion-capable worker exists.
            emit('launch_attempt_consumed')
            bridge = spawn(['python3', '-m', 'sim.go2_bridge', 'serve',
                            '--binding', str(config_dir / 'binding.json'),
                            '--alignment', str(config_dir / 'alignment.json'),
                            '--profile', str(config_dir / 'profile.json'),
                            '--token-file', str(config_dir / 'bridge-token'),
                            '--runtime', 'http://127.0.0.1:8890',
                            '--listen-port', str(cfg['bridge_port'])])
            if ready(bridge, cfg['bridge_port'], 10, closing) and gateway.poll() is None and not closing.is_set():
                executor = spawn([str(APP / 'world-go2-executor'),
                                  '--credentials', str(config_dir / 'executor.json'),
                                  '--adapter', 'http://127.0.0.1:' + str(cfg['bridge_port']),
                                  '--adapter-token-file', str(config_dir / 'bridge-token'),
                                  '--binding', str(config_dir / 'binding.json'),
                                  '--alignment', str(config_dir / 'alignment.json'),
                                  '--profile', str(config_dir / 'profile.json'),
                                  '--task-id', cfg['task_id'],
                                  '--fault-snapshot', str(DATA / 'first-fault.json'),
                                  '--fault-snapshot-log', '--observation-log'])
                emit('executor_started_generation_unconfirmed')
            else:
                stop(bridge)
                bridge = None
                emit('motion_start_failed_reporting_only')
        while not closing.wait(.1):
            if gateway.poll() is not None:
                raise BootError('gateway_exited')
            if ((executor is not None and executor.poll() is not None) or
                    (bridge is not None and bridge.poll() is not None)):
                stop(executor)
                stop(bridge)
                executor = bridge = None
                emit('motion_worker_exited_reporting_only')
        return 0
    finally:
        stop(executor)
        stop(bridge)
        stop(gateway)


def main():
    lock = None
    try:
        os.umask(0o077)
        cfg, config_hash = checked_config()
        lock = prepare_state(config_hash)
        drop_privileges()
        closing = threading.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, lambda *_: closing.set())
        return supervise(cfg, config_hash, closing)
    except (BootError, OSError):
        emit('boot_refused')
        return 1
    finally:
        if lock is not None:
            os.close(lock)


if __name__ == '__main__':
    sys.exit(main())

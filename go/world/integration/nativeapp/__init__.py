"""Launch the integrated Wendy desktop app without touching an existing instance."""
import json
import os
from pathlib import Path
import stat
import subprocess
import time


class ReviewCancelled(RuntimeError):
    pass


def executable(app=None):
    """Use an explicit app/binary, WENDY_DESKTOP_APP, or installed Wendy.app."""
    selected = app or os.environ.get('WENDY_DESKTOP_APP')
    candidates = [Path(selected).expanduser()] if selected else [Path('/Applications/Wendy.app'), Path('/Applications/WendyDesktop.app'), Path.home() / 'Applications/Wendy.app', Path.home() / 'Applications/WendyDesktop.app']
    for candidate in candidates:
        binary = candidate / 'Contents/MacOS/WendyDesktop' if candidate.suffix == '.app' else candidate
        if binary.is_file() and os.access(binary, os.X_OK):
            return str(binary.resolve())
    raise ValueError('Supply --app /absolute/path/Wendy.app (or WendyDesktop binary), or set WENDY_DESKTOP_APP; build the integrated macOS app first')


def bounded(path, limit):
    path = Path(path)
    # O_NOFOLLOW also closes the final-component symlink race.
    with os.fdopen(os.open(path, os.O_RDONLY | os.O_NOFOLLOW | os.O_NONBLOCK), 'rb') as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or not 0 < info.st_size <= limit:
            raise ValueError('Expected bounded regular file: ' + str(path))
        raw = stream.read(limit + 1)
        if len(raw) != info.st_size or len(raw) > limit:
            raise ValueError('File changed while reading: ' + str(path))
        return raw


def document(raw):
    def pairs(values):
        result = {}
        for key, value in values:
            if key in result:
                raise ValueError('Duplicate JSON field')
            result[key] = value
        return result
    def nonfinite(_):
        raise ValueError('Nonfinite JSON number')
    return json.loads(raw, object_pairs_hook=pairs, parse_constant=nonfinite)


def stop_owned(process):
    """Only accepts a subprocess handle created by this launcher, never a PID lookup."""
    if process.poll() is None:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def wait_review(process, input_path, original, output, validate, timeout=3600, cancel=None, interval=.1):
    """Panel completion is an output document, not desktop application exit."""
    deadline = time.monotonic() + timeout
    while True:
        if bounded(input_path, 512 << 10) != original:
            raise ValueError('Review input changed; approval refused')
        if cancel is not None and cancel():
            raise ReviewCancelled('Review canceled; no mission started')
        if os.path.lexists(output):
            value = document(bounded(output, 16384))
            if value == {'action': 'cancel'}:
                raise ReviewCancelled('Review canceled; no mission started')
            if not isinstance(value, dict):
                raise ValueError('Review output must be an object')
            validate(value, document(original))
            if bounded(input_path, 512 << 10) != original:
                raise ValueError('Review input changed; approval refused')
            return value
        if process.poll() is not None:
            raise ReviewCancelled('Review app exited without approval; no mission started')
        if time.monotonic() >= deadline:
            raise TimeoutError('Review deadline expired; no mission started')
        time.sleep(min(interval, max(0, deadline-time.monotonic())))


def review(app, flag, input_path, output, validate, timeout=3600):
    input_path, output = Path(input_path).resolve(), Path(output).absolute()
    if os.path.lexists(output):
        raise ValueError('Review requires a fresh output file')
    original = bounded(input_path, 512 << 10)
    document(original)
    output_flag = '--offline-selection' if flag == '--offline-patrol-kits' else '--schedule-output'
    # Direct execution creates an owned process. Do not use `open` (which may
    # route to the user's running app) or kill by application name.
    process = subprocess.Popen([executable(app), flag, str(input_path), output_flag, str(output)], start_new_session=True)
    try:
        return wait_review(process, input_path, original, output, validate, timeout)
    finally:
        stop_owned(process)


def validate_selection(value, entries):
    if value == {'action': 'prepare'}:
        return
    if set(value) != {'action', 'id'} or value['action'] != 'activate' or not isinstance(value['id'], str):
        raise ValueError('Explicit kit activation or preparation required')
    matches = [entry for entry in entries if entry.get('profile', {}).get('id') == value['id'] and entry.get('state') == 'ready']
    if len(matches) != 1:
        raise ValueError('Selected kit is not in the reviewed ready inventory')


def validate_schedule(value, source, tour=False):
    # Full policy/authority validation remains in world-patrol before signing.
    allowed = {'tour', 'return', 'exclusions', 'environment', 'work', 'world_watch'} if tour else {'revisit_seconds', 'return', 'exclusions', 'environment', 'work', 'world_watch'}
    if set(value) - allowed:
        raise ValueError('Unknown review output fields')
    rooms = {room['id'] for room in source['rooms']}
    if not tour:
        intervals = value.get('revisit_seconds')
        if not isinstance(intervals, dict) or set(intervals) != rooms or not all(type(n) is int and 10 <= n <= 86400 for n in intervals.values()):
            raise ValueError('Reviewed intervals must match the original room inventory')
        return
    policy = value.get('tour', {})
    stops = policy.get('stops', [])
    if policy.get('method') != 'inspect-selected-rooms-once-and-return-v1' or not 1 <= len(stops) <= 8:
        raise ValueError('Invalid reviewed inspection tour')
    ids = [stop.get('room_id') for stop in stops]
    if len(set(ids)) != len(ids) or not set(ids) <= rooms:
        raise ValueError('Tour differs from the reviewed room inventory')
    options = {option['id']: option for option in source['rendezvous_options']}
    for stop in stops:
        option = options.get(stop['room_id'])
        if not option or stop.get('position') != [option['x'], .3, -option['y']] or stop.get('yaw') != option['yaw']:
            raise ValueError('Tour target differs from the reviewed observation point')
    start = source['rendezvous_options'][0]
    if value.get('return') != {'position': [start['x'], .3, -start['y']], 'yaw': start['yaw'], 'before_expiry_seconds': 0}:
        raise ValueError('Tour return differs from the reviewed rendezvous')

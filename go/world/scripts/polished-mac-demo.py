#!/usr/bin/env python3
"""Build and launch the private native macOS World demonstration."""
import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import time
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))
from integration import nativeapp


def dependencies():
    if platform.system() != 'Darwin':
        raise ValueError('This native demo requires macOS.')
    if platform.machine() != 'arm64' or int(platform.mac_ver()[0].split('.')[0]) < 14:
        raise ValueError('This build requires Apple Silicon and macOS 14 or later.')
    required = ('go', 'python3.12')
    missing = [name for name in required if not shutil.which(name)]
    if missing:
        raise ValueError('Missing tools: ' + ', '.join(missing))
    return {name: shutil.which(name) for name in required}


def write_new(path, value):
    with path.open('x') as stream:
        json.dump(value, stream, indent=2, allow_nan=False)
        stream.write('\n')


def fresh_output(path):
    path = path.absolute()
    local = ROOT / '.local'
    if path == local or not path.is_relative_to(local):
        raise ValueError('Choose a new directory under this repository\'s .local folder.')
    if any(part in ('.', '..') for part in path.parts):
        raise ValueError('Output path must not contain dot segments.')
    for parent in [path, *path.parents]:
        if parent.is_symlink():
            raise ValueError('Output path must not use symbolic links.')
        if parent == ROOT:
            break
    path.mkdir(parents=True, mode=0o700, exist_ok=False)
    return path


def wait_ready(process, path, timeout=45):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if process.poll() is not None:
            raise RuntimeError('Simulation exited before readiness; inspect bridge.log.')
        if path.is_file():
            with path.open() as stream:
                return json.load(stream)
        time.sleep(.1)
    raise RuntimeError('Simulation startup timed out; inspect bridge.log.')


def freeze_twin(out):
    """Keep this run's exact synthetic scene beside its shareable twin archive."""
    original = ROOT / 'examples/collaborative-office'
    raw = (original / 'scene.json').read_bytes()
    if len(raw) > 2 << 20:
        raise ValueError('Scene metadata exceeds the demo bound.')
    config = json.loads(raw)
    if config.get('model', {}).get('path') != 'office.xml':
        raise ValueError('Expected the self-contained office model.')
    model = (original / 'office.xml').read_bytes()
    if len(model) > 8 << 20 or hashlib.sha256(model).hexdigest() != config['model']['sha256']:
        raise ValueError('Office model does not match its scene digest.')
    twin = out / 'digital-twin'
    twin.mkdir(mode=0o700)
    files = {'scene.json': raw, 'office.xml': model,
             'README.md': ('# Wendy World office demo\n\n'
                 'This is a synthetic furnished office for the private Wendy World '
                 'macOS demo, using MuJoCo 4. It is not a scan of a real office.\n\n'
                 'Coordinates use metres with X/Y on the floor and Z up. '
                 'The scene includes three simplified planar robots, the known '
                 'navigation grid and six inspection destinations. The model '
                 'digest is bound by scene.json. No hardware authority is included.\n').encode()}
    inventory = {}
    for name, data in files.items():
        with (twin / name).open('xb') as stream:
            stream.write(data)
        inventory[name] = {'bytes': len(data), 'sha256': hashlib.sha256(data).hexdigest()}
    write_new(twin / 'manifest.json', {'version': 1, 'synthetic': True, 'files': inventory})
    with zipfile.ZipFile(out / 'office-digital-twin.zip', 'x', zipfile.ZIP_DEFLATED) as archive:
        for path in sorted(twin.iterdir()):
            archive.write(path, 'office-digital-twin/' + path.name)
    return twin / 'scene.json', inventory


def stop_owned(process):
    if process is None or process.poll() is not None:
        return
    try:
        process.wait(timeout=8)
    except subprocess.TimeoutExpired:
        process.terminate()
        try:
            process.wait(timeout=5)
        except subprocess.TimeoutExpired:
            process.kill()
            process.wait(timeout=5)


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--app', type=Path, help='integrated Wendy.app or WendyDesktop executable')
    parser.add_argument('--out', type=Path, help='new private directory under .local')
    parser.add_argument('--duration', type=int, default=1800, help='wall-clock lifetime, 30..3600 seconds')
    parser.add_argument('--preflight', action='store_true', help='check tools without starting or changing a demo')
    args = parser.parse_args()
    tools = dependencies()
    executable = nativeapp.executable(args.app)
    if args.preflight:
        print(json.dumps({'platform': platform.platform(), 'tools': tools, 'app': executable, 'runtime_started': False}, indent=2))
        return
    if args.out is None or not 30 <= args.duration <= 3600:
        parser.error('--out and a duration from 30 to 3600 seconds are required')
    os.umask(0o077)
    out = fresh_output(args.out)
    run_id = 'macos-office-' + uuid.uuid4().hex[:12]
    scene, twin_inventory = freeze_twin(out)
    binary = out / 'world-collab-demo'
    fusion_binary = out / 'world-fusion-demo'
    bridge = app = None
    started = time.monotonic()
    receipt = {'version': 1, 'run_id': run_id, 'synthetic': True,
               'scene': str(scene.relative_to(out)), 'twin_files': twin_inventory,
               'duration_seconds': args.duration,
               'physics': 'MuJoCo 4', 'view': 'native SwiftUI and SceneKit',
               'network': 'simulated local peer mesh', 'hardware': False,
               'fusion': 'calibrated camera + LiDAR location, analytic gas and generated audio evidence'}
    write_new(out / 'run.json', receipt)
    try:
        with (out / 'build.log').open('x') as log:
            print('Building the simulation coordinator...', flush=True)
            subprocess.run([tools['go'], 'build', '-o', str(binary), './cmd/world-collab-demo'],
                           cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
            subprocess.run([tools['go'], 'build', '-o', str(fusion_binary), './cmd/world-fusion-demo'],
                           cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
            print('Preparing MuJoCo 4...', flush=True)
            venv = out / 'venv'
            subprocess.run([tools['python3.12'], '-m', 'venv', str(venv)],
                           stdout=log, stderr=subprocess.STDOUT, check=True, timeout=90)
            python = venv / 'bin/python'
            subprocess.run([str(python), '-m', 'pip', 'install', '-r', str(ROOT / 'sim/requirements.txt')],
                           stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
        with (out / 'bridge.log').open('x') as bridge_log, (out / 'app.log').open('x') as app_log:
            runtime_deadline = time.monotonic() + args.duration
            bridge = subprocess.Popen([str(python), '-u', '-m', 'sim.macos_demo_bridge',
                '--scene', str(scene), '--coordinator', str(binary), '--out', str(out),
                '--fusion', str(fusion_binary),
                '--run-id', run_id, '--duration', str(args.duration)], cwd=ROOT,
                env=dict(os.environ, PYTHONDONTWRITEBYTECODE='1'), stdout=bridge_log, stderr=subprocess.STDOUT)
            wait_ready(bridge, out / 'bridge-ready.json')
            app = subprocess.Popen([executable, '--world-session', str(out)], cwd=ROOT,
                                   stdout=app_log, stderr=subprocess.STDOUT)
            print('Opening Wendy World. The simulation waits for Start mission.', flush=True)
            print('Sensor observations, localized hazards, viewpoints and map replicas are in the sidebar.', flush=True)
            print(f'Quit this owned Wendy instance or create {out / "stop"} to stop this run.', flush=True)
            while app.poll() is None and bridge.poll() is None and time.monotonic() < runtime_deadline:
                time.sleep(.2)
            if app.poll() not in (None, 0):
                raise RuntimeError('Native app exited unsuccessfully; inspect app.log.')
            if bridge.poll() not in (None, 0):
                raise RuntimeError('Simulation exited unsuccessfully; inspect bridge.log.')
    finally:
        (out / 'stop').touch(exist_ok=True)
        stop_owned(bridge)
        stop_owned(app)
        write_new(out / 'launcher-final.json', {'run_id': run_id,
            'elapsed_seconds': time.monotonic() - started,
            'bridge_exit': None if bridge is None else bridge.returncode,
            'app_exit': None if app is None else app.returncode,
            'owned_processes_stopped': all(p is None or p.poll() is not None for p in (bridge, app))})


def interrupt(signum, frame):
    raise KeyboardInterrupt


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupt)
    try:
        run()
    except KeyboardInterrupt:
        sys.exit(130)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as error:
        print(f'Demo failed: {error}', file=sys.stderr)
        sys.exit(1)

#!/usr/bin/env python3
"""Build and open the private, simulation-only collaborative office demo."""
import argparse
import json
import os
from pathlib import Path
import platform
import shutil
import signal
import subprocess
import sys
import uuid

ROOT = Path(__file__).resolve().parents[1]


def run():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', type=Path, required=True, help='new output folder under .local')
    parser.add_argument('--duration', type=int, default=1800)
    args = parser.parse_args()
    if platform.system() != 'Darwin':
        parser.error('the native launcher requires macOS')
    if not 30 <= args.duration <= 3600:
        parser.error('duration must be 30..3600 seconds')
    for name in ('go', 'python3.12'):
        if not shutil.which(name):
            parser.error(f'{name} is required')
    out = args.out.resolve()
    if not out.is_relative_to(ROOT/'.local') or out == ROOT/'.local':
        parser.error('output must be a new directory inside this repository .local folder')
    os.umask(0o077)
    out.mkdir(parents=True, mode=0o700, exist_ok=False)
    binary = out/'world-collab-demo'
    with (out/'build.log').open('w') as log:
        print('Building the Go team coordinator...', flush=True)
        subprocess.run(['go', 'build', '-o', str(binary), './cmd/world-collab-demo'],
                       cwd=ROOT, stdout=log, stderr=subprocess.STDOUT, check=True, timeout=180)
        print('Preparing the native MuJoCo renderer...', flush=True)
        venv = out/'venv'
        subprocess.run(['python3.12', '-m', 'venv', str(venv)], check=True, stdout=log, stderr=subprocess.STDOUT, timeout=90)
        python = venv/'bin/python'
        subprocess.run([str(python), '-m', 'pip', 'install', '-r', str(ROOT/'sim/requirements.txt')],
                       check=True, stdout=log, stderr=subprocess.STDOUT, timeout=180)
    run_id = 'collaborative-office-'+uuid.uuid4().hex[:10]
    receipt = {'run_id': run_id, 'synthetic': True, 'transport': 'simulated peer-to-peer mesh with multi-hop forwarding and voxel gossip',
               'coordinator': 'central Go dispatcher', 'duration_seconds': args.duration,
               'scene': 'examples/collaborative-office/scene.json'}
    (out/'run.json').write_text(json.dumps(receipt, indent=2)+'\n')
    print('Opening the office. Press START TEAM or Space to authorize this simulation.', flush=True)
    print('Click any mesh arch to cut or restore its edge. Keys 1/2/3 toggle direct hub edges.', flush=True)
    print(f'Close the window or create {out / "stop"} to stop. Log: {out / "viewer.log"}', flush=True)
    process = None
    try:
        with (out/'viewer.log').open('w') as log:
            log.write('Starting native GLFW renderer; saved-window restoration disabled for this process.\n')
            log.flush()
            process = subprocess.Popen([str(python), '-u', '-m', 'sim.collaborative_viewer',
                '--scene', str(ROOT/receipt['scene']), '--coordinator', str(binary),
                '--out', str(out), '--run-id', run_id, '--duration', str(args.duration),
                '-ApplePersistenceIgnoreState', 'YES'], cwd=ROOT, stdout=log, stderr=subprocess.STDOUT)
            code = process.wait()
            if code:
                raise RuntimeError(f'viewer exited with code {code}; inspect {out / "viewer.log"}')
    finally:
        if process and process.poll() is None:
            # Ask the render loop to stop and close its owned coordinator first.
            (out/'stop').touch()
            try:
                process.wait(timeout=8)
            except subprocess.TimeoutExpired:
                process.terminate()
                process.wait(timeout=5)


def interrupt(signum, frame):
    raise KeyboardInterrupt


if __name__ == '__main__':
    signal.signal(signal.SIGTERM, interrupt)
    try:
        run()
    except KeyboardInterrupt:
        sys.exit(130)
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(f'Demo failed: {exc}', file=sys.stderr)
        sys.exit(1)

"""Fetch only the pinned official follower assets; never regenerate the lock."""
import argparse
import hashlib
import json
from pathlib import Path, PurePosixPath
from urllib.request import urlopen

ROOT = Path(__file__).resolve().parents[1]


def fetch(check=False):
    lock = json.loads((ROOT / 'assets.lock.json').read_text())
    base = f"https://raw.githubusercontent.com/TheRobotStudio/SO-ARM100/{lock['revision']}/"
    for name, digest in lock['sha256'].items():
        path = PurePosixPath(name)
        if path.is_absolute() or '..' in path.parts:
            raise ValueError('unsafe asset path')
        target = ROOT / 'model' / name
        if target.exists():
            data = target.read_bytes()
        elif check:
            raise ValueError(f'missing asset: {name}')
        else:
            upstream = 'LICENSE' if name == 'LICENSE' else 'Simulation/SO101/' + ('README.md' if name == 'UPSTREAM_README.md' else name)
            with urlopen(base + upstream, timeout=60) as response:
                data = response.read()
        if hashlib.sha256(data).hexdigest() != digest:
            raise ValueError(f'asset digest mismatch: {name}')
        if not check and not target.exists():
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
    fetch_viewer(check)


def fetch_viewer(check=False):
    lock = json.loads((ROOT / 'viewer-assets.lock.json').read_text())
    base = f"https://raw.githubusercontent.com/mrdoob/three.js/{lock['revision']}/"
    for name, entry in lock['files'].items():
        if name not in {'three.module.js', 'three.core.js', 'OrbitControls.js'}:
            raise ValueError('unexpected viewer module')
        target = ROOT / 'so101_sim' / 'vendor' / name
        if target.exists():
            data = target.read_bytes()
            # The only local adaptation is the browser-relative import.
            if name == 'OrbitControls.js':
                data = data.replace(b"from './three.module.js';", b"from 'three';")
        elif check:
            raise ValueError(f'missing viewer module: {name}')
        else:
            with urlopen(base + entry['path'], timeout=60) as response:
                data = response.read(4 * 1024 * 1024 + 1)
        if hashlib.sha256(data).hexdigest() != entry['sha256']:
            raise ValueError(f'viewer digest mismatch: {name}')
        if not check and not target.exists():
            target.parent.mkdir(parents=True, exist_ok=True)
            if name == 'OrbitControls.js':
                data = data.replace(b"from 'three';", b"from './three.module.js';")
            target.write_bytes(data)


if __name__ == '__main__':
    parser = argparse.ArgumentParser()
    parser.add_argument('--check', action='store_true')
    fetch(parser.parse_args().check)

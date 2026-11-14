#!/usr/bin/env python3
"""Run the actual pinned expert scene and historical controls; never synthesize assets."""
import argparse
import hashlib
import importlib.metadata
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import xml.etree.ElementTree as ET

ROOT = Path(__file__).resolve().parents[2]
EXAMPLE = ROOT / 'Examples/G1CokeDemo'
SPEC = importlib.util.spec_from_file_location('coke_scene_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNER)


def digest(path):
    value = hashlib.sha256()
    with path.open('rb') as stream:
        for chunk in iter(lambda: stream.read(1024 * 1024), b''):
            value.update(chunk)
    return value.hexdigest()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--expert', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    source, output = args.expert.resolve(strict=True), args.output.resolve()
    if sys.version_info[:2] != (3, 12):
        raise RuntimeError('Coke requires Python3.12')
    for name, version in {'mujoco': '3.12.0', 'numpy': '2.2.6', 'imageio-ffmpeg': '0.6.0'}.items():
        if importlib.metadata.version(name) != version:
            raise RuntimeError(f'requires pinned {name}=={version}')
    output.mkdir(parents=True, exist_ok=False)
    lock = json.loads((EXAMPLE / 'assets.lock.json').read_text())
    assets = {name.removeprefix('expert/'): record for name, record in lock['files'].items() if name.startswith('expert/')}
    def verify_assets():
        for name, record in assets.items():
            candidate = source / name
            if not candidate.resolve().is_relative_to(source) or not candidate.is_file() or candidate.stat().st_size != record['bytes'] or digest(candidate) != record['sha256']:
                raise RuntimeError('exact recorded expert asset unavailable or changed: ' + name)
    verify_assets()
    originals = {name: (EXAMPLE / 'coke_demo' / name).read_text() for name in ('__init__.py', 'simulation.py', 'scene.py')}
    controls = [
        ('virtual-reset', 'simulation.py', '        Simulation.reset(self)', '        self.reset()', 'COKE_INITIAL_EPOCH'),
        ('required-target', 'scene.py', '    def step(self, target_q43=None):', '    def step(self, target_q43):', 'COKE_DEFAULT_REFERENCE'),
        ('old-effort-field', 'scene.py', '"joint_effort43": self._pose_data.qfrc_actuator', '"joint_effort": self._pose_data.qfrc_actuator', 'COKE_EFFORT_SCHEMA'),
    ]
    results = {}
    evidence = {'audit': '2022', 'status': 'failed', 'assets': assets, 'results': results,
                'versions': {name: importlib.metadata.version(name) for name in ('mujoco', 'numpy', 'imageio-ffmpeg', 'pytest')},
                'fixture_sources': {str(p.relative_to(ROOT)): digest(p) for p in (Path(__file__).resolve(), EXAMPLE / 'tests/test_scene_runtime.py', EXAMPLE / 'assets.lock.json', ROOT / '.github/scripts/run-owned-media-test.py')},
                'sources': {name: hashlib.sha256(text.encode()).hexdigest() for name, text in originals.items()},
                'scope': 'actual recorded attempt-000001 scene/replay/renderer, no frozen-policy inference or physical attainment'}
    try:
        with tempfile.TemporaryDirectory(prefix='coke-scene-') as temporary:
            work = Path(temporary)
            package, expert = work / 'coke_demo', work / 'expert'
            package.mkdir(); expert.mkdir()
            for name in assets:
                target = expert / name
                target.parent.mkdir(parents=True, exist_ok=True)
                target.symlink_to(source / name)
            shutil.copyfile(EXAMPLE / 'assets.lock.json', work / 'assets.lock.json')
            for mode, filename, before, after, marker in [('normal', None, None, None, 'COKE_SCENE_RUNTIME_VERIFIED')] + controls:
                verify_assets()
                for name, text in originals.items():
                    if name == filename:
                        if text.count(before) != 1:
                            raise RuntimeError(mode + ' historical boundary changed')
                        text = text.replace(before, after)
                    (package / name).write_text(text)
                environment = dict(os.environ, PYTHONPATH=str(work), WENDY_COKE_EXPERT=str(expert),
                                   MUJOCO_GL='osmesa' if sys.platform == 'linux' else 'cgl',
                                   PYTEST_DISABLE_PLUGIN_AUTOLOAD='1', PYTHONDONTWRITEBYTECODE='1')
                report = output / (mode + '.xml')
                with (output / (mode + '.log')).open('w') as log:
                    code = OWNER.run([sys.executable, '-m', 'pytest', '-q', '-s', '-p', 'no:cacheprovider', '--junitxml', str(report), str(EXAMPLE / 'tests/test_scene_runtime.py')], 240 if mode == 'normal' else 90, cwd=work, env=environment, stdout=log)
                text = (output / (mode + '.log')).read_text()
                print(text)
                verify_assets()
                cases = ET.parse(report).findall('.//testcase')
                if len(cases) != 1 or cases[0].find('skipped') is not None or cases[0].find('error') is not None:
                    raise RuntimeError('scene case missing, skipped or errored')
                failure = cases[0].find('failure')
                if mode == 'normal':
                    accepted = code == 0 and failure is None and marker in text
                else:
                    accepted = code == 1 and failure is not None and failure.get('message', '').startswith('AssertionError: ' + marker) and 'COKE_SCENE_RUNTIME_VERIFIED' not in text
                if not accepted:
                    raise RuntimeError(mode + ' failed outside required scene boundary')
                results[mode] = {'exit_code': code, 'expected': True}
        evidence['status'] = 'passed'
    finally:
        (output / 'evidence.json').write_text(json.dumps(evidence, indent=2) + '\n')
    print('COKE_RECORDED_SCENE_MATRIX_VERIFIED exact expert and three historical controls')


if __name__ == '__main__':
    main()

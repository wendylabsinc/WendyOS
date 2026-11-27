#!/usr/bin/env python3
"""Prepare exact offline ordinary-app contexts; no Docker or runtime execution."""
import argparse
import os
from pathlib import Path
import shutil
import stat

import libraries

shared, pair = libraries.stage, libraries.pair
ROOT = Path(__file__).resolve().parents[2]
TOOLS = ('agent', 'cli', 'fixture', 'pair-fixture', 'initializer', 'app-runner', 'camera', 'people', 'reader')
RUNTIME = 'sha256:022af2368ce23973649848d7ca34cffe77842c9428f3f08f76903cebdf410d97'
ROOTFS = {'scene': 'sha256:105d48a69fbbbc7ad4105841893269f46fecc2dffd1578a69964d00e8dccba47',
          'pose': 'sha256:e949980b00449fe7013aa4944e926a9151cd9ed09d972f25b4d92927653ba374'}
SERVICES = {'a': ('scene', 'camera-a', 'camera-b', 'people'), 'b': ('reader',)}
REMOTE = '/var/lib/people-stage'


def inputs(raw):
    value = pair.strict_json(raw)
    pair.require(isinstance(value, dict) and set(value) == {'version', 'run_id', 'runtime_image', 'binaries', 'rootfs', 'anchor_unix_ns', 'model'}, 'exact people inputs required')
    pair.require(value['version'] == 1 and value['runtime_image'] == RUNTIME, 'pinned runtime required')
    pair.sample_domain(value['run_id'])
    pair.require(type(value['anchor_unix_ns']) is int and value['anchor_unix_ns'] == 1767225600000000000, 'commissioned January 2026 simulation anchor required')
    pair.require(set(value['binaries']) == set(TOOLS), 'exact tool set required')
    for row in value['binaries'].values():
        pair.require(set(row) == {'path', 'sha256'} and pair.digest_shape(row['sha256']), 'binary pin required')
        raw = pair.read_regular(row['path'], 96 << 20)
        pair.require(pair.digest(raw) == row['sha256'] and raw[:6] == b'\x7fELF\x02\x01' and len(raw) >= 64 and int.from_bytes(raw[18:20], 'little') == 183, 'exact Linux arm64 binary required')
    pair.require(set(value['rootfs']) == set(ROOTFS), 'two offline bases required')
    for kind, row in value['rootfs'].items():
        pair.require(set(row) == {'image', 'path', 'sha256', 'bytes'} and row['image'] == ROOTFS[kind] and pair.digest_shape(row['sha256']), 'pinned rootfs metadata required')
        path = shared.regular(row['path'], 2 << 30)
        pair.require(path.stat().st_size == row['bytes'] and shared.digest_file(path) == row['sha256'], 'rootfs bytes differ')
    m = value['model']
    pair.require(isinstance(m, dict) and set(m) == {'model_id', 'model_version', 'model_sha256'} and pair.digest_shape(m['model_sha256']) and all(isinstance(m[k], str) and 0 < len(m[k]) <= 256 for k in ('model_id', 'model_version')), 'exact pose model metadata required')
    return value


def dockerfile(service, run_id, anchor):
    prefix = 'FROM scratch\n'
    if service in ('scene', 'people'):
        prefix += 'ADD rootfs.tar /\n'
    prefix += 'COPY app-runner /app-runner\n'
    if service == 'scene':
        prefix += 'COPY source/ /world/\nWORKDIR /world\nENV PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin PYTHONPATH=/world MUJOCO_GL=osmesa MESA_SHADER_CACHE_DISABLE=true LP_NUM_THREADS=2 OMP_NUM_THREADS=2\n'
        command = ['/usr/local/bin/python3', '-m', 'integration.people.scene', '--bind', '0.0.0.0', '--port', '8091', '--run-id', run_id, '--anchor-unix-ns', str(anchor), '--read-token-file', '/config/scene-read-token', '--control-token-file', '/config/scene-control-token']
    else:
        binary = 'camera' if service.startswith('camera-') else service
        prefix += 'COPY ' + binary + ' /service\n'
        if service == 'people':
            prefix += 'ENV PATH=/usr/local/bin:/usr/local/sbin:/usr/sbin:/usr/bin:/sbin:/bin OMP_NUM_THREADS=2 OPENBLAS_NUM_THREADS=1\n'
        command = ['/service', '--config', '/config/config.json']
        if service == 'people':
            command += ['--pose-model', '/model/movenet-multipose-lightning.onnx']
    wrapper = ['/app-runner', '--directory', '/proofs', '--duration', '600s', '--stdout-limit', str(32 << 20), '--stderr-limit', str(1 << 20)]
    if service != 'scene':
        wrapper += ['--start-file', '/config/go']
    return (prefix + 'USER 0:0\nENTRYPOINT ' + pair.json_bytes(wrapper + ['--'] + command).decode()).encode()


def copied(source, target, digest=None, mode=0o600):
    source = shared.regular(source, 2 << 30)
    expected = digest or shared.digest_file(source)
    target.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    with source.open('rb') as stream, target.open('xb') as out:
        shutil.copyfileobj(stream, out, 1 << 20)
    os.chmod(target, mode)
    pair.require(shared.digest_file(target) == expected and shared.digest_file(source) == expected, 'source changed during staging')


def inventory(directory):
    files = {}
    for path in sorted(directory.rglob('*')):
        if path.is_file():
            files[str(path.relative_to(directory))] = {'sha256': shared.digest_file(path), 'size': path.stat().st_size, 'mode': stat.S_IMODE(path.stat().st_mode)}
    shared.write_new(directory / 'files.json', pair.json_bytes({'version': 1, 'files': files}))


def prepare(raw, output):
    plan = inputs(raw)
    output = shared.safe_output(Path(output), ROOT / '.local')
    shared.write_new(output / 'inputs.json', pair.json_bytes(plan))
    for node, services in SERVICES.items():
        directory = output / node
        directory.mkdir(mode=0o700)
        for name, row in plan['binaries'].items():
            copied(row['path'], directory / 'tools' / name, row['sha256'], 0o755)
        for service in services:
            context = directory / 'project' / 'services' / service
            shared.write_new(context / 'Dockerfile', dockerfile(service, plan['run_id'], plan['anchor_unix_ns']), 0o644)
            copied(plan['binaries']['app-runner']['path'], context / 'app-runner', plan['binaries']['app-runner']['sha256'], 0o755)
            if service in ('scene', 'people'):
                row = plan['rootfs']['scene' if service == 'scene' else 'pose']
                copied(row['path'], context / 'rootfs.tar', row['sha256'])
            if service == 'scene':
                sources = [*sorted((ROOT / 'sim').glob('*.py')), ROOT / 'integration/people/scene.py', ROOT / 'examples/collaborative-office/office.xml']
                for source in sources:
                    copied(source, context / 'source' / source.relative_to(ROOT), mode=0o644)
            else:
                name = 'camera' if service.startswith('camera-') else service
                copied(plan['binaries'][name]['path'], context / name, plan['binaries'][name]['sha256'], 0o755)
        inventory(directory)
    shared.write_new(output / 'stage.json', pair.json_bytes({'version': 1, 'inputs_sha256': pair.digest((output / 'inputs.json').read_bytes()), 'nodes': {node: pair.digest((output / node / 'files.json').read_bytes()) for node in SERVICES}}))
    return validate(output)


def validate(directory):
    directory = Path(directory)
    raw = pair.read_regular(str(directory / 'inputs.json'), 256 << 10)
    plan = pair.strict_json(raw)
    receipt = pair.strict_json(pair.read_regular(str(directory / 'stage.json'), 8192))
    pair.require(set(receipt) == {'version', 'inputs_sha256', 'nodes'} and receipt['version'] == 1 and receipt['inputs_sha256'] == pair.digest(raw) and set(receipt['nodes']) == set(SERVICES), 'stage control differs')
    pair.require(set(p.name for p in directory.iterdir()) == {'a', 'b', 'inputs.json', 'stage.json'}, 'extra stage entry')
    for node in SERVICES:
        index, files, _ = libraries.runtime.upload_inventory(directory / node)
        pair.require(pair.digest(index) == receipt['nodes'][node], 'staged inventory differs')
        for name, row in files.items():
            p = directory / node / name
            pair.require(p.stat().st_size == row['size'] and stat.S_IMODE(p.stat().st_mode) == row['mode'] and shared.digest_file(p) == row['sha256'], 'staged bytes changed')
    return plan


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('action', choices=('prepare', 'validate'))
    parser.add_argument('--inputs', type=Path)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if args.action == 'prepare':
        pair.require(args.inputs is not None, '--inputs required')
        prepare(pair.read_regular(str(args.inputs.resolve()), 256 << 10), args.output.resolve())
    else:
        validate(args.output.resolve())
    print('people stage verified')

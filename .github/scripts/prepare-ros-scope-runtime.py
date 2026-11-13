#!/usr/bin/env python3
"""Prepare pinned ROS image and controls without privileged host operations."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import uuid

ROOT = Path(__file__).resolve().parents[2]
BASE = 'ros:humble-ros-base@sha256:d9f14e84747bea639a65e00cf871d2c68ae55385d2acf6239614de0cadb41dcc'
SPEC = importlib.util.spec_from_file_location('owned_scope', Path(__file__).with_name('run-owned-media-test.py'))
OWNED = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNED)


def output(args):
    return subprocess.check_output(args, text=True, timeout=15).strip()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    destination = args.output.resolve()
    destination.mkdir(parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex
    image = 'docker.io/library/wendy-ros-scope:'+nonce
    container = 'wendy-ros-scope-'+nonce
    label = 'dev.wendy.regression.ros-scope='+nonce
    evidence = {'status': 'failed', 'base': BASE, 'image': image, 'cleanup_errors': []}
    evidence['source_sha256'] = {name: hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in (
        'go/internal/agent/containerd/ros2_scope_runtime_test.go',
        '.github/scripts/test-ros-scope-runtime.sh',
        '.github/scripts/fixtures/ros-scope-transport.py',
        '.github/scripts/fixtures/ros-scope-smoke.py')}
    def interrupted(signum, frame):
        raise InterruptedError(f'interrupted by signal {signum}')
    previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
    def run(name, command, timeout, env=None):
        with (destination/(name+'.log')).open('w') as stream:
            result = OWNED.run(command, timeout, cwd=ROOT, env=env, stdout=stream)
        if result:
            raise RuntimeError(f'{name} failed with exit {result}')
    try:
        builder = output(['docker', 'context', 'show'])
        if not re.search(r'^Driver:\s+docker\s*$', output(['docker', 'buildx', 'inspect', builder]), re.M):
            raise RuntimeError('requires the current context existing docker builder')
        arch = output(['docker', 'info', '--format', '{{.Architecture}}'])
        arch = {'aarch64': 'arm64', 'arm64': 'arm64', 'x86_64': 'amd64', 'amd64': 'amd64'}[arch]
        evidence['architecture'] = arch
        dockerfile = destination/'Dockerfile'
        dockerfile.write_text('FROM '+BASE+'\n'
            'RUN apt-get update && apt-get install -y --no-install-recommends ros-humble-rmw-cyclonedds-cpp && rm -rf /var/lib/apt/lists/*\n'
            'ENV PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1\n'
            'COPY ros-scope-transport.py /scope.py\nCOPY ros-scope-smoke.py /smoke.py\n')
        run('image-build', ['docker', 'buildx', 'build', '--builder', builder, '--load', '--label', label,
            '--tag', image, '--file', str(dockerfile), str(ROOT/'.github/scripts/fixtures')], 480)
        run('image-smoke', ['docker', 'run', '--name', container, '--label', label,
            '--network', 'none', '--cap-drop=ALL', '--security-opt=no-new-privileges', '--read-only',
            '--tmpfs', '/tmp:rw,nosuid,nodev,size=64m', '--user', f'{os.getuid()}:{os.getgid()}',
            '-e', 'HOME=/tmp', '-e', 'ROS_LOCALHOST_ONLY=1', '-e', 'ROS_DOMAIN_ID=61',
            '-e', 'RMW_IMPLEMENTATION=rmw_cyclonedds_cpp', image, '/bin/bash', '-c',
            'source /opt/ros/humble/setup.bash && exec python3 /smoke.py'], 20)
        run('image-save', ['docker', 'image', 'save', '--output', str(destination/'ros-image.tar'), image], 120)
        digest = hashlib.sha256()
        with (destination/'ros-image.tar').open('rb') as archive:
            for block in iter(lambda: archive.read(1 << 20), b''):
                digest.update(block)
        evidence['archive_sha256'] = digest.hexdigest()
        env = dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0')
        client = ROOT/'go/internal/agent/containerd/client.go'
        source = client.read_text()
        beginning = source.index('func cycloneDDSBeyondDeviceConfig(')
        ending = source.index('\n}\n', beginning)+2
        replacement = source[:beginning]+'func cycloneDDSBeyondDeviceConfig(interfaces []string) string { return cycloneDDSInlineConfig }'+source[ending:]
        routes = ROOT/'go/internal/rtps/peripherals.go'
        route_source = routes.read_text()
        before = 'if err != nil {\n\t\treturn nil, nil\n\t}'
        if route_source.count(before) != 1:
            raise RuntimeError('route failure control source changed')
        for name, original, content in [('old-loopback', client, replacement),
                ('fail-open-routes', routes, route_source.replace(before, 'if err != nil {\n\t\treturn wired, nil\n\t}'))]:
            mutant = destination/(name+'.go')
            mutant.write_text(content)
            overlay = destination/(name+'.json')
            overlay.write_text(json.dumps({'Replace': {str(original): str(mutant)}}))
            run(name+'-compile', ['go', 'test', '-overlay', str(overlay), '-c', '-o',
                str(destination/(name+'.test')), './go/internal/agent/containerd'], 240, env)
        evidence['status'] = 'prepared'
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        for kind, name in [('container', container), ('image', image)]:
            try:
                inspect = subprocess.run(['docker', kind, 'inspect', name], capture_output=True, text=True, timeout=15)
                if inspect.returncode:
                    if 'No such' in inspect.stderr:
                        continue
                    raise RuntimeError(inspect.stderr.strip())
                obj = json.loads(inspect.stdout)[0]
                if obj.get('Config', {}).get('Labels', {}).get('dev.wendy.regression.ros-scope') != nonce:
                    raise RuntimeError('refusing cleanup after ownership mismatch')
                subprocess.run(['docker', kind, 'rm']+(['--force'] if kind == 'container' else [])+[name],
                               capture_output=True, check=True, timeout=20)
            except Exception as error:
                evidence['cleanup_errors'].append(str(error))
                evidence['status'] = 'failed'
        (destination/'manifest.json').write_text(json.dumps(evidence, indent=2)+'\n')
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    if evidence['status'] != 'prepared':
        raise RuntimeError('preparation cleanup failed')
    print('Prepared ROS scope image and controls:', destination)


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Real ROS/Agent/Pion deadline checks in one ordinary private container.

Uses the current Docker context's existing docker driver, never creates a
builder, loads devices, changes the default builder, or prunes shared caches.
"""
import argparse
import importlib.util
import hashlib
import json
import os
from pathlib import Path
import re
import signal
import subprocess
import sys
import time
import uuid

BASE = 'ros:humble-ros-base@sha256:d9f14e84747bea639a65e00cf871d2c68ae55385d2acf6239614de0cadb41dcc'
ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('owned_go2', Path(__file__).with_name('run-owned-media-test.py'))
OWNED = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNED)


def output(command):
    return subprocess.check_output(command, text=True, timeout=15).strip()


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    destination = args.output.resolve()
    destination.mkdir(parents=True, exist_ok=False)
    nonce = uuid.uuid4().hex
    image = 'wendy-go2-deadline:'+nonce
    network = 'wendy-go2-'+nonce
    label = 'dev.wendy.regression.go2='+nonce
    containers = []
    network_created = False
    image_created = False
    evidence = {'status': 'failed', 'base': BASE, 'cases': [], 'cleanup_errors': [], 'durations_seconds': {}}
    evidence['source_sha256'] = {name: hashlib.sha256((ROOT/name).read_bytes()).hexdigest() for name in (
        '.github/scripts/test-go2-bridge-deadlines.py',
        'go/internal/agent/services/unitree_ros_runtime_test.go',
        'go/ros2/go2-bridge/tests/command_deadlines_test.py',
        'go/ros2/go2-bridge/go2_bridge/node.py')}

    def interrupted(signum, frame):
        raise InterruptedError(f'interrupted by signal {signum}')

    previous = {s: signal.signal(s, interrupted) for s in (signal.SIGINT, signal.SIGTERM)}

    def run(name, command, timeout, env=None, expected=0):
        started = time.monotonic()
        with (destination/(name+'.log')).open('w') as log:
            result = OWNED.run(command, timeout, cwd=ROOT, env=env, stdout=log)
        evidence['durations_seconds'][name] = time.monotonic()-started
        if result != expected:
            raise RuntimeError(f'{name} exited {result}; see {destination/name}.log')
        return result

    try:
        builder = output(['docker', 'context', 'show'])
        description = output(['docker', 'buildx', 'inspect', builder])
        if not re.search(r'^Driver:\s+docker\s*$', description, re.M):
            raise RuntimeError('requires the current context existing docker driver')
        arch = output(['docker', 'info', '--format', '{{.Architecture}}'])
        arch = {'aarch64': 'arm64', 'x86_64': 'amd64', 'arm64': 'arm64', 'amd64': 'amd64'}[arch]
        evidence['architecture'] = arch
        source = ROOT/'go/ros2/go2-bridge'
        dockerfile = (source/'Dockerfile').read_text()
        if not dockerfile.startswith('FROM ros:humble-ros-base\n'):
            raise RuntimeError('review bridge image base before updating the fixture')
        pinned = destination/'Dockerfile'
        pinned.write_text(dockerfile.replace('FROM ros:humble-ros-base', 'FROM '+BASE, 1))
        # The tag is fixture-owned even if a build gets interrupted after load.
        image_created = True
        run('image-build', ['docker', 'buildx', 'build', '--builder', builder, '--load',
            '--label', label, '--tag', image, '--file', str(pinned), str(source)], 600)
        evidence['image_id'] = output(['docker', 'image', 'inspect', '--format', '{{.Id}}', image])
        env = dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0')
        binary = destination/'services.test'
        run('compile', ['go', 'test', '-c', '-o', str(binary), './go/internal/agent/services'], 240, env)
        production = ROOT/'go/internal/agent/services/unitree_robot_service.go'
        original = production.read_text()
        before = 's.armLease(req.GetSerial(), gen, link)'
        if original.count(before) != 1:
            raise RuntimeError('move lease control source changed')
        mutant = destination/'no-lease.go'
        mutant.write_text(original.replace(before, '_ = gen // regression: no move expiry'))
        overlay = destination/'no-lease.json'
        overlay.write_text(json.dumps({'Replace': {str(production): str(mutant)}}))
        mutant_binary = destination/'no-lease.test'
        run('compile-control', ['go', 'test', '-overlay', str(overlay), '-c', '-o', str(mutant_binary),
                               './go/internal/agent/services'], 240, env)
        node = (source/'go2_bridge/node.py').read_text()
        before = 'if mapped[0] in POSTURE_COMMANDS:'
        if node.count(before) != 1:
            raise RuntimeError('posture callback control source changed')
        synchronous = destination/'synchronous-node.py'
        synchronous.write_text(node.replace(before, 'if False:  # regression: block the ROS executor'))
        network_created = True
        output(['docker', 'network', 'create', '--internal', '--label', label, network])
        for name, executable, patch in [('normal', binary, None),
                ('synchronous-posture', binary, synchronous), ('disabled-move-lease', mutant_binary, None)]:
            container = 'wendy-go2-'+nonce+'-'+name
            containers.append(container)
            command = ['docker', 'create', '--name', container, '--label', label, '--network', network,
                '--cap-drop=ALL', '--security-opt=no-new-privileges', '--read-only',
                '--tmpfs', '/tmp:rw,nosuid,nodev,size=128m', '--user', f'{os.getuid()}:{os.getgid()}',
                '-e', 'HOME=/tmp', '-e', 'ROS_LOCALHOST_ONLY=1', '-e', 'ROS_DOMAIN_ID=37',
                '-e', 'RMW_IMPLEMENTATION=rmw_cyclonedds_cpp', '-e', 'PYTHONPATH=/opt/go2-bridge',
                '-e', 'WENDY_GO2_BRIDGE_ROS=1', '-e', 'WENDY_GO2_BRIDGE_TEST=/checks/command_deadlines_test.py',
                '-e', 'GO2_DEADLINE_EVIDENCE=/evidence/'+name+'.json',
                '-v', str(executable)+':/services.test:ro', '-v', str(source/'tests')+':/checks:ro',
                '-v', str(destination)+':/evidence:rw']
            if patch:
                command += ['-v', str(patch)+':/opt/go2-bridge/go2_bridge/node.py:ro']
            command += [image, '/bin/bash', '-c',
                'source /opt/go2-bridge/ros_ws/install/setup.bash && exec /services.test -test.run ^TestGo2BridgeROSDeadlines$ -test.v -test.timeout=90s']
            output(command)
            log = destination/(name+'.log')
            started = time.monotonic()
            with log.open('w') as stream:
                result = OWNED.run(['docker', 'start', '--attach', container], 100, stdout=stream)
            evidence['durations_seconds'][name] = time.monotonic()-started
            text = log.read_text()
            result_evidence = destination/(name+'.json')
            if name == 'normal':
                if result != 0 or '--- PASS: TestGo2BridgeROSDeadlines' not in text or '--- SKIP:' in text:
                    raise RuntimeError('normal ROS deadline journey failed')
                if json.loads(result_evidence.read_text())['status'] != 'passed':
                    raise RuntimeError('normal journey lacks passing evidence')
            else:
                reason = {'synchronous-posture': 'delayed posture acknowledgement blocked concurrent ROS callbacks',
                          'disabled-move-lease': 'move lease did not stop on the actual data channel'}[name]
                if result == 0 or '--- FAIL: TestGo2BridgeROSDeadlines' not in text or reason not in text:
                    raise RuntimeError(name+' did not fail its intended regression assertion')
            evidence['cases'].append({'name': name, 'exit_code': result, 'expected_failure': name != 'normal'})
        patrol_spec = importlib.util.spec_from_file_location('patrol_ros', Path(__file__).with_name('test-patrol-ros-runtime.py'))
        patrol = importlib.util.module_from_spec(patrol_spec)
        patrol_spec.loader.exec_module(patrol)
        patrol.run_cases(ROOT, destination, image, network, nonce, containers, run, output)
        evidence['patrol'] = json.loads((destination/'patrol/summary.json').read_text())
        evidence['status'] = 'passed'
    finally:
        for sig in previous:
            signal.signal(sig, signal.SIG_IGN)
        # Each name includes a fresh nonce. Verify its label before removing
        # resources; never stop an existing user container or shared network.
        for kind, name in [('container', c) for c in containers] + ([('network', network)] if network_created else []) + ([('image', image)] if image_created else []):
            try:
                inspect = subprocess.run(['docker', kind, 'inspect', name], capture_output=True, text=True, timeout=15)
                if inspect.returncode:
                    if 'No such' in inspect.stderr:
                        continue
                    raise RuntimeError('cannot inspect owned '+kind+': '+inspect.stderr.strip())
                item = json.loads(inspect.stdout)[0]
                labels = item.get('Config', {}).get('Labels', {}) if kind != 'network' else item.get('Labels', {})
                if labels.get('dev.wendy.regression.go2') != nonce:
                    raise RuntimeError('ownership label mismatch for '+name)
                command = ['docker', kind, 'rm'] + (['--force'] if kind == 'container' else []) + [name]
                subprocess.run(command, check=True, capture_output=True, timeout=20)
            except Exception as error:
                evidence['cleanup_errors'].append(str(error))
                evidence['status'] = 'failed'
        for binary in (destination/'services.test', destination/'no-lease.test'):
            try:
                binary.unlink(missing_ok=True)
            except OSError as error:
                evidence['cleanup_errors'].append(str(error))
                evidence['status'] = 'failed'
        (destination/'summary.json').write_text(json.dumps(evidence, indent=2)+'\n')
        for sig, handler in previous.items():
            signal.signal(sig, handler)
    if evidence['status'] != 'passed':
        raise RuntimeError('fixture cleanup failed')
    print('Go2 deadline evidence:', destination)


if __name__ == '__main__':
    main()

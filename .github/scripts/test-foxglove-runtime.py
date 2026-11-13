#!/usr/bin/env python3
"""Hosted real Foxglove generation/build/deploy/WebSocket/DDS acceptance."""
import argparse
import hashlib
import importlib.util
import json
import os
import re
from pathlib import Path
import shutil
import signal
import stat
import subprocess
import sys
import tempfile
import time
import uuid

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('foxglove_owned', Path(__file__).with_name('run-owned-media-test.py'))
OWNED = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNED)


def checked(command, *, env=None, timeout=20):
    result = subprocess.run(command, cwd=ROOT, env=env, stdin=subprocess.DEVNULL,
                            stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=timeout, check=True)
    if len(result.stdout) > 2**20 or len(result.stderr) > 2**20:
        raise ValueError('oversized fixture command observation')
    return result.stdout.decode().strip()


def validate_case(name, code, text, directory):
    """Require runtime proof before accepting a normal or historical-control exit."""
    if name not in ('normal','named-service','named-forward'):
        raise ValueError('unknown Foxglove case')
    for filename in ('image-digest.txt','installed-packages.txt','Dockerfile','wendy.json'):
        path=directory/filename
        if path.is_symlink() or not path.is_file():
            raise ValueError('missing regular build/runtime evidence: '+filename)
    if not re.fullmatch(r'sha256:[0-9a-f]{64}\n?',(directory/'image-digest.txt').read_text()):
        raise ValueError('missing actual deployed image digest')
    packages=(directory/'installed-packages.txt').read_text()
    for package in ('ros-humble-foxglove-bridge','ros-humble-rmw-cyclonedds-cpp','socat'):
        if not re.search(r'^'+re.escape(package)+r' [^\s]+$',packages,re.M):
            raise ValueError('missing installed package provenance: '+package)
    expected = 'forwarding cancellation changed deployed bridge or left legacy services' if name == 'named-service' else 'Foxglove real deployment/readiness failed'
    if name == 'normal':
        if code or '--- PASS: TestFoxgloveGeneratedAppDeploysAndCarriesROS ' not in text or 'FOXGLOVE_RUNTIME_VERIFIED ' not in text or '--- SKIP:' in text:
            raise RuntimeError('normal actual Foxglove deployment/WebSocket/DDS gate failed')
    else:
        if code != 1 or '--- FAIL: TestFoxgloveGeneratedAppDeploysAndCarriesROS ' not in text or expected not in text or 'FOXGLOVE_DEPLOYED_IMAGE ' not in text or not (directory/'installed-packages.txt').is_file():
            raise RuntimeError(f'{name} did not fail at its actual regression boundary')
        if name == 'named-service' and 'FOXGLOVE_ROS_BYTES_VERIFIED ' not in text:
            raise RuntimeError('descriptor control did not carry real ROS bytes before its layout assertion')
        if name == 'named-forward' and ('waiting for foxglove_bridge WebSocket' not in text or 'sh.wendy.foxglovebridge_foxglove' not in text or 'not found' not in text):
            raise RuntimeError('forwarding control did not prove the missing actual named container')

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    if sys.platform != 'linux' or os.geteuid() == 0 or os.environ.get('GITHUB_ACTIONS') != 'true' or os.environ.get('RUNNER_ENVIRONMENT') != 'github-hosted':
        parser.error('run as the ordinary user on the disposable GitHub-hosted Linux runner')
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False, mode=0o700)
    nonce = uuid.uuid4().hex
    base_builder = 'wendy-foxglove-' + nonce
    builder = base_builder + '-oci'
    container = 'buildx_buildkit_' + builder + '0'
    docker_config = output/'docker-config'
    docker_config.mkdir(mode=0o700)
    env = {**os.environ, 'CGO_ENABLED': '1', 'DOCKER_CONFIG': str(docker_config),
           'WENDY_BUILDX_BUILDER': base_builder, 'BUILDX_CONFIG': str(docker_config/'buildx')}
    for key in ('DOCKER_CONTEXT', 'DOCKER_TLS_VERIFY', 'DOCKER_CERT_PATH'):
        env.pop(key, None)
    start = time.monotonic()
    deadline = start + 20*60
    evidence = {'status': 'failed', 'audit_entries': ['2306'], 'physical_execution': 'not_run',
                'audit_correction': 'Production generates a Dockerfile from ros:humble and installs the published ROS bridge package; there is no prebuilt published bridge image in this path.',
                'nonce': nonce, 'builder': builder, 'cases': [], 'cleanup_errors': [], 'sources': {}}
    work = None
    unknown_drain = False
    builder_may_exist = False
    previous = {}
    def interrupted(signum, frame):
        raise InterruptedError(f'interrupted by signal {signum}')
    def remaining(cap):
        available = deadline-time.monotonic()
        if available <= 0:
            raise TimeoutError('Foxglove overall 20-minute deadline exhausted')
        return min(cap, available)
    def run(name, command, cap, command_env=env):
        started = time.monotonic()
        with (output/(name+'.log')).open('w') as log:
            result = OWNED.run(command, remaining(cap), cwd=ROOT, env=command_env, stdout=log)
        evidence.setdefault('durations_seconds', {})[name] = round(time.monotonic()-started, 3)
        return result
    try:
        previous = {sig: signal.signal(sig, interrupted) for sig in (signal.SIGINT, signal.SIGTERM)}
        if shutil.disk_usage(output).free < 8*1024**3:
            raise ValueError('require at least 8 GiB free before ROS build/export')
        if os.environ.get('DOCKER_CONTEXT') and os.environ.get('DOCKER_HOST'):
            raise ValueError('ambiguous Docker context/host selectors')
        socket = os.environ.get('DOCKER_HOST') or checked(['docker', 'context', 'inspect', '--format', '{{.Endpoints.docker.Host}}'])
        if not socket.startswith('unix://'):
            raise ValueError('require an explicitly local Unix Docker daemon')
        socket_path = Path(socket[7:]).resolve(strict=True)
        if not str(socket_path).startswith('/run/') or not stat.S_ISSOCK(socket_path.stat().st_mode):
            raise ValueError('require a local Docker socket beneath /run for private rebinding')
        env['DOCKER_HOST'] = 'unix://' + str(socket_path)
        checked(['docker', 'version', '--format', '{{.Server.Version}}'], env=env)
        # Do not reuse a name/config entry even in the practically impossible
        # event of a nonce collision. The daemon/default builder is unchanged.
        existing = checked(['docker', 'ps', '-a', '--format', '{{.Names}}'], env=env).splitlines()
        if container in existing:
            raise ValueError('fresh fixture builder container already exists')
        for name in ('go/internal/cli/commands/foxglove.go', 'go/internal/cli/commands/foxglove_forward.go',
                     'go/internal/cli/commands/foxglove_runtime_test.go', '.github/scripts/foxglove-runtime-namespace.sh',
                     '.github/scripts/test-foxglove-runtime.py', 'go.mod', 'go.sum'):
            evidence['sources'][name] = hashlib.sha256((ROOT/name).read_bytes()).hexdigest()
        evidence['source_commit'] = checked(['git', 'rev-parse', 'HEAD'])
        source = ROOT/'go/internal/cli/commands/foxglove.go'
        original = source.read_text()
        before = '    { "type": "network", "mode": "host" }\n  ]\n}\n`, opts.domain'
        after = '    { "type": "network", "mode": "host" }\n  ],\n  "services": { "foxglove": { "context": "." } }\n}\n`, opts.domain'
        if original.count(before) != 1:
            raise ValueError('historical descriptor mutation no longer identifies exactly one site')
        old_descriptor = output/'named-service.go'
        old_descriptor.write_text(original.replace(before, after))
        forwarding = ROOT/'go/internal/cli/commands/foxglove_forward.go'
        old_forwarding = output/'named-forward.go'
        text = forwarding.read_text()
        before = 'return openContainerConnection(ctx, client, foxgloveAppID,'
        if text.count(before) != 1:
            raise ValueError('historical forwarding mutation no longer identifies exactly one site')
        old_forwarding.write_text(text.replace(before, 'return openContainerConnection(ctx, client, multiServiceContainerName(foxgloveAppID, "foxglove"),'))
        variants = {'normal': None, 'named-service': (source, old_descriptor), 'named-forward': (forwarding, old_forwarding)}
        for name, replacement in variants.items():
            command = ['go', 'test', '-tags', 'regression_foxglove', '-c', '-o', str(output/(name+'.test'))]
            if replacement:
                overlay = output/(name+'.json')
                overlay.write_text(json.dumps({'Replace': {str(replacement[0]): str(replacement[1])}}))
                command += ['-overlay', str(overlay)]
            command += ['./go/internal/cli/commands']
            code = run('compile-'+name, command, 240)
            evidence.setdefault('binary_sha256', {})[name] = hashlib.sha256((output/(name+'.test')).read_bytes()).hexdigest() if code == 0 else None
            if code:
                raise RuntimeError(f'{name} compile failed ({code})')
        for name in variants:
            for path, digest in evidence['sources'].items():
                if hashlib.sha256((ROOT/path).read_bytes()).hexdigest() != digest:
                    raise ValueError('fixture source changed after compile: '+path)
            case = output/name
            case.mkdir(mode=0o700)
            work = Path(tempfile.mkdtemp(prefix='wendy-fg-'))
            (work/'owner.json').write_text(json.dumps({'nonce': nonce, 'case': name}))
            case_env = {**env, **{f'WENDY_FOXGLOVE_PARENT_{kind}': os.readlink('/proc/self/ns/'+kind) for kind in ('mnt', 'net', 'pid')}}
            command = ['sudo', '-n', 'env', *[key+'='+case_env[key] for key in ('PATH', 'DOCKER_CONFIG', 'BUILDX_CONFIG', 'WENDY_BUILDX_BUILDER', 'GITHUB_ACTIONS', 'RUNNER_ENVIRONMENT', 'WENDY_FOXGLOVE_PARENT_mnt', 'WENDY_FOXGLOVE_PARENT_net', 'WENDY_FOXGLOVE_PARENT_pid')],
                       'unshare', '--mount', '--net', '--pid', '--fork', '--kill-child=KILL', '--propagation', 'private', '--mount-proc',
                       'bash', str(ROOT/'.github/scripts/foxglove-runtime-namespace.sh'), str(output/(name+'.test')), str(work), str(case), str(socket_path)]
            builder_may_exist = True
            unknown_drain = True
            code = run(name, command, 720 if name == 'normal' else 180)
            if code >= 124:
                raise RuntimeError(f'{name} namespace drain unknown ({code}); retaining work and builder')
            unknown_drain = False
            # A waited PID-namespace init has terminated its descendants. Never
            # delete mounted paths or a directory whose ownership nonce changed.
            if work.is_symlink() or json.loads((work/'owner.json').read_text()) != {'nonce': nonce, 'case': name}:
                raise ValueError('owned work identity changed')
            mounts = checked(['findmnt', '-rn', '-o', 'TARGET']).splitlines()
            if any(path == str(work) or path.startswith(str(work)+'/') for path in mounts):
                raise RuntimeError('owned work remains mounted; retaining it')
            checked(['sudo', '-n', 'rm', '-rf', '--', str(work)], timeout=30)
            work = None
            text = (output/(name+'.log')).read_text()
            validate_case(name, code, text, case)
            evidence['cases'].append({'case': name, 'exit_code': code, 'status': 'passed', 'negative_control': name != 'normal'})
        evidence['status'] = 'passed'
    except BaseException as error:
        evidence['error'] = str(error)
    finally:
        try:
            if work is not None:
                evidence['retained_work'] = str(work)
            if builder_may_exist and not unknown_drain:
                try:
                    # Require this private registration plus the exact unique
                    # container name before deleting only the fixture builder.
                    registration = docker_config/'buildx/instances'/builder
                    sudo_docker = ['sudo', '-n', 'env', *[key+'='+env[key] for key in ('DOCKER_CONFIG', 'BUILDX_CONFIG', 'DOCKER_HOST')], 'docker']
                    checked(['sudo', '-n', 'test', '-f', str(registration)])
                    inspected = json.loads(checked([*sudo_docker, 'inspect', container]))
                    if len(inspected) != 1 or inspected[0]['Name'] != '/'+container:
                        raise ValueError('owned builder identity is incomplete; refusing removal')
                    evidence['builder_container_id'] = inspected[0]['Id']
                    checked([*sudo_docker, 'buildx', 'rm', '--force', builder], timeout=45)
                    checked(['sudo', '-n', 'rm', '-rf', '--', str(docker_config)], timeout=20)
                except BaseException as error:
                    evidence['cleanup_errors'].append(str(error))
            elif builder_may_exist:
                evidence['cleanup_errors'].append('unknown namespace drain; retained owned builder')
            if evidence['cleanup_errors'] or work is not None:
                evidence['status'] = 'failed'
            for binary in output.glob('*.test'):
                if not unknown_drain:
                    binary.unlink()
            evidence['elapsed_seconds'] = round(time.monotonic()-start, 3)
            (output/'evidence.json').write_text(json.dumps(evidence, indent=2)+'\n')
        finally:
            for sig, handler in previous.items():
                signal.signal(sig, handler)
    print(json.dumps(evidence, indent=2))
    return 0 if evidence['status'] == 'passed' else 1


if __name__ == '__main__':
    sys.exit(main())

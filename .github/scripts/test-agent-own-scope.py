#!/usr/bin/env python3
"""Audit1890 actual Agent startup, using the existing setup job's disposable image."""
import argparse
import hashlib
import importlib.util
import json
import os
import signal
from pathlib import Path
import sys
import uuid

ROOT = Path(__file__).resolve().parents[2]
SPEC = importlib.util.spec_from_file_location('scope_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(OWNER)
CASES = ('legacy-strict', 'spiffe-strict', 'unknown-off', 'unknown-strict', 'unknown-grace')


def execute(args, cancellation):
    if sys.platform != 'linux':
        raise RuntimeError('requires the existing Linux Agent startup job; no host Agent execution')
    agent, output = args.agent.resolve(), args.output.resolve()
    if not agent.is_file() or any(c in str(agent) + str(output) + str(ROOT) for c in ',\r\n'):
        raise RuntimeError('existing Agent and Docker-mount-safe paths required')
    output.mkdir(parents=True, exist_ok=False)
    records = []
    sequence = 0
    cleaning = False

    def run(argv, seconds=60, name='command', env=None):
        nonlocal sequence
        if cancellation and not cleaning:
            raise InterruptedError('startup qualification interrupted')
        sequence += 1
        path = output / f'{sequence:02d}-{name}.log'
        with path.open('w') as log:
            code = OWNER.run(argv, seconds, cwd=ROOT, env=env, stdout=log)
        if code in (130, 143):
            cancellation.append(code)
        text = path.read_text()
        records.append({'command': argv, 'exit_code': code, 'log': path.name})
        return code, text

    def require(argv, seconds=60, name='command', env=None):
        code, text = run(argv, seconds, name, env)
        if code:
            raise RuntimeError(f'{name} failed ({code}): {text[-3000:]}')
        return text

    source = ROOT / 'go/cmd/wendy-agent/main.go'
    original = source.read_text()
    boundary = '''\t\tif orgMode != interceptor.OrgModeOff && !haveScope {
\t\t\tlogger.Error("cannot determine device identity scope; refusing to start mTLS server",
\t\t\t\tzap.String("configuredMode", orgMode.String()))
\t\t\treturn
\t\t}'''
    if original.count(boundary) != 1:
        raise RuntimeError('actual Agent startup refusal boundary changed')
    mutation = output / 'permissive-startup.go'
    mutation.write_text(original.replace(boundary, '''\t\tif orgMode != interceptor.OrgModeOff && !haveScope {
\t\t\tlogger.Error("historical unknown-scope enforcement downgrade")
\t\t\teffectiveMode = interceptor.OrgModeOff
\t\t}'''))
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(mutation)}}))
    fixture, permissive = output / 'scope.test', output / 'permissive-agent'
    build_env = dict(os.environ, GOOS='linux', CGO_ENABLED='0')
    require(['go', 'test', '-c', '-o', str(fixture), './go/internal/testfixture/agentscope'], 240, 'build-fixture', build_env)
    require(['go', 'build', '-overlay', str(overlay), '-o', str(permissive), './go/cmd/wendy-agent'], 300, 'build-control', build_env)
    # The preceding existing startup step already uses this image. Resolve the
    # local immutable ID; do not introduce another image or pull tag during tests.
    image = require(['docker', 'image', 'inspect', '--format', '{{.Id}}', 'python:3.13-slim'], name='image').strip()
    if not image.startswith('sha256:') or len(image) != 71 or any(c not in '0123456789abcdef' for c in image[7:]):
        raise RuntimeError('existing startup image ID unavailable')
    errors = []
    results = []
    try:
        for variant, binary, cases in (('normal', agent, CASES), ('permissive', permissive, CASES[-2:])):
            for case in cases:
                if cancellation:
                    raise InterruptedError('startup qualification interrupted after owned cleanup')
                nonce = uuid.uuid4().hex
                name = 'wendy-own-scope-' + nonce
                admission = output / (nonce + '.admission')
                admission.write_text(nonce)
                owned = False
                try:
                    require(['docker', 'create', '--name', name, '--label', 'wendy.scope-fixture=' + nonce,
                             '--network', 'none', '--cap-drop', 'ALL', '--security-opt', 'no-new-privileges',
                             '--cpus', '2', '--memory', '2g', '--pids-limit', '128',
                             '--mount', f'type=bind,src={binary},dst=/scope-agent,readonly',
                             '--mount', f'type=bind,src={fixture},dst=/scope.test,readonly',
                             '--mount', f'type=bind,src={admission},dst=/scope-admission,readonly',
                             '-e', 'WENDY_AGENT_SCOPE_FIXTURE=' + nonce, '-e', 'WENDY_SCOPE_CASE=' + case,
                             '--entrypoint', '/scope.test', image,
                             '-test.v', '-test.run=^Test(AgentOwnScopeStartup|ScopeStartupPortAdmission)$', '-test.timeout=95s'], name='create')
                    owned = True
                    status, log = run(['docker', 'start', '--attach', name], 110, variant + '-' + case)
                    if variant == 'normal':
                        accepted = status == 0 and 'AGENT_OWN_SCOPE_VERIFIED case=' + case + ' ' in log and '--- PASS: TestAgentOwnScopeStartup ' in log
                    else:
                        accepted = status == 1 and 'UNKNOWN_SCOPE_STARTUP_EXPOSED actual mTLS Agent RPC succeeded' in log and '--- FAIL: TestAgentOwnScopeStartup ' in log
                    if not accepted or '--- SKIP:' in log:
                        raise RuntimeError(f'{variant}/{case} did not reach its required actual startup boundary: {log[-3000:]}')
                    results.append({'variant': variant, 'case': case, 'exit_code': status, 'expected': True})
                finally:
                    cleaning = True
                    # Even interrupted create may have made the container. Never
                    # delete by name alone: inspect its nonce ownership first.
                    code, raw = run(['docker', 'container', 'inspect', name], 20, 'inspect-cleanup')
                    if code == 0:
                        try:
                            inspected = json.loads(raw)
                        except (ValueError, TypeError):
                            errors.append('invalid container ownership response: ' + name)
                            inspected = []
                        if len(inspected) != 1 or inspected[0].get('Config', {}).get('Labels', {}).get('wendy.scope-fixture') != nonce:
                            errors.append('refused cleanup of unmatched container ' + name)
                        else:
                            code, raw = run(['docker', 'rm', '--force', name], 20, 'remove-cleanup')
                            if code:
                                errors.append('owned container removal failed: ' + name)
                    elif owned or 'No such container' not in raw:
                        errors.append('container ownership/drain unproven: ' + name)
                    cleaning = False
                    if errors:
                        raise RuntimeError('; '.join(errors))
    finally:
        paths = [source, ROOT / 'go/internal/testfixture/agentscope/startup_linux_test.go', ROOT / 'go/internal/testfixture/agentscope/ports_test.go', agent, fixture, permissive]
        (output / 'evidence.json').write_text(json.dumps({'audit': '1890', 'image_id': image, 'results': results,
            'commands': records, 'cleanup_errors': errors,
            'sha256': {str(p): hashlib.sha256(p.read_bytes()).hexdigest() for p in paths if p.is_file()},
            'scope': 'actual production Agent startup in isolated filesystem/network; private ECDSA test PKI; no deployed host or Cloud'}, indent=2) + '\n')
    if cancellation:
        raise InterruptedError('startup qualification interrupted, including during cleanup')
    print('AGENT_OWN_SCOPE_MATRIX_VERIFIED five normal startup cases and two historical downgrade controls')


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--agent', type=Path, required=True)
    parser.add_argument('--output', type=Path, required=True)
    args = parser.parse_args()
    cancellation = []
    previous = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        for sig in previous:
            signal.signal(sig, lambda received, _frame: cancellation.append(received))
        execute(args, cancellation)
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


if __name__ == '__main__':
    main()

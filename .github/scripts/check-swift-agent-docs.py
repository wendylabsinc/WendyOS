#!/usr/bin/env python3
"""Execute actual Swift configuration and verify documented macOS defaults.

No app, listener, TCC, Keychain or device starts. Real listener behavior remains
in WendyAgentStartupTests; this gate checks configuration/environment claims.
"""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import subprocess
import sys
import tempfile

ROOT = Path(__file__).resolve().parents[2]
DOC = ROOT / 'go/internal/cli/assets/docs/development/debugging.md'
SOURCE_DIR = ROOT / 'swift' / 'WendyAgentCore' / 'Sources' / 'WendyAgent'
SOURCES = [SOURCE_DIR / name for name in ('WendyAgentEnvironment.swift', 'WendyAgentConfiguration.swift')]
SPEC = importlib.util.spec_from_file_location('owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(OWNER)
# Actual documentation at c69fe2880, before the separate correction.
ORIGINAL_HOST_ROW = '| `WENDY_AGENT_HOST` | `::` | Host address for the plaintext gRPC server (Swift macOS agent only; accepted values: `127.0.0.1`, `::1`, `localhost`) |'
PROBE = '''import Foundation
let c = WendyAgentConfiguration.default
let value: [String: Any] = ["host": c.host, "port": c.port, "otel_port": c.otelPort,
                          "allows_network_access": c.allowsNetworkAccess]
let data = try JSONSerialization.data(withJSONObject: value, options: [.sortedKeys])
print(String(decoding: data, as: UTF8.self))
'''


def require(ok, message):
    if not ok:
        raise RuntimeError(message)


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def rows(text):
    result = {}
    for name in ('WENDY_AGENT_HOST', 'WENDY_AGENT_PORT', 'WENDY_OTEL_PORT'):
        found = [line for line in text.splitlines() if line.startswith('| `' + name + '` |')]
        require(len(found) == 1, 'DOC_AGENT_TABLE: missing or ambiguous ' + name)
        cells = found[0].split('|')
        require(len(cells) == 5 and re.fullmatch(r'\s*`[^`]+`\s*', cells[2]), 'DOC_AGENT_TABLE: malformed default')
        result[name] = cells[2].strip().strip('`')
    return result


def compare(text, observed):
    values = rows(text)
    expected = dict(host=values['WENDY_AGENT_HOST'], port=int(values['WENDY_AGENT_PORT']),
                    otel_port=int(values['WENDY_OTEL_PORT']))
    require(all(observed.get(k) == v for k, v in expected.items()),
            'DOC_AGENT_DEFAULT: documented defaults disagree with compiled production configuration')
    require(observed.get('allows_network_access') is False,
            'DOC_AGENT_LOCAL_ONLY: default configuration allows remote access')


def run(evidence):
    require(sys.platform == 'darwin', 'this gate qualifies macOS configuration')
    evidence = evidence.resolve(); evidence.mkdir(parents=True, exist_ok=False)
    work = Path(tempfile.mkdtemp(prefix='wendy-agent-docs-', dir=os.environ.get('RUNNER_TEMP')))
    def source_hashes():
        return {str(p.relative_to(ROOT)): sha(p) for p in SOURCES + [DOC]}
    report = dict(status='failed', source_sha256=source_hashes(), observations={},
                  scope='compiled production configuration/environment; no listener or hardware claim')
    env = {k: v for k, v in os.environ.items() if not k.startswith('WENDY_')}
    successful = False

    def execute(command, name, timeout, overrides=None):
        log = evidence / (name + '.log')
        with log.open('w') as stream:
            code = OWNER.run(command, timeout, env=dict(env, **(overrides or {})), stdout=stream)
        require(code == 0, name + ' failed with exit ' + str(code))
        return log.read_text()

    try:
        swiftc = subprocess.check_output(['xcrun', '--find', 'swiftc'], text=True, timeout=15).strip()
        report['swift_version'] = execute([swiftc, '--version'], 'swift-version', 15).strip()
        (work / 'main.swift').write_text(PROBE)
        binary = work / 'configuration'
        execute([swiftc, '-module-cache-path', str(work / 'modules'), *map(str, SOURCES),
                 str(work / 'main.swift'), '-o', str(binary)], 'compile', 90)
        report['binary_sha256'] = sha(binary)
        default = json.loads(execute([str(binary)], 'default', 10))
        report['observations']['default'] = default
        text = DOC.read_text(); compare(text, default)
        host_row = next(line for line in text.splitlines() if line.startswith('| `WENDY_AGENT_HOST` |'))
        accepted = re.search(r'accepted values: (.*?)\)', host_row)
        require(accepted is not None, 'DOC_AGENT_HOSTS: accepted host values missing')
        hosts = re.findall(r'`([^`]+)`', accepted[1])
        require(len(hosts) == len(set(hosts)) == 3, 'DOC_AGENT_HOSTS: ambiguous accepted values')
        for i, host in enumerate(hosts):
            observed = json.loads(execute([str(binary)], 'accepted-host-' + str(i), 10, {'WENDY_AGENT_HOST': host}))
            require(observed['host'] == host and observed['allows_network_access'] is False,
                    'DOC_AGENT_HOSTS: local override rejected or allows network')
            report['observations']['host=' + host] = observed
        for i, host in enumerate(('::', '0.0.0.0', '127.0.0.1\n')):
            observed = json.loads(execute([str(binary)], 'refused-host-' + str(i), 10, {'WENDY_AGENT_HOST': host}))
            require(observed == default, 'DOC_AGENT_HOST_REFUSAL: invalid host changed defaults')
            report['observations']['refused-host-' + str(i)] = observed
        for name, overrides, expected in (
            ('port-overrides', {'WENDY_AGENT_PORT': '50052', 'WENDY_OTEL_PORT': '0'}, dict(default, port=50052, otel_port=0)),
            ('invalid-ports', {'WENDY_AGENT_PORT': '0', 'WENDY_OTEL_PORT': '65536'}, default)):
            observed = json.loads(execute([str(binary)], name, 10, overrides))
            require(observed == expected, 'DOC_AGENT_PORTS: configured port handling changed')
            report['observations'][name] = observed
        try:
            compare(text.replace(host_row, ORIGINAL_HOST_ROW), default)
        except RuntimeError as error:
            require(str(error).startswith('DOC_AGENT_DEFAULT:'), 'old documentation failed unrelated boundary')
            report['historical_doc_control'] = dict(status='rejected', error=str(error), source_revision='c69fe2880',
                original_row_sha256=hashlib.sha256(ORIGINAL_HOST_ROW.encode()).hexdigest())
        else:
            raise RuntimeError('historical wildcard default was accepted')
        require(report['source_sha256'] == source_hashes(), 'source changed during acceptance')
        report['status'] = 'passed'; successful = True
    finally:
        if successful:
            shutil.rmtree(work)
        else:
            report['retained_scratch'] = str(work)
        (evidence / 'result.json').write_text(json.dumps(report, indent=2) + '\n')
    print('SWIFT_AGENT_DOCUMENTED_DEFAULTS_VERIFIED')


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--evidence', type=Path, required=True)
    run(parser.parse_args().evidence)

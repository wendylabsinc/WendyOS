#!/usr/bin/env python3
"""Require audit1890 certificate policy through real TLS handshakes."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('mldsa_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(spec)
spec.loader.exec_module(OWNER)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path)
    output = parser.parse_args().output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = ROOT / 'go/internal/shared/certs/peer_chain.go'
    text = source.read_text()
    boundary = 'if len(ca.UnhandledCriticalExtensions) != 0 {'
    if text.count(boundary) != 1:
        raise RuntimeError('critical issuer policy boundary changed')
    # Restore the historical direct-issuer behavior that verified the signature
    # without rejecting an unsupported critical extension on the trusted issuer.
    mutation = output / 'unchecked-issuer.go'
    mutation.write_text(text.replace(boundary, 'if false {'))
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(mutation)}}))
    results = {}
    for mode in ('normal', 'unchecked-issuer'):
        command = ['go', 'test', '-race', '-v', '-count=1', '-timeout=30s']
        selection = '^TestDirectMLDSAPolicyOverTLS$'
        if mode != 'normal':
            command += ['-overlay', str(overlay)]
            selection += '/(client|server)/issuer-critical$'
        command += ['./go/internal/agent/mtls', '-run', selection]
        with (output / (mode + '.log')).open('w') as log:
            status = OWNER.run(command, 180, cwd=ROOT, env=dict(os.environ), stdout=log)
        log = (output / (mode + '.log')).read_text()
        print(log)
        if '--- SKIP:' in log or 'WARNING: DATA RACE' in log:
            raise RuntimeError('skipped or raced certificate evidence')
        if mode == 'normal':
            if status or log.count('--- PASS: TestDirectMLDSAPolicyOverTLS/') != 8 or log.count('MLDSA_POLICY_REJECTED ') != 6:
                raise RuntimeError('full TLS policy matrix incomplete')
        elif status != 1 or log.count('MLDSA_TRANSPORT_POLICY malformed chain admitted: client=<nil> server=<nil> delivered=1 policy=<nil>') != 2 or any(
            '--- FAIL: TestDirectMLDSAPolicyOverTLS/' + peer + '/issuer-critical ' not in log
            for peer in ('client', 'server')
        ):
            raise RuntimeError('permissive issuer control did not fail both transport directions')
        results[mode] = {'exit_code': status, 'expected': True}
    sources = [source, ROOT / 'go/internal/agent/mtls/server.go',
               ROOT / 'go/internal/agent/mtls/peer_chain_test.go',
               ROOT / 'go/internal/agent/mtls/peer_chain_transport_test.go']
    (output / 'evidence.json').write_text(json.dumps({
        'audit': '1890', 'results': results,
        'sources': {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in sources},
        'scope': 'actual loopback TLS13 with ECDSA endpoint possession and ML-DSA issuer signatures; no deployed public PKI'
    }, indent=2) + '\n')


if __name__ == '__main__':
    main()

#!/usr/bin/env python3
"""Audit2048: numeric sensor pairing over actual loopback TLS and gRPC."""
import argparse
import hashlib
import importlib.util
import json
import os
from pathlib import Path

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location('sensor_identity_owner', ROOT / '.github/scripts/run-owned-media-test.py')
OWNER = importlib.util.module_from_spec(spec)
spec.loader.exec_module(OWNER)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('output', type=Path)
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    source = ROOT / 'go/internal/agent/mcusource/mtls_dialer.go'
    text = source.read_text()
    old = 'scope, _ := certs.ScopeFromCertPEM(ownCertPEM)'
    if text.count(old) != 1:
        raise RuntimeError('tenant derivation boundary changed')
    # Reintroduce the old caller assumption, adapted to the current shared
    # resolver: the pairing's legacy org substitutes for the identity tenant.
    mutation = output / 'pairing-org.go'
    mutation.write_text(text.replace(old, 'scope := certs.Scope{OrgID: p.OrgID}'))
    overlay = output / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(mutation)}}))
    hashes = {str(p.relative_to(ROOT)): hashlib.sha256(p.read_bytes()).hexdigest() for p in (
        source, ROOT / 'go/internal/agent/mcusource/grpc_transport.go',
        ROOT / 'go/internal/agent/mcusource/mtls_identity_transport_test.go')}
    results = {}
    for mode in ('normal', 'pairing-org'):
        command = ['go', 'test', '-race', '-v', '-count=1', '-timeout=45s']
        if mode != 'normal':
            command += ['-overlay', str(overlay)]
        command += ['./go/internal/agent/mcusource', '-run', '^TestSensorAssetIdentityOverTLSAndGRPC$']
        with (output / (mode + '.log')).open('w') as log:
            status = OWNER.run(command, 180, cwd=ROOT, env=dict(os.environ), stdout=log)
        log = (output / (mode + '.log')).read_text()
        print(log)
        if '--- SKIP:' in log or 'WARNING: DATA RACE' in log:
            raise RuntimeError('skipped or raced identity evidence')
        if mode == 'normal':
            if status or log.count('--- PASS: TestSensorAssetIdentityOverTLSAndGRPC/') != 12:
                raise RuntimeError('normal transport matrix incomplete')
        elif status != 1 or 'ASSET_TENANT_TLS:' not in log or 'ASSET_TENANT_RPC:' not in log:
            raise RuntimeError('old tenant assumption did not fail both intended transport boundaries')
        results[mode] = {'status': status, 'expected': True}
    (output / 'evidence.json').write_text(json.dumps({'audit': '2048', 'sources': hashes, 'results': results,
        'scope': 'actual loopback TCP/TLS and sensor gRPC; locally issued certificates, no public PKI or hardware'}, indent=2) + '\n')


if __name__ == '__main__':
    main()

import base64
import copy
import json
from pathlib import Path
import tempfile
import unittest

import model


def pair_fixture():
    tenant = '10000000-0000-4000-8000-000000000000'
    pair_id = '20000000-0000-4000-8000-000000000000'
    root, issuer = b'public-root-test-only', b'public-issuer-test-only'
    pair = {'kind': 'UNTRUSTED-PRIVATE-AGENT-PAIR-FIXTURE-v1', 'pair_id': pair_id, 'tenant': tenant,
            'root_sha256': model.digest(root), 'issuer_sha256': model.digest(issuer), 'nodes': []}
    peers = {key: value for key, value in pair.items() if key != 'nodes'}
    peers.update(authority_time='fixture-host-wall-time-only', nodes=[])
    for index, name in enumerate(('a', 'b')):
        device = f'30000000-0000-4000-8000-00000000000{index}'
        public = {'kind': 'UNTRUSTED-PRIVATE-AGENT-VM-FIXTURE-v1', 'pair_id': pair_id, 'tenant': tenant,
                  'device_id': device, 'principal': 'spiffe://wendy.sh/tenant/' + tenant + '/device/' + device,
                  'leaf_serial': index + 3, 'root_der': base64.b64encode(root).decode(), 'issuer_der': base64.b64encode(issuer).decode(),
                  'root_sha256': pair['root_sha256'], 'issuer_sha256': pair['issuer_sha256'],
                  'config_path': model.config_path(name) + '/world.json', 'simulation_clock_path': model.config_path(name) + '/simclock.json',
                  'not_before': '2026-11-12T00:00:00Z', 'not_after': '2026-11-12T01:00:00Z'}
        pair['nodes'].append({'directory': model.config_path(name), 'state_directory': model.state_path(name),
                              'public': public, 'public_sha256': model.digest(model.json_bytes(public))})
        peers['nodes'].append({'device_id': device, 'principal': public['principal'], 'leaf_serial': index + 3,
                               'leaf_sha256': str(index + 1) * 64, 'device_spki_sha256': str(index + 3) * 64,
                               'issued_receipt_sha256': str(index + 5) * 64})
    return pair, peers


class ModelTests(unittest.TestCase):
    def test_canonical_domain_contract_across_commission_clock_probe_and_proof(self):
        import control
        import proof
        from test_proof import fixture, update_encoded
        inputs = fixture()
        run_id = inputs['run_id']
        expected = 'simulation:' + run_id
        pair, peers = inputs['pair'], inputs['peers']
        self.assertEqual(model.sample_domain(run_id), expected)
        for key in ('a', 'b'):
            self.assertEqual(model.commission(pair, peers, key, run_id)['policy']['sample_clock_domain'], expected)
            probe = model.probe_config(pair, run_id, key)
            self.assertEqual(probe['run_id'], run_id)
            self.assertEqual(probe['sample_clock_domain'], expected)
        raw, _ = control.sample_clock(run_id, 0)
        self.assertEqual(model.strict_json(raw)['domain'], expected)
        bundle = proof.prepare(**inputs)  # Structural adapter only, fake unsigned fixture.
        request = model.strict_json(bundle.request)
        self.assertEqual(request['timing']['clockDomain'], expected)
        sim = request['provenance']['simulation']
        self.assertEqual(sim['clockDomain'], 'simulation:' + sim['runId'])

        # Reproduce D's mutually consistent but signer-incompatible namespace.
        broken = copy.deepcopy(inputs)
        wrong = 'simulation:agentpair:' + run_id
        broken['publication_receipt']['clock']['sample_clock_domain'] = wrong
        update_encoded(broken['publication_receipt'], 'request_json', lambda q: q['timing'].update(clockDomain=wrong))
        update_encoded(broken['publication_receipt'], 'request_json', lambda q: q['provenance']['simulation'].update(clockDomain=wrong))
        with self.assertRaises(model.Refused): proof.prepare(**broken)
        # The request guard rejects it even if the observed Clock is canonical.
        broken['publication_receipt']['clock']['sample_clock_domain'] = expected
        with self.assertRaises(model.Refused): proof.prepare(**broken)
        for bad in ('agentpair:' + run_id, '', '../run', 'run with space', None):
            with self.subTest(run_id=bad), self.assertRaises(model.Refused): model.sample_domain(bad)

    def test_exact_reciprocal_commission_and_no_app_secret(self):
        pair, peers = pair_fixture()
        for index, node in enumerate(('a', 'b')):
            value = model.commission(pair, peers, node, 'test-1')
            peer = value['replication']['peers'][0]
            self.assertEqual(peer['device'], pair['nodes'][1-index]['public']['principal'])
            self.assertEqual(peer['credential_sha256'], peers['nodes'][1-index]['leaf_sha256'])
            self.assertEqual(peer['import'][0]['device'], peer['device'])
            self.assertEqual(peer['export'][0]['device'], pair['nodes'][index]['public']['principal'])
            self.assertNotEqual(value['journal_path'], peer['remote_path'])
            self.assertEqual(value['policy']['grants'][0]['subscribe'][0]['device'], peer['device'])
            manifest = model.app_manifest(pair, 'test-1', node)
            entitlements = manifest['services']['probe']['entitlements']
            self.assertEqual([e['type'] for e in entitlements], ['persist', 'persist', 'persist', 'world'])
            self.assertEqual(entitlements[-1]['subscribe'][0]['device'], peer['device'])
            config = model.probe_config(pair, 'test-1', node)
            self.assertNotIn('roots_der', config)
            self.assertNotIn('private_key', config)
            self.assertEqual(config['sample_clock_domain'], value['policy']['sample_clock_domain'])

    def test_pair_and_peer_mismatches_refuse_before_commission(self):
        mutations = [lambda p, q: p['nodes'].pop(),
                     lambda p, q: p['nodes'][0].update(directory='/etc/wendy-agent'),
                     lambda p, q: p['nodes'][1]['public'].update(device_id=p['nodes'][0]['public']['device_id']),
                     lambda p, q: p['nodes'][0]['public'].update(leaf_serial=True),
                     lambda p, q: q.update(tenant='40000000-0000-4000-8000-000000000000'),
                     lambda p, q: q['nodes'].reverse(),
                     lambda p, q: q['nodes'][1].update(device_spki_sha256=q['nodes'][0]['device_spki_sha256']),
                     lambda p, q: q.update(authority_time='authenticated')]
        for change in mutations:
            with self.subTest(change=change):
                pair, peers = pair_fixture(); change(pair, peers)
                with self.assertRaises(model.Refused):
                    model.commission(pair, peers, 'a', 'test-1')

    def test_observed_numeric_private_carrier_only(self):
        pair, _ = pair_fixture()
        actual = model.mesh_configuration(pair, 'a', {'a': '172.20.0.2', 'b': '172.20.0.3'})
        self.assertEqual(actual, {'listen': '172.20.0.2:43020', 'peers': [{'device': pair['nodes'][1]['public']['device_id'], 'address': '172.20.0.3:43020'}]})
        for bad in ('localhost', '127.0.0.1', '8.8.8.8', '169.254.1.1', '172.20.0.2'):
            with self.subTest(bad=bad), self.assertRaises(model.Refused):
                model.mesh_configuration(pair, 'a', {'a': '172.20.0.2', 'b': bad})

    def test_pins_and_unsafe_input_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory).resolve() / 'binary'
            header = bytearray(64); header[:6] = b'\x7fELF\x02\x01'; header[18:20] = (183).to_bytes(2, 'little'); path.write_bytes(header)
            value = {'version': 1, 'run_id': 'fixture', 'runtime_image': 'sha256:' + '1' * 64,
                     'binaries': {name: {'path': str(path), 'sha256': model.digest(header)} for name in model.TOOLS_REQUIRED}}
            self.assertEqual(model.validate_inputs(model.json_bytes(value)), value)
            with_proof = copy.deepcopy(value)
            with_proof['binaries']['proof'] = dict(value['binaries']['agent'])
            self.assertEqual(model.validate_inputs(model.json_bytes(with_proof)), with_proof)
            wrong_proof = copy.deepcopy(with_proof)
            wrong_proof['binaries']['proof']['sha256'] = '2' * 64
            with self.assertRaises(model.Refused):
                model.validate_inputs(model.json_bytes(wrong_proof))
            unknown = copy.deepcopy(value)
            unknown['binaries']['extra'] = dict(value['binaries']['agent'])
            missing = copy.deepcopy(with_proof)
            del missing['binaries']['probe']
            for invalid in (unknown, missing):
                with self.assertRaises(model.Refused):
                    model.validate_inputs(model.json_bytes(invalid))
            for mutation in ('changed', 'tag', 'symlink', 'writable', 'duplicate'):
                with self.subTest(mutation=mutation):
                    item = copy.deepcopy(value)
                    if mutation == 'changed': item['binaries']['agent']['sha256'] = '2' * 64
                    if mutation == 'tag': item['runtime_image'] = 'ubuntu:latest'
                    if mutation == 'symlink':
                        alias = path.parent / 'alias'; alias.symlink_to(path); item['binaries']['agent']['path'] = str(alias)
                    if mutation == 'writable': path.chmod(0o666)
                    raw = model.json_bytes(item)
                    if mutation == 'duplicate': raw = raw.replace(b'"version":1', b'"version":1,"version":1')
                    with self.assertRaises((model.Refused, OSError)):
                        model.validate_inputs(raw)
                    path.chmod(0o600)

    def test_fresh_generation_volumes_do_not_overlap(self):
        all_names = [name for node in ('a', 'b') for generation in (1, 2) for name in model.volume_names('fixture', node, generation).values()]
        self.assertEqual(len(all_names), len(set(all_names)))
        for invalid in (0, True, 9):
            with self.assertRaises(model.Refused):
                model.volume_names('fixture', 'a', invalid)


if __name__ == '__main__':
    unittest.main()

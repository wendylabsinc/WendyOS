import copy
import unittest

from integration.distributedfusion import model
from integration.hazard import model as original
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.peers = pair_fixture()
        self.scene = identity_fixture()

    def config(self, node):
        return model.commission(node, self.pair, self.peers, self.scene['run_id'], self.scene,
                                scene_url='http://10.20.0.1:8091' if node == 'a' else 'http://172.20.0.2:8092',
                                read_token_file='/protected/' + node + '/scene-read-token')

    def test_b_gas_export_and_a_result_export_are_exact_and_separate(self):
        a, b = self.config('a'), self.config('b')
        gas, result = model.gas_source(self.pair, self.scene), model.result_source(self.pair, self.scene)
        self.assertEqual(a['replication']['peers'][0]['import'], [gas])
        self.assertEqual(a['replication']['peers'][0]['export'], [result])
        self.assertEqual(b['replication']['peers'][0]['export'], [gas])
        self.assertEqual(b['replication']['peers'][0]['import'], [result])
        self.assertNotIn('gas-1', [g['service'] for g in a['policy']['grants']])
        self.assertEqual([g['service'] for g in b['policy']['grants']], ['gas-1', 'reader'])

    def test_b_has_same_http_source_binding_without_a_second_clock(self):
        a, b = self.config('a'), self.config('b')
        for key in ('run_id', 'boot_id', 'package_sha256', 'frame', 'anchor_unix_ns'):
            self.assertEqual(a['simulation_clock'][key], b['simulation_clock'][key])
        self.assertEqual(b['simulation_clock']['url'], 'http://172.20.0.2:8092')
        self.assertEqual(b['simulation_clock']['read_token_file'], '/protected/b/scene-read-token')
        self.assertNotIn('simulation_clock_file', b)
        self.assertIn('proof_archive_path', b)
        with self.assertRaises(model.Refused):
            model.commission('b', self.pair, self.peers, self.scene['run_id'], self.scene)

    def test_config_manifest_and_fuser_name_same_b_gas_device(self):
        a = self.config('a')
        scope = next(g for g in a['policy']['grants'] if g['service'] == 'fusion')['subscribe']
        fuser = model.fusion_config(self.pair, self.scene['run_id'], self.scene, 9)
        manifest = model.app_manifest(self.pair, self.scene['run_id'], 'a')
        wire = next(e for e in manifest['services']['fusion']['entitlements'] if e['type'] == 'world')['subscribe']
        for rows in (scope, fuser['sources'], wire):
            self.assertEqual(next(s for s in rows if s['service'] == 'gas-1')['device'], self.pair['nodes'][1]['public']['principal'])
            self.assertTrue(all('device' not in s for s in rows if s['service'] != 'gas-1'))
        self.assertEqual(fuser['request_timeout_ms'], 3000)
        self.assertEqual(fuser['max_skew_ms'], 350)
        self.assertEqual(model.capture_config('gas-1', self.scene['run_id'], self.scene, 'http://172.20.0.2:8092')['ttl_ms'], 3000)

    def test_scoped_volume_sets_and_original_modules_unchanged(self):
        names = []
        for node in ('a', 'b'):
            manifest = model.app_manifest(self.pair, self.scene['run_id'], node)
            self.assertEqual(tuple(manifest['services']), model.SERVICES[node])
            for service in model.SERVICES[node]:
                names.extend(model.volume_names(self.scene['run_id'], node, service).values())
        self.assertEqual(len(names), 16)
        self.assertEqual(len(set(names)), 16)
        self.assertTrue(all(n.startswith('world-distributedfusion-') for n in names))
        self.assertEqual(original.APP, 'com.wendy.world.hazard')
        self.assertIn('gas-1', original.SERVICES['a'])
        self.assertEqual(original.SERVICES['b'], ('reader',))

    def test_only_b_gas_gets_bridge_egress_to_the_fixture_source(self):
        for node in ('a', 'b'):
            manifest = model.app_manifest(self.pair, self.scene['run_id'], node)
            self.assertEqual(manifest['isolation'], 'isolated')
            for service, config in manifest['services'].items():
                network = [e for e in config['entitlements'] if e['type'] == 'network']
                self.assertEqual(network, [{'type': 'network', 'mode': 'bridge'}]
                                 if node == 'b' and service == 'gas-1' else [])
        original_manifest = original.app_manifest(self.pair, self.scene['run_id'], 'a')
        self.assertFalse(any(e['type'] == 'network'
                             for s in original_manifest['services'].values()
                             for e in s['entitlements']))

    def test_observed_scene_not_mutated(self):
        before = copy.deepcopy(self.scene)
        self.config('b')['simulation_clock']['frame']['revision'] = 'changed'
        self.assertEqual(self.scene, before)


if __name__ == '__main__':
    unittest.main()

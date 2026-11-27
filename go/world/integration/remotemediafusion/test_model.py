from copy import deepcopy
import unittest

from integration.remotemediafusion import model
from integration.hazard import model as original
from integration.distributedfusion import model as split
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

    def test_exact_two_media_sources_and_result_use_separate_grants(self):
        a, b = self.config('a'), self.config('b')
        captures = [model.capture_source(self.pair, self.scene, sensor) for sensor in model.REMOTE_SENSORS]
        result = model.result_source(self.pair, self.scene)
        self.assertEqual(a['replication']['peers'][0]['import'], captures)
        self.assertEqual(a['replication']['peers'][0]['export'], [result])
        self.assertEqual(b['replication']['peers'][0]['export'], captures)
        self.assertEqual(b['replication']['peers'][0]['import'], [result])
        self.assertEqual([g['service'] for g in a['policy']['grants']], [*model.LOCAL_SENSORS, 'fusion'])
        self.assertEqual([g['service'] for g in b['policy']['grants']], ['camera-b', 'audio-1', 'reader'])
        self.assertFalse(next(g for g in b['policy']['grants'] if g['service'] == 'reader').get('publish'))
        with self.assertRaises(model.Refused):
            model.capture_source(self.pair, self.scene, 'gas-1')

    def test_same_http_clock_and_distinct_origin_store_and_receiver_cache(self):
        a, b = self.config('a'), self.config('b')
        for key in ('run_id', 'boot_id', 'package_sha256', 'frame', 'anchor_unix_ns'):
            self.assertEqual(a['simulation_clock'][key], b['simulation_clock'][key])
        self.assertNotIn('simulation_clock_file', b)
        self.assertEqual(b['simulation_clock']['url'], 'http://172.20.0.2:8092')
        self.assertEqual(b['evidence_directory'], model.state_path('b') + '/evidence')
        peer = a['replication']['peers'][0]
        self.assertEqual(peer['evidence_path'], model.state_path('a') + '/remote-evidence-b')
        self.assertNotIn(peer['evidence_path'], [a['evidence_directory'], a['journal_path'], peer['remote_path'], a['replication']['authority_path']])
        self.assertNotIn('evidence_path', b['replication']['peers'][0])
        with self.assertRaises(model.Refused):
            model.commission('b', self.pair, self.peers, self.scene['run_id'], self.scene)

    def test_fuser_config_and_manifest_pin_b_for_both_media_sources(self):
        a = self.config('a')
        fuser = model.fusion_config(self.pair, self.scene['run_id'], self.scene, 9)
        manifest = model.app_manifest(self.pair, self.scene['run_id'], 'a')
        selectors = next(g for g in a['policy']['grants'] if g['service'] == 'fusion')['subscribe']
        wire = next(e for e in manifest['services']['fusion']['entitlements'] if e['type'] == 'world')['subscribe']
        for rows in (selectors, fuser['sources'], wire):
            for source in rows:
                if source['service'] in model.REMOTE_SENSORS:
                    self.assertEqual(source['device'], self.pair['nodes'][1]['public']['principal'])
                else:
                    self.assertNotIn('device', source)
        self.assertEqual(fuser['request_timeout_ms'], 3000)
        self.assertEqual(fuser['max_skew_ms'], 350)
        self.assertEqual(fuser['expected_generation'], 9)
        for sensor in model.REMOTE_SENSORS:
            self.assertEqual(model.capture_config(sensor, self.scene['run_id'], self.scene, 'http://172.20.0.2:8092')['ttl_ms'], 3000)

    def test_only_remote_capture_apps_receive_bridge_egress(self):
        names = []
        for node in ('a', 'b'):
            manifest = model.app_manifest(self.pair, self.scene['run_id'], node)
            self.assertEqual(tuple(manifest['services']), model.SERVICES[node])
            for service, value in manifest['services'].items():
                names += list(model.volume_names(self.scene['run_id'], node, service).values())
                network = [e for e in value['entitlements'] if e['type'] == 'network']
                self.assertEqual(network, [{'type': 'network', 'mode': 'bridge'}]
                                 if node == 'b' and service in model.REMOTE_SENSORS else [])
        self.assertEqual(len(names), 16)
        self.assertEqual(len(set(names)), 16)
        self.assertTrue(all(name.startswith('world-remotemediafusion-') for name in names))

    def test_existing_modules_and_observed_identity_unchanged(self):
        before = deepcopy(self.scene)
        self.config('b')['simulation_clock']['frame']['revision'] = 'changed'
        self.assertEqual(self.scene, before)
        self.assertEqual(original.APP, 'com.wendy.world.hazard')
        self.assertEqual(original.SERVICES['b'], ('reader',))
        self.assertEqual(split.SERVICES['b'], ('gas-1', 'reader'))
        self.assertEqual(split.APP, 'com.wendy.world.distributedfusion')


if __name__ == '__main__':
    unittest.main()

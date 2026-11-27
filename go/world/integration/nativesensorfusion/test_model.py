import unittest
from integration.nativesensorfusion import model
from integration.hazard import model as original
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.peers = pair_fixture()
        self.scene = identity_fixture()
        self.source = model.native_source_id(self.pair['nodes'][0]['public']['principal'])

    def test_one_exact_native_binding_preserves_five_capture_grants(self):
        cfg = model.commission('a', self.pair, self.peers, self.scene['run_id'], self.scene,
            'http://10.1.0.2:8091', '/protected/token', source_id=self.source)
        self.assertEqual(cfg['native_cameras'], [{'app': model.APP, 'service': 'camera-b',
            'component_type': model.CAPTURE, 'stream': 'camera-b', 'source_id': self.source}])
        self.assertEqual(len(cfg['policy']['grants']), 6)
        self.assertEqual(cfg['replication']['peers'][0]['import'], [])
        self.assertEqual(cfg['replication']['peers'][0]['export'], [model.result_source(self.pair, self.scene)])
        b = model.commission('b', self.pair, self.peers, self.scene['run_id'], self.scene)
        self.assertNotIn('native_cameras', b)
        with self.assertRaises(model.Refused):
            model.commission('a', self.pair, self.peers, self.scene['run_id'], self.scene,
                'http://10.1.0.2:8091', '/protected/token',
                source_id=model.native_source_id(self.pair['nodes'][1]['public']['principal']))

    def test_bridge_config_has_no_direct_camera_network_path_or_credentials(self):
        cfg = model.native_bridge_config(self.scene['run_id'], self.scene, source_id=self.source,
                                         metadata_url='http://172.21.0.2:50061')
        self.assertEqual(cfg['bounds']['max_frames'], 2)
        self.assertEqual(cfg['commission']['boot_id'], self.scene['boot_id'])
        self.assertEqual(cfg['channel_id'], 4)
        self.assertEqual(cfg['source_id'], self.source)
        for key in ('tcp_address', 'scene_url', 'scene_read_token_file', 'private_key'):
            self.assertNotIn(key, cfg)
        with self.assertRaises(model.Refused):
            model.capture_config('camera-b', self.scene['run_id'], self.scene, 'http://10.1.0.2:8091')

    def test_only_camera_metadata_needs_bridge_egress_and_existing_fixture_unchanged(self):
        manifest = model.app_manifest(self.pair, self.scene['run_id'], 'a')
        self.assertEqual(manifest['appId'], model.APP)
        self.assertEqual(manifest['isolation'], 'isolated')
        for service, spec in manifest['services'].items():
            network = [e for e in spec['entitlements'] if e['type'] == 'network']
            self.assertEqual(network, [{'type': 'network', 'mode': 'bridge'}] if service == 'camera-b' else [])
        self.assertEqual(original.APP, 'com.wendy.world.hazard')
        self.assertNotEqual(model.volume_names(self.scene['run_id'], 'a', 'camera-b'),
                            original.volume_names(self.scene['run_id'], 'a', 'camera-b'))

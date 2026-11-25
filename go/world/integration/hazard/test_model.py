import copy
import unittest

from integration.hazard import model
from integration.people.test_model import pair_fixture


def identity_fixture():
    return {'schema_version': 'wendy.sim.clock.v1', 'run_id': 'hazard-test',
            'boot_id': 'observed-boot', 'package_sha256': 'a' * 64,
            'frame': {'id': 'office-floor', 'revision': 'office-' + 'b' * 16},
            'clock_domain': 'simulation:hazard-test', 'anchor_unix_ns': model.ANCHOR,
            'site': 'studio-01', 'scenario_version': 'furnished-office-fusion-v1', 'seed': 7}


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.peers = pair_fixture()
        self.scene = identity_fixture()
        self.run_id = self.scene['run_id']
        self.url = 'http://10.47.0.5:8091'

    def commission(self, node):
        kwargs = {'scene_url': self.url, 'read_token_file': '/config/scene-read-token'} if node == 'a' else {}
        return model.commission(node, self.pair, self.peers, self.run_id, self.scene, **kwargs)

    def test_only_exact_origin_hazard_crosses_native_boundary(self):
        a, b = self.commission('a'), self.commission('b')
        source = model.result_source(self.pair, self.scene)
        self.assertEqual(source['service'], 'fusion')
        self.assertEqual(source['stream'], 'hazard')
        self.assertEqual(source['device'], self.pair['nodes'][0]['public']['principal'])
        self.assertEqual(a['replication']['peers'][0]['export'], [source])
        self.assertEqual(a['replication']['peers'][0]['import'], [])
        self.assertEqual(b['replication']['peers'][0]['import'], [source])
        self.assertEqual(b['replication']['peers'][0]['export'], [])
        self.assertNotIn('publish', b['policy']['grants'][0])
        for key in ('proof_archive_path', 'evidence_directory', 'simulation_clock'):
            self.assertNotIn(key, b)
        self.assertEqual(b['simulation_clock_file'], self.pair['nodes'][1]['public']['simulation_clock_path'])

    def test_five_captures_have_exact_publish_and_fuser_subscribe_grants(self):
        a = self.commission('a')
        self.assertEqual(model.SENSORS, ('camera-a', 'camera-b', 'lidar-atlas', 'gas-1', 'audio-1'))
        grants = {g['service']: g for g in a['policy']['grants']}
        self.assertEqual(set(grants), {*model.SENSORS, 'fusion'})
        for sensor in model.SENSORS:
            self.assertEqual(grants[sensor]['publish'], [{'component_type': model.CAPTURE, 'stream': sensor}])
        self.assertEqual([s['service'] for s in grants['fusion']['subscribe']], list(model.SENSORS))
        self.assertEqual(a['policy']['sample_clock_domain'], 'simulation:hazard-test')
        self.assertEqual(a['simulation_clock']['boot_id'], self.scene['boot_id'])

    def test_manifest_matches_grants_and_all_volume_names_are_distinct(self):
        names = set()
        for node in ('a', 'b'):
            manifest = model.app_manifest(self.pair, self.run_id, node)
            grants = {g['service']: g for g in self.commission(node)['policy']['grants']}
            self.assertEqual(tuple(manifest['services']), model.SERVICES[node])
            self.assertEqual(manifest['appId'], 'com.wendy.world.hazard')
            for service, spec in manifest['services'].items():
                world = [e for e in spec['entitlements'] if e['type'] == 'world']
                persist = [e for e in spec['entitlements'] if e['type'] == 'persist']
                for e in persist:
                    self.assertNotIn(e['name'], names)
                    names.add(e['name'])
                if service == 'scene':
                    self.assertEqual(world, [])
                    continue
                self.assertEqual(len(world), 1)
                for action in ('publish', 'subscribe'):
                    converted = [{('component_type' if k == 'componentType' else k): v for k, v in row.items()}
                                 for row in world[0].get(action, [])]
                    self.assertEqual(converted, grants[service].get(action, []))
        self.assertEqual(len(names), 16)

    def test_configs_preserve_original_source_identity_and_bounds(self):
        fusion = model.fusion_config(self.pair, self.run_id, self.scene, 8)
        self.assertEqual(fusion['expected_generation'], 8)
        self.assertEqual(fusion['request_timeout_ms'], 3000)
        self.assertEqual(fusion['max_skew_ms'], 350)
        self.assertEqual([s['sensor_id'] for s in fusion['sources']], list(model.SENSORS))
        for sensor in model.SENSORS:
            capture = model.capture_config(sensor, self.run_id, self.scene, self.url)
            self.assertEqual(capture['sensor_id'], sensor)
            self.assertEqual(capture['ttl_ms'], 3000)
            self.assertEqual(capture['request_timeout_ms'], 1500)
            self.assertEqual(capture['simulation_anchor_unix_ns'], fusion['simulation_anchor_unix_ns'])
        reader = model.reader_config(self.pair, self.run_id, self.scene, 9)
        self.assertEqual(reader['source'], {'device': self.pair['nodes'][0]['public']['principal'],
            'app': model.APP, 'service': 'fusion', 'stream': 'hazard'})
        self.assertEqual(reader['request_timeout_ms'], 3000)
        for key in ('scene_url', 'read_token_file', 'sources', 'model_sha256'):
            self.assertNotIn(key, reader)

    def test_wrong_scene_source_or_generation_refuses(self):
        for key, value in [('run_id', 'other'), ('clock_domain', 'simulation:agentpair:hazard-test'),
                           ('scenario_version', 'people-annex-v1'), ('anchor_unix_ns', model.ANCHOR + 1),
                           ('seed', True), ('package_sha256', 'not-a-digest')]:
            bad = copy.deepcopy(self.scene); bad[key] = value
            with self.subTest(key=key), self.assertRaises(model.Refused):
                model.fusion_config(self.pair, self.run_id, bad)
        with self.assertRaises(model.Refused):
            model.capture_config('lidar-unknown', self.run_id, self.scene, self.url)
        for generation in (True, -1, 1 << 64):
            with self.assertRaises(model.Refused):
                model.reader_config(self.pair, self.run_id, self.scene, generation)

    def test_returned_configs_do_not_mutate_observed_values(self):
        before = model.json_bytes([self.pair, self.peers, self.scene])
        self.commission('a')['policy']['frames'][0]['revision'] = 'changed'
        model.fusion_config(self.pair, self.run_id, self.scene)['frame']['id'] = 'changed'
        self.assertEqual(before, model.json_bytes([self.pair, self.peers, self.scene]))


if __name__ == '__main__':
    unittest.main()

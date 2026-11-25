from copy import deepcopy
import unittest

from integration.sharedperception import model
from integration.remotemediafusion import model as remote
from integration.people import model as people
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


def scene_fixture():
    value = identity_fixture()
    value['scenario_version'] = 'shared-office-perception-v1'
    value['frame'] = {'id': 'office-floor', 'revision': 'shared-office-test'}
    return value


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.peers = pair_fixture()
        self.scene = scene_fixture()

    def commission(self, node):
        return model.commission(node, self.pair, self.peers, self.scene['run_id'], self.scene,
                                'http://10.20.0.1:' + ('8091' if node == 'a' else '8092'),
                                '/protected/' + node + '/read-token')

    def test_one_identity_two_exact_derived_streams_and_remote_originals(self):
        a, b = self.commission('a'), self.commission('b')
        captures = [model.capture_source(self.pair, self.scene, s) for s in model.REMOTE_SENSORS]
        derived = [model.result_source(self.pair, self.scene), model.track_source(self.pair, self.scene)]
        self.assertEqual(a['replication']['peers'][0]['import'], captures)
        self.assertEqual(b['replication']['peers'][0]['export'], captures)
        self.assertEqual(a['replication']['peers'][0]['export'], derived)
        self.assertEqual(b['replication']['peers'][0]['import'], derived)
        for key in ('run_id', 'boot_id', 'package_sha256', 'frame', 'anchor_unix_ns'):
            self.assertEqual(a['simulation_clock'][key], b['simulation_clock'][key])
        self.assertNotIn('simulation_clock_file', b)
        self.assertEqual(a['replication']['peers'][0]['evidence_path'], model.state_path('a') + '/remote-evidence-b')
        self.assertEqual({g['service'] for g in a['policy']['grants']}, set(model.SERVICES['a']) - {'scene'})
        self.assertEqual({g['service'] for g in b['policy']['grants']}, set(model.SERVICES['b']))
        self.assertEqual(len(a['policy']['grants']) + len(b['policy']['grants']), 11)

    def test_five_and_two_sources_stay_separate_and_readers_have_own_generation(self):
        args = self.pair, self.scene['run_id'], self.scene
        fuser = model.fusion_config(*args, 13)
        tracker = model.people_config(*args, {'model_id': 'pose', 'model_version': '1', 'model_sha256': 'a' * 64}, 14)
        self.assertEqual([s['sensor_id'] for s in fuser['sources']], list(model.HAZARD_SENSORS))
        self.assertEqual([s['sensor_id'] for s in tracker['sources']], list(model.PEOPLE_CAMERAS))
        self.assertEqual(fuser['scenario_version'], tracker['scenario_version'])
        self.assertEqual(fuser['frame'], tracker['frame'])
        for sensor in fuser['sources']:
            self.assertEqual(sensor.get('device'), self.pair['nodes'][1]['public']['principal']
                             if sensor['sensor_id'] in model.REMOTE_SENSORS else None)
        hazard = model.reader_config(*args, 21)
        tracks = model.people_reader_config(*args, 22)
        self.assertEqual((hazard['service'], hazard['expected_generation'], hazard['source']['stream']), ('reader', 21, 'hazard'))
        self.assertEqual((tracks['service'], tracks['expected_generation'], tracks['source']['stream']), ('people-reader', 22, 'tracks'))
        self.assertEqual(tracks['source']['service'], 'people')

    def test_manifest_and_commission_agree_for_all_eleven_world_mounts(self):
        volumes = []
        for node in ('a', 'b'):
            manifest, cfg = model.app_manifest(self.pair, self.scene['run_id'], node), self.commission(node)
            self.assertEqual(tuple(manifest['services']), model.SERVICES[node])
            grants = {g['service']: g for g in cfg['policy']['grants']}
            for service, entry in manifest['services'].items():
                world = [e for e in entry['entitlements'] if e['type'] == 'world']
                self.assertEqual(len(world), 0 if service == 'scene' else 1)
                if world:
                    normalized = model.strict_json(model.json_bytes(world[0]).replace(b'componentType', b'component_type'))
                    self.assertEqual({k: v for k, v in normalized.items() if k != 'type'},
                                     {k: v for k, v in grants[service].items() if k not in ('app', 'service')})
                network = [e for e in entry['entitlements'] if e['type'] == 'network']
                self.assertEqual(network, [{'type': 'network', 'mode': 'bridge'}]
                                 if node == 'b' and service in model.REMOTE_SENSORS else [])
                volumes.extend(model.volume_names(self.scene['run_id'], node, service).values())
        self.assertEqual(len(set(volumes)), 24)

    def test_generic_capture_all_seven_and_original_fixtures_unchanged(self):
        original = deepcopy(self.scene)
        for sensor in model.SENSORS:
            cfg = model.capture_config(sensor, self.scene['run_id'], self.scene, 'http://10.20.0.1:8091')
            self.assertEqual((cfg['sensor_id'], cfg['ttl_ms'], cfg['frame']), (sensor, 3000, self.scene['frame']))
        self.commission('a')['simulation_clock']['frame']['revision'] = 'modified'
        self.assertEqual(self.scene, original)
        with self.assertRaises(model.Refused): model.validate_scene_identity(self.scene['run_id'], identity_fixture())
        self.assertEqual(remote.APP, 'com.wendy.world.remotemediafusion')
        self.assertEqual(len(remote.SENSORS), 5)
        self.assertEqual(people.CAMERAS, ('camera-a', 'camera-b'))


if __name__ == '__main__': unittest.main()

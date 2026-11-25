from copy import deepcopy
import base64
import unittest

from integration.sharedinspection import model
from integration.sharedperception import model as original
from integration.sharedperception.test_model import scene_fixture
from integration.people.test_model import pair_fixture


def viewer_fixture(pair, scene):
    task = {'node': 'b', 'app': model.APP, 'service': 'viewer', 'generation': 17,
            'device': pair['nodes'][1]['public']['principal'], 'ipv4': '10.0.0.8',
            'socket_directory': model.state_path('b') + '/sockets/observed-viewer'}
    public = {'version': 1, 'tenant': pair['tenant'], 'site': scene['site'],
              'audience': model.VIEWER_AUDIENCE, 'root_public_key': base64.b64encode(b'v' * 32).decode(),
              'revocations': {'revocations': {'version': 1, 'tenant': pair['tenant'],
                  'site': scene['site'], 'domain': 'simulation', 'revision': 1,
                  'issued_at_unix_ms': 1794500000000, 'expires_at_unix_ms': 1794503540000},
                  'signature': base64.b64encode(b's' * 64).decode()}}
    return task, public


class ModelTests(unittest.TestCase):
    def test_continuous_publishers_have_a_full_minute_sample_budget(self):
        scene = scene_fixture()
        for sensor in model.SENSORS:
            config = model.capture_config(sensor, scene['run_id'], scene, 'http://10.0.0.3:8091')
            self.assertEqual(config['max_samples'], 64)
            self.assertEqual(config['poll_interval_ms'], 1000)
            self.assertEqual(config['ttl_ms'], 3000)
            self.assertGreaterEqual((config['max_samples'] - 1) * config['poll_interval_ms'], 60_000)
        # Existing stepped fixture limits are not changed through module aliases.
        old = original.capture_config('camera-a', scene['run_id'], scene, 'http://10.0.0.3:8091')
        self.assertEqual((old['max_samples'], old['poll_interval_ms']), (32, 100))

    def test_mission_services_have_separate_http_authority_and_owned_state(self):
        pair, peers = pair_fixture(); scene = scene_fixture()
        original_services = deepcopy(original.SERVICES)
        for node in ('a', 'b'):
            manifest = model.app_manifest(pair, scene['run_id'], node)
            config = model.commission(node, pair, peers, scene['run_id'], scene,
                                      'http://10.20.0.1:8091', '/protected/read-token')
            world = {service for service, value in manifest['services'].items()
                     if any(e['type'] == 'world' for e in value['entitlements'])}
            self.assertEqual(world, {g['service'] for g in config['policy']['grants']})
            self.assertEqual(len(world), 7 if node == 'a' else 5)
        services = model.app_manifest(pair, scene['run_id'], 'a')['services']
        for service in ('mission-gateway', 'atlas-executor'):
            self.assertIn({'type': 'network', 'mode': 'bridge'}, services[service]['entitlements'])
            self.assertFalse(any(e['type'] == 'world' for e in services[service]['entitlements']))
        self.assertIn('state', model.volume_names(scene['run_id'], 'a', 'mission-gateway'))
        self.assertEqual(original.SERVICES, original_services)
        self.assertEqual(original.APP, 'com.wendy.world.sharedperception')
        self.assertNotIn('mission-gateway', original.SERVICES['a'])
        self.assertNotIn('viewer', original.SERVICES['b'])
        self.assertEqual(model.people_reader_config(pair, scene['run_id'], scene, 7)['mission_floor_y'], .24)
        self.assertNotIn('mission_floor_y', original.people_reader_config(pair, scene['run_id'], scene, 7))
        hazard = model.reader_config(pair, scene['run_id'], scene, 11)
        legacy = original.reader_config(pair, scene['run_id'], scene, 11)
        self.assertEqual(hazard.pop('mission_floor_y'), .24)
        self.assertEqual(hazard, model._shared.reader_config(pair, scene['run_id'], scene, 11))
        self.assertNotIn('mission_floor_y', legacy)

    def test_commission_input_binds_observed_model_and_gateway_not_a_guessed_port(self):
        pair, _ = pair_fixture(); scene = scene_fixture()
        scene['frame']['revision'] = 'shared-office-' + 'b' * 16
        presentation = {'geometry': {'run_id': scene['run_id'], 'model_sha256': 'b' * 64}, 'simulation_time_ns': 123}
        value = model.mission_input(pair, scene['run_id'], scene, presentation,
                                    'http://10.0.0.3:8091', '10.0.0.4', 'a' * 64)
        self.assertEqual(value['tenant'], pair['tenant'])
        self.assertEqual(value['device'], pair['nodes'][0]['public']['principal'])
        self.assertEqual(value['frame'], scene['frame'])
        self.assertEqual(value['package_sha256'], scene['package_sha256'])
        self.assertEqual(value['gateway_url'], 'http://10.0.0.4:8093')
        self.assertNotIn('operator_url', value)
        self.assertNotIn('viewer_url', value)
        exported = model.mission_input(pair, scene['run_id'], scene, presentation,
            'http://10.0.0.3:8091', '10.0.0.4', 'a' * 64, viewer_url='http://127.0.0.1:15432')
        self.assertEqual(exported['viewer_url'], 'http://127.0.0.1:15432')
        presentation['geometry']['model_sha256'] = 'c' * 64
        with self.assertRaises(ValueError):
            model.mission_input(pair, scene['run_id'], scene, presentation, 'http://10.0.0.3:8091', '10.0.0.4', 'a' * 64)

    def test_viewer_reads_both_exact_a_results_on_one_observed_b_identity(self):
        pair, peers = pair_fixture(); scene = scene_fixture()
        task, public = viewer_fixture(pair, scene)
        config = model.viewer_config(pair, scene['run_id'], scene, task, public)
        self.assertIs(config['reader']['continuous'], True)
        people, hazards = (config['reader'][kind] for kind in ('people', 'hazards'))
        for value in (people, hazards):
            self.assertEqual((value['service'], value['expected_generation']), ('viewer', 17))
            self.assertEqual(value['device'], pair['nodes'][1]['public']['principal'])
            self.assertEqual(value['roots_der'], [pair['nodes'][1]['public']['root_der']])
            self.assertEqual((value['run_id'], value['frame']), (scene['run_id'], scene['frame']))
            self.assertEqual((value['duration_ms'], value['max_ticks']), (120000, 480))
            self.assertNotIn('mission_floor_y', value)
            self.assertEqual(value['source']['device'], pair['nodes'][0]['public']['principal'])
            self.assertEqual(value['source']['app'], model.APP)
        self.assertEqual((people['source']['service'], people['source']['stream']), ('people', 'tracks'))
        self.assertEqual((hazards['source']['service'], hazards['source']['stream']), ('fusion', 'hazard'))
        self.assertEqual({k: v for k, v in people.items() if k != 'source'},
                         {k: v for k, v in hazards.items() if k != 'source'})
        self.assertEqual(config['listen'], '0.0.0.0:8094')
        self.assertEqual(config['root_public_key'], public['root_public_key'])
        self.assertEqual(config['evidence_sources'], model.viewer_evidence_selectors(pair))
        self.assertNotIn('private_key', model.json_bytes(config).decode())
        manifest = model.app_manifest(pair, scene['run_id'], 'b')['services']['viewer']
        world = next(e for e in manifest['entitlements'] if e['type'] == 'world')
        self.assertEqual(world, {'type': 'world', 'subscribe': model.viewer_selectors(pair, wire=True)})
        self.assertIn({'type': 'network', 'mode': 'bridge'}, manifest['entitlements'])
        grants = model.commission('b', pair, peers, scene['run_id'], scene,
            'http://10.0.0.3:8091', '/protected/read-token')['policy']['grants']
        self.assertEqual(next(g for g in grants if g['service'] == 'viewer'),
                         {'app': model.APP, 'service': 'viewer', 'subscribe': model.viewer_selectors(pair)})
        for field, value in [('service', 'people-reader'), ('generation', 0), ('device', pair['nodes'][0]['public']['principal'])]:
            changed = deepcopy(task); changed[field] = value
            with self.assertRaises(ValueError): model.viewer_config(pair, scene['run_id'], scene, changed, public)
        for field, value in [('tenant', 'another'), ('audience', 'atlas-mission-gateway'), ('private_key', 'secret')]:
            changed = deepcopy(public); changed[field] = value
            with self.assertRaises(ValueError): model.viewer_config(pair, scene['run_id'], scene, task, changed)

    def test_viewer_media_follows_all_capture_owners_and_existing_peer_route(self):
        pair, peers = pair_fixture(); scene = scene_fixture()
        sources = model.viewer_evidence_selectors(pair)
        self.assertEqual(len(sources), 5)
        actual = {(s['device'], s['service'], s['stream'], s['component_type']) for s in sources}
        expected = {(pair['nodes'][('a', 'b').index(node)]['public']['principal'], service, service, model.CAPTURE)
                    for node, service in model.VIEWER_MEDIA_SOURCES}
        self.assertEqual(actual, expected)
        self.assertFalse(any(s['service'] in ('fusion', 'people') for s in sources))
        configs = {node: model.commission(node, pair, peers, scene['run_id'], scene,
            'http://10.0.0.3:8091', '/protected/read-token') for node in ('a', 'b')}
        a_peer = configs['a']['replication']['peers'][0]
        b_peer = configs['b']['replication']['peers'][0]
        a_media = [s for s in a_peer['export'] if s['component'] == model.CAPTURE]
        b_media = [s for s in b_peer['import'] if s['component'] == model.CAPTURE]
        self.assertEqual(a_media, b_media)
        self.assertEqual({s['service'] for s in a_media}, {'camera-a', 'people-front', 'people-oblique'})
        self.assertTrue(all(s['device'] == pair['nodes'][0]['public']['principal'] and
            s['tenant'] == pair['tenant'] and s['site'] == scene['site'] for s in a_media))
        self.assertEqual(b_peer['evidence_path'], model.state_path('b') + '/remote-evidence-a')
        # The existing reverse route continues to supply A's fusion app.
        self.assertEqual({s['service'] for s in a_peer['import']}, {'camera-b', 'audio-1'})
        self.assertEqual({s['service'] for s in b_peer['export']}, {'camera-b', 'audio-1'})
        self.assertEqual(a_peer['evidence_path'], model.state_path('a') + '/remote-evidence-b')
        previous = original.commission('b', pair, peers, scene['run_id'], scene,
            'http://10.0.0.3:8091', '/protected/read-token')
        self.assertFalse(any(s['component'] == model.CAPTURE for s in previous['replication']['peers'][0]['import']))


if __name__ == '__main__': unittest.main()

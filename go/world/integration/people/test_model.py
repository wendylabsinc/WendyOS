import base64
import copy
import unittest

from integration.people import model


def pair_fixture():
    # Structural public metadata only. No signing or deployment takes place.
    tenant = '10000000-0000-4000-8000-000000000000'
    pair_id = '20000000-0000-4000-8000-000000000000'
    root, issuer = b'public-root-test-only', b'public-issuer-test-only'
    pair = {'kind': 'UNTRUSTED-PRIVATE-AGENT-PAIR-FIXTURE-v1', 'pair_id': pair_id, 'tenant': tenant,
            'root_sha256': model.digest(root), 'issuer_sha256': model.digest(issuer), 'nodes': []}
    peers = {key: value for key, value in pair.items() if key != 'nodes'}
    peers.update(authority_time='fixture-host-wall-time-only', nodes=[])
    for index, node in enumerate(('a', 'b')):
        device = f'30000000-0000-4000-8000-00000000000{index}'
        public = {'kind': 'UNTRUSTED-PRIVATE-AGENT-VM-FIXTURE-v1', 'pair_id': pair_id, 'tenant': tenant,
                  'device_id': device, 'principal': 'spiffe://wendy.sh/tenant/' + tenant + '/device/' + device,
                  'leaf_serial': index + 3, 'root_der': base64.b64encode(root).decode(),
                  'issuer_der': base64.b64encode(issuer).decode(), 'root_sha256': pair['root_sha256'],
                  'issuer_sha256': pair['issuer_sha256'], 'config_path': model.config_path(node) + '/world.json',
                  'simulation_clock_path': model.config_path(node) + '/simclock.json',
                  'not_before': '2026-11-12T00:00:00Z', 'not_after': '2026-11-12T01:00:00Z'}
        pair['nodes'].append({'directory': model.config_path(node), 'state_directory': model.state_path(node),
                              'public': public, 'public_sha256': model.digest(model.json_bytes(public))})
        peers['nodes'].append({'device_id': device, 'principal': public['principal'], 'leaf_serial': index + 3,
                               'leaf_sha256': str(index + 1) * 64, 'device_spki_sha256': str(index + 3) * 64,
                               'issued_receipt_sha256': str(index + 5) * 64})
    return pair, peers


def identity_fixture():
    return {'schema_version': 'wendy.sim.clock.v1', 'run_id': 'people-test', 'boot_id': 'observed-boot',
            'package_sha256': 'a' * 64, 'frame': {'id': 'people-annex-floor', 'revision': 'a' * 16},
            'clock_domain': 'simulation:people-test', 'anchor_unix_ns': model.ANCHOR,
            'site': 'studio-01', 'scenario_version': 'people-annex-v1', 'seed': 7}


class ModelTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.peers = pair_fixture()
        self.scene = identity_fixture()
        self.run_id = self.scene['run_id']
        self.url = 'http://10.47.0.5:8091'
        self.token = model.config_path('a') + '/scene-read-token'
        self.pose_model = {'model_id': 'movenet-multipose-lightning', 'model_version': '1', 'model_sha256': 'b' * 64}

    def commission(self, node):
        kwargs = {'scene_url': self.url, 'read_token_file': self.token} if node == 'a' else {}
        return model.commission(node, self.pair, self.peers, self.run_id, self.scene, **kwargs)

    def test_only_origin_track_result_crosses_agent_boundary(self):
        a, b = self.commission('a'), self.commission('b')
        pa, pb = a['replication']['peers'][0], b['replication']['peers'][0]
        self.assertEqual(pa['import'], [])
        self.assertEqual(pb['export'], [])
        self.assertEqual(pa['export'], pb['import'])
        self.assertEqual(pa['export'], [{'tenant': self.pair['tenant'], 'site': 'studio-01',
            'device': self.pair['nodes'][0]['public']['principal'], 'app': model.APP,
            'service': 'people', 'component': 'wendy.inference.result.v1', 'stream': 'tracks'}])
        for index, cfg in enumerate((a, b)):
            peer = cfg['replication']['peers'][0]
            self.assertEqual(peer['credential_sha256'], self.peers['nodes'][1-index]['leaf_sha256'])
            self.assertEqual(peer['device'], self.pair['nodes'][1-index]['public']['principal'])
            self.assertEqual(peer['generation'], 1)
            paths = [cfg['journal_path'], cfg['replication']['authority_path'], peer['remote_path']]
            self.assertEqual(len(set(paths)), 3)
        self.assertEqual([g['service'] for g in a['policy']['grants']], ['camera-a', 'camera-b', 'people'])
        self.assertEqual([g['service'] for g in b['policy']['grants']], ['reader'])
        self.assertNotIn('publish', b['policy']['grants'][0])
        self.assertEqual(b['policy']['grants'][0]['subscribe'][0]['device'], pa['export'][0]['device'])

    def test_http_origin_and_file_receiver_keep_same_scene_clock(self):
        a, b = self.commission('a'), self.commission('b')
        for cfg in (a, b):
            self.assertEqual(cfg['policy']['sample_clock_domain'], 'simulation:' + self.run_id)
            self.assertEqual(cfg['policy']['frames'], [self.scene['frame']])
            self.assertEqual(cfg['policy']['site'], self.scene['site'])
            self.assertEqual(cfg['limits'], {'MessagesPerSecond': 200, 'MessageBurst': 400,
                                             'BytesPerSecond': 8 << 20, 'ByteBurst': 16 << 20})
        self.assertNotIn('simulation_clock_file', a)
        self.assertEqual(a['simulation_clock'], {'url': self.url, 'read_token_file': self.token,
            'run_id': self.run_id, 'boot_id': self.scene['boot_id'], 'package_sha256': self.scene['package_sha256'],
            'frame': self.scene['frame'], 'anchor_unix_ns': model.ANCHOR})
        self.assertIn('proof_archive_path', a)
        self.assertIn('evidence_directory', a)
        self.assertEqual(b['simulation_clock_file'], self.pair['nodes'][1]['public']['simulation_clock_path'])
        for field in ('simulation_clock', 'capture_source', 'evidence_directory', 'proof_archive_path'):
            self.assertNotIn(field, b)

    def test_app_entitlements_match_commission_without_scene_world_access(self):
        names = set()
        for node in ('a', 'b'):
            app, cfg = model.app_manifest(self.pair, self.run_id, node), self.commission(node)
            self.assertEqual(app['isolation'], 'isolated')
            self.assertEqual(tuple(app['services']), model.SERVICES[node])
            grants = {g['service']: g for g in cfg['policy']['grants']}
            for service, spec in app['services'].items():
                self.assertEqual(spec['context'], 'services/' + service)
                self.assertNotIn('dependsOn', spec)
                persists = [e for e in spec['entitlements'] if e['type'] == 'persist']
                self.assertEqual([e['path'] for e in persists], ['/config', '/proofs'])
                for volume in persists:
                    self.assertNotIn(volume['name'], names)
                    names.add(volume['name'])
                    self.assertTrue(volume['name'].startswith('world-people-' + self.run_id + '-' + node + '-'))
                world = [e for e in spec['entitlements'] if e['type'] == 'world']
                if service == 'scene':
                    self.assertEqual(world, [])
                    continue
                self.assertEqual(len(world), 1)
                for action in ('publish', 'subscribe'):
                    rows = [{('component_type' if k == 'componentType' else k): v for k, v in row.items()}
                            for row in world[0].get(action, [])]
                    self.assertEqual(rows, grants[service].get(action, []))

    def test_capture_and_people_config_preserve_observed_metadata(self):
        people = model.people_config(self.pair, self.run_id, self.scene, self.pose_model, 19)
        self.assertEqual(people['expected_generation'], 19)
        self.assertEqual(people['scene_boot_id'], self.scene['boot_id'])
        self.assertEqual(people['package_sha256'], self.scene['package_sha256'])
        self.assertEqual(people['model_sha256'], self.pose_model['model_sha256'])
        self.assertEqual(people['ankle_height_m'], .08)
        self.assertEqual(people['request_timeout_ms'], 3000)
        self.assertEqual(people['sources'], [{'sensor_id': c, 'app': model.APP, 'service': c} for c in model.CAMERAS])
        for camera in model.CAMERAS:
            config = model.camera_config(camera, self.run_id, self.scene, self.url)
            for key in ('run_id', 'site', 'frame', 'simulation_anchor_unix_ns'):
                self.assertEqual(config[key], people[key])
            self.assertEqual(config['sensor_id'], camera)
            self.assertEqual(config['ttl_ms'], 3000)
            self.assertEqual(config['request_timeout_ms'], 1500)
            self.assertEqual(config['read_token_file'], '/config/scene-read-token')
        self.assertNotIn('expected_generation', model.people_config(self.pair, self.run_id, self.scene, self.pose_model))

    def test_reader_has_origin_device_and_no_image_or_model_inputs(self):
        reader = model.reader_config(self.pair, self.run_id, self.scene, 5)
        self.assertEqual(reader['expected_generation'], 5)
        self.assertEqual(reader['request_timeout_ms'], 3000)
        self.assertEqual(reader['device'], self.pair['nodes'][1]['public']['principal'])
        self.assertEqual(reader['source'], {'device': self.pair['nodes'][0]['public']['principal'],
            'app': model.APP, 'service': 'people', 'stream': 'tracks'})
        for key in ('read_token_file', 'scene_url', 'model_sha256', 'sources', 'publish'):
            self.assertNotIn(key, reader)

    def test_scene_mismatch_refuses_instead_of_relabeling_samples(self):
        mutations = [('run_id', 'different'), ('clock_domain', 'simulation:agentpair:people-test'),
                     ('anchor_unix_ns', model.ANCHOR + 1), ('anchor_unix_ns', True),
                     ('package_sha256', 'wrong'), ('site', 'office'), ('seed', True),
                     ('scenario_version', 'other'), ('frame', {'id': 'floor'}), ('boot_id', '')]
        for field, value in mutations:
            bad = copy.deepcopy(self.scene)
            bad[field] = value
            with self.subTest(field=field), self.assertRaises(model.Refused):
                model.camera_config('camera-a', self.run_id, bad, self.url)
        with self.assertRaises(model.Refused):
            model.validate_scene_identity(self.run_id, {**self.scene, 'simulation_time_ns': 3})

    def test_pair_peer_and_service_mismatch_refuse(self):
        bad = copy.deepcopy(self.peers)
        bad['nodes'][1]['principal'] = bad['nodes'][0]['principal']
        with self.assertRaises(model.Refused):
            model.commission('a', self.pair, bad, self.run_id, self.scene, self.url, self.token)
        for node, service in (('b', 'camera-a'), ('a', 'reader'), ('c', 'scene')):
            with self.subTest(node=node, service=service), self.assertRaises(model.Refused):
                model.volume_names(self.run_id, node, service)
        with self.assertRaises(model.Refused):
            model.commission('b', self.pair, self.peers, self.run_id, self.scene, self.url, self.token)

    def test_model_generation_and_url_validation(self):
        for change in ({'model_sha256': 'bad'}, {'model_id': ''}, {'appearance': 'not accepted'}):
            with self.subTest(change=change), self.assertRaises(model.Refused):
                model.people_config(self.pair, self.run_id, self.scene, {**self.pose_model, **change})
        for generation in (True, -1, 1 << 64):
            with self.subTest(generation=generation), self.assertRaises(model.Refused):
                model.reader_config(self.pair, self.run_id, self.scene, generation)
        for url in ('https://10.0.0.2:8091', 'http://u:p@10.0.0.2:8091', self.url + '/v1/clock', self.url + '?x=1'):
            with self.subTest(url=url), self.assertRaises(model.Refused):
                model.camera_config('camera-a', self.run_id, self.scene, url)

    def test_configs_are_owned_public_values_and_deterministic(self):
        before = model.json_bytes([self.pair, self.peers, self.scene, self.pose_model])
        a = self.commission('a')
        a['policy']['frames'][0]['revision'] = 'changed-output'
        model.people_config(self.pair, self.run_id, self.scene, self.pose_model)['frame']['id'] = 'changed-output'
        self.assertEqual(before, model.json_bytes([self.pair, self.peers, self.scene, self.pose_model]))
        self.assertEqual(model.json_bytes(self.commission('a')), model.json_bytes(self.commission('a')))
        raw = model.json_bytes([self.commission('a'), self.commission('b'),
            model.people_config(self.pair, self.run_id, self.scene, self.pose_model),
            model.reader_config(self.pair, self.run_id, self.scene)])
        for field in (b'private_key', b'issuer_key', b'eab_hmac', b'control_token', b'certificate_chain'):
            self.assertNotIn(field, raw)


if __name__ == '__main__':
    unittest.main()

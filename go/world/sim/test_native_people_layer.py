"""Unsigned structural fixtures; ordinary reader crypto remains outside this adapter."""
import base64
from copy import deepcopy
import json
import unittest

from sim.native_replica_bridge import NativeReplicaBridge, digest, timestamp
from sim.macos_demo_bridge import COMMAND_SCHEMA
from sim.test_native_replica_bridge import Clock, live_fixture, signed, encoded, instant
from sim.native_people_layer import ALGORITHM, LIFETIME_NS, NativePeopleLayer

PHYSICAL = 1_794_484_800_000_000_000


def pack(record, payload, refs):
    record['publication']['payload'] = encoded(json.dumps({'version': 1,
        'result_schema': 'wendy.person.track.v1', 'result_json': encoded(json.dumps(payload).encode()),
        'inputs': refs, 'retain_until_unix_ms': 1794484860000}).encode())
    raw, original = signed(record)
    return {'envelope_json': encoded(raw), 'envelope_sha256': digest(raw), 'record_sha256': digest(original),
            'version': {key: record[key] for key in ('epoch', 'sequence')}, 'payload': payload}


def people_fixture():
    config, presentation, hazard = live_fixture()
    source = {**config['source'], 'service': 'people', 'stream': 'tracks'}
    config['people'] = {'source': source, 'reader_generation': 12, 'reader_boot_id': 'people-reader-boot',
        'input_sources': {name: {**source, 'service': name, 'stream': name}
                          for name in ('people-front', 'people-oblique')}}
    presentation['sensor_sources'] += [{'id': name, 'kind': 'camera', 'enabled': True}
                                       for name in config['people']['input_sources']]
    hazard['presentation'] = deepcopy(presentation)
    refs, members, leaves = [], [], []
    for index, (sensor, member_source) in enumerate(config['people']['input_sources'].items()):
        ref = {'key': {'tenant': config['tenant'], 'site': config['site'], 'entity': 'sensor/'+sensor,
                       'type': 'wendy.sensor.capture.v1', 'source': member_source},
               'version': {'epoch': 1, 'sequence': 1}, 'envelope_sha256': str(index+1)*64,
               'record_sha256': str(index+3)*64}
        refs.append(ref)
        members.append({'proof': ref, 'sensor_id': sensor, 'sample_id': config['run_id']+'/'+sensor+'/1',
            'capture_sequence': 1, 'member_index': 0, 'sample_time': instant(1_000_000_000),
            'valid_until': instant(2_250_000_000), 'physical_until': '2026-11-12T12:00:05Z',
            'position_xz': [1., 2.], 'covariance': [.1, 0, 0, .2],
            'image': {'sha256': str(index+5)*64, 'size': 1234, 'content_type': 'image/png'},
            'model_id': 'google/movenet/multipose/lightning', 'model_version': 'fixture-v1',
            'model_sha256': '7'*64, 'calibration_revision': 'shared-office-cameras-v1',
            'calibration_sha256': '8'*64, 'scene_boot_id': 'scene-boot',
            'captured_unix_ns': PHYSICAL, 'simulation_anchor_unix_ns': config['anchor_unix_ns']})
        leaves.append({'id': 'sha256:'+ref['envelope_sha256'], 'origin': 'simulated',
                       'simulation': {'run_id': config['run_id'], 'clock_domain': config['clock_domain'],
                                      'scenario_version': 'shared-office-perception-v1', 'seed': 7}})
    payload = {'schema': 'wendy.person.track.v1', 'algorithm': ALGORITHM, 'algorithm_version': '1',
        'track_id': 'anonymous-1', 'revision': 1, 'run_id': config['run_id'],
        'clock_domain': config['clock_domain'], 'frame': config['frame'],
        'observed_at': instant(1_000_000_000), 'valid_until': instant(2_250_000_000),
        'position_xz': [1., 2.], 'covariance': [.1, 0, 0, .2], 'ambiguous': False,
        'motion_known': False, 'current': members}
    record = {'epoch': 4, 'sequence': 1, 'issued_at_unix_ms': PHYSICAL//1_000_000,
        'expires_at_unix_ms': PHYSICAL//1_000_000+60_000,
        'scope': {'tenant': config['tenant'], 'site': config['site'], 'execution_domain': 'simulation',
                  **{key: source[key] for key in ('device', 'app', 'service')}},
        'publication': {'entity': config['run_id']+'/track/anonymous-1',
            'component_type': 'wendy.inference.result.v1', 'stream': 'tracks', 'frame': config['frame'],
            'tombstone': False, 'ttl_ns': LIFETIME_NS,
            'timing': {'sample_time': instant(1_000_000_000), 'clock_domain': config['clock_domain'],
                       'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0},
            'provenance': {'origin': 'inferred', 'app_version': 'world-live-people-v1',
                           'model_id': ALGORITHM, 'model_version': '1', 'inputs': leaves}}}
    row = pack(record, payload, refs)
    reader = {'version': 1, 'run_id': config['run_id'], 'tick': 1, 'reader_generation': 12,
        'sample_time': instant(1_000_000_000), 'status': 'ready', 'tracks': [row],
        'origin_attested_inference': True, 'camera_inputs_independently_verified': False,
        'clock': {**hazard['clock'], 'server_boot_id': 'people-reader-boot', 'deployment_generation': 12}}
    update = {'version': 1, 'run_id': config['run_id'], 'sequence': 1,
              'collection_started_monotonic_ns': 9_900_000_000, 'reader': reader, 'presentation': deepcopy(presentation)}
    return config, presentation, hazard, update


def rewrite(update, change):
    row = update['reader']['tracks'][0]
    record = json.loads(base64.b64decode(json.loads(base64.b64decode(row['envelope_json']))['record']))
    generic = json.loads(base64.b64decode(record['publication']['payload']))
    payload = deepcopy(row['payload'])
    change(record, payload, generic['inputs'])
    update['reader']['tracks'][0] = pack(record, payload, generic['inputs'])


class NativePeopleLayerTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.config, self.presentation, self.hazard, self.update = people_fixture()
        self.bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)

    def tracks(self):
        return self.bridge.export()['snapshot']['tracking']['tracks']

    def next_tick(self):
        self.update['sequence'] += 1
        self.update['reader']['tick'] += 1
        self.update['collection_started_monotonic_ns'] = self.clock.now

    def test_track_evidence_preserves_each_original_camera_and_result_binding(self):
        def equal_images(record, payload, refs):
            payload['current'][1]['image'] = deepcopy(payload['current'][0]['image'])
        rewrite(self.update, equal_images)
        original = deepcopy(self.update)
        self.bridge.ingest_people(self.update)
        retained = deepcopy(self.bridge.people.latest)
        track = self.tracks()[0]
        self.assertEqual(track['original_envelope_sha256'], self.update['reader']['tracks'][0]['envelope_sha256'])
        descriptors = track['native_evidence']
        members = self.update['reader']['tracks'][0]['payload']['current']
        self.assertEqual(len(descriptors), 2)
        for descriptor, member in zip(descriptors, members):
            self.assertEqual(descriptor, {'label': member['sensor_id'], 'role': 'active',
                'source': {**member['proof']['key']['source'], 'component_type': 'wendy.sensor.capture.v1'},
                **member['image'], 'proof_envelope_sha256': member['proof']['envelope_sha256'],
                'sample_time': member['sample_time']})
        self.assertEqual(descriptors[0]['sha256'], descriptors[1]['sha256'])
        self.assertNotEqual(descriptors[0]['source'], descriptors[1]['source'])
        self.assertNotEqual(descriptors[0]['proof_envelope_sha256'], descriptors[1]['proof_envelope_sha256'])
        descriptors[0]['source']['device'] = 'changed-output-copy'
        self.assertEqual(self.bridge.people.latest, retained)
        self.assertEqual(self.update, original)
        self.clock.now = track['display_deadline_monotonic_ns']
        self.assertEqual(self.tracks(), [])
        self.assertEqual(self.bridge.people.latest['started'], retained['started'])

    def test_combined_catalog_preserves_hazard_and_original_people_geometry(self):
        self.bridge.ingest(self.hazard)
        original_hazard, original_time = deepcopy(self.bridge.latest), self.bridge.sample_ns
        original_update = deepcopy(self.update)
        self.assertTrue(self.bridge.ingest_people(self.update))
        state = self.bridge.export()['snapshot']
        self.assertEqual(len(state['sensor_sources']), 7)
        self.assertEqual(state['fusion']['state'], 'hazard')
        self.assertEqual(self.bridge.latest, original_hazard)
        self.assertEqual(self.bridge.sample_ns, original_time)
        self.assertEqual(self.update, original_update)
        track = state['tracking']['tracks'][0]
        payload = self.update['reader']['tracks'][0]['payload']
        self.assertEqual(track['last_observed'], payload['observed_at'])
        self.assertEqual(track['valid_until'], payload['valid_until'])
        self.assertEqual(track['position_xz'], payload['position_xz'])
        self.assertEqual(track['original_envelope_sha256'], self.update['reader']['tracks'][0]['envelope_sha256'])
        self.assertEqual(track['display_deadline_monotonic_ns'], 10_900_000_000)
        self.assertNotIn('velocity_xz', track)
        self.assertEqual(state['tracking']['cameras'], [])
        self.assertEqual({row['id'] for row in state['tracking']['camera_sources']}, {'people-front', 'people-oblique'})

    def test_original_physical_deadline_and_duplicate_reads_do_not_renew(self):
        self.update['reader']['clock'].update(physical_earliest='2026-11-12T12:00:04.700Z',
                                             physical_latest='2026-11-12T12:00:04.800Z')
        self.bridge.ingest_people(self.update)
        deadline = self.tracks()[0]['display_deadline_monotonic_ns']
        self.assertEqual(deadline, 10_100_000_000)
        self.clock.now += 50_000_000
        self.update['sequence'] += 1
        self.update['collection_started_monotonic_ns'] = self.clock.now
        self.assertFalse(self.bridge.ingest_people(self.update))
        self.assertEqual(self.tracks()[0]['display_deadline_monotonic_ns'], deadline)
        self.clock.now = deadline
        self.assertEqual(self.tracks(), [])

    def test_paused_sample_clock_does_not_advance_from_host_elapsed_time(self):
        self.update['reader']['sample_time'] = instant(2_200_000_000)
        self.update['reader']['clock']['sample_time'] = instant(2_200_000_000)
        self.update['presentation']['simulation_time_ns'] = 2_200_000_000
        self.bridge.ingest_people(self.update)
        self.clock.now += 200_000_000  # More than the 50ms sample remainder; simulation stays paused.
        self.assertEqual(len(self.tracks()), 1)
        self.bridge.sample_ns = 2_250_000_000  # A later known source sample expires it independently.
        self.assertEqual(self.tracks(), [])

    def test_newer_people_presentation_then_older_hazard_never_rewinds_or_refreshes(self):
        self.bridge.ingest(self.hazard)
        old_hazard = deepcopy(self.bridge.latest)
        self.update['reader']['sample_time'] = instant(2_000_000_000)
        self.update['reader']['clock']['sample_time'] = instant(2_000_000_000)
        self.update['presentation'].update(simulation_time_ns=2_000_000_000,
            dynamic_geoms=[{'id': 7, 'position': [2, 0, 0], 'rotation': [1, 0, 0, 0, 1, 0, 0, 0, 1]}])
        self.bridge.ingest_people(self.update)
        self.assertEqual(self.bridge.sample_ns, 2_000_000_000)
        self.assertEqual(self.bridge.latest, old_hazard)
        people_deadline = self.tracks()[0]['display_deadline_monotonic_ns']
        self.hazard['sequence'] += 1; self.hazard['reader']['tick'] += 1
        self.bridge.ingest(self.hazard)  # Still an original 1s read, arriving later.
        state = self.bridge.export()
        self.assertEqual(state['snapshot']['time_s'], 2)
        self.assertEqual(state['dynamic_geoms'], self.update['presentation']['dynamic_geoms'])
        self.assertEqual(self.tracks()[0]['display_deadline_monotonic_ns'], people_deadline)
        self.next_tick(); self.update['reader']['tracks'] = []
        self.update['reader']['sample_time'] = instant(4_100_000_000)
        self.update['reader']['clock']['sample_time'] = instant(4_100_000_000)
        self.update['presentation']['simulation_time_ns'] = 4_100_000_000
        self.bridge.ingest_people(self.update)
        self.assertEqual(self.bridge.export()['snapshot']['fusion']['state'], 'expired')
        self.assertEqual(self.bridge.latest['started'], old_hazard['started'])

    def test_motion_uses_exact_previous_proofs_and_shortest_original_physical_bound(self):
        def motion(record, payload, refs):
            previous = deepcopy(payload['current'])
            for index, member in enumerate(previous):
                member.update(sample_time=instant(800_000_000), valid_until=instant(2_050_000_000),
                              physical_until='2026-11-12T12:00:00.500Z')
                member['proof']['envelope_sha256'] = ('a' if index == 0 else 'b')*64
                refs.append(deepcopy(member['proof']))
                leaf = deepcopy(record['publication']['provenance']['inputs'][index])
                leaf['id'] = 'sha256:'+member['proof']['envelope_sha256']
                record['publication']['provenance']['inputs'].append(leaf)
            payload.update(previous=previous, motion_known=True, previous_observed_at=instant(800_000_000),
                           velocity_xz=[.3, -.1], velocity_covariance=[.1, 0, 0, .2],
                           valid_until=instant(2_050_000_000))
            record['publication']['timing']['sample_time'] = instant(800_000_000)
        rewrite(self.update, motion)
        self.bridge.ingest_people(self.update)
        track = self.tracks()[0]
        self.assertEqual(track['velocity_xz'], [.3, -.1])
        self.assertEqual([ref['role'] for ref in track['native_evidence']], ['active', 'active', 'previous', 'previous'])
        self.assertEqual([ref['sample_time'] for ref in track['native_evidence']],
                         [instant(1_000_000_000)]*2 + [instant(800_000_000)]*2)
        self.assertEqual([ref['proof_envelope_sha256'] for ref in track['native_evidence']],
                         ['1'*64, '2'*64, 'a'*64, 'b'*64])
        self.assertEqual(track['display_deadline_monotonic_ns'], 10_390_000_000)
        self.clock.now = track['display_deadline_monotonic_ns']
        self.assertEqual(self.tracks(), [])

    def test_people_expire_independently_and_empty_waiting_failure_never_clear_hazard(self):
        self.bridge.ingest_people(self.update)
        self.clock.now += 900_000_000
        self.hazard['collection_started_monotonic_ns'] = self.clock.now
        self.bridge.ingest(self.hazard)
        self.assertEqual(self.tracks(), [])
        self.assertEqual(self.bridge.export()['snapshot']['fusion']['state'], 'hazard')
        self.next_tick(); self.update['reader']['tracks'] = []
        self.bridge.ingest_people(self.update)
        self.assertEqual(self.tracks(), [])
        self.next_tick(); self.update['reader'].update(status='failed', origin_attested_inference=False)
        self.update['reader'].pop('clock')
        self.bridge.ingest_people(self.update)
        self.assertEqual(self.bridge.export()['snapshot']['fusion']['state'], 'hazard')
        waiting = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)
        self.update['reader'].update(status='waiting_for_source', startup_attempt=1)
        waiting.ingest_people(self.update)
        self.assertEqual(waiting.export()['snapshot']['tracking']['tracks'], [])

    def test_fresh_new_originals_recover_after_sample_expiry(self):
        self.bridge.ingest_people(self.update)
        self.bridge.sample_ns = 3_000_000_000
        self.assertEqual(self.tracks(), [])
        self.next_tick()
        self.update['reader']['sample_time'] = instant(3_000_000_000)
        self.update['reader']['clock']['sample_time'] = instant(3_000_000_000)
        self.update['presentation']['simulation_time_ns'] = 3_000_000_000
        def newer(record, payload, refs):
            record['sequence'] += 1
            record['publication']['timing']['sample_time'] = instant(3_000_000_000)
            payload.update(observed_at=instant(3_000_000_000), valid_until=instant(4_250_000_000), revision=2)
            for member, ref in zip(payload['current'], refs):
                ref['version']['sequence'] += 1
                member['proof'] = deepcopy(ref)
                member.update(sample_time=payload['observed_at'], valid_until=payload['valid_until'], capture_sequence=2)
        rewrite(self.update, newer)
        self.assertTrue(self.bridge.ingest_people(self.update))
        self.assertEqual(len(self.tracks()), 1)
        self.assertEqual(self.tracks()[0]['last_observed'], instant(3_000_000_000))

    def test_scope_clock_original_payload_and_member_mismatches_refuse_people_only(self):
        changes = [lambda u: u['reader'].update(reader_generation=13),
            lambda u: u['reader']['clock'].update(server_boot_id='other'),
            lambda u: u['reader']['tracks'][0]['payload'].update(position_xz=[7, 8]),
            lambda u: rewrite(u, lambda r, p, refs: r['scope'].update(device='other')),
            lambda u: rewrite(u, lambda r, p, refs: p['current'][0]['proof']['key']['source'].update(device='other')),
            lambda u: rewrite(u, lambda r, p, refs: p['current'][0].update(physical_until='2026-11-12T12:00:06Z')),
            lambda u: rewrite(u, lambda r, p, refs: refs.reverse()),
            lambda u: rewrite(u, lambda r, p, refs: r['publication'].update(entity='wrong')),
            lambda u: u['presentation'].update(simulation_time_ns=2_000_000_000)]
        for change in changes:
            with self.subTest(change=change):
                config, presentation, hazard, update = people_fixture()
                bridge = NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
                bridge.ingest(hazard); bridge.ingest_people(update)
                update['sequence'] += 1; update['reader']['tick'] += 1
                change(update)
                with self.assertRaises(ValueError): bridge.ingest_people(update)
                state = bridge.export()['snapshot']
                self.assertEqual(state['tracking']['tracks'], [])
                self.assertEqual(state['fusion']['state'], 'hazard')

    def test_people_toggle_uses_existing_seven_source_request_without_optimistic_change(self):
        self.bridge.ingest(self.hazard); self.bridge.ingest_people(self.update)
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'people-front'}
        request = self.bridge.begin_source_step(command, 2_000_000_000)
        self.assertEqual(set(request['sensors']), set(self.bridge.source_catalog)-{'people-front'})
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])
        self.assertTrue(all(s['enabled'] for s in self.bridge.export()['snapshot']['sensor_sources']))
        self.assertEqual(len(self.tracks()), 1)
        self.bridge.finish_source_step({'sequence': 1, **request})
        self.assertFalse(self.bridge.ingest_people(self.update))

    def test_shared_frame_is_optional_and_fixed_native_anchor_is_explicit(self):
        config, presentation = deepcopy(self.config), deepcopy(self.presentation)
        config['frame']['revision'] = 'shared-office-'+config['model_sha256'][:16]
        bridge = NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
        self.assertIn('tracking', bridge.export()['snapshot'])
        config['anchor_unix_ns'] += 1
        with self.assertRaises(ValueError): NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
        config, presentation, _ = live_fixture()
        config['frame']['revision'] = 'shared-office-'+config['model_sha256'][:16]
        with self.assertRaises(ValueError): NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)




class ContinuousPeopleTests(unittest.TestCase):
    def test_people_clock_is_independent_and_presentations_never_renew_tracks(self):
        config, presentation, hazard, update = people_fixture()
        config['presentation_mode'] = 'continuous'
        clock = Clock()
        bridge = NativeReplicaBridge(config, presentation, monotonic_ns=clock)
        update['presentation']['simulation_time_ns'] -= 100_000_000
        update['reader']['clock']['sample_time'] = instant(2_100_000_000)
        before = deepcopy(update)
        self.assertTrue(bridge.ingest_people(update))
        original = deepcopy(bridge.people.latest)
        tracks = bridge.export()['snapshot']['tracking']['tracks']
        self.assertEqual(len(tracks), 1)
        self.assertEqual(tracks[0]['display_deadline_monotonic_ns'],
                         update['collection_started_monotonic_ns']+150_000_000)
        later = deepcopy(presentation); later['simulation_time_ns'] += 50_000_000
        bridge.present(later)
        self.assertEqual(bridge.people.latest, original)
        self.assertEqual(update, before)
        clock.now = tracks[0]['display_deadline_monotonic_ns']
        self.assertEqual(bridge.export()['snapshot']['tracking']['tracks'], [])
        self.assertEqual(bridge.people.sample, timestamp(update['reader']['clock']['sample_time']))

    def test_queued_precommand_readers_cannot_satisfy_actual_ack_minimum(self):
        config, presentation, hazard, people = people_fixture()
        config['presentation_mode'] = 'continuous'
        bridge = NativeReplicaBridge(config, presentation, monotonic_ns=Clock())
        bridge.ingest(hazard); bridge.ingest_people(people)
        command = dict(schema=COMMAND_SCHEMA, run_id=config['run_id'], sequence=1,
                       action='toggle_sensor', target='camera-b')
        request = bridge.begin_source_step(command, 1_000_000_001)
        response = {**request, 'sequence': 15, 'simulation_time_ns': 1_200_000_000}
        self.assertTrue(bridge.finish_source_step(response)['accepted'])
        presentation['simulation_time_ns'] = response['simulation_time_ns']
        for update in (hazard, people):
            update['sequence'] += 1; update['reader']['tick'] += 1
            update['presentation'] = deepcopy(presentation)
        old_people = deepcopy(bridge.people.latest)
        self.assertFalse(bridge.ingest(hazard))
        self.assertFalse(bridge.ingest_people(people))
        self.assertIsNone(bridge.latest)
        self.assertEqual(bridge.people.latest, old_people)
        self.assertFalse(bridge.export()['snapshot']['controls_available'])
        # A genuinely later ordinary read can still attest the earlier live
        # immutable result. The result's capture/validity fields are unchanged.
        for update in (hazard, people):
            update['reader']['sample_time'] = instant(response['simulation_time_ns'])
            update.get('clock', update['reader'].get('clock'))['sample_time'] = update['reader']['sample_time']
        self.assertTrue(bridge.ingest(hazard))
        self.assertTrue(bridge.ingest_people(people))
        self.assertTrue(bridge.export()['snapshot']['controls_available'])

    def test_actual_later_presentation_expires_tracks_without_changing_original_time(self):
        config, presentation, hazard, update = people_fixture()
        config['presentation_mode'] = 'continuous'
        bridge = NativeReplicaBridge(config, presentation, monotonic_ns=Clock())
        bridge.ingest_people(update)
        original = deepcopy(bridge.people.latest)
        later = deepcopy(presentation); later['simulation_time_ns'] = 2_250_000_000
        bridge.present(later)
        self.assertEqual(bridge.export()['snapshot']['tracking']['tracks'], [])
        self.assertEqual(bridge.people.latest, original)


if __name__ == '__main__':
    unittest.main()

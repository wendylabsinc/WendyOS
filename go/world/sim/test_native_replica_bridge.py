"""Live display adapter checks with unsigned receipt fixtures, no VM or window."""
import base64
from copy import deepcopy
import json
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

from sim.native_replica_bridge import (NativeReplicaBridge, MAX_DISPLAY_AGE_NS, timestamp, digest,
                                       hazard_native_evidence)
from sim.macos_demo_bridge import SessionFiles, COMMAND_SCHEMA
from integration.remotemediafusion.test_proof import fixture, rewrite_hazard, signed, encoded, instant


class Clock:
    def __init__(self): self.now = 10_000_000_000
    def __call__(self): return self.now


def live_fixture():
    reader, origins, receipts, runner, ns = fixture()
    args = (reader, origins, receipts, runner, ns)
    rewrite_hazard(args, lambda h: h.update(confidence=.8, uncertainty_m=.2, gas_value_ppm=90,
        cameras=[{'sensor_id': name, 'red_fraction': .1} for name in ('camera-a', 'camera-b')],
        audio={'tone_fraction': .9}))
    row = reader['hazards'][0]
    record = json.loads(base64.b64decode(json.loads(base64.b64decode(row['envelope_json']))['record']))
    record.update(issued_at_unix_ms=1794484800000, expires_at_unix_ms=1794484860000)
    record['publication']['timing'].update(error_known=True, error_bound_ns=0, age_hint_ns=0)
    raw, inner = signed(record)
    row.update(envelope_json=encoded(raw), envelope_sha256=digest(raw), record_sha256=digest(inner))
    pub = next(iter(origins.values()))['publication']
    scene = runner.scene
    config = {'version': 1, 'run_id': scene['run_id'], 'tenant': runner.pair['tenant'], 'site': scene['site'],
              'frame': scene['frame'], 'model_sha256': 'b' * 64, 'anchor_unix_ns': 1767225600000000000,
              'clock_domain': scene['clock_domain'], 'reader_generation': 9, 'reader_boot_id': 'reader-boot',
              'replica_node': 'gateway-b', 'source': {**record['scope'], 'stream': 'hazard'},
              'input_sources': {i['sensor_id']: i['source'] for i in row['payload']['inputs']},
              'device_labels': {runner.pair['nodes'][0]['public']['principal']: 'Gateway A',
                                runner.pair['nodes'][1]['public']['principal']: 'Gateway B'}}
    config['source'] = {k: config['source'][k] for k in ('device', 'app', 'service', 'stream')}
    geometry = {'schema': 'wendy.macos-demo.geometry.v1', 'run_id': scene['run_id'], 'model_sha256': 'b' * 64,
                'geoms': [], 'presentation': {'name': 'Live native office', 'subtitle': 'Simulation', 'labels': []}}
    presentation = {'geometry': geometry, 'simulation_time_ns': ns, 'dynamic_geoms': [],
                    'sensor_sources': [{'id': key, 'enabled': True} for key in config['input_sources']]}
    update = {'version': 1, 'run_id': scene['run_id'], 'sequence': 1, 'collection_started_monotonic_ns': 9_900_000_000,
              'reader': reader, 'presentation': presentation, 'clock': {'sample_time': instant(ns),
                  'sample_clock_domain': scene['clock_domain'], 'server_boot_id': 'reader-boot', 'deployment_generation': 9,
                  'physical_earliest': '2026-11-12T12:00:00Z', 'physical_latest': '2026-11-12T12:00:00.010Z'},
              'media': deepcopy(pub['media'])}
    return config, presentation, update


def restored(update, delta):
    """Construct a second unsigned observation with genuinely newer original times."""
    row = update['reader']['hazards'][0]
    record = json.loads(base64.b64decode(json.loads(base64.b64decode(row['envelope_json']))['record']))
    generic = json.loads(base64.b64decode(record['publication']['payload']))
    hazard = row['payload']
    for used, ref in zip(hazard['inputs'], generic['inputs']):
        for key in ('sample_time', 'capture_start', 'valid_until'):
            used[key] = instant(timestamp(used[key]) - 1767225600000000000 + delta)
        used['version']['sequence'] += 1
        ref['version'] = deepcopy(used['version'])
        if 'sample' in used:
            used['sample']['sample_ns'] += delta
    hazard['valid_until'] = instant(timestamp(hazard['valid_until']) - 1767225600000000000 + delta)
    record['publication']['timing']['sample_time'] = instant(timestamp(record['publication']['timing']['sample_time']) - 1767225600000000000 + delta)
    generic['result_json'] = encoded(json.dumps(hazard).encode())
    record['publication']['payload'] = encoded(json.dumps(generic).encode())
    record['sequence'] += 1
    raw, inner = signed(record)
    row.update(envelope_json=encoded(raw), envelope_sha256=digest(raw), record_sha256=digest(inner),
               version={'epoch': record['epoch'], 'sequence': record['sequence']})
    update['reader']['sample_time'] = instant(1_000_000_000 + delta)
    update['clock']['sample_time'] = update['reader']['sample_time']
    update['presentation']['simulation_time_ns'] = 1_000_000_000 + delta


class BridgeTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.config, self.presentation, self.update = live_fixture()
        self.bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)

    def view(self): return self.bridge.export()['snapshot']['fusion']
    def ingest(self): return self.bridge.ingest(self.update)
    def new_tick(self):
        self.update['sequence'] += 1
        self.update['reader']['tick'] += 1
        self.update['collection_started_monotonic_ns'] = self.clock.now

    def test_native_display_deadlines_do_not_renew_on_export(self):
        self.config['display_clock'] = 'mach_absolute_time_ns'
        bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)
        bridge.ingest(self.update)
        first = bridge.export()
        self.assertEqual(first['display_clock'], 'mach_absolute_time_ns')
        self.assertEqual(first['observation_deadline_monotonic_ns'],
                         self.update['collection_started_monotonic_ns'] + MAX_DISPLAY_AGE_NS)
        self.assertGreater(first['display_deadline_monotonic_ns'], self.clock.now)
        self.clock.now += 10_000_000
        second = bridge.export()
        self.assertEqual(first['display_deadline_monotonic_ns'], second['display_deadline_monotonic_ns'])
        self.assertEqual(first['observation_deadline_monotonic_ns'], second['observation_deadline_monotonic_ns'])
        self.clock.now = first['observation_deadline_monotonic_ns']
        expired = bridge.export()
        self.assertEqual(expired['display_deadline_monotonic_ns'], 0)
        self.assertEqual(expired['observation_deadline_monotonic_ns'], 0)

    def test_native_display_deadline_preserves_shorter_original_physical_cap(self):
        self.config['display_clock'] = 'mach_absolute_time_ns'
        until = self.update['reader']['hazards'][0]['payload']['physical_valid_until']
        # Place the existing clock upper bound 200ms before the original cap.
        physical = timestamp(until) - 200_000_000
        from datetime import datetime, timezone
        upper = datetime.fromtimestamp(physical // 10**9, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S')
        upper += '.%09dZ' % (physical % 10**9)
        self.update['clock'].update(physical_earliest=upper, physical_latest=upper)
        bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)
        bridge.ingest(self.update)
        current = bridge.export()
        self.assertEqual(current['display_deadline_monotonic_ns'],
                         self.update['collection_started_monotonic_ns'] + 200_000_000)
        self.assertLess(current['display_deadline_monotonic_ns'], current['observation_deadline_monotonic_ns'])

    def test_live_received_result_uses_original_geometry_lineage_and_labels(self):
        before = deepcopy(self.update)
        self.assertTrue(self.ingest())
        state = self.bridge.export(); view = state['snapshot']['fusion']
        self.assertEqual(self.update, before)
        self.assertEqual((state['mode'], state['replay']), ('live-native-replica', False))
        self.assertEqual(state['snapshot']['map_node'], 'gateway-b')
        self.assertEqual(view['position'], [1, -2, .7])
        self.assertIn('LIVE SIMULATION', view['basis'])
        self.assertEqual(view['reason'], 'Camera, LiDAR, gas and audio support this location.')
        self.assertNotIn('replay', view['reason'])
        camera = next(i for i in view['observations'] if i['id'] == 'camera-b')
        self.assertIn('Gateway B', camera['label'])
        self.assertEqual(camera['evidence_reference'], self.update['media'][2]['reference'])
        self.assertNotIn('delivery', camera)  # Result reader did not fetch raw inputs.
        self.assertEqual(state['snapshot']['robots'], [])

    def test_repeated_file_or_new_collection_of_same_reader_tick_never_refreshes(self):
        self.ingest(); self.clock.now += MAX_DISPLAY_AGE_NS
        self.assertEqual(self.view()['state'], 'unknown')
        self.update['sequence'] = 2; self.update['collection_started_monotonic_ns'] = self.clock.now
        self.assertFalse(self.ingest())
        self.assertEqual(self.view()['state'], 'unknown')
        self.new_tick(); self.assertTrue(self.ingest())
        self.assertEqual(self.view()['state'], 'hazard')

    def test_native_evidence_preserves_exact_owners_without_cached_media_or_clock_renewal(self):
        self.update['media'] = []
        original = deepcopy(self.update)
        self.ingest()
        retained = deepcopy(self.bridge.latest)
        view = self.view()
        refs = view['native_evidence']
        self.assertEqual(len(refs), 3)
        self.assertEqual(view['original_envelope_sha256'], self.update['reader']['hazards'][0]['envelope_sha256'])
        inputs = {i['sensor_id']: i for i in self.update['reader']['hazards'][0]['payload']['inputs']}
        for descriptor in refs:
            used = inputs[descriptor['source']['stream']]
            self.assertEqual(descriptor, {'label': used['sensor_id'], 'role': 'active',
                'source': {**used['source'], 'component_type': 'wendy.sensor.capture.v1'},
                **used['evidence'], 'proof_envelope_sha256': used['capture']['proof_sha256'],
                'sample_time': used['sample_time']})
        cameras = [row for row in refs if row['content_type'] == 'image/png']
        self.assertEqual(cameras[0]['sha256'], cameras[1]['sha256'])
        self.assertNotEqual(cameras[0]['source']['device'], cameras[1]['source']['device'])
        self.assertTrue(any(row['content_type'] == 'audio/wav' for row in refs))
        refs[0]['source']['device'] = 'changed-output-copy'
        self.assertEqual(self.bridge.latest, retained)
        self.assertEqual(self.update, original)
        self.clock.now += MAX_DISPLAY_AGE_NS
        self.assertNotIn('native_evidence', self.view())
        self.assertEqual(self.bridge.latest['started'], retained['started'])

    def test_hazard_evidence_keeps_active_jpeg_separate_from_original_png(self):
        row = self.update['reader']['hazards'][0]
        record = json.loads(base64.b64decode(json.loads(base64.b64decode(row['envelope_json']))['record']))
        generic = json.loads(base64.b64decode(record['publication']['payload']))
        hazard = deepcopy(row['payload'])
        camera = next(i for i in hazard['inputs'] if i['sensor_id'] == 'camera-b')
        png = deepcopy(camera['evidence'])
        jpeg = {'sha256': 'f'*64, 'size': 777, 'content_type': 'image/jpeg'}
        camera['capture']['original_evidence'] = png
        camera['evidence'] = camera['sample']['evidence'] = jpeg
        descriptors = hazard_native_evidence(hazard, generic['inputs'],
                                            tenant=self.config['tenant'], site=self.config['site'])
        pair = [i for i in descriptors if i['source']['stream'] == 'camera-b']
        self.assertEqual([i['role'] for i in pair], ['active', 'original'])
        self.assertEqual([i['sha256'] for i in pair], [jpeg['sha256'], png['sha256']])
        self.assertEqual([i['content_type'] for i in pair], ['image/jpeg', 'image/png'])
        self.assertEqual(pair[0]['source'], pair[1]['source'])
        self.assertEqual(pair[0]['proof_envelope_sha256'], camera['capture']['proof_sha256'])
        self.assertEqual(pair[0]['proof_envelope_sha256'], pair[1]['proof_envelope_sha256'])

    def test_hazard_evidence_omits_incomplete_or_mismatched_ownership(self):
        row = self.update['reader']['hazards'][0]
        record = json.loads(base64.b64decode(json.loads(base64.b64decode(row['envelope_json']))['record']))
        generic = json.loads(base64.b64decode(record['publication']['payload']))
        cases = {
            'missing size': lambda used, ref: used['evidence'].pop('size'),
            'invalid size': lambda used, ref: used['evidence'].update(size=8*1024*1024+1),
            'invalid digest': lambda used, ref: used['evidence'].update(sha256='not-a-digest'),
            'unsupported MIME': lambda used, ref: used['evidence'].update(content_type='application/octet-stream'),
            'wrong original': lambda used, ref: used['sample'].update(evidence={}),
            'missing owner': lambda used, ref: ref['key']['source'].pop('device'),
            'wrong owner': lambda used, ref: ref['key']['source'].update(device='different-device'),
            'wrong component': lambda used, ref: ref['key'].update(type='other.type'),
            'wrong tenant': lambda used, ref: ref['key'].update(tenant='other-tenant'),
            'wrong proof': lambda used, ref: ref.update(envelope_sha256='e'*64),
            'wrong version': lambda used, ref: ref.update(version={'epoch': 2, 'sequence': 2}),
        }
        for name, change in cases.items():
            with self.subTest(name=name):
                hazard, refs = deepcopy(row['payload']), deepcopy(generic['inputs'])
                index = next(i for i, used in enumerate(hazard['inputs']) if used['sensor_id'] == 'camera-a')
                change(hazard['inputs'][index], refs[index])
                descriptors = hazard_native_evidence(hazard, refs, tenant=self.config['tenant'], site=self.config['site'])
                self.assertEqual({i['source']['stream'] for i in descriptors}, {'camera-b', 'audio-1'})
        self.assertEqual(hazard_native_evidence(row['payload'], generic['inputs'][:-1],
                         tenant=self.config['tenant'], site=self.config['site']), [])

    def test_physical_expiry_while_simulation_paused_charges_collection_and_display_age(self):
        self.update['clock']['physical_latest'] = '2026-11-12T12:00:04.700Z'
        self.ingest(); self.assertEqual(self.view()['state'], 'hazard')
        self.clock.now += 200_000_000  # 100 ms collection + 200 ms display reaches 5s cap.
        view = self.view()
        self.assertEqual(view['state'], 'expired')
        self.assertEqual(view['sample_time_s'], 1)
        self.assertNotIn('position', view)
        self.assertEqual(view['observations'], [])

    def test_sample_clock_advance_expires_original_without_restamping_then_fresh_recovers(self):
        self.ingest()
        self.new_tick()
        self.update['clock']['sample_time'] = instant(4_100_000_000)
        self.update['presentation']['simulation_time_ns'] = 4_100_000_000
        self.ingest()
        self.assertEqual(self.view()['state'], 'expired')
        # Fresh readiness with no result is distinct from stale/expired state.
        self.new_tick(); self.update['reader']['hazards'] = []; self.update['media'] = []
        self.update['reader']['sample_time'] = instant(4_100_000_000)
        self.ingest(); self.assertEqual(self.view()['state'], 'no_live_result')
        original_hash = live_fixture()[2]['reader']['hazards'][0]['envelope_sha256']
        fresh = live_fixture()[2]
        restored(fresh, 3_200_000_000)
        fresh['sequence'] = self.update['sequence'] + 1
        fresh['reader']['tick'] = self.update['reader']['tick'] + 1
        fresh['collection_started_monotonic_ns'] = self.clock.now
        self.assertTrue(self.bridge.ingest(fresh))
        view = self.view()
        self.assertEqual(view['state'], 'hazard')
        self.assertEqual(view['sample_time_s'], 4.2)
        self.assertNotEqual(view['original_envelope_sha256'], original_hash)

    def test_startup_attempts_and_ready_same_tick_are_distinct_from_absence(self):
        self.update['reader'].update(status='waiting_for_source', startup_attempt=1, hazards=[], origin_attested_inference=False)
        self.update['media'] = []
        self.ingest(); self.assertEqual(self.view()['state'], 'waiting')
        self.update['sequence'] += 1; self.update['reader']['startup_attempt'] += 1
        self.assertTrue(self.ingest())
        self.update['sequence'] += 1
        self.update['reader'].update(status='ready', origin_attested_inference=True)
        self.assertTrue(self.ingest()); self.assertEqual(self.view()['state'], 'no_live_result')

    def test_waiting_without_clock_has_no_current_time_or_controls_then_ready_recovers(self):
        original = deepcopy(self.update)
        self.update['reader'].update(status='waiting_for_source', startup_attempt=1, hazards=[], origin_attested_inference=False)
        self.update.pop('clock'); self.update['media'] = []
        self.assertTrue(self.ingest())
        state = self.bridge.export()
        self.assertEqual(state['snapshot']['fusion']['state'], 'waiting')
        self.assertFalse(state['snapshot']['controls_available'])
        original['sequence'] += 1
        original['reader']['clock'] = original.pop('clock')
        self.assertTrue(self.bridge.ingest(original))
        self.assertEqual(self.view()['state'], 'hazard')
        self.assertTrue(self.bridge.export()['snapshot']['controls_available'])

    def test_scope_envelope_frame_and_companion_clock_mismatch_clear_old_result(self):
        changes = [lambda u: u['reader'].update(reader_generation=10),
                   lambda u: u['clock'].update(server_boot_id='other'),
                   lambda u: u['clock'].update(sample_clock_domain='other'),
                   lambda u: u['clock'].update(physical_latest='2026-11-12T11:59:59Z'),
                   lambda u: u['presentation'].update(simulation_time_ns=2_000_000_000),
                   lambda u: u['presentation']['geometry'].update(model_sha256='f' * 64),
                   lambda u: u['reader']['hazards'][0]['payload'].update(confidence=.1),
                   lambda u: u['reader']['hazards'][0].update(envelope_sha256='f' * 64)]
        for index, change in enumerate(changes):
            with self.subTest(index=index):
                config, presentation, update = live_fixture()
                bridge = NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
                bridge.ingest(update); update['sequence'] += 1; update['reader']['tick'] += 1; change(update)
                with self.assertRaises(ValueError): bridge.ingest(update)
                self.assertEqual(bridge.export()['snapshot']['fusion']['state'], 'unknown')

    def test_source_qualified_media_uses_exact_bytes_and_optional_absence(self):
        with tempfile.TemporaryDirectory() as directory:
            with_files = SessionFiles(directory, self.config['run_id'])
            self.addCleanup(with_files.close)
            bridge = NativeReplicaBridge(self.config, self.presentation, with_files, monotonic_ns=self.clock)
            bridge.ingest(self.update); view = bridge.export()['snapshot']['fusion']
            camera = next(i for i in view['observations'] if i['id'] == 'camera-b')
            raw = (Path(directory) / camera['evidence_path']).read_bytes()
            self.assertEqual(digest(raw), camera['evidence_reference']['sha256'])
            self.assertEqual(json.loads((Path(directory) / 'state.json').read_bytes())['mode'], 'live-native-replica')
        self.update['media'] = []; self.ingest()
        self.assertEqual(self.view()['state'], 'hazard')
        self.assertTrue(all('evidence_path' not in i for i in self.view()['observations']))

    def test_media_mismatch_and_wrong_source_clear_display(self):
        for key in ('data', 'source'):
            config, presentation, update = live_fixture(); bridge = NativeReplicaBridge(config, presentation, monotonic_ns=self.clock)
            if key == 'data': update['media'][0]['data'] = encoded(b'changed')
            else: update['media'][0]['source']['device'] = 'other'
            with self.subTest(key=key), self.assertRaises(ValueError): bridge.ingest(update)
            self.assertEqual(bridge.export()['snapshot']['fusion']['state'], 'unknown')

    def test_late_camera_media_adds_exact_thumbnails_without_new_observation_or_rewrites(self):
        self.config['display_clock'] = 'mach_absolute_time_ns'
        rows = deepcopy(self.update['media']); self.update['media'] = []
        original_update = deepcopy(self.update)
        with tempfile.TemporaryDirectory() as directory:
            files = SessionFiles(directory, self.config['run_id'])
            self.addCleanup(files.close)
            bridge = NativeReplicaBridge(self.config, self.presentation, files, monotonic_ns=self.clock)
            bridge.ingest(self.update)
            before = bridge.export()
            original = deepcopy(bridge.latest)
            reader_state = (bridge.last_collection, bridge.last_tick, bridge.last_attempt,
                            bridge.last_clock_sample, bridge.last_clock_lower, bridge.sample_ns)
            envelope = original['parsed']['sha256']
            self.clock.now += 100_000_000
            with patch('sim.native_replica_bridge.os.replace', wraps=os.replace) as replace:
                self.assertTrue(bridge.attach_media(rows[:1], envelope_sha256=envelope))
                self.assertTrue(bridge.attach_media(rows, envelope_sha256=envelope))
                written = replace.call_count
                self.assertEqual(written, 2)
                self.assertFalse(bridge.attach_media(rows, envelope_sha256=envelope))
                self.assertFalse(bridge.attach_media([], envelope_sha256=envelope))
                self.assertEqual(replace.call_count, written)
            after = bridge.export()
            self.assertEqual({k: v for k, v in bridge.latest.items() if k != 'media'},
                             {k: v for k, v in original.items() if k != 'media'})
            self.assertEqual(reader_state, (bridge.last_collection, bridge.last_tick, bridge.last_attempt,
                bridge.last_clock_sample, bridge.last_clock_lower, bridge.sample_ns))
            for key in ('observation_deadline_monotonic_ns', 'display_deadline_monotonic_ns'):
                self.assertEqual(before[key], after[key])
            images = [o for o in after['snapshot']['fusion']['observations'] if o['kind'] == 'camera']
            self.assertEqual(len(images), 2)
            for image in images:
                raw = (Path(directory) / image['evidence_path']).read_bytes()
                expected = next(r for r in rows if r['source']['service'] == image['id'])
                self.assertEqual(raw, base64.b64decode(expected['data']))
            self.assertEqual(self.update, original_update)
            rows[0]['source']['device'] = 'caller-change'
            self.assertNotEqual(bridge.latest['media'][next(iter(bridge.latest['media']))]['source']['device'], 'caller-change')

    def test_late_media_rejects_mismatch_without_replacing_current_reader_or_partial_files(self):
        rows = deepcopy(self.update['media']); self.update['media'] = []
        for changed in ('source', 'reference', 'data', 'duplicate', 'too-many'):
            with self.subTest(changed=changed), tempfile.TemporaryDirectory() as directory:
                files = SessionFiles(directory, self.config['run_id'])
                try:
                    bridge = NativeReplicaBridge(self.config, self.presentation, files, monotonic_ns=self.clock)
                    bridge.ingest(self.update); original = deepcopy(bridge.latest)
                    bad = deepcopy(rows)
                    if changed == 'source': bad[-1]['source']['device'] = 'wrong-gateway'
                    elif changed == 'reference': bad[-1]['reference']['size'] += 1
                    elif changed == 'data': bad[-1]['data'] = encoded(b'not-original')
                    elif changed == 'duplicate': bad[-1] = deepcopy(bad[0])
                    else: bad.append(deepcopy(bad[0]))
                    with self.assertRaisesRegex(ValueError, 'invalid late source evidence'):
                        bridge.attach_media(bad, envelope_sha256=original['parsed']['sha256'])
                    self.assertEqual(bridge.latest, original)
                    self.assertEqual(list(Path(directory).glob('native-*')), [])
                    self.assertEqual(bridge.export()['snapshot']['fusion']['state'], 'hazard')
                finally:
                    files.close()

    def test_late_media_cannot_follow_an_old_result_into_a_newer_reader_tuple(self):
        rows = deepcopy(self.update['media']); self.update['media'] = []
        self.ingest(); old = self.bridge.latest['parsed']['sha256']
        restored(self.update, 100_000_000); self.new_tick(); self.ingest()
        current = deepcopy(self.bridge.latest)
        self.assertNotEqual(current['parsed']['sha256'], old)
        self.assertFalse(self.bridge.attach_media(rows, envelope_sha256=old))
        self.assertEqual(self.bridge.latest, current)
        self.assertTrue(self.bridge.attach_media(rows, envelope_sha256=current['parsed']['sha256']))

    def test_late_media_does_not_revive_waiting_empty_stale_or_expired_results(self):
        for mode in ('waiting', 'empty', 'stale', 'physical', 'sample', 'regressed'):
            with self.subTest(mode=mode):
                clock = Clock(); config, presentation, update = live_fixture()
                rows = update.pop('media'); envelope = update['reader']['hazards'][0]['envelope_sha256']
                if mode in ('waiting', 'empty'):
                    update['reader']['hazards'] = []
                    if mode == 'waiting': update['reader'].update(status='waiting_for_source', origin_attested_inference=False)
                elif mode == 'physical': update['clock']['physical_latest'] = '2026-11-12T12:00:04.900Z'
                elif mode == 'sample':
                    update['clock']['sample_time'] = instant(4_100_000_000)
                    update['presentation']['simulation_time_ns'] = 4_100_000_000
                bridge = NativeReplicaBridge(config, presentation, monotonic_ns=clock)
                bridge.ingest(update)
                if mode == 'stale': clock.now += MAX_DISPLAY_AGE_NS
                elif mode == 'regressed': clock.now -= 1
                self.assertFalse(bridge.attach_media(rows, envelope_sha256=envelope))
                self.assertNotEqual(bridge.export()['snapshot']['fusion']['state'], 'hazard')

    def test_late_media_finishing_at_original_expiry_does_not_attach(self):
        rows = self.update.pop('media')
        self.update['clock']['physical_latest'] = '2026-11-12T12:00:04.700Z'
        with tempfile.TemporaryDirectory() as directory:
            files = SessionFiles(directory, self.config['run_id'])
            self.addCleanup(files.close)
            bridge = NativeReplicaBridge(self.config, self.presentation, files, monotonic_ns=self.clock)
            bridge.ingest(self.update); before = deepcopy(bridge.latest)
            real_replace = os.replace
            def expire(*args, **kwargs):
                real_replace(*args, **kwargs)
                self.clock.now = before['started'] + 300_000_000
            with patch('sim.native_replica_bridge.os.replace', side_effect=expire):
                self.assertFalse(bridge.attach_media(rows, envelope_sha256=before['parsed']['sha256']))
            self.assertEqual(bridge.latest, before)
            view = bridge.export()['snapshot']['fusion']
            self.assertEqual(view['state'], 'expired')
            self.assertEqual(view['observations'], [])

    def test_source_toggle_calls_existing_step_once_and_waits_for_observed_update(self):
        self.ingest(); calls = []
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'audio-1'}
        def step(request):
            calls.append(request); return {'sequence': 2, **request}
        receipt = self.bridge.source_step(command, 2_000_000_000, step)
        self.assertTrue(receipt['accepted']); self.assertEqual(len(calls), 1)
        self.assertNotIn('audio-1', calls[0]['sensors'])
        self.assertEqual(self.view()['state'], 'unknown')
        self.assertTrue(next(i for i in self.bridge.presentation['sensor_sources'] if i['id'] == 'audio-1')['enabled'])
        with self.assertRaises(ValueError): self.bridge.source_step(command, 2_000_000_001, step)
        self.assertEqual(len(calls), 1)

    def test_async_begin_keeps_original_hazard_deadline_without_controls_or_optimistic_switch(self):
        self.config['display_clock'] = 'mach_absolute_time_ns'
        self.bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)
        self.ingest(); original = self.bridge.export()
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'camera-b'}
        request = self.bridge.begin_source_step(command, 2_000_000_000)
        self.assertNotIn('camera-b', request['sensors'])
        self.assertEqual(self.bridge.command_sequence, 1)
        self.assertEqual(self.bridge.minimum_presentation_ns, 0)
        state = self.bridge.export()
        self.assertFalse(state['snapshot']['controls_available'])
        self.assertEqual(state['snapshot']['fusion']['state'], 'hazard')
        self.assertTrue(next(r for r in state['snapshot']['sensor_sources'] if r['id'] == 'camera-b')['enabled'])
        self.assertEqual(state['display_deadline_monotonic_ns'], original['display_deadline_monotonic_ns'])
        self.assertEqual(state['observation_deadline_monotonic_ns'], original['observation_deadline_monotonic_ns'])
        self.new_tick()
        self.assertTrue(self.ingest())  # The source still reports its old instant while rendering.
        fresh_pending = self.bridge.export()
        self.assertEqual(fresh_pending['snapshot']['fusion']['state'], 'hazard')
        self.assertFalse(fresh_pending['snapshot']['controls_available'])
        self.clock.now = fresh_pending['display_deadline_monotonic_ns']
        self.assertNotEqual(self.view()['state'], 'hazard')
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])

    def test_async_finish_acknowledges_once_and_only_after_exact_response(self):
        with tempfile.TemporaryDirectory() as directory:
            with_files = SessionFiles(Path(directory), self.config['run_id'])
            try:
                bridge = NativeReplicaBridge(self.config, self.presentation, with_files, monotonic_ns=self.clock)
                bridge.ingest(self.update)
                command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                           'action': 'toggle_sensor', 'target': 'camera-b'}
                request = bridge.begin_source_step(command, 2_000_000_000)
                self.assertFalse((Path(directory)/'receipts/000001.json').exists())
                with self.assertRaises(ValueError):
                    bridge.begin_source_step({**command, 'sequence': 2}, 3_000_000_000)
                receipt = bridge.finish_source_step({'sequence': 3, **request})
                self.assertTrue(receipt['accepted'])
                self.assertEqual(bridge.minimum_presentation_ns, 2_000_000_000)
                self.assertEqual(json.loads((Path(directory)/'receipts/000001.json').read_bytes()), receipt)
                self.assertIsNone(bridge.pending_source)
                self.assertFalse(bridge.export()['snapshot']['controls_available'])
                with self.assertRaises(ValueError): bridge.finish_source_step({'sequence': 3, **request})
                with self.assertRaises(ValueError): bridge.begin_source_step(command, 2_000_000_000)
            finally:
                with_files.close()

    def test_async_request_is_owned_and_unknown_outcome_cannot_retry_consumed_command(self):
        self.ingest()
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'camera-b'}
        request = self.bridge.begin_source_step(command, 2_000_000_000)
        request['sensors'].append('camera-b')
        receipt = self.bridge.finish_source_step({'sequence': 3, **request})
        self.assertFalse(receipt['accepted'])
        self.assertEqual(receipt['error'], 'unknown_outcome')
        self.assertNotIn('camera-b', receipt['source_request']['sensors'])
        self.assertEqual(self.bridge.minimum_presentation_ns, 0)
        with self.assertRaises(ValueError): self.bridge.begin_source_step(command, 2_000_000_000)

    def test_async_future_observation_cannot_enable_controls_before_completion(self):
        self.ingest()
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'camera-b'}
        self.bridge.begin_source_step(command, 2_000_000_000)
        self.new_tick(); self.update['reader']['hazards'] = []; self.update['media'] = []
        self.update['presentation']['simulation_time_ns'] = 2_000_000_000
        self.update['clock']['sample_time'] = self.update['reader']['sample_time'] = instant(2_000_000_000)
        for row in self.update['presentation']['sensor_sources']:
            if row['id'] == 'camera-b': row['enabled'] = False
        self.assertTrue(self.ingest())
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])
        receipt = self.bridge.finish_source_step(error=TimeoutError('private diagnostic'))
        self.assertFalse(receipt['accepted'])
        self.assertNotIn('private diagnostic', json.dumps(receipt))
        self.assertIsNone(self.bridge.pending_source)

    def test_sensor_switches_remain_observed_and_live_controls_follow_freshness(self):
        self.update['presentation']['sensor_sources'][0].update(kind='audio', label='Audio source')
        initial = self.bridge.export()['snapshot']
        self.assertFalse(initial['controls_available'])
        self.ingest()
        snapshot = self.bridge.export()['snapshot']
        self.assertTrue(snapshot['controls_available'])
        self.assertEqual(snapshot['sensor_sources'], self.update['presentation']['sensor_sources'])
        snapshot['sensor_sources'][0]['enabled'] = False
        self.assertTrue(self.bridge.export()['snapshot']['sensor_sources'][0]['enabled'])
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_sensor', 'target': 'audio-1'}
        self.bridge.source_step(command, 2_000_000_000, lambda request: {'sequence': 2, **request})
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])
        self.assertTrue(next(r for r in self.bridge.export()['snapshot']['sensor_sources'] if r['id'] == 'audio-1')['enabled'])
        self.new_tick(); self.update['reader']['hazards'] = []; self.update['media'] = []
        self.update['presentation']['sensor_sources'][0]['enabled'] = False
        self.assertFalse(self.ingest())  # A queued pre-command row cannot acknowledge the source change.
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])
        self.update['presentation']['simulation_time_ns'] = 2_000_000_000
        self.update['clock']['sample_time'] = self.update['reader']['sample_time'] = instant(2_000_000_000)
        self.ingest()
        self.assertFalse(self.bridge.export()['snapshot']['sensor_sources'][0]['enabled'])
        self.assertTrue(self.bridge.export()['snapshot']['controls_available'])
        self.clock.now += MAX_DISPLAY_AGE_NS
        self.assertFalse(self.bridge.export()['snapshot']['controls_available'])
        self.assertFalse(self.bridge.export(closed=True)['snapshot']['controls_available'])

    def test_unknown_mutation_is_not_retried_and_mission_commands_have_no_endpoint(self):
        self.ingest(); calls = []
        command = {'schema': COMMAND_SCHEMA, 'run_id': self.config['run_id'], 'sequence': 1,
                   'action': 'toggle_camera', 'target': 'camera-b'}
        def step(request):
            calls.append(request); raise TimeoutError('completed or not unknown')
        receipt = self.bridge.source_step(command, 2_000_000_000, step)
        self.assertFalse(receipt['accepted']); self.assertEqual(receipt['error'], 'unknown_outcome')
        self.assertEqual(len(calls), 1)
        command.update(action='start', target='', sequence=2)
        with self.assertRaises(ValueError): self.bridge.source_step(command, 2_000_000_001, step)
        self.assertEqual(len(calls), 1)

    def test_stale_collection_and_monotonic_regression_do_not_display_current(self):
        self.update['collection_started_monotonic_ns'] = self.clock.now - MAX_DISPLAY_AGE_NS
        with self.assertRaises(ValueError): self.ingest()
        self.update['collection_started_monotonic_ns'] = self.clock.now; self.ingest()
        self.clock.now -= 1
        self.assertEqual(self.view()['state'], 'unknown')




class ContinuousPresentationTests(unittest.TestCase):
    def setUp(self):
        self.clock = Clock()
        self.config, self.presentation, self.update = live_fixture()
        self.config.update(presentation_mode='continuous', display_clock='mach_absolute_time_ns')
        self.bridge = NativeReplicaBridge(self.config, self.presentation, monotonic_ns=self.clock)

    def test_completed_physics_frame_advances_without_renewing_reader_or_interpolating(self):
        self.bridge.ingest(self.update)
        original = deepcopy(self.bridge.latest)
        initial_deadline = self.bridge.export()['display_deadline_monotonic_ns']
        presentation = deepcopy(self.presentation)
        presentation['simulation_time_ns'] += 50_000_000
        presentation['dynamic_geoms'] = [{'id': 42, 'position': [-4., 1., .24],
                                          'rotation': [1,0,0,0,1,0,0,0,1]}]
        self.assertTrue(self.bridge.present(presentation))
        self.clock.now += 10_000_000
        state = self.bridge.export()
        self.assertEqual(state['dynamic_geoms'], presentation['dynamic_geoms'])
        self.assertEqual(state['snapshot']['time_s'], 1.05)
        self.assertEqual(self.bridge.latest, original)
        self.assertEqual(state['display_deadline_monotonic_ns'], initial_deadline)
        self.assertFalse(self.bridge.present(self.presentation))
        self.clock.now = original['started'] + MAX_DISPLAY_AGE_NS
        self.assertTrue(self.bridge.present(presentation))
        self.assertEqual(self.bridge.export()['snapshot']['fusion']['state'], 'unknown')

    def test_continuous_reader_and_presentation_clocks_remain_distinct(self):
        update = deepcopy(self.update)
        update['presentation']['simulation_time_ns'] -= 100_000_000
        before = deepcopy(update)
        self.assertTrue(self.bridge.ingest(update))
        self.assertEqual(self.bridge.latest['sample'], timestamp(update['clock']['sample_time']))
        self.assertEqual(update, before)
        stepped = NativeReplicaBridge({k:v for k,v in self.config.items() if k != 'presentation_mode'},
                                      self.presentation, monotonic_ns=self.clock)
        with self.assertRaises(ValueError): stepped.ingest(update)
        with self.assertRaises(ValueError): stepped.present(self.presentation)

    def test_remaining_sample_budget_expires_without_new_presentation(self):
        until = timestamp(self.update['reader']['hazards'][0]['payload']['valid_until'])
        # The original final clock is near the signed sample cap. No times in
        # the signed result or initial reader sample are rewritten.
        self.update['clock']['sample_time'] = instant(until-self.config['anchor_unix_ns']-150_000_000)
        self.bridge.ingest(self.update)
        state = self.bridge.export()
        deadline = self.update['collection_started_monotonic_ns']+150_000_000
        self.assertEqual(state['display_deadline_monotonic_ns'], deadline)
        self.clock.now = deadline
        self.assertEqual(self.bridge.export()['snapshot']['fusion']['state'], 'expired')

    def test_actual_capture_ack_is_at_least_minimum_only_in_continuous_mode(self):
        self.bridge.ingest(self.update)
        command = dict(schema=COMMAND_SCHEMA, run_id=self.config['run_id'], sequence=1,
                       action='toggle_sensor', target='camera-b')
        request = self.bridge.begin_source_step(command, self.presentation['simulation_time_ns']+1)
        response = dict(sequence=38, **request)
        response['simulation_time_ns'] += 200_000_000
        receipt = self.bridge.finish_source_step(response)
        self.assertTrue(receipt['accepted'])
        self.assertEqual(receipt['source_request'], request)
        self.assertEqual(receipt['source_response'], response)
        self.assertEqual(self.bridge.minimum_presentation_ns, response['simulation_time_ns'])
        self.assertIsNone(self.bridge.latest)
        self.assertFalse(self.bridge.present(self.presentation))
        stepped_config = {k:v for k,v in self.config.items() if k != 'presentation_mode'}
        stepped = NativeReplicaBridge(stepped_config, self.presentation, monotonic_ns=self.clock)
        stepped.ingest(self.update)
        stepped.begin_source_step(command, request['simulation_time_ns'])
        self.assertFalse(stepped.finish_source_step(response)['accepted'])


if __name__ == '__main__':
    unittest.main()

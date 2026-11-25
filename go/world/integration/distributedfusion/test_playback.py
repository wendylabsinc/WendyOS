"""Unsigned receipt-correspondence fixtures, not deployed or crypto evidence."""
from copy import deepcopy
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

from integration.distributedfusion import model, playback
from integration.hazard.test_model import identity_fixture, pair_fixture
from integration.hazard.test_playback import PNG, encoded, instant


def signed_bytes(record):
    inner = json.dumps(record, indent=3).encode()
    outer = b' \n' + json.dumps({'record': encoded(inner), 'signature': encoded(b'unsigned-test-fixture')}, indent=2).encode() + b'\n'
    return outer, inner


def publication(pair, scene, ns, sequence, gas_device=None):
    inputs, proofs, refs, captures, media, gas = [], [], [], {}, {}, None
    a, b = [n['public']['principal'] for n in pair['nodes']]
    for sensor in sorted(model.SENSORS):
        kind = sensor.split('-')[0]
        owner = (gas_device or b) if sensor == 'gas-1' else a
        source = {'device': owner, 'app': model.APP, 'service': sensor, 'stream': sensor}
        sample = {'id': f'{scene["run_id"]}/{sensor}/{sequence}', 'sensor_id': sensor, 'kind': kind,
                  'sequence': sequence, 'run_id': scene['run_id'], 'frame': scene['frame'], 'sample_ns': ns,
                  'site_id': scene['site'], 'package_sha256': scene['package_sha256'], 'position_m': [1, 2, 3]}
        content = PNG if kind == 'camera' else b'RIFF-test-wave' if kind == 'audio' else b''
        if content:
            sample['evidence'] = {'sha256': model.digest(content), 'size': len(content),
                                  'content_type': 'image/png' if kind == 'camera' else 'audio/wav'}
            media[sensor] = content
        capture = {'version': 1, 'scene_boot_id': scene['boot_id'], 'simulation_anchor_unix_ns': model.ANCHOR,
                   'captured_unix_ns': 1794484800000000000 + sequence * 1_000_000_000,
                   'sample_json': encoded(model.json_bytes(sample))}
        captures[sensor] = {'capture': capture, 'media': encoded(content)}
        record = {'epoch': 1, 'sequence': sequence,
                  'scope': {'tenant': pair['tenant'], 'site': scene['site'], **{k: source[k] for k in ('device', 'app', 'service')},
                            'execution_domain': 'simulation'},
                  'publication': {'entity': 'sensor/' + sensor, 'component_type': model.CAPTURE, 'stream': sensor,
                                  'frame': scene['frame'], 'tombstone': False, 'payload': encoded(model.json_bytes(capture)),
                                  'ttl_ns': 3_000_000_000,
                                  'timing': {'sample_time': instant(ns), 'clock_domain': scene['clock_domain'],
                                             'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
        raw, inner = signed_bytes(record)
        ref = {'key': {'tenant': pair['tenant'], 'site': scene['site'], 'entity': 'sensor/' + sensor,
                       'type': model.CAPTURE, 'source': source}, 'version': {'epoch': 1, 'sequence': sequence},
               'envelope_sha256': model.digest(raw), 'record_sha256': model.digest(inner)}
        refs.append(ref)
        proofs.append({'reference': ref, 'envelope_json': encoded(raw)})
        item = {'sensor_id': sensor, 'kind': kind, 'sample_id': sample['id'], 'sample_time': instant(ns),
                'valid_until': instant(ns + 2_000_000_000), 'source': source, 'version': ref['version'],
                'capture': {'scene_boot_id': scene['boot_id'], 'simulation_anchor': instant(0),
                            'proof_sha256': ref['envelope_sha256']}}
        if kind == 'lidar':
            item['lidar_summary'] = {'point_count': 144, 'max_range_m': 6.1, 'normalized_sample_sha256': 'd' * 64}
        else:
            item['sample'] = sample
            if content:
                item['evidence'] = sample['evidence']
        inputs.append(item)
        if sensor == 'gas-1':
            gas = {'capture_id': sample['id'], 'native_sequence': sequence, 'scene_boot_id': scene['boot_id'],
                   'captured_unix_ns': capture['captured_unix_ns'], 'sample_ns': ns,
                   'request_json': encoded(b'{}'), 'signed_record_json': encoded(raw),
                   'record_sha256': ref['record_sha256'], 'proof_sha256': ref['envelope_sha256']}
    hazard = {'label': 'possible-gas-hazard', 'confidence': .8, 'uncertainty_m': .4,
              'position_m': [4, .8, -2], 'valid_until': instant(ns + 2_000_000_000),
              'physical_valid_until': '2026-11-12T12:00:05Z', 'inputs': inputs,
              'gas_value_ppm': 80, 'audio': {'tone_fraction': .9},
              'cameras': [{'sensor_id': c, 'red_fraction': .1} for c in model.CAMERAS]}
    generic = {'version': 1, 'result_schema': 'wendy.hazard.v1', 'result_json': encoded(model.json_bytes(hazard)), 'inputs': refs}
    record = {'epoch': 2, 'sequence': sequence,
              'scope': {'tenant': pair['tenant'], 'site': scene['site'], 'device': a, 'app': model.APP,
                        'service': 'fusion', 'execution_domain': 'simulation'},
              'publication': {'component_type': model.RESULT, 'stream': model.STREAM,
                  'entity': scene['run_id'] + '/possible-gas-hazard', 'frame': scene['frame'],
                  'tombstone': False, 'payload': encoded(model.json_bytes(generic)), 'ttl_ns': 2_000_000_000,
                  'timing': {'sample_time': instant(ns), 'clock_domain': scene['clock_domain'],
                             'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
    raw, inner = signed_bytes(record)
    digest = model.digest(raw)
    row = {'envelope_json': encoded(raw), 'envelope_sha256': digest, 'record_sha256': model.digest(inner),
           'version': {'epoch': 2, 'sequence': sequence}, 'payload': hazard}
    origins = {digest: {'publication': {'status': 'complete', 'locally_verified': True, 'inputs': proofs,
        'proof_sha256': digest, 'record_sha256': row['record_sha256'], 'signed_record_json': encoded(raw)}}}
    gas_ref = next(r for r in refs if r['key']['source']['stream'] == 'gas-1')
    correspondence = {'version': 1, 'source_devices': {i['sensor_id']: i['source']['device'] for i in inputs},
        'gas_reference': gas_ref, 'gas_envelope_sha256': gas['proof_sha256'], 'a_input_envelope_sha256': gas['proof_sha256'],
        'gas_sample_valid_until_unix_ns': model.ANCHOR + ns + 3_000_000_000,
        'hazard_envelope_sha256': digest, 'reader_envelope_sha256': digest, 'fuser_device': a, 'reader_device': b,
        'exact_bytes_matched': True, 'independent_crypto_reverification': False}
    return row, origins, gas, correspondence, captures, media


def fixture(gas_device=None):
    pair, _ = pair_fixture()
    scene = identity_fixture()
    geometry = {'schema': 'wendy.macos-demo.geometry.v1', 'run_id': scene['run_id'],
                'model_sha256': 'b' * 64, 'geoms': []}
    steps = []
    for index, ns in enumerate((1_000_000_000, 4_100_000_000, 4_200_000_000)):
        active = index != 1
        sensors = list(model.SENSORS if active else model.LOCAL_SENSORS)
        row, origins, gas, correspondence, captures, media = publication(pair, scene, ns, index + 1, gas_device)
        if not active:
            del captures['gas-1']
            origins, gas, correspondence = {}, None, None
        reader = {'version': 1, 'tick': index + 3, 'status': 'ready', 'run_id': scene['run_id'],
                  'reader_generation': 9, 'sample_time': instant(ns), 'hazards': [row] if active else [],
                  'origin_attested_inference': True, 'raw_inputs_independently_verified': False}
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        steps.append({'name': playback.NAMES[index], 'simulation_time_ns': ns, 'reader': reader,
                      'origins': origins, 'gas_origin': gas, 'correspondence': correspondence,
                      'images': {s: media[s] for s in model.CAMERAS} if active else {},
                      'capture_values': captures, 'media_values': media,
                      'control': {'request': request, 'response': {'sequence': index + 1, **request},
                          'source_clock_shared': True, 'clock_domain': scene['clock_domain'],
                          'anchor_unix_ns': model.ANCHOR, 'no_mutation_retry': True},
                      'presentation': {'geometry': geometry, 'simulation_time_ns': ns, 'dynamic_geoms': [],
                          'sensor_sources': [{'id': s, 'enabled': s in sensors} for s in model.SENSORS],
                          'camera_sources': [{'id': s, 'enabled': s in sensors} for s in model.CAMERAS]}})
    return pair, scene, steps


def write_run(directory, pair, scene, steps):
    def write(name, value, raw=False):
        path = directory / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(value if raw else model.json_bytes(value))
    write('result.json', {'passed': True, 'gate_completed': True})
    write('pair-public.json', pair)
    write('scene-identity.json', scene)
    write('task-b-reader.json', {'node': 'b', 'app': model.APP, 'service': 'reader',
                               'device': pair['nodes'][1]['public']['principal'], 'generation': 9})
    rows = []
    for index, step in enumerate(steps, 1):
        prefix = 'steps/%04d' % index
        row = {k: step[k] for k in ('name', 'simulation_time_ns')}
        for key in ('presentation', 'control', 'reader', 'gas_origin', 'correspondence'):
            row[key] = prefix + '-' + key + '.json' if step[key] is not None else None
            if row[key]:
                write(row[key], step[key])
        row['origin'] = prefix + '-origin.json' if step['origins'] else None
        if row['origin']:
            write(row['origin'], step['origins'])
        row['captures'], row['media'] = {}, {}
        for sensor, capture in step['capture_values'].items():
            row['captures'][sensor] = prefix + '-' + sensor + '.json'
            write(row['captures'][sensor], capture)
        for sensor, content in step['media_values'].items():
            row['media'][sensor] = prefix + '-' + sensor + ('.png' if sensor in model.CAMERAS else '.wav')
            write(row['media'][sensor], content, raw=True)
        row['original_envelope_sha256'] = [h['envelope_sha256'] for h in step['reader']['hazards']]
        row['hazards'] = len(step['reader']['hazards'])
        rows.append(row)
    write('playback-steps.json', {'version': 1, 'steps': rows})


class PlaybackTests(unittest.TestCase):
    def test_preserves_exact_chain_deadlines_and_device_labels(self):
        pair, scene, steps = fixture()
        before = deepcopy(steps)
        _, frames, originals, assets = playback.build_timeline(pair, scene, 9, steps)
        self.assertEqual(steps, before)
        self.assertEqual(len(originals), 4)  # Two A results and their two B gas originals.
        self.assertTrue(all(raw.startswith(b' \n') and raw.endswith(b'\n') for raw in originals.values()))
        self.assertEqual(len(assets), 1)
        view = frames[0]['state']['snapshot']['fusion']
        self.assertEqual(view['position'], [4, 2, .8])
        self.assertEqual(view['valid_until_s'], 3)
        self.assertIn('144 LiDAR returns', view['basis'])
        self.assertIn('Gateway B', next(o for o in view['observations'] if o['id'] == 'gas-1')['label'])
        self.assertTrue(all('Gateway A' in o['label'] for o in view['observations'] if o['id'] != 'gas-1'))
        self.assertIn('RECORDED SYNTHETIC', frames[0]['chapter']['series'])
        self.assertEqual(frames[0]['source_step']['gas_original_envelope_sha256'], steps[0]['gas_origin']['proof_sha256'])

    def test_remote_only_expiry_keeps_local_sources_enabled_but_clears_result(self):
        pair, scene, steps = fixture()
        _, frames, _, _ = playback.build_timeline(pair, scene, 9, steps)
        empty = next(f for f in frames if f['source_step']['name'] == playback.NAMES[1])
        view = empty['state']['snapshot']['fusion']
        self.assertEqual(view['observations'], [])
        self.assertNotIn('position', view)
        self.assertEqual(empty['assets'], {})
        self.assertIn('remain enabled', view['basis'])
        self.assertIn('not clearance', view['reason'])
        self.assertEqual(steps[1]['control']['request']['sensors'], list(model.LOCAL_SENSORS))

    def test_expiry_step_must_follow_the_original_gas_deadline_too(self):
        pair, scene, steps = fixture()
        ns = 3_100_000_000  # Hazard expired at 3 s, but original B gas lasts to 4 s.
        middle = steps[1]
        middle['simulation_time_ns'] = middle['presentation']['simulation_time_ns'] = ns
        middle['reader']['sample_time'] = instant(ns)
        for key in ('request', 'response'):
            middle['control'][key]['simulation_time_ns'] = ns
        with self.assertRaisesRegex(model.Refused, 'gas deadline'):
            playback.build_timeline(pair, scene, 9, steps)

        pair, scene, steps = fixture()
        steps[0]['correspondence']['gas_sample_valid_until_unix_ns'] -= 1
        with self.assertRaisesRegex(model.Refused, 'correspondence'):
            playback.build_timeline(pair, scene, 9, steps)

    def test_self_consistent_same_agent_gas_cannot_be_relabelled_remote(self):
        pair, _ = pair_fixture()
        pair, scene, steps = fixture(pair['nodes'][0]['public']['principal'])
        with self.assertRaises(model.Refused):
            playback.build_timeline(pair, scene, 9, steps)

    def test_original_gas_fusion_and_receiver_bytes_must_match(self):
        for change in ('gas_bytes', 'input_bytes', 'reference', 'payload', 'ack', 'correspondence'):
            with self.subTest(change=change):
                pair, scene, steps = fixture()
                first = steps[0]
                origin = next(iter(first['origins'].values()))['publication']
                gas_input = next(p for p in origin['inputs'] if p['reference']['key']['source']['stream'] == 'gas-1')
                if change == 'gas_bytes':
                    first['gas_origin']['signed_record_json'] = encoded(b'changed')
                elif change == 'input_bytes':
                    gas_input['envelope_json'] = encoded(b'changed')
                elif change == 'reference':
                    gas_input['reference']['version']['sequence'] += 1
                elif change == 'payload':
                    first['reader']['hazards'][0]['payload']['physical_valid_until'] = '2026-11-12T12:00:06Z'
                elif change == 'ack':
                    origin['signed_record_json'] = encoded(b'changed')
                else:
                    first['correspondence']['independent_crypto_reverification'] = True
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_waiting_scope_and_expired_results_are_not_ready(self):
        for field, value in (('status', 'waiting_for_source'), ('reader_generation', 8), ('run_id', 'other'),
                             ('sample_time', instant(3_000_000_000)), ('raw_inputs_independently_verified', True)):
            with self.subTest(field=field):
                pair, scene, steps = fixture()
                steps[0]['reader'][field] = value
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_requires_recorded_gas_only_controls_and_shared_clock(self):
        for change in ('all_off', 'gas_on', 'clock', 'presentation', 'images'):
            with self.subTest(change=change):
                pair, scene, steps = fixture()
                if change == 'all_off':
                    steps[1]['control']['request']['sensors'] = []
                    steps[1]['control']['response']['sensors'] = []
                elif change == 'gas_on':
                    steps[1]['control']['request']['sensors'] = list(model.SENSORS)
                    steps[1]['control']['response']['sensors'] = list(model.SENSORS)
                elif change == 'clock':
                    steps[0]['control']['source_clock_shared'] = False
                elif change == 'presentation':
                    steps[1]['presentation']['camera_sources'][0]['enabled'] = False
                else:
                    steps[0]['images']['camera-a'] += b'changed'
                with self.assertRaises(model.Refused):
                    playback.build_timeline(pair, scene, 9, steps)

    def test_assemble_validates_retained_local_expiry_captures_and_native_format(self):
        pair, scene, steps = fixture()
        with tempfile.TemporaryDirectory() as tmp:
            source, out = Path(tmp) / 'run', Path(tmp) / 'playback'
            source.mkdir()
            write_run(source, pair, scene, steps)
            report = playback.assemble(source, out)
            self.assertEqual(report['recorded_steps'], 3)
            self.assertEqual(report['live_commands'], 0)
            self.assertFalse(report['new_crypto_verification'])
            self.assertFalse(report['receiver_raw_inputs_independently_verified'])
            self.assertTrue(report['gas_original_bytes_matched_to_fusion_inputs'])
            self.assertIn('steps/0002-camera-a.json', report['inputs'])
            self.assertEqual(len(report['originals']), 4)
            for digest, metadata in report['originals'].items():
                raw = (out / 'originals' / (digest + '.json')).read_bytes()
                self.assertEqual(model.digest(raw), metadata['sha256'])
            path = Path(__file__).resolve().parents[2] / 'integration/nativeapp/render.py'
            spec = importlib.util.spec_from_file_location('distributedfusion_promo_contract', path)
            module = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(module)
            _, _, frames, assets = module.timeline_inputs(out / 'timeline')
            self.assertEqual(len(frames), 5)
            self.assertTrue(assets)
            with self.assertRaises(model.Refused):
                playback.assemble(source, out)
            (source / 'result.json').write_bytes(model.json_bytes({'passed': False, 'gate_completed': True}))
            with self.assertRaises(model.Refused):
                playback.assemble(source, Path(tmp) / 'failed')
            self.assertFalse((Path(tmp) / 'failed').exists())


if __name__ == '__main__':
    unittest.main()

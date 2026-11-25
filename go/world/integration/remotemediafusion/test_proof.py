"""Unsigned structural fixtures. These tests perform no crypto or native transfer."""
import base64
from copy import deepcopy
import json
from types import SimpleNamespace
import unittest

from integration.remotemediafusion import model, proof
from integration.hazard.test_model import identity_fixture, pair_fixture
from integration.hazard.test_playback import PNG, encoded, instant


def signed(record):
    inner = json.dumps(record, indent=2).encode()
    raw = b' \n' + json.dumps({'record': encoded(inner), 'signature': encoded(b'unsigned-fixture')}).encode() + b'\n'
    return raw, inner


def request(pub):
    value = {key: deepcopy(pub[key]) for key in ('entity', 'component_type', 'stream', 'frame', 'provenance')}
    value['payload_json'] = pub['payload']
    value['ttl_ns'] = str(pub['ttl_ns'])
    value['timing'] = {'sample_unix_ns': str(proof.instant(pub['timing']['sample_time'])),
                       'clock_domain': pub['timing']['clock_domain'], 'error_known': True}
    for key in ('error_bound_ns', 'age_hint_ns'):
        if pub['timing'][key]:
            value['timing'][key] = str(pub['timing'][key])
    value['provenance']['origin'] = 'ORIGIN_SIMULATED'
    value['provenance']['simulation']['seed'] = str(value['provenance']['simulation']['seed'])
    return value


def fixture(camera_valid=60_000_000_000, error=20_000_000, age=40_000_000):
    pair, _ = pair_fixture(); scene = identity_fixture(); ns = 1_000_000_000
    a, b = [row['public']['principal'] for row in pair['nodes']]
    tasks = {'a/fusion': {'generation': 8}, 'b/reader': {'generation': 9}}
    refs, proofs, inputs, media, receipts = [], [], [], [], {}
    for sensor in sorted(model.SENSORS):
        kind = sensor.split('-')[0]; node, owner = ('b', b) if sensor in model.REMOTE_SENSORS else ('a', a)
        tasks[node + '/' + sensor] = {'generation': 7}
        source = {'device': owner, 'app': model.APP, 'service': sensor, 'stream': sensor}
        sample = {'schema_version': 'wendy.sensor.sample.v1', 'id': scene['run_id'] + '/' + sensor + '/1',
                  'sensor_id': sensor, 'kind': kind, 'sequence': 1, 'sample_ns': ns,
                  'run_id': scene['run_id'], 'site_id': scene['site'], 'frame': scene['frame'],
                  'package_sha256': scene['package_sha256'], 'scenario_version': scene['scenario_version'],
                  'seed': scene['seed'], 'sensor_model': 'test-' + kind, 'calibration_revision': 'calibration-1',
                  'position_m': [1, .7, 2], 'uncertainty_m': .02}
        if kind == 'camera':
            sample['camera'] = {'width': 1, 'height': 1, 'fx': 1, 'fy': 1, 'cx': .5, 'cy': .5,
                'camera_to_site': [1, 0, 0, 1, 0, 1, 0, .7, 0, 0, 1, 2, 0, 0, 0, 1],
                'calibration_valid_for_ns': camera_valid}
        elif kind == 'audio':
            sample['audio'] = {'duration_ns': 250_000_000, 'sample_rate_hz': 8000, 'channels': 1}
        elif kind == 'lidar':
            sample['lidar'] = {'points_m': [[1, .7, 2]], 'max_range_m': 6.1}
        else:
            sample['gas'] = {'value_ppm': 90, 'uncertainty_ppm': 1.5, 'species': 'synthetic-test-gas'}
        content = PNG if kind == 'camera' else b'RIFF-explicit-structural-fixture' if kind == 'audio' else None
        if content:
            sample['evidence'] = {'sha256': model.digest(content), 'size': len(content),
                                  'content_type': 'image/png' if kind == 'camera' else 'audio/wav'}
            media.append({'source': {**source, 'component_type': model.CAPTURE},
                          'reference': sample['evidence'], 'data': encoded(content)})
        capture = {'version': 1, 'scene_boot_id': scene['boot_id'], 'simulation_anchor_unix_ns': model.ANCHOR,
                   'captured_unix_ns': 1794484800000000000, 'sample_json': encoded(model.json_bytes(sample))}
        pub = {'entity': 'sensor/' + sensor, 'component_type': model.CAPTURE, 'stream': sensor, 'frame': scene['frame'],
               'timing': {'sample_time': instant(ns), 'clock_domain': scene['clock_domain'], 'error_known': True,
                          'error_bound_ns': error, 'age_hint_ns': age}, 'ttl_ns': 3_000_000_000,
               'payload': encoded(model.json_bytes(capture)), 'tombstone': False,
               'provenance': {'origin': 'simulated', 'app_version': 'world-sensor-capture-v1', 'model_id': sample['sensor_model'],
                   'model_version': scene['scenario_version'], 'simulation': {'run_id': scene['run_id'],
                   'scenario_version': scene['scenario_version'], 'seed': scene['seed'], 'clock_domain': scene['clock_domain']}}}
        record = {'epoch': 1, 'sequence': 1, 'scope': {'tenant': pair['tenant'], 'site': scene['site'],
                  'device': owner, 'app': model.APP, 'service': sensor, 'deployment_generation': 7,
                  'execution_domain': 'simulation'}, 'publication': pub}
        raw, inner = signed(record)
        ref = {'key': {'tenant': pair['tenant'], 'site': scene['site'], 'entity': 'sensor/' + sensor,
                      'type': model.CAPTURE, 'source': source}, 'version': {'epoch': 1, 'sequence': 1},
               'envelope_sha256': model.digest(raw), 'record_sha256': model.digest(inner)}
        refs.append(ref); proofs.append({'reference': ref, 'envelope_json': encoded(raw)})
        until, _, start = proof.deadlines(pub, sample, capture)
        item = {'sensor_id': sensor, 'kind': kind, 'sample_id': sample['id'], 'source': source, 'version': ref['version'],
                'sample_time': instant(ns), 'capture_start': instant(start - model.ANCHOR),
                'valid_until': instant(until - model.ANCHOR), 'capture': {'proof_sha256': ref['envelope_sha256'],
                    'scene_boot_id': scene['boot_id'], 'simulation_anchor': instant(0)}}
        if kind == 'lidar':
            item['lidar_summary'] = {'point_count': 1, 'max_range_m': 6.1, 'normalized_sample_sha256': 'd' * 64}
        else:
            item['sample'] = sample
        if content:
            item['evidence'] = sample['evidence']
        inputs.append(item)
        if sensor in model.REMOTE_SENSORS:
            receipts[sensor] = {'capture_id': sample['id'], 'native_sequence': 1, 'sample_ns': ns,
                'scene_boot_id': scene['boot_id'], 'captured_unix_ns': capture['captured_unix_ns'],
                'evidence_sha256': sample['evidence']['sha256'], 'request_json': encoded(model.json_bytes(request(pub))),
                'signed_record_json': encoded(raw), 'proof_sha256': ref['envelope_sha256'], 'record_sha256': ref['record_sha256']}
    hazard = {'inputs': inputs, 'label': 'possible-gas-hazard', 'position_m': [1, .7, 2],
              'valid_until': min(inputs, key=lambda i: proof.instant(i['valid_until']))['valid_until'],
              'physical_valid_until': '2026-11-12T12:00:05Z'}
    start = min(proof.instant(i['capture_start']) for i in inputs)
    generic = {'version': 1, 'result_schema': 'wendy.hazard.v1', 'result_json': encoded(model.json_bytes(hazard)), 'inputs': refs}
    record = {'epoch': 2, 'sequence': 1, 'scope': {'tenant': pair['tenant'], 'site': scene['site'], 'device': a,
              'app': model.APP, 'service': 'fusion', 'deployment_generation': 8, 'execution_domain': 'simulation'},
              'publication': {'entity': scene['run_id'] + '/possible-gas-hazard', 'component_type': model.RESULT,
                  'stream': model.STREAM, 'frame': scene['frame'], 'tombstone': False,
                  'timing': {'sample_time': instant(start - model.ANCHOR), 'clock_domain': scene['clock_domain']},
                  'ttl_ns': proof.instant(hazard['valid_until']) - start, 'payload': encoded(model.json_bytes(generic))}}
    raw, inner = signed(record); digest = model.digest(raw)
    origins = {digest: {'publication': {'status': 'complete', 'locally_verified': True, 'run_id': scene['run_id'],
        'signed_record_json': encoded(raw), 'proof_sha256': digest, 'record_sha256': model.digest(inner),
        'inputs': proofs, 'media': media}}}
    reader = {'version': 1, 'tick': 3, 'status': 'ready', 'run_id': scene['run_id'], 'reader_generation': 9,
              'sample_time': instant(ns), 'origin_attested_inference': True, 'raw_inputs_independently_verified': False,
              'hazards': [{'envelope_json': encoded(raw), 'envelope_sha256': digest, 'record_sha256': model.digest(inner),
                          'version': {'epoch': 2, 'sequence': 1}, 'payload': hazard}]}
    runner = SimpleNamespace(pair=pair, scene=scene, inputs={'run_id': scene['run_id'], 'anchor_unix_ns': model.ANCHOR}, tasks=tasks)
    return reader, origins, receipts, runner, ns


def publication(args):
    return next(iter(args[1].values()))['publication']


def rewrite_hazard(args, change):
    """Reseal structural bytes so negative cases reach lineage checks, not hash checks."""
    reader, origins = args[:2]; pub = publication(args)
    old = reader['hazards'][0]
    record = json.loads(base64.b64decode(json.loads(base64.b64decode(old['envelope_json']))['record']))
    generic = json.loads(base64.b64decode(record['publication']['payload']))
    change(old['payload'])
    generic['result_json'] = encoded(model.json_bytes(old['payload']))
    record['publication']['payload'] = encoded(model.json_bytes(generic))
    raw, inner = signed(record); digest = model.digest(raw)
    old.update(envelope_json=encoded(raw), envelope_sha256=digest, record_sha256=model.digest(inner))
    pub.update(signed_record_json=encoded(raw), proof_sha256=digest, record_sha256=model.digest(inner))
    origins.clear(); origins[digest] = {'publication': pub}


class CorrespondenceTests(unittest.TestCase):
    def test_exact_remote_media_chain_and_original_deadlines(self):
        args = fixture(); before = deepcopy(args[:3])
        checked = proof.correspondence(*args)
        self.assertEqual(args[:3], before)
        self.assertEqual(checked['remote_sample_valid_until_unix_ns'],
                         {'camera-b': model.ANCHOR + 3_960_000_000, 'audio-1': model.ANCHOR + 3_710_000_000})
        self.assertFalse(checked['independent_crypto_reverification'])
        self.assertFalse(checked['receiver_raw_inputs_independently_verified'])
        self.assertEqual(checked['hazard_envelope_sha256'], checked['reader_envelope_sha256'])
        for sensor, row in checked['remote_inputs'].items():
            self.assertEqual(row['b_envelope_sha256'], row['a_input_envelope_sha256'])
            self.assertEqual(row['evidence']['sha256'], args[2][sensor]['evidence_sha256'])
            self.assertEqual(row['physical_valid_until_unix_ns'], 1794484805000000000)

    def test_camera_calibration_and_error_margin_shorten_original_bound(self):
        checked = proof.correspondence(*fixture(camera_valid=1_000_000_000, error=80_000_000, age=40_000_000))
        self.assertEqual(checked['remote_sample_valid_until_unix_ns']['camera-b'], model.ANCHOR + 1_920_000_000)
        self.assertEqual(checked['remote_sample_valid_until_unix_ns']['audio-1'], model.ANCHOR + 3_670_000_000)

    def test_fetched_bytes_reference_and_owner_are_exact(self):
        for change in ('bytes', 'digest', 'size', 'mime', 'device', 'missing', 'duplicate'):
            args = fixture(); rows = publication(args)['media']; row = next(r for r in rows if r['source']['service'] == 'camera-b')
            if change == 'bytes': row['data'] = encoded(b'different')
            elif change == 'digest': row['reference']['sha256'] = 'f' * 64
            elif change == 'size': row['reference']['size'] += 1
            elif change == 'mime': row['reference']['content_type'] = 'image/jpeg'
            elif change == 'device': row['source']['device'] = args[3].pair['nodes'][0]['public']['principal']
            elif change == 'missing': rows.remove(row)
            else: rows[0] = deepcopy(row)
            with self.subTest(change=change), self.assertRaises(model.Refused): proof.correspondence(*args)

    def test_original_b_receipt_metadata_and_exact_bytes_are_required(self):
        for field, value in [('signed_record_json', encoded(b'{}')), ('proof_sha256', 'f' * 64),
                             ('record_sha256', 'f' * 64), ('native_sequence', 2), ('capture_id', 'different'),
                             ('sample_ns', 2_000_000_000), ('scene_boot_id', 'different'),
                             ('captured_unix_ns', 1794484801000000000), ('evidence_sha256', 'e' * 64)]:
            args = fixture(); args[2]['camera-b'][field] = value
            with self.subTest(field=field), self.assertRaises(model.Refused): proof.correspondence(*args)

    def test_protojson_payload_and_timing_cannot_be_replaced_or_restamped(self):
        for change in ('payload', 'renamed', 'time', 'ttl', 'frame', 'ancestry'):
            args = fixture(); receipt = args[2]['audio-1']; value = json.loads(base64.b64decode(receipt['request_json']))
            if change == 'payload': value['payload_json'] = encoded(b'{}')
            elif change == 'renamed': value['payload'] = value.pop('payload_json')
            elif change == 'time': value['timing']['sample_unix_ns'] = str(model.ANCHOR + 2_000_000_000)
            elif change == 'ttl': value['ttl_ns'] = '4000000000'
            elif change == 'frame': value['frame']['revision'] = 'different'
            else: value['provenance']['simulation']['seed'] = '8'
            receipt['request_json'] = encoded(model.json_bytes(value))
            with self.subTest(change=change), self.assertRaises(model.Refused): proof.correspondence(*args)

    def test_derived_calibration_source_time_and_caps_are_not_rewritten(self):
        changes = [lambda h: h['inputs'][0]['source'].update(device='foreign'),
                   lambda h: h['inputs'][0]['version'].update(sequence=2),
                   lambda h: h['inputs'][0].update(capture_start=instant(0)),
                   lambda h: h['inputs'][0]['capture'].update(scene_boot_id='different'),
                   lambda h: h['inputs'][1]['sample']['camera'].update(fx=9),
                   lambda h: h['inputs'][1]['sample'].update(calibration_revision='different'),
                   lambda h: h.update(physical_valid_until='2026-11-12T12:00:06Z')]
        for index, change in enumerate(changes):
            args = fixture(); rewrite_hazard(args, change)
            with self.subTest(index=index), self.assertRaises(model.Refused): proof.correspondence(*args)

    def test_source_generation_and_record_reference_are_bound(self):
        for change in ('generation', 'version', 'record', 'envelope', 'order'):
            args = fixture(); rows = publication(args)['inputs']
            if change == 'generation': args[3].tasks['b/camera-b']['generation'] += 1
            elif change == 'version': rows[0]['reference']['version']['sequence'] += 1
            elif change == 'record': rows[0]['reference']['record_sha256'] = 'f' * 64
            elif change == 'envelope': rows[0]['envelope_json'] = encoded(b'{}')
            else: rows.reverse()
            with self.subTest(change=change), self.assertRaises(model.Refused): proof.correspondence(*args)

    def test_reader_waiting_changed_payload_or_origin_unknown_cannot_pass(self):
        for change in ('waiting', 'empty', 'generation', 'time', 'payload', 'unknown'):
            args = fixture(); reader = args[0]
            if change == 'waiting': reader['status'] = 'waiting_for_source'
            elif change == 'empty': reader['hazards'] = []
            elif change == 'generation': reader['reader_generation'] = 10
            elif change == 'time': reader['sample_time'] = instant(2_000_000_000)
            elif change == 'payload': reader['hazards'][0]['payload']['label'] = 'changed'
            else: publication(args)['status'] = 'unknown_outcome'
            with self.subTest(change=change), self.assertRaises(model.Refused): proof.correspondence(*args)


if __name__ == '__main__':
    unittest.main()

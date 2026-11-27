"""Unsigned structural receipts exercise correspondence, not runtime or crypto."""
import base64
from copy import deepcopy
import json
import unittest

from integration.distributedfusion.test_playback import publication, signed_bytes
from integration.hazard.test_model import identity_fixture, pair_fixture
from integration.hazard.test_playback import encoded, instant
from integration.hazard.model import digest, json_bytes, Refused
from integration.nativesensorfusion.proof import check_native_correspondence


def decode(value):
    return json.loads(base64.b64decode(value))


def fixture():
    pair, _ = pair_fixture()
    scene = identity_fixture()
    device = pair['nodes'][0]['public']['principal']
    row, origins, _, _, captures, _ = publication(pair, scene, 1_000_000_000, 1, device)
    published = next(iter(origins.values()))['publication']
    source = deepcopy(captures['camera-b'])
    capture = source['capture']
    sample = decode(capture['sample_json'])
    sample.update(scenario_version=scene['scenario_version'], seed=scene['seed'],
                  calibration_revision='fixture-calibration',
                  camera={'width': 1, 'height': 1, 'camera_to_site': list(range(16))})
    capture['sample_json'] = encoded(json_bytes(sample))
    # Deliberately uninterpreted image bytes: JPEG validity is checked by the
    # native adapter/client. This fixture tests exact byte ancestry only.
    jpeg = b'structural-original-jpeg'
    jpeg_ref = {'sha256': digest(jpeg), 'content_type': 'image/jpeg', 'size': len(jpeg)}
    metadata = {'version': 1, 'capture_json': encoded(json_bytes(capture)), 'channel_id': 4,
                'frame_sequence': 1, 'timestamp_us': capture['captured_unix_ns'] // 1000,
                'flags': 1, 'jpeg_sha256': digest(jpeg), 'jpeg_size': len(jpeg)}
    bound = deepcopy(capture)
    bound['sensorlink'] = {'protocol': 'wendylite.v1.sensorlink', 'channel_id': 4,
        'frame_sequence': 1, 'timestamp_us': metadata['timestamp_us'], 'flags': 1,
        'scene_boot_id': capture['scene_boot_id'], 'capture_sequence': sample['sequence'],
        'captured_unix_ns': capture['captured_unix_ns'],
        'sample_sha256': digest(base64.b64decode(capture['sample_json'])),
        'calibration_sha256': 'c' * 64, 'evidence': jpeg_ref}
    proof = next(p for p in published['inputs'] if p['reference']['key']['source']['stream'] == 'camera-b')
    record = decode(decode(proof['envelope_json'])['record'])
    record['scope']['deployment_generation'] = 3
    record['publication']['payload'] = encoded(json_bytes(bound))
    record['publication']['provenance'] = {'origin': 'simulated', 'simulation': {
        'run_id': scene['run_id'], 'scenario_version': scene['scenario_version'],
        'seed': scene['seed'], 'clock_domain': scene['clock_domain']}}
    envelope, inner = signed_bytes(record)
    ref = proof['reference']
    ref.update(envelope_sha256=digest(envelope), record_sha256=digest(inner))
    proof['envelope_json'] = encoded(envelope)
    hazard = row['payload']
    item = next(i for i in hazard['inputs'] if i['sensor_id'] == 'camera-b')
    item['sample'] = deepcopy(sample)
    item['sample']['evidence'] = jpeg_ref
    item['evidence'] = jpeg_ref
    item['capture'].update(proof_sha256=digest(envelope), original_evidence=sample['evidence'])
    result_record = decode(decode(row['envelope_json'])['record'])
    result_record['scope']['deployment_generation'] = 4
    generic = decode(result_record['publication']['payload'])
    generic['inputs'] = [p['reference'] for p in published['inputs']]
    generic['result_json'] = encoded(json_bytes(hazard))
    result_record['publication']['payload'] = encoded(json_bytes(generic))
    result_raw, result_inner = signed_bytes(result_record)
    row.update(envelope_json=encoded(result_raw), envelope_sha256=digest(result_raw),
               record_sha256=digest(result_inner))
    published.update(signed_record_json=encoded(result_raw), proof_sha256=digest(result_raw),
                     record_sha256=digest(result_inner))
    origins = {digest(result_raw): {'publication': published}}
    source_id = 'sensorlink:' + device + ':4'
    native = {k: v for k, v in metadata.items() if k != 'capture_json'}
    native.update(source_id=source_id, source_session_id='native-session-1', transport='tcp',
                  timestamp_basis='unknown', codec='mjpeg', width=1, height=1,
                  receipt={'boot_id': 'gateway-boot', 'before_boot_ns': 100,
                           'received_boot_ns': 105, 'after_boot_ns': 110})
    ack = {'capture_id': sample['id'], 'native_sequence': sample['sequence'],
           'scene_boot_id': capture['scene_boot_id'], 'captured_unix_ns': capture['captured_unix_ns'],
           'sample_ns': sample['sample_ns'], 'evidence_sha256': digest(jpeg),
           'request_json': encoded(json_bytes({'payload_json': record['publication']['payload']})),
           'signed_record_json': encoded(envelope), 'record_sha256': digest(inner),
           'proof_sha256': digest(envelope)}
    bridge = {'version': 1, 'mode': 'native-bridge', 'status': 'complete', 'run_id': scene['run_id'],
              'sensor_id': 'camera-b', 'receipts': [ack],
              'native_frames': [{'metadata': native, 'proof_sha256': digest(envelope)}]}
    expected = {'run_id': scene['run_id'], 'scene_boot_id': scene['boot_id'],
        'package_sha256': scene['package_sha256'], 'anchor_unix_ns': scene['anchor_unix_ns'],
        'frame': scene['frame'], 'site': scene['site'], 'sample_ns': 1_000_000_000,
        'source_id': source_id, 'transport': 'tcp', 'channel_id': 4, 'tenant': pair['tenant'],
        'device': device, 'app': record['scope']['app'], 'camera_service': 'camera-b',
        'camera_generation': 3, 'fusion_generation': 4, 'reader_generation': 9}
    reader = {'status': 'ready', 'run_id': scene['run_id'], 'reader_generation': 9,
              'sample_time': instant(1_000_000_000), 'hazards': [row],
              'origin_attested_inference': True, 'raw_inputs_independently_verified': False}
    return dict(expected=expected, source_capture=source, source_metadata=metadata,
                native_jpeg=jpeg, bridge=bridge, origins=origins, reader=reader)


class ProofTests(unittest.TestCase):
    def test_preserves_source_bytes_native_receipt_and_remote_original(self):
        values = fixture()
        before = deepcopy(values)
        result = check_native_correspondence(**values)
        self.assertEqual(values, before)
        self.assertTrue(result['exact_bytes_matched'])
        self.assertEqual(result['native_jpeg_sha256'], digest(values['native_jpeg']))
        self.assertNotEqual(result['native_jpeg_sha256'], result['source_png_sha256'])
        self.assertEqual(result['timestamp_basis'], 'unknown')
        self.assertFalse(result['physical_capture_time_established'])
        self.assertFalse(result['independent_crypto_reverification'])

    def test_cannot_substitute_capture_image_or_calibration(self):
        for change in ('jpeg', 'png', 'timestamp', 'calibration'):
            with self.subTest(change=change):
                values = fixture()
                if change == 'jpeg':
                    values['native_jpeg'] += b'changed'
                elif change == 'png':
                    values['source_capture']['media'] = encoded(b'other PNG')
                elif change == 'timestamp':
                    values['source_metadata']['timestamp_us'] += 1
                else:
                    capture = values['source_capture']['capture']
                    sample = decode(capture['sample_json'])
                    sample['camera']['camera_to_site'][3] += 1
                    capture['sample_json'] = encoded(json_bytes(sample))
                with self.assertRaises(Refused):
                    check_native_correspondence(**values)

    def test_requires_original_proto_request_and_exact_acknowledged_envelope(self):
        for change in ('request', 'envelope', 'duplicate'):
            with self.subTest(change=change):
                values = fixture()
                ack = values['bridge']['receipts'][0]
                if change == 'request':
                    ack['request_json'] = encoded(json_bytes({'payload_json': encoded(b'{}')}))
                elif change == 'envelope':
                    ack['signed_record_json'] = encoded(b'changed envelope')
                else:
                    values['bridge']['receipts'].append(deepcopy(ack))
                with self.assertRaises(Refused):
                    check_native_correspondence(**values)

    def test_native_clock_and_scope_cannot_be_relabelled(self):
        for change in ('basis', 'receipt', 'source', 'generation'):
            with self.subTest(change=change):
                values = fixture()
                native = values['bridge']['native_frames'][0]['metadata']
                if change == 'basis':
                    native['timestamp_basis'] = 'utc'
                elif change == 'receipt':
                    native['receipt']['received_boot_ns'] = 111
                elif change == 'source':
                    native['source_id'] += '/other'
                else:
                    values['expected']['camera_generation'] += 1
                with self.assertRaises(Refused):
                    check_native_correspondence(**values)

    def test_remote_state_must_match_acknowledged_result_and_original_clock(self):
        for change in ('payload', 'clock', 'verification'):
            with self.subTest(change=change):
                values = fixture()
                if change == 'payload':
                    values['reader']['hazards'][0]['payload']['confidence'] = .1
                elif change == 'clock':
                    values['reader']['sample_time'] = instant(2_000_000_000)
                else:
                    values['reader']['raw_inputs_independently_verified'] = True
                with self.assertRaises(Refused):
                    check_native_correspondence(**values)


if __name__ == '__main__':
    unittest.main()

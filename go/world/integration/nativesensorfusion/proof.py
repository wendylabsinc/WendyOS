"""Exact native-frame → simulated capture → fusion → remote-read correspondence.

This checks retained original bytes and observed scope, not cryptographic
signatures or current liveness. The ordinary applications perform those checks.
Gateway receipt time is retained as receipt time and never used to reconstruct
the MuJoCo sample, capture time, source timestamp or camera calibration.
"""
import base64
from copy import deepcopy

from integration.hazard.model import digest, digest_shape, json_bytes, require, strict_json
from integration.hazard.scenario import instant, original_matches

CAPTURE = 'wendy.sensor.capture.v1'
RESULT = 'wendy.inference.result.v1'


def _bytes(value, limit=384 << 10):
    require(isinstance(value, str) and len(value) <= 4 * ((limit + 2) // 3), 'bounded original bytes required')
    try:
        raw = base64.b64decode(value, validate=True)
    except (ValueError, TypeError) as error:
        raise ValueError('invalid original byte encoding') from error
    require(0 < len(raw) <= limit and base64.b64encode(raw).decode() == value, 'canonical original bytes required')
    return raw


def _owned(value, limit=8 << 20):
    return strict_json(value if isinstance(value, bytes) else json_bytes(value), limit)


def _one(rows, predicate, label):
    require(isinstance(rows, list) and len(rows) <= 64, 'bounded ' + label + ' required')
    found = [row for row in rows if isinstance(row, dict) and predicate(row)]
    require(len(found) == 1, 'one exact ' + label + ' required')
    return found[0]


def _scope(record, expected, service, generation):
    scope = record['scope']
    wanted = {key: expected[key] for key in ('tenant', 'site', 'device', 'app')}
    wanted.update(service=service, deployment_generation=generation, execution_domain='simulation')
    require(all(scope.get(key) == value for key, value in wanted.items()), 'signed app scope differs')


def _record(raw):
    signed = strict_json(raw, 384 << 10)
    original = _bytes(signed['record'], 64 << 10)
    return strict_json(original, 64 << 10), original


def check_native_correspondence(*, expected, source_capture, source_metadata,
                               native_jpeg, bridge, origins, reader):
    """Check one active step using the terminal bridge and exact saved receipts.

    expected is built from observed commissioning/task metadata, independently
    of the receipts. source_capture is the exact camera-b HTTP response;
    source_metadata is its livesensorlink.Metadata response. native_jpeg is the
    original wire JPEG, distinct from source_capture's original PNG.
    """
    e = _owned(expected, 16 << 10)
    needed = {'run_id', 'scene_boot_id', 'package_sha256', 'anchor_unix_ns', 'frame', 'site',
              'sample_ns', 'source_id', 'transport', 'channel_id', 'tenant', 'device', 'app',
              'camera_service', 'camera_generation', 'fusion_generation', 'reader_generation'}
    require(set(e) == needed and e['camera_service'] == 'camera-b', 'exact observed native camera expectation required')
    for key in ('anchor_unix_ns', 'sample_ns', 'channel_id', 'camera_generation', 'fusion_generation', 'reader_generation'):
        require(type(e[key]) is int and e[key] > 0, 'positive observed expectation required')
    require(digest_shape(e['package_sha256']) and e['source_id'].startswith('sensorlink:') and
            e['transport'] in ('tcp', 'grpc', 'wendycom'), 'native source expectation differs')
    source = _owned(source_capture)
    metadata = _owned(source_metadata, 64 << 10)
    bridge, origins, reader = _owned(bridge, 32 << 20), _owned(origins), _owned(reader)
    require(isinstance(native_jpeg, bytes) and 0 < len(native_jpeg) <= 8 << 20, 'original wire JPEG required')
    require(set(source) == {'capture', 'media'}, 'original source response required')
    capture = source['capture']
    require(set(capture) == {'version', 'scene_boot_id', 'captured_unix_ns', 'simulation_anchor_unix_ns', 'sample_json'},
            'unbound original source capture required')
    source_sample = _bytes(capture['sample_json'], 32 << 10)
    sample = strict_json(source_sample, 32 << 10)
    png = _bytes(source['media'], 8 << 20)
    require(capture['version'] == 1 and capture['scene_boot_id'] == e['scene_boot_id'] and
            capture['simulation_anchor_unix_ns'] == e['anchor_unix_ns'] and
            type(capture['captured_unix_ns']) is int and capture['captured_unix_ns'] > 0 and
            capture['captured_unix_ns'] % 1000 == 0, 'original capture clocks differ')
    require(all(sample.get(key) == value for key, value in {
        'sensor_id': e['camera_service'], 'kind': 'camera', 'run_id': e['run_id'], 'site_id': e['site'],
        'frame': e['frame'], 'package_sha256': e['package_sha256'], 'sample_ns': e['sample_ns']}.items()),
        'original sample identity differs')
    require(type(sample['sequence']) is int and sample['sequence'] > 0 and
            sample['evidence'] == {'sha256': digest(png), 'content_type': 'image/png', 'size': len(png)},
            'original PNG lineage differs')
    require(strict_json(_bytes(metadata['capture_json'], 32 << 10), 32 << 10) == capture,
            'native metadata points to another original capture')
    require(metadata['version'] == 1 and metadata['channel_id'] == e['channel_id'] and
            type(metadata['frame_sequence']) is int and 0 < metadata['frame_sequence'] < 2**32 and
            metadata['timestamp_us'] == capture['captured_unix_ns'] // 1000 and metadata['flags'] == 1 and
            metadata['jpeg_sha256'] == digest(native_jpeg) and metadata['jpeg_size'] == len(native_jpeg),
            'native source JPEG or wire identity differs')

    require(reader.get('status') == 'ready' and reader.get('run_id') == e['run_id'] and
            reader.get('reader_generation') == e['reader_generation'] and
            instant(reader['sample_time']) == e['anchor_unix_ns'] + e['sample_ns'] and
            reader.get('origin_attested_inference') is True and
            reader.get('raw_inputs_independently_verified') is False, 'ready remote observation differs')
    hazards = original_matches(reader, origins)
    require(len(hazards) == 1, 'one exact native-path hazard required')
    result_sha = hazards[0]
    published = origins[result_sha]['publication']
    result, _ = _record(_bytes(published['signed_record_json']))
    _scope(result, e, 'fusion', e['fusion_generation'])
    require(result['publication']['component_type'] == RESULT and
            result['publication']['stream'] == 'hazard' and
            result['publication']['frame'] == e['frame'], 'signed result type or frame differs')
    generic = strict_json(_bytes(result['publication']['payload'], 32 << 10), 32 << 10)
    proofs = published['inputs']
    require([row['reference'] for row in proofs] == generic['inputs'], 'generic proof order differs')
    proof = _one(proofs, lambda row: row['reference']['key']['source']['stream'] == e['camera_service'], 'camera input')
    ref = proof['reference']
    expected_source = {'device': e['device'], 'app': e['app'], 'service': e['camera_service'], 'stream': e['camera_service']}
    require(ref['key'] == {'tenant': e['tenant'], 'site': e['site'], 'entity': 'sensor/' + e['camera_service'],
                          'type': CAPTURE, 'source': expected_source}, 'camera input reference scope differs')
    envelope = _bytes(proof['envelope_json'])
    record, record_bytes = _record(envelope)
    require(digest(envelope) == ref['envelope_sha256'] and digest(record_bytes) == ref['record_sha256'] and
            ref['version'] == {key: record[key] for key in ('epoch', 'sequence')}, 'camera original hash/version differs')
    _scope(record, e, e['camera_service'], e['camera_generation'])
    publication = record['publication']
    require(publication['component_type'] == CAPTURE and publication['entity'] == 'sensor/' + e['camera_service'] and
            publication['stream'] == e['camera_service'] and publication['frame'] == e['frame'] and
            publication['tombstone'] is False and
            instant(publication['timing']['sample_time']) == e['anchor_unix_ns'] + e['sample_ns'] and
            publication['timing']['clock_domain'] == 'simulation:' + e['run_id'], 'signed camera sample was restamped')
    provenance = publication['provenance']
    require(provenance['origin'] == 'simulated' and provenance['simulation'] == {
        'run_id': e['run_id'], 'scenario_version': sample['scenario_version'], 'seed': sample['seed'],
        'clock_domain': 'simulation:' + e['run_id']}, 'simulation camera was relabeled')
    bound = strict_json(_bytes(publication['payload'], 32 << 10), 32 << 10)
    require({key: value for key, value in bound.items() if key != 'sensorlink'} == capture,
            'published capture or calibration bytes differ from source')
    wire = bound['sensorlink']
    jpeg_ref = {'sha256': digest(native_jpeg), 'content_type': 'image/jpeg', 'size': len(native_jpeg)}
    require(wire['protocol'] == 'wendylite.v1.sensorlink' and wire['channel_id'] == metadata['channel_id'] and
            wire['frame_sequence'] == metadata['frame_sequence'] and wire['timestamp_us'] == metadata['timestamp_us'] and
            wire['flags'] == metadata['flags'] and wire['scene_boot_id'] == e['scene_boot_id'] and
            wire['capture_sequence'] == sample['sequence'] and wire['captured_unix_ns'] == capture['captured_unix_ns'] and
            wire['sample_sha256'] == digest(source_sample) and digest_shape(wire['calibration_sha256']) and
            wire['evidence'] == jpeg_ref, 'signed wire binding differs')

    require(bridge.get('version') == 1 and bridge.get('mode') == 'native-bridge' and bridge.get('status') == 'complete' and
            bridge.get('run_id') == e['run_id'] and bridge.get('sensor_id') == e['camera_service'],
            'complete ordinary native bridge required')
    ack = _one(bridge['receipts'], lambda row: row.get('proof_sha256') == ref['envelope_sha256'], 'camera acknowledgment')
    native = _one(bridge['native_frames'], lambda row: row.get('proof_sha256') == ref['envelope_sha256'], 'native frame receipt')['metadata']
    require(_bytes(ack['signed_record_json']) == envelope and ack['record_sha256'] == ref['record_sha256'] and
            ack['capture_id'] == sample['id'] and ack['native_sequence'] == sample['sequence'] and
            ack['scene_boot_id'] == capture['scene_boot_id'] and ack['captured_unix_ns'] == capture['captured_unix_ns'] and
            ack['sample_ns'] == sample['sample_ns'] and ack['evidence_sha256'] == jpeg_ref['sha256'],
            'ordinary camera acknowledgment differs from fuser input')
    request = strict_json(_bytes(ack['request_json'], 64 << 10), 64 << 10)
    require(_bytes(request['payload_json'], 32 << 10) == _bytes(publication['payload'], 32 << 10),
            'acknowledged request original differs')
    for key in ('channel_id', 'frame_sequence', 'timestamp_us', 'flags', 'jpeg_sha256', 'jpeg_size'):
        require(native[key] == metadata[key], 'native packet changed original ' + key)
    require(native['version'] == 1 and native['source_id'] == e['source_id'] and native['transport'] == e['transport'] and
            isinstance(native['source_session_id'], str) and bool(native['source_session_id']) and
            native['timestamp_basis'] == 'unknown' and native['codec'] == 'mjpeg' and
            native['width'] == sample['camera']['width'] and native['height'] == sample['camera']['height'],
            'native source/session/format differs')
    receipt = native['receipt']
    before, received, after = (receipt[key] for key in ('before_boot_ns', 'received_boot_ns', 'after_boot_ns'))
    require(all(type(value) is int for value in (before, received, after)) and 0 <= before <= after and
            received == before + (after - before) // 2 and bool(receipt['boot_id']), 'gateway receipt bracket differs')

    hazard = reader['hazards'][0]['payload']
    item = _one(hazard['inputs'], lambda row: row.get('sensor_id') == e['camera_service'], 'derived camera input')
    normalized = deepcopy(sample)
    normalized['evidence'] = jpeg_ref
    require(item['sample'] == normalized and item['source'] == expected_source and item['version'] == ref['version'] and
            instant(item['sample_time']) == e['anchor_unix_ns'] + e['sample_ns'] and item['evidence'] == jpeg_ref and
            item['capture']['proof_sha256'] == ref['envelope_sha256'] and
            item['capture']['scene_boot_id'] == e['scene_boot_id'] and
            instant(item['capture']['simulation_anchor']) == e['anchor_unix_ns'] and
            item['capture']['original_evidence'] == sample['evidence'], 'derived camera interpretation lost original lineage')
    return {'version': 1, 'run_id': e['run_id'], 'sample_ns': e['sample_ns'], 'source_id': e['source_id'],
            'source_session_id': native['source_session_id'], 'source_transport': native['transport'],
            'channel_id': native['channel_id'], 'frame_sequence': native['frame_sequence'], 'timestamp_us': native['timestamp_us'],
            'timestamp_basis': 'unknown', 'gateway_receipt': deepcopy(receipt),
            'source_capture_sample_sha256': digest(source_sample), 'source_png_sha256': digest(png),
            'native_jpeg_sha256': digest(native_jpeg), 'calibration_sha256': wire['calibration_sha256'],
            'camera_envelope_sha256': ref['envelope_sha256'], 'fuser_input_envelope_sha256': ref['envelope_sha256'],
            'hazard_envelope_sha256': result_sha, 'reader_envelope_sha256': result_sha,
            'exact_bytes_matched': True, 'simulation_provenance_preserved': True,
            'independent_crypto_reverification': False, 'physical_capture_time_established': False}

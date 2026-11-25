"""Retained byte/lineage correspondence; no independent cryptographic check."""
import base64
import binascii
import re

if __package__:
    from . import model
else:
    import model

from integration.hazard.scenario import instant, original_matches, reader_matches


def decode(value, limit=384 << 10):
    model.require(isinstance(value, str) and len(value) <= 4 * ((limit + 2) // 3), 'bounded encoded original required')
    raw = base64.b64decode(value, validate=True)
    model.require(0 < len(raw) <= limit, 'bounded original required')
    return raw


def integer(value, minimum=0, maximum=(1 << 63) - 1):
    model.require(type(value) is int and minimum <= value <= maximum, 'bounded original integer required')
    return value


def wire_integer(value):
    # protojson emits int64/uint64 as decimal strings, omitting zero defaults.
    model.require(isinstance(value, str) and re.fullmatch(r'0|[1-9][0-9]{0,19}', value) is not None,
                  'protojson decimal integer required')
    return integer(int(value), maximum=(1 << 64) - 1)


def check_request(receipt, publication):
    request = model.strict_json(decode(receipt['request_json'], 96 << 10), 96 << 10)
    model.require(set(request) <= {'entity', 'component_type', 'stream', 'frame', 'timing', 'ttl_ns',
                                  'provenance', 'payload_json', 'tombstone'}, 'capture request fields differ')
    model.require(all(request[key] == publication[key] for key in ('entity', 'component_type', 'stream', 'frame')) and
                  request.get('tombstone', False) is False and
                  decode(request['payload_json'], 32 << 10) == decode(publication['payload'], 32 << 10) and
                  wire_integer(request['ttl_ns']) == publication['ttl_ns'], 'original capture request differs')
    timing, original = request['timing'], publication['timing']
    model.require(set(timing) <= {'sample_unix_ns', 'clock_domain', 'error_known', 'error_bound_ns', 'age_hint_ns'} and
                  wire_integer(timing['sample_unix_ns']) == instant(original['sample_time']) and
                  timing['clock_domain'] == original['clock_domain'] and timing['error_known'] is True and
                  all(wire_integer(timing.get(key, '0')) == original[key] for key in ('error_bound_ns', 'age_hint_ns')),
                  'capture request timing differs')
    provenance = dict(request['provenance'])
    model.require(provenance['origin'] == 'ORIGIN_SIMULATED', 'capture request origin differs')
    provenance['origin'] = 'simulated'
    simulation = dict(provenance['simulation'])
    simulation['seed'] = wire_integer(simulation.get('seed', '0'))
    provenance['simulation'] = simulation
    model.require(provenance == publication['provenance'], 'capture request ancestry differs')


def deadlines(publication, sample, capture):
    timing = publication['timing']
    margin = max(integer(timing['error_bound_ns']), integer(timing['age_hint_ns']))
    ttl = integer(publication['ttl_ns'], 1, 24 * 3600 * 1_000_000_000)
    span = integer(sample['audio']['duration_ns'], 1) if sample['kind'] == 'audio' else 0
    model.require(ttl > margin + span, 'original capture interval has no lifetime')
    start = instant(timing['sample_time'])
    until = start + ttl - margin - span
    if sample['kind'] == 'camera':
        calibration = integer(sample['camera']['calibration_valid_for_ns'], 1)
        model.require(calibration > margin, 'original calibration has no lifetime')
        until = min(until, start + calibration - margin)
    physical = integer(capture['captured_unix_ns'], 1, (1 << 63) - 1 - 5_000_000_000) + 5_000_000_000
    return until, physical, start - span


def correspondence(reader, origins, remote_receipts, runner, ns):
    """Bind the ordinary apps' retained results without replaying their authority checks."""
    try:
        return _correspondence(reader, origins, remote_receipts, runner, ns)
    except (KeyError, TypeError, ValueError, OverflowError, binascii.Error) as exc:
        raise model.Refused('malformed remote-media correspondence') from exc


def _correspondence(reader, origins, remote_receipts, runner, ns):
    integer(ns, 1)
    model.require(set(remote_receipts) == set(model.REMOTE_SENSORS), 'both original B receipts required')
    model.require(reader_matches(reader, runner, ns, True) and len(reader['hazards']) == len(origins) == 1,
                  'one ready original hazard required')
    digests = original_matches(reader, origins)
    digest = digests[0]
    publication = origins[digest]['publication']
    model.require(publication['proof_sha256'] == digest and publication['run_id'] == runner.inputs['run_id'],
                  'fusion acknowledgment identity differs')
    signed = model.strict_json(decode(publication['signed_record_json']))
    record = model.strict_json(decode(signed['record'], 64 << 10), 64 << 10)
    generic = model.strict_json(decode(record['publication']['payload'], 32 << 10), 32 << 10)
    hazard = reader['hazards'][0]['payload']
    proofs, inputs = publication['inputs'], hazard['inputs']
    model.require(len(proofs) == len(inputs) == len(model.SENSORS) and
                  [p['reference'] for p in proofs] == generic['inputs'], 'complete ordered lineage required')
    a, b = [row['public']['principal'] for row in runner.pair['nodes']]
    scene, current = runner.scene, model.ANCHOR + ns
    scope = record['scope']
    model.require(all(scope[key] == value for key, value in {'tenant': runner.pair['tenant'], 'site': scene['site'],
                  'device': a, 'app': model.APP, 'service': 'fusion', 'execution_domain': 'simulation',
                  'deployment_generation': runner.tasks['a/fusion']['generation']}.items()), 'fusion origin scope differs')
    output = record['publication']
    model.require(output['component_type'] == model.RESULT and output['stream'] == model.STREAM and
                  output['entity'] == scene['run_id'] + '/possible-gas-hazard' and output['frame'] == scene['frame'] and
                  output['tombstone'] is False and output['timing']['clock_domain'] == scene['clock_domain'],
                  'hazard publication scope differs')
    media_rows = publication['media']
    model.require(isinstance(media_rows, list) and len(media_rows) == 3, 'three actual fetched media receipts required')
    media, total = {}, 0
    for row in media_rows:
        source = row['source']; sensor = source['service']
        model.require(sensor in (*model.CAMERAS, 'audio-1') and sensor not in media, 'one media receipt per exact owner required')
        owner = b if sensor in model.REMOTE_SENSORS else a
        model.require(source == {'device': owner, 'app': model.APP, 'service': sensor,
                      'component_type': model.CAPTURE, 'stream': sensor}, 'fetched media owner differs')
        raw = decode(row['data'], 8 << 20); ref = row['reference']; total += len(raw)
        model.require(total <= 16 << 20 and integer(ref['size'], 1, 8 << 20) == len(raw) and
                      model.digest(raw) == ref['sha256'] and
                      ref['content_type'] == ('audio/wav' if sensor == 'audio-1' else 'image/png'),
                      'fetched original media differs')
        media[sensor] = ref
    source_devices, remote, sample_caps, physical_caps, starts = {}, {}, [], [], []
    for proof, used in zip(proofs, inputs):
        ref = proof['reference']; key, version = ref['key'], ref['version']; source = key['source']
        sensor = source['stream']
        model.require(sensor in model.SENSORS and sensor not in source_devices, 'one original per sensor required')
        node, owner = ('b', b) if sensor in model.REMOTE_SENSORS else ('a', a)
        model.require(source == {'device': owner, 'app': model.APP, 'service': sensor, 'stream': sensor} and
                      key['tenant'] == runner.pair['tenant'] and key['site'] == scene['site'] and
                      key['type'] == model.CAPTURE and key['entity'] == 'sensor/' + sensor, 'input source differs')
        raw = decode(proof['envelope_json']); signed_input = model.strict_json(raw)
        inner = decode(signed_input['record'], 64 << 10); original = model.strict_json(inner, 64 << 10)
        model.require(model.digest(raw) == ref['envelope_sha256'] and model.digest(inner) == ref['record_sha256'] and
                      version == {key: original[key] for key in ('epoch', 'sequence')} and
                      all(type(value) is int and value > 0 for value in version.values()), 'input original hash/version differs')
        model.require(all(original['scope'][key] == value for key, value in {'tenant': runner.pair['tenant'],
                      'site': scene['site'], 'device': owner, 'app': model.APP, 'service': sensor,
                      'execution_domain': 'simulation', 'deployment_generation': runner.tasks[node + '/' + sensor]['generation']}.items()),
                      'original source generation differs')
        pub = original['publication']; timing = pub['timing']
        model.require(pub['component_type'] == model.CAPTURE and pub['stream'] == sensor and
                      pub['entity'] == key['entity'] and pub['frame'] == scene['frame'] and pub['tombstone'] is False and
                      timing['error_known'] is True and timing['clock_domain'] == scene['clock_domain'] and
                      instant(timing['sample_time']) == current, 'original capture publication differs')
        capture = model.strict_json(decode(pub['payload'], 32 << 10), 32 << 10)
        sample = model.strict_json(decode(capture['sample_json'], 32 << 10), 32 << 10)
        model.require(capture['version'] == 1 and capture.get('sensorlink') is None and
                      capture['scene_boot_id'] == scene['boot_id'] and capture['simulation_anchor_unix_ns'] == model.ANCHOR and
                      sample['sample_ns'] == ns and sample['sensor_id'] == sensor and
                      sample['kind'] == sensor.split('-')[0] and sample['site_id'] == scene['site'] and
                      sample['frame'] == scene['frame'] and sample['run_id'] == scene['run_id'] == runner.inputs['run_id'] and
                      sample['package_sha256'] == scene['package_sha256'] and
                      sample['scenario_version'] == scene['scenario_version'] and sample['seed'] == scene['seed'],
                      'original simulation capture differs')
        ancestry = pub['provenance']
        model.require(ancestry['origin'] == 'simulated' and ancestry['model_id'] == sample['sensor_model'] and
                      ancestry['model_version'] == sample['scenario_version'] and ancestry['simulation'] == {
                          'run_id': scene['run_id'], 'scenario_version': scene['scenario_version'],
                          'seed': scene['seed'], 'clock_domain': scene['clock_domain']}, 'original simulation ancestry differs')
        until, physical, start = deadlines(pub, sample, capture)
        model.require(used['sensor_id'] == sensor and used['kind'] == sample['kind'] and used['sample_id'] == sample['id'] and
                      used['source'] == source and used['version'] == version and instant(used['sample_time']) == current and
                      instant(used['capture_start']) == start and current < instant(used['valid_until']) <= until and
                      used['capture'] == {'proof_sha256': ref['envelope_sha256'], 'scene_boot_id': scene['boot_id'],
                                          'simulation_anchor': used['capture']['simulation_anchor']} and
                      instant(used['capture']['simulation_anchor']) == model.ANCHOR, 'derived original lineage differs')
        if sample['kind'] == 'lidar':
            summary = used['lidar_summary']
            model.require('sample' not in used and summary['point_count'] == len(sample['lidar']['points_m']) and
                          summary['max_range_m'] == sample['lidar']['max_range_m'] and
                          model.digest_shape(summary['normalized_sample_sha256']), 'LiDAR summary differs')
        else:
            model.require(used['sample'] == sample, 'derived sample/calibration differs from original')
        if sensor in media:
            model.require(sample['evidence'] == used['evidence'] == media[sensor], 'derived fetched evidence differs')
        else:
            model.require(not sample.get('evidence') and not used.get('evidence'), 'inline source unexpectedly has media')
        if sensor in model.REMOTE_SENSORS:
            receipt = remote_receipts[sensor]
            model.require(decode(receipt['signed_record_json']) == raw and receipt['proof_sha256'] == ref['envelope_sha256'] and
                          receipt['record_sha256'] == ref['record_sha256'] and receipt['capture_id'] == sample['id'] and
                          receipt['native_sequence'] == sample['sequence'] and receipt['sample_ns'] == ns and
                          receipt['scene_boot_id'] == capture['scene_boot_id'] and
                          receipt['captured_unix_ns'] == capture['captured_unix_ns'] and
                          receipt['evidence_sha256'] == sample['evidence']['sha256'], 'B acknowledgment differs from A input/media')
            check_request(receipt, pub)
            remote[sensor] = {'reference': ref, 'b_envelope_sha256': receipt['proof_sha256'],
                             'a_input_envelope_sha256': ref['envelope_sha256'], 'evidence': media[sensor],
                             'capture_id': sample['id'], 'native_sequence': sample['sequence'],
                             'captured_unix_ns': capture['captured_unix_ns'], 'sample_ns': ns,
                             'calibration_revision': sample['calibration_revision'],
                             'sample_valid_until_unix_ns': until, 'physical_valid_until_unix_ns': physical}
        source_devices[sensor] = owner; sample_caps.append(instant(used['valid_until'])); physical_caps.append(physical); starts.append(start)
    model.require(current < instant(hazard['valid_until']) == min(sample_caps) and
                  instant(hazard['physical_valid_until']) <= min(physical_caps) and
                  instant(output['timing']['sample_time']) == min(starts) and
                  integer(output['ttl_ns'], 1) == instant(hazard['valid_until']) - min(starts), 'hazard original deadlines differ')
    return {'version': 1, 'source_devices': source_devices, 'remote_inputs': remote,
            'remote_sample_valid_until_unix_ns': {sensor: remote[sensor]['sample_valid_until_unix_ns'] for sensor in model.REMOTE_SENSORS},
            'hazard_envelope_sha256': digest, 'reader_envelope_sha256': digest, 'fuser_device': a, 'reader_device': b,
            'exact_bytes_matched': True, 'origin_attested_inference': True,
            'independent_crypto_reverification': False, 'receiver_raw_inputs_independently_verified': False}

"""Three explicit scene steps; final exact native-camera correspondence."""
import base64
import time

if __package__:
    from . import control, libraries, model
else:
    import control
    import libraries
    import model

from integration.hazard.scenario import instant, wait_value, reader_matches, original_matches
from integration.distributedfusion.scenario import recorded_reader_match

SOURCE_OUTPUT = model.TOOLS + '/native-source-output'


def wait_reader(node, runner, ns, present, budget=10):
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        response = node.python(control.READER_OBSERVATIONS,
            node.service_volume('reader', 'proofs') + '/stdout.json', check=False)
        if response.returncode == 0:
            value = recorded_reader_match(response.stdout, runner, ns, present)
            if value is not None:
                return value
        live = node.python('import os,sys; assert not os.path.exists(sys.argv[1])',
                           node.service_volume('reader', 'proofs') + '/exit.json', check=False)
        model.require(live.returncode == 0, 'reader ended before the requested original observation')
        time.sleep(.2)
    raise model.Refused('bounded original reader observation did not arrive')


def aggregate(node, directory, budget=10):
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        result = node.python(control.AGGREGATE, directory + '/stdout.json', check=False)
        ended = node.python("import json,sys; print(json.dumps(json.load(open(sys.argv[1]))))",
                            directory + '/exit.json', check=False)
        if ended.returncode == 0:
            status = model.strict_json(ended.stdout, 4096)
            model.require(status.get('succeeded') is True and status.get('exit_code') == 0 and
                          status.get('output_truncated') is False, 'native source or bridge terminated unsuccessfully')
            model.require(result.returncode == 0, 'complete native aggregate missing')
            return model.strict_json(result.stdout, 4 << 20)
        time.sleep(.2)
    raise model.Refused('bounded native terminal aggregate did not arrive')


def save_capture(runner, prefix, sensor, ns):
    raw = runner.nodes['a'].scene_request('/v1/captures/' + sensor)
    value = model.strict_json(raw, 4 << 20)
    sample = model.strict_json(base64.b64decode(value['capture']['sample_json'], validate=True))
    media = base64.b64decode(value['media'], validate=True)
    model.require(sample['sample_ns'] == ns and sample['sensor_id'] == sensor and
                  sample['run_id'] == runner.inputs['run_id'], 'source capture identity differs')
    path = prefix + '-' + sensor
    libraries.stage.write_new(runner.out / (path + '.json'), raw)
    media_path = None
    if sample.get('evidence'):
        ref = sample['evidence']
        model.require(model.digest(media) == ref['sha256'] and len(media) == ref['size'], 'source media differs')
        model.require(ref['content_type'] in ('image/png', 'audio/wav'), 'unexpected media type')
        media_path = path + ('.png' if ref['content_type'] == 'image/png' else '.wav')
        libraries.stage.write_new(runner.out / media_path, media)
    else:
        model.require(not media, 'inline sensor has media')
    return path + '.json', media_path


def execute(runner):
    a, b = runner.nodes['a'], runner.nodes['b']
    runner.step('source-readiness')
    runner.save('reader-readiness.json', wait_value(b, 'reader', lambda r: reader_matches(r, runner, 0, False), 15))
    runner.save('fusion-readiness.json', wait_value(a, 'fusion', lambda r:
        r.get('run_id') == runner.inputs['run_id'] and r.get('status') in ('waiting', 'insufficient_evidence'), 10))
    # Only initial native acquisition is bounded to five seconds. Release this
    # ordinary app immediately before the first explicit scene capture.
    a.write(a.service_volume(model.NATIVE_CAMERA) + '/go', b'')
    rows, active, first = [], [], set()
    ns = 1_000_000_000
    for index, name in enumerate(('native-camera-hazard', 'original-expiry', 'native-camera-restoration'), 1):
        sensors = list(model.SENSORS) if index != 2 else []
        runner.step('sample-' + str(index))
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        response = model.strict_json(a.scene_request('/v1/step', request))
        model.require(response == {'sequence': index, **request}, 'single source step differs; no mutation retry')
        clock = control.sample_clock(runner.inputs['run_id'], runner.inputs['anchor_unix_ns'], ns)
        b.atomic_write(model.config_path('b') + '/simclock.json', clock, previous=b.clock_raw)
        b.clock_raw = clock
        prefix = f'steps/{index:04d}'
        runner.save(prefix + '-control.json', {'request': request, 'response': response,
                    'receiver_clock': model.strict_json(clock), 'no_mutation_retry': True})
        reader = wait_reader(b, runner, ns, bool(sensors))
        runner.save(prefix + '-reader.json', reader)
        digests = [h['envelope_sha256'] for h in reader['hazards']]
        origins = {}
        if digests:
            raw = a.python(control.MATCH_PUBLICATIONS, a.service_volume('fusion', 'proofs') + '/stdout.json',
                           model.json_bytes(digests).decode()).stdout
            origins = model.strict_json(raw, 4 << 20)
            original_matches(reader, origins)
            runner.save(prefix + '-origin.json', origins)
            active.append((index, ns, reader, origins))
        if index == 1:
            first = set(digests)
        elif index == 3:
            model.require(first.isdisjoint(digests), 'restored result reused an original')
        presentation = a.scene_request('/v1/presentation')
        model.require(model.strict_json(presentation, 4 << 20)['simulation_time_ns'] == ns, 'presentation step differs')
        libraries.stage.write_new(runner.out / (prefix + '-presentation.json'), presentation)
        captures, media = {}, {}
        for sensor in sensors:
            captures[sensor], media_path = save_capture(runner, prefix, sensor, ns)
            if media_path:
                media[sensor] = media_path
        rows.append({'name': name, 'simulation_time_ns': ns, 'presentation': prefix + '-presentation.json',
            'control': prefix + '-control.json', 'reader': prefix + '-reader.json',
            'origin': prefix + '-origin.json' if digests else None, 'captures': captures, 'media': media,
            'original_envelope_sha256': digests, 'hazards': len(reader['hazards']),
            'reader_observation_semantics': 'live_at_original_tick_not_at_host_collection'})
        if index == 1:
            ns = max(instant(h['payload']['valid_until']) for h in reader['hazards']) - model.ANCHOR + 100_000_000
        else:
            ns += 100_000_000

    bridge = aggregate(a, a.service_volume(model.NATIVE_CAMERA, 'proofs'))
    source = aggregate(a, SOURCE_OUTPUT)
    model.require(bridge.get('mode') == 'native-bridge' and len(bridge.get('native_frames', [])) == 2 and
                  len(bridge.get('receipts', [])) == 2 and source.get('mode') == 'native-source' and
                  source.get('frames_sent') == 2, 'exact two-frame native session required')
    runner.save('native-bridge.json', bridge); runner.save('native-source-result.json', source)
    pairing = model.strict_json(runner.native_pair('status').stdout, 64 << 10)
    runner.save('native-pairing-final.json', pairing)
    from integration.nativesensorfusion.proof import check_native_correspondence
    for frame_number, (index, ns, reader, origins) in enumerate(active, 1):
        prefix = f'steps/{index:04d}'
        paths = {}
        for suffix, limit in (('.jpg', 8 << 20), ('.json', 64 << 10)):
            filename = f'frame-{frame_number:06d}' + suffix
            relative = 'native-frames/' + filename
            receipt = a.snapshot_file(SOURCE_OUTPUT + '/' + filename, runner.out / relative,
                                      limit, container=a.target())
            model.require(receipt.get('status') == 'captured', 'exact retained native frame unavailable')
            paths[suffix] = relative
        metadata = model.strict_json((runner.out / paths['.json']).read_bytes(), 64 << 10)
        model.require(source['frames'][frame_number-1]['metadata'] == metadata, 'source aggregate metadata differs')
        expected = {'run_id': runner.inputs['run_id'], 'scene_boot_id': runner.scene['boot_id'],
            'package_sha256': runner.scene['package_sha256'], 'anchor_unix_ns': model.ANCHOR,
            'frame': runner.scene['frame'], 'site': runner.scene['site'], 'sample_ns': ns,
            'source_id': runner.source_id, 'transport': 'tcp', 'channel_id': model.CHANNEL,
            'tenant': runner.pair['tenant'], 'device': runner.pair['nodes'][0]['public']['principal'],
            'app': model.APP, 'camera_service': model.NATIVE_CAMERA,
            'camera_generation': runner.tasks['a/camera-b']['generation'],
            'fusion_generation': runner.tasks['a/fusion']['generation'],
            'reader_generation': runner.tasks['b/reader']['generation']}
        source_capture = model.strict_json((runner.out / rows[index-1]['captures'][model.NATIVE_CAMERA]).read_bytes(), 4 << 20)
        checked = check_native_correspondence(expected=expected, source_capture=source_capture,
            source_metadata=metadata, native_jpeg=(runner.out / paths['.jpg']).read_bytes(),
            bridge=bridge, origins=origins, reader=reader)
        runner.save(prefix + '-native-correspondence.json', checked)
        rows[index-1].update(native_correspondence=prefix + '-native-correspondence.json',
            native_metadata=paths['.json'], native_jpeg=paths['.jpg'])
    for index, row in enumerate(rows, 1):
        runner.save(f'steps/{index:04d}.json', row)
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'native_sensorlink_camera_to_world_observed': True,
        'native_camera_exact_correspondence': True, 'ordinary_remote_originals_matched': True,
        'sample_expiry_observed': True, 'fresh_restoration_observed': True,
        'origin_attested_inference': True, 'raw_inputs_independently_verified_at_receiver': False,
        'physical_sensor_or_hardware_verified': False, 'gateway_receipt_is_source_capture_time': False}

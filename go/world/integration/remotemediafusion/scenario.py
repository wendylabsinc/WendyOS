"""B camera/audio originals and media → A fusion → B result observations."""
import base64
import time

if __package__:
    from . import control, libraries, model
else:
    import control
    import libraries
    import model

from integration.hazard.scenario import instant, wait_value, reader_matches, original_matches


def recorded_reader_match(raw, runner, ns, present):
    """Select an original live observation, not a claim of current liveness."""
    model.require(isinstance(raw, bytes) and len(raw) <= 32 << 20, 'bounded reader history required')
    lines = raw.split(b'\n')[:-1]
    model.require(len(lines) <= 512, 'bounded reader observation count required')
    for line in lines:
        value = model.strict_json(line, 2 << 20)
        if reader_matches(value, runner, ns, present):
            return value
    return None


def wait_reader_observation(node, runner, ns, present, budget=10):
    deadline = time.monotonic() + budget
    path = node.service_volume('reader', 'proofs') + '/stdout.json'
    while time.monotonic() < deadline:
        result = node.python(control.READER_OBSERVATIONS, path, check=False)
        if result.returncode == 0:
            value = recorded_reader_match(result.stdout, runner, ns, present)
            if value is not None:
                return value
        exited = node.python('import os,sys; assert not os.path.exists(sys.argv[1])',
                             node.service_volume('reader', 'proofs') + '/exit.json', check=False)
        model.require(exited.returncode == 0, 'ordinary reader terminated before required recorded observation')
        time.sleep(.2)
    raise model.Refused('bounded recorded reader observation did not arrive')


def decode(value, limit=384 << 10):
    raw = base64.b64decode(value, validate=True)
    model.require(0 < len(raw) <= limit, 'bounded original bytes required')
    return raw


def remote_reference(origins, sensor):
    model.require(sensor in model.REMOTE_SENSORS and len(origins) == 1, 'one selected remote-media hazard required')
    publication = next(iter(origins.values()))['publication']
    rows = [row for row in publication['inputs'] if row['reference']['key']['source']['stream'] == sensor]
    model.require(len(rows) == 1, 'one exact remote input required')
    return rows[0]['reference']


def correspondence(reader, origins, remote_receipts, runner, ns):
    # The independently owned checker binds original B media/proofs to the
    # actual A GetEvidence media receipts, signed result and B observation.
    if __package__:
        from . import proof
    else:
        import proof
    return proof.correspondence(reader, origins, remote_receipts, runner, ns)


def execute(runner):
    a, b = runner.nodes['a'], runner.nodes['b']
    runner.step('source-readiness')
    runner.save('reader-readiness.json', wait_value(b, 'reader', lambda r: reader_matches(r, runner, 0, False), 15))
    runner.save('fusion-readiness.json', wait_value(a, 'fusion', lambda r:
        r.get('run_id') == runner.inputs['run_id'] and r.get('status') in ('waiting', 'insufficient_evidence'), 10))
    for key, node in runner.nodes.items():
        status = node.cli('net', 'status', timeout=8, check=False)
        runner.save('native-status-' + key + '.json', {'command_exit': status.returncode,
                    'scope': 'owned native agent telemetry, not remote attestation'})
    rows, previous, first_digests, first_remote_until = [], 0, set(), None
    ns = 1_000_000_000
    for index, name in enumerate(('multimodal-hazard', 'original-expiry', 'fresh-restoration'), 1):
        present = index != 2
        sensors = list(model.SENSORS if present else model.LOCAL_SENSORS)
        runner.step('sample-' + str(index))
        model.require(ns > previous, 'sample clock cannot rewind')
        if not present:
            model.require(model.ANCHOR + ns > first_remote_until, 'both remote captures must expire before absence step')
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        response = model.strict_json(a.scene_request('/v1/step', request))
        model.require(response == {'sequence': index, **request}, 'source step differs; no retry')
        previous = ns
        prefix = f'steps/{index:04d}'
        runner.save(prefix + '-control.json', {'request': request, 'response': response,
                    'source_clock_shared': True, 'clock_domain': runner.scene['clock_domain'],
                    'anchor_unix_ns': model.ANCHOR, 'no_mutation_retry': True})
        reader = wait_reader_observation(b, runner, ns, present)
        runner.save(prefix + '-reader.json', reader)
        digests = [item['envelope_sha256'] for item in reader['hazards']]
        if digests:
            selected = a.python(control.MATCH_PUBLICATIONS, a.service_volume('fusion', 'proofs') + '/stdout.json', model.json_bytes(digests).decode())
            origins = model.strict_json(selected.stdout, 4 << 20)
            remote_receipts, remote_paths = {}, {}
            for sensor in model.REMOTE_SENSORS:
                ref = remote_reference(origins, sensor)
                selected_capture = b.python(control.MATCH_CAPTURE,
                    b.service_volume(sensor, 'proofs') + '/stdout.json', ref['envelope_sha256'])
                remote_receipts[sensor] = model.strict_json(selected_capture.stdout, 384 << 10)
                remote_paths[sensor] = prefix + '-' + sensor + '-origin.json'
            checked = correspondence(reader, origins, remote_receipts, runner, ns)
            model.require(set(checked['remote_sample_valid_until_unix_ns']) == set(model.REMOTE_SENSORS) and
                          all(type(value) is int and value > model.ANCHOR + ns
                              for value in checked['remote_sample_valid_until_unix_ns'].values()),
                          'exact original remote capture deadlines required')
            runner.save(prefix + '-origin.json', origins)
            for sensor, receipt in remote_receipts.items():
                runner.save(remote_paths[sensor], receipt)
            runner.save(prefix + '-correspondence.json', checked)
        if index == 1:
            first_digests = set(digests)
            first_remote_until = max(checked['remote_sample_valid_until_unix_ns'].values())
        elif index == 3:
            model.require(first_digests.isdisjoint(digests), 'restoration reused an original result')
        presentation = a.scene_request('/v1/presentation')
        model.require(model.strict_json(presentation, 4 << 20)['simulation_time_ns'] == ns, 'presentation step differs')
        libraries.stage.write_new(runner.out / (prefix + '-presentation.json'), presentation)
        captures, media_paths = {}, {}
        for sensor in sensors:
            raw = a.scene_request('/v1/captures/' + sensor)
            value = model.strict_json(raw, 4 << 20)
            sample = model.strict_json(decode(value['capture']['sample_json'], 32 << 10))
            media = base64.b64decode(value['media'], validate=True)
            model.require(sample['sample_ns'] == ns and sample['sensor_id'] == sensor and sample['run_id'] == runner.inputs['run_id'], 'original source step differs')
            path = prefix + '-' + sensor
            libraries.stage.write_new(runner.out / (path + '.json'), raw)
            captures[sensor] = path + '.json'
            if sample.get('evidence'):
                evidence = sample['evidence']
                model.require(model.digest(media) == evidence['sha256'] and len(media) == evidence['size'] and
                              evidence['content_type'] in ('image/png', 'audio/wav'), 'original media differs')
                suffix = '.png' if evidence['content_type'] == 'image/png' else '.wav'
                libraries.stage.write_new(runner.out / (path + suffix), media)
                media_paths[sensor] = path + suffix
            else:
                model.require(not media, 'inline observation has unexpected media')
        row = {'name': name, 'simulation_time_ns': ns, 'presentation': prefix + '-presentation.json',
               'control': prefix + '-control.json', 'reader': prefix + '-reader.json',
               'origin': prefix + '-origin.json' if digests else None,
               'remote_origins': remote_paths if digests else {},
               'correspondence': prefix + '-correspondence.json' if digests else None,
               'captures': captures, 'media': media_paths, 'original_envelope_sha256': digests,
               'hazards': len(reader['hazards']),
               'reader_observation_semantics': 'live_at_original_tick_not_at_host_collection'}
        runner.save(prefix + '.json', row); rows.append(row)
        if index == 1:
            ns = max(first_remote_until, *(instant(h['payload']['valid_until']) for h in reader['hazards'])) - model.ANCHOR + 100_000_000
        else:
            ns += 100_000_000
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'ordinary_remote_originals_matched': True,
            'b_camera_audio_ack_and_media_to_a_input_to_a_hazard_to_b_reader_matched': True,
            'sample_expiry_observed': True, 'fresh_restoration_observed': True,
            'origin_attested_inference': True, 'raw_inputs_independently_verified_at_receiver': False,
            'physical_sensor_or_hardware_verified': False,
            'long_running_apps_stopped_by_owned_cleanup': True}

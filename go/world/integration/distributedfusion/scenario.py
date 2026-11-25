"""Exact B gas acknowledgment → A fusion input → A hazard → B read evidence."""
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


def gas_reference(origins):
    model.require(len(origins) == 1, 'one selected hazard required')
    publication = next(iter(origins.values()))['publication']
    rows = [p for p in publication['inputs'] if p['reference']['key']['source']['stream'] == 'gas-1']
    model.require(len(rows) == 1, 'one exact gas input required')
    return rows[0]['reference']


def correspondence(reader, origins, gas, runner, ns):
    """Byte/scope correspondence only; the ordinary apps did live verification."""
    digests = original_matches(reader, origins)
    model.require(len(digests) == 1, 'one fixed hazard required')
    publication = origins[digests[0]]['publication']
    proofs = publication.get('inputs', [])
    model.require(len(proofs) == len(model.SENSORS), 'complete fuser proof set required')
    signed_result = model.strict_json(decode(publication['signed_record_json']))
    result_record = model.strict_json(decode(signed_result['record'], 64 << 10))
    generic = model.strict_json(decode(result_record['publication']['payload'], 32 << 10))
    model.require([p['reference'] for p in proofs] == generic['inputs'], 'hazard lineage differs from fuser proof order')
    a, b = [row['public']['principal'] for row in runner.pair['nodes']]
    model.require(result_record['scope']['device'] == a and result_record['scope']['app'] == model.APP and
                  result_record['scope']['service'] == 'fusion' and
                  result_record['scope']['deployment_generation'] == runner.tasks['a/fusion']['generation'], 'fusion origin scope differs')
    source_devices, gas_raw, gas_until = {}, None, None
    for proof in proofs:
        ref = proof['reference']; key, version = ref['key'], ref['version']
        source = key['source']; sensor = source['stream']
        model.require(sensor in model.SENSORS and sensor not in source_devices, 'one proof per sensor required')
        expected = b if sensor == 'gas-1' else a
        model.require(source == {'device': expected, 'app': model.APP, 'service': sensor, 'stream': sensor} and
                      key['tenant'] == runner.pair['tenant'] and key['site'] == runner.scene['site'] and
                      key['type'] == model.CAPTURE and key['entity'] == 'sensor/' + sensor, 'input device or scope differs')
        raw = decode(proof['envelope_json'])
        signed = model.strict_json(raw)
        inner = decode(signed['record'], 64 << 10)
        record = model.strict_json(inner)
        model.require(model.digest(raw) == ref['envelope_sha256'] and model.digest(inner) == ref['record_sha256'] and
                      version == {k: record[k] for k in ('epoch', 'sequence')}, 'input exact hash/version differs')
        scope = record['scope']
        model.require(all(scope[k] == v for k, v in {'tenant': key['tenant'], 'site': key['site'],
                      'device': expected, 'app': model.APP, 'service': sensor}.items()), 'signed source differs')
        pub = record['publication']
        model.require(pub['component_type'] == model.CAPTURE and pub['stream'] == sensor and
                      pub['entity'] == key['entity'] and pub['frame'] == runner.scene['frame'] and
                      not pub['tombstone'] and instant(pub['timing']['sample_time']) == model.ANCHOR + ns,
                      'signed source publication differs')
        capture = model.strict_json(decode(pub['payload'], 32 << 10))
        sample = model.strict_json(decode(capture['sample_json'], 32 << 10))
        model.require(capture['scene_boot_id'] == runner.scene['boot_id'] and
                      capture['simulation_anchor_unix_ns'] == model.ANCHOR and sample['sample_ns'] == ns and
                      sample['sensor_id'] == sensor and sample['run_id'] == runner.inputs['run_id'], 'original capture changed')
        source_devices[sensor] = expected
        if sensor == 'gas-1':
            gas_raw = raw
            timing = pub['timing']
            ttl, error, age = pub['ttl_ns'], timing['error_bound_ns'], timing['age_hint_ns']
            model.require(all(type(n) is int and n >= 0 for n in (ttl, error, age)) and ttl > max(error, age), 'gas lifetime differs')
            gas_until = instant(timing['sample_time']) + ttl - max(error, age)
            model.require(gas['sample_ns'] == ns and gas['scene_boot_id'] == capture['scene_boot_id'] and
                          gas['captured_unix_ns'] == capture['captured_unix_ns'] and
                          gas['capture_id'] == sample['id'] and gas['native_sequence'] == sample['sequence'] and
                          scope['deployment_generation'] == runner.tasks['b/gas-1']['generation'], 'gas acknowledgment capture differs')
    ref = gas_reference(origins)
    model.require(gas_raw == decode(gas['signed_record_json']) and model.digest(gas_raw) == gas['proof_sha256'] == ref['envelope_sha256'] and
                  gas['record_sha256'] == ref['record_sha256'] and not gas.get('evidence_sha256'), 'B acknowledgment differs from A input')
    return {'version': 1, 'source_devices': source_devices, 'gas_reference': ref,
            'gas_envelope_sha256': gas['proof_sha256'], 'a_input_envelope_sha256': ref['envelope_sha256'],
            'gas_sample_valid_until_unix_ns': gas_until,
            'hazard_envelope_sha256': digests[0], 'reader_envelope_sha256': digests[0],
            'fuser_device': a, 'reader_device': b, 'exact_bytes_matched': True,
            'independent_crypto_reverification': False}


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
    rows, previous, first_digests, first_gas_until = [], 0, set(), None
    ns = 1_000_000_000
    for index, name in enumerate(('multimodal-hazard', 'original-expiry', 'fresh-restoration'), 1):
        present = index != 2
        sensors = list(model.SENSORS if present else model.LOCAL_SENSORS)
        runner.step('sample-' + str(index))
        model.require(ns > previous, 'sample clock cannot rewind')
        if not present:
            model.require(model.ANCHOR + ns > first_gas_until, 'remote gas must expire before absence step')
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
            ref = gas_reference(origins)
            selected_gas = b.python(control.MATCH_CAPTURE, b.service_volume('gas-1', 'proofs') + '/stdout.json', ref['envelope_sha256'])
            gas = model.strict_json(selected_gas.stdout, 384 << 10)
            evidence = correspondence(reader, origins, gas, runner, ns)
            runner.save(prefix + '-origin.json', origins)
            runner.save(prefix + '-gas-origin.json', gas)
            runner.save(prefix + '-correspondence.json', evidence)
        if index == 1:
            first_digests = set(digests)
            first_gas_until = evidence['gas_sample_valid_until_unix_ns']
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
               'gas_origin': prefix + '-gas-origin.json' if digests else None,
               'correspondence': prefix + '-correspondence.json' if digests else None,
               'captures': captures, 'media': media_paths, 'original_envelope_sha256': digests,
               'hazards': len(reader['hazards'])}
        runner.save(prefix + '.json', row); rows.append(row)
        if index == 1:
            ns = max(first_gas_until, *(instant(h['payload']['valid_until']) for h in reader['hazards'])) - model.ANCHOR + 100_000_000
        else:
            ns += 100_000_000
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'ordinary_remote_originals_matched': True,
            'b_gas_ack_to_a_input_to_a_hazard_to_b_reader_matched': True,
            'sample_expiry_observed': True, 'fresh_restoration_observed': True,
            'origin_attested_inference': True, 'raw_inputs_independently_verified_at_receiver': False,
            'long_running_apps_stopped_by_owned_cleanup': True}

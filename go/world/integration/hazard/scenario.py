"""Observed active, expired and restored native hazards; no mutation retries."""
import base64
import calendar
from datetime import datetime
import re
import time

if __package__:
    from . import control, libraries, model
else:
    import control
    import libraries
    import model


def instant(value):
    model.require(isinstance(value, str), 'sample time required')
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value)
    model.require(match is not None, 'exact UTC sample time required')
    seconds = calendar.timegm(datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').timetuple())
    return seconds * 1_000_000_000 + int((match[2] or '').ljust(9, '0'))


def reader_matches(value, runner, ns, present):
    if isinstance(value, dict) and value.get('status') == 'waiting_for_source':
        return False
    if not isinstance(value, dict) or 'hazards' not in value or 'reader_generation' not in value:
        return False
    model.require(value.get('status') == 'ready' and value.get('version') == 1 and
                  value.get('run_id') == runner.inputs['run_id'] and
                  value.get('reader_generation') == runner.tasks['b/reader']['generation'] and
                  value.get('origin_attested_inference') is True and
                  value.get('raw_inputs_independently_verified') is False,
                  'reader scope or trust label differs')
    hazards = value['hazards']
    model.require(isinstance(hazards, list) and len(hazards) <= 8, 'bounded hazards required')
    expected = runner.inputs['anchor_unix_ns'] + ns
    current = any({i['sensor_id'] for i in h['payload']['inputs']} == set(model.SENSORS) and
                  all(instant(i['sample_time']) == expected for i in h['payload']['inputs'])
                  for h in hazards)
    return instant(value['sample_time']) == expected and (current if present else not hazards)


def wait_value(node, service, predicate, budget):
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        value = node.latest(service)
        if value is not None and predicate(value):
            return value
        exited = node.python("import os,sys; assert not os.path.exists(sys.argv[1])",
                              node.service_volume(service, 'proofs') + '/exit.json', check=False)
        model.require(exited.returncode == 0, 'ordinary app terminated before required observation')
        time.sleep(.2)
    raise model.Refused('bounded current observation did not arrive')


def original_matches(reader, origins):
    seen = set()
    for item in reader['hazards']:
        raw = base64.b64decode(item['envelope_json'], validate=True)
        digest = model.digest(raw)
        model.require(digest == item['envelope_sha256'] and digest not in seen and digest in origins,
                      'remote original digest differs')
        seen.add(digest)
        publication = origins[digest]['publication']
        model.require(publication['status'] == 'complete' and publication['locally_verified'] is True and
                      base64.b64decode(publication['signed_record_json'], validate=True) == raw,
                      'remote original differs from acknowledged origin')
        signed = model.strict_json(raw, 384 << 10)
        record_raw = base64.b64decode(signed['record'], validate=True)
        model.require(model.digest(record_raw) == item['record_sha256'] == publication['record_sha256'],
                      'original record hash differs')
        record = model.strict_json(record_raw)
        model.require({k: record[k] for k in ('epoch', 'sequence')} == item['version'], 'original version differs')
        generic = model.strict_json(base64.b64decode(record['publication']['payload'], validate=True))
        payload = model.strict_json(base64.b64decode(generic['result_json'], validate=True))
        model.require(generic['result_schema'] == 'wendy.hazard.v1' and payload == item['payload'],
                      'typed receiver payload differs from original signed result')
    return sorted(seen)


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
    rows, previous, first_digests = [], 0, set()
    ns = 1_000_000_000
    for index, name in enumerate(('multimodal-hazard', 'original-expiry', 'fresh-restoration'), 1):
        sensors = list(model.SENSORS) if index != 2 else []
        runner.step('sample-' + str(index))
        model.require(ns > previous, 'sample clock cannot rewind')
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        response = model.strict_json(a.scene_request('/v1/step', request))
        model.require(response == {'sequence': index, **request}, 'source step differs; no retry')
        clock = control.sample_clock(runner.inputs['run_id'], runner.inputs['anchor_unix_ns'], ns)
        b.atomic_write(model.config_path('b') + '/simclock.json', clock, previous=b.clock_raw)
        b.clock_raw, previous = clock, ns
        prefix = f'steps/{index:04d}'
        runner.save(prefix + '-control.json', {'request': request, 'response': response,
                    'receiver_clock': model.strict_json(clock), 'no_mutation_retry': True})
        reader = wait_value(b, 'reader', lambda r: reader_matches(r, runner, ns, bool(sensors)), 10)
        runner.save(prefix + '-reader.json', reader)
        digests = [item['envelope_sha256'] for item in reader['hazards']]
        if digests:
            response = a.python(control.MATCH_PUBLICATIONS, a.service_volume('fusion', 'proofs') + '/stdout.json', model.json_bytes(digests).decode())
            origins = model.strict_json(response.stdout, 4 << 20)
            original_matches(reader, origins)
            runner.save(prefix + '-origin.json', origins)
        if index == 1:
            first_digests = set(digests)
        elif index == 3:
            model.require(first_digests.isdisjoint(digests), 'restoration reused an original result')
        presentation = a.scene_request('/v1/presentation')
        model.require(model.strict_json(presentation, 4 << 20)['simulation_time_ns'] == ns, 'presentation step differs')
        libraries.stage.write_new(runner.out / (prefix + '-presentation.json'), presentation)
        captures, media_paths = {}, {}
        for sensor in sensors:
            raw = a.scene_request('/v1/captures/' + sensor)
            value = model.strict_json(raw, 4 << 20)
            sample = model.strict_json(base64.b64decode(value['capture']['sample_json'], validate=True))
            media = base64.b64decode(value['media'], validate=True)
            model.require(sample['sample_ns'] == ns and sample['sensor_id'] == sensor and
                          sample['run_id'] == runner.inputs['run_id'], 'original capture identity differs')
            path = prefix + '-' + sensor
            libraries.stage.write_new(runner.out / (path + '.json'), raw)
            captures[sensor] = path + '.json'
            if sample.get('evidence'):
                evidence = sample['evidence']
                model.require(model.digest(media) == evidence['sha256'] and len(media) == evidence['size'], 'original media differs')
                suffix = '.png' if evidence['content_type'] == 'image/png' else '.wav'
                model.require(evidence['content_type'] in ('image/png', 'audio/wav'), 'unexpected evidence type')
                libraries.stage.write_new(runner.out / (path + suffix), media)
                media_paths[sensor] = path + suffix
            else:
                model.require(not media, 'inline observation has unexpected media')
        row = {'name': name, 'simulation_time_ns': ns, 'presentation': prefix + '-presentation.json',
               'control': prefix + '-control.json', 'reader': prefix + '-reader.json',
               'origin': prefix + '-origin.json' if digests else None, 'captures': captures,
               'media': media_paths, 'original_envelope_sha256': digests, 'hazards': len(reader['hazards'])}
        runner.save(prefix + '.json', row); rows.append(row)
        if index == 1:
            ns = max(instant(h['payload']['valid_until']) for h in reader['hazards']) - runner.inputs['anchor_unix_ns'] + 100_000_000
        else:
            ns += 100_000_000
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'ordinary_remote_originals_matched': True, 'sample_expiry_observed': True,
            'fresh_restoration_observed': True, 'origin_attested_inference': True,
            'raw_inputs_independently_verified_at_receiver': False,
            'long_running_apps_stopped_by_owned_cleanup': True}

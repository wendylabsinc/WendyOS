"""Four observed native-transfer steps. Never retries a source mutation."""
import base64
import calendar
from datetime import datetime
import re
import time

import control
import libraries
import model

STEPS = ((100_000_000, ('camera-a', 'camera-b'), 'two-camera-original'),
         (500_000_000, ('camera-a', 'camera-b'), 'new-observation'),
         (2_000_000_000, (), 'original-expiry'),
         (2_100_000_000, ('camera-a', 'camera-b'), 'fresh-restoration'))


def instant(value):
    model.require(isinstance(value, str), 'sample time required')
    match = re.fullmatch(r'(\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d)(?:\.(\d{1,9}))?Z', value)
    model.require(match is not None, 'exact UTC sample time required')
    seconds = calendar.timegm(datetime.strptime(match[1], '%Y-%m-%dT%H:%M:%S').timetuple())
    return seconds * 1_000_000_000 + int((match[2] or '').ljust(9, '0'))


def reader_matches(value, runner, ns, present):
    if isinstance(value, dict) and value.get('status') == 'waiting_for_source':
        return False
    if not isinstance(value, dict) or 'tracks' not in value or 'reader_generation' not in value:
        return False
    expected = model.strict_json(control.sample_clock(runner.inputs['run_id'], runner.inputs['anchor_unix_ns'], ns))['now']
    model.require(value.get('status') == 'ready' and value.get('version') == 1 and value.get('run_id') == runner.inputs['run_id'] and value.get('reader_generation') == runner.tasks['b/reader']['generation'] and value.get('origin_attested_inference') is True and value.get('camera_inputs_independently_verified') is False, 'reader scope or trust label differs')
    tracks = value['tracks']
    model.require(isinstance(tracks, list) and len(tracks) <= 48, 'bounded reader tracks required')
    latest = any(instant(track['payload']['observed_at']) == instant(expected) for track in tracks)
    return instant(value['sample_time']) == instant(expected) and (latest if present else not tracks)


def wait_value(node, service, predicate, budget):
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        value = node.latest(service)
        if value is not None and predicate(value):
            return value
        # A terminated app cannot make a later observation. Preserve its output
        # through the caller's unconditional cleanup snapshot.
        exit_file = node.python("import os,sys; assert not os.path.exists(sys.argv[1])", node.service_volume(service, 'proofs') + '/exit.json', check=False)
        model.require(exit_file.returncode == 0, 'ordinary app terminated before required observation')
        time.sleep(.2)
    raise model.Refused('bounded current observation did not arrive')


def original_matches(reader, origins):
    seen = set()
    for track in reader['tracks']:
        raw = base64.b64decode(track['envelope_json'], validate=True)
        digest = model.digest(raw)
        model.require(digest == track['envelope_sha256'] and digest not in seen and digest in origins, 'remote original digest differs')
        seen.add(digest)
        publication = origins[digest]['publication']
        model.require(publication['locally_verified'] is True and base64.b64decode(publication['signed_record_json'], validate=True) == raw, 'remote original is not exact acknowledged origin bytes')
        signed = model.strict_json(raw, 384 << 10)
        record_raw = base64.b64decode(signed['record'], validate=True)
        model.require(model.digest(record_raw) == track['record_sha256'], 'original record digest differs')
    return sorted(seen)


def execute(runner):
    a, b = runner.nodes['a'], runner.nodes['b']
    runner.step('source-readiness')
    ready = wait_value(b, 'reader', lambda row: reader_matches(row, runner, 0, False), 15)
    runner.save('reader-readiness.json', ready)
    people = wait_value(a, 'people', lambda row: row.get('run_id') == runner.inputs['run_id'] and row.get('status') == 'waiting', 10)
    runner.save('people-readiness.json', people)
    for key, node in runner.nodes.items():
        result = node.cli('net', 'status', timeout=8, check=False)
        runner.save('native-status-' + key + '.json', {'command_exit': result.returncode, 'scope': 'owned native agent telemetry, not remote attestation'})
    rows = []
    prior = 0
    for index, (ns, cameras, name) in enumerate(STEPS, 1):
        runner.step('sample-' + str(index))
        model.require(ns > prior, 'sample clock cannot rewind')
        request = {'simulation_time_ns': ns, 'cameras': list(cameras)}
        response = model.strict_json(a.scene_request('/v1/step', request))
        model.require(response == {'sequence': index, **request}, 'source step response differs; no retry')
        clock = control.sample_clock(runner.inputs['run_id'], runner.inputs['anchor_unix_ns'], ns)
        b.atomic_write(model.config_path('b') + '/simclock.json', clock, previous=b.clock_raw)
        b.clock_raw, prior = clock, ns
        prefix = f'steps/{index:04d}'
        runner.save(prefix + '-control.json', {'request': request, 'response': response, 'receiver_clock': model.strict_json(clock), 'no_mutation_retry': True})
        reader = wait_value(b, 'reader', lambda value: reader_matches(value, runner, ns, bool(cameras)), 8)
        runner.save(prefix + '-reader.json', reader)
        digests = [track['envelope_sha256'] for track in reader['tracks']]
        if digests:
            response = a.python(control.MATCH_PUBLICATIONS, a.service_volume('people', 'proofs') + '/stdout.json', model.json_bytes(digests).decode())
            origins = model.strict_json(response.stdout, 4 << 20)
            original_matches(reader, origins)
            runner.save(prefix + '-origin.json', origins)
        presentation = a.scene_request('/v1/presentation')
        model.require(model.strict_json(presentation, 4 << 20)['simulation_time_ns'] == ns, 'presentation belongs to another step')
        libraries.stage.write_new(runner.out / (prefix + '-presentation.json'), presentation)
        camera_paths = {}
        for camera in cameras:
            raw = a.scene_request('/v1/cameras/' + camera)
            value = model.strict_json(raw, 4 << 20)
            sample = model.strict_json(base64.b64decode(value['capture']['sample_json'], validate=True))
            media = base64.b64decode(value['media'], validate=True)
            model.require(sample['sample_ns'] == ns and sample['sensor_id'] == camera and sample['run_id'] == runner.inputs['run_id'] and model.digest(media) == sample['evidence']['sha256'] and len(media) == sample['evidence']['size'], 'original camera receipt differs')
            path = prefix + '-' + camera
            libraries.stage.write_new(runner.out / (path + '.json'), raw)
            libraries.stage.write_new(runner.out / (path + '.png'), media)
            camera_paths[camera] = path + '.png'
        rows.append({'name': name, 'simulation_time_ns': ns, 'presentation': prefix + '-presentation.json', 'control': prefix + '-control.json', 'reader': prefix + '-reader.json', 'origin': prefix + '-origin.json' if digests else None, 'camera_images': camera_paths, 'original_envelope_sha256': digests, 'tracks': len(reader['tracks'])})
        runner.save(prefix + '.json', rows[-1])
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'ordinary_remote_originals_matched': True, 'sample_expiry_observed': True, 'fresh_restoration_observed': True, 'origin_attested_inference': True, 'camera_inputs_independently_verified_at_receiver': False, 'long_running_apps_stopped_by_owned_cleanup': True}

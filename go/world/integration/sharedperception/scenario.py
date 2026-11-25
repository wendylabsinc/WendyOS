"""Three shared source instants, observed independently by two ordinary readers."""
import base64
import time

from integration.remotemediafusion import libraries
from integration.hazard.scenario import instant, reader_matches as hazard_matches, wait_value
from integration.sharedperception import model
from integration.sharedperception.run import control

# Keep the existing five-source hazard checker and two-camera track decoder.
# Their module globals are private copies with the combined app/source names.
_hazard_proof = libraries.load('sharedperception_hazard_proof', libraries.HERE / 'proof.py', {'model': model._remote})
_track_view = libraries.load('sharedperception_track_view', libraries.HERE.parent / 'people/playback.py')
_track_view.model = model._people
_people_control = libraries.load('sharedperception_people_control', libraries.HERE.parent / 'people/control.py',
                                {'libraries': libraries.base, 'model': model._people})
PEOPLE_PUBLICATIONS = _people_control.MATCH_PUBLICATIONS.replace(
    "'publication':p}", "'publication':p,'inputs':row.get('inputs',[])}")


def reader_matches(value, runner, ns, kind, present):
    model.require(kind in ('hazard', 'people'), 'known reader kind required')
    if not isinstance(value, dict) or value.get('status') == 'waiting_for_source':
        return False
    field, service = ('hazards', 'reader') if kind == 'hazard' else ('tracks', 'people-reader')
    if field not in value or 'reader_generation' not in value:
        return False  # terminal summaries are not observations
    generation = runner.tasks['b/' + service]['generation']
    model.require(value.get('status') == 'ready' and value.get('version') == 1 and
                  value.get('run_id') == runner.inputs['run_id'] and value['reader_generation'] == generation and
                  value.get('origin_attested_inference') is True, 'scoped ready reader required')
    clock = value.get('clock', {})
    model.require(isinstance(clock, dict) and clock.get('deployment_generation') == generation and
                  clock.get('sample_clock_domain') == runner.scene['clock_domain'] and
                  clock.get('sample_time') == value.get('sample_time') and
                  isinstance(clock.get('server_boot_id'), str) and clock['server_boot_id'] and
                  instant(clock['physical_earliest']) <= instant(clock['physical_latest']),
                  'each reader must retain its own final clock')
    if kind == 'hazard':
        return hazard_matches(value, runner, ns, present)
    model.require(value.get('camera_inputs_independently_verified') is False and
                  isinstance(value['tracks'], list) and len(value['tracks']) <= 8, 'bounded origin-attested tracks required')
    now = runner.inputs['anchor_unix_ns'] + ns
    current = bool(value['tracks']) and all(instant(track['payload']['observed_at']) == now for track in value['tracks'])
    return instant(value['sample_time']) == now and (current if present else not value['tracks'])


def recorded_reader_match(raw, runner, ns, kind, present):
    model.require(isinstance(raw, bytes) and len(raw) <= 32 << 20, 'bounded original reader history required')
    lines = raw.split(b'\n')[:-1]
    model.require(len(lines) <= 512 and all(0 < len(line) <= 2 << 20 for line in lines), 'bounded complete reader rows required')
    for line in lines:
        value = model.strict_json(line, 2 << 20)
        if reader_matches(value, runner, ns, kind, present):
            return value
    return None


def wait_reader(runner, ns, kind, present, budget=10):
    node = runner.nodes['b']
    service = 'reader' if kind == 'hazard' else 'people-reader'
    path = node.service_volume(service, 'proofs') + '/stdout.json'
    deadline = time.monotonic() + budget
    while time.monotonic() < deadline:
        result = node.python(control.READER_OBSERVATIONS, path, check=False)
        if result.returncode == 0:
            selected = recorded_reader_match(result.stdout, runner, ns, kind, present)
            if selected is not None:
                return selected
        alive = node.python('import os,sys; assert not os.path.exists(sys.argv[1])',
                            node.service_volume(service, 'proofs') + '/exit.json', check=False)
        model.require(alive.returncode == 0, 'ordinary reader ended before required observation')
        time.sleep(.2)
    raise model.Refused('bounded shared-source reader observation did not arrive')


def retain_captures(runner, prefix, sensors, ns):
    captures, media, values = {}, {}, {}
    for sensor in sensors:
        raw = runner.nodes['a'].scene_request('/v1/captures/' + sensor)
        value = model.strict_json(raw, 4 << 20)
        sample = model.strict_json(_hazard_proof.decode(value['capture']['sample_json'], 32 << 10))
        data = base64.b64decode(value['media'], validate=True)
        model.require(sample['sensor_id'] == sensor and sample['sample_ns'] == ns and
                      sample['run_id'] == runner.inputs['run_id'] and sample['frame'] == runner.scene['frame'] and
                      sample['package_sha256'] == runner.scene['package_sha256'], 'original shared capture differs')
        path = prefix + '-' + sensor
        libraries.stage.write_new(runner.out / (path + '.json'), raw)
        captures[sensor], values[sensor] = path + '.json', value
        evidence = sample.get('evidence')
        if evidence:
            model.require(evidence['sha256'] == model.digest(data) and evidence['size'] == len(data) and
                          evidence['content_type'] in ('image/png', 'audio/wav'), 'original source media differs')
            suffix = '.wav' if evidence['content_type'] == 'audio/wav' else '.png'
            libraries.stage.write_new(runner.out / (path + suffix), data)
            media[sensor] = path + suffix
        else:
            model.require(not data, 'inline source has unexpected media')
    return captures, media, values


def people_correspondence(reader, origins, captures, runner, ns):
    """Exact retained bytes and ancestry only; the ordinary reader verifies origin."""
    tracks, originals = _track_view.receiver_tracks(reader, origins, runner.pair, runner.scene, ns,
                                                    runner.tasks['b/people-reader']['generation'])
    used, caps = {}, []
    for row in reader['tracks']:
        digest = row['envelope_sha256']; origin = origins[digest]
        model.require(origin['run_id'] == runner.inputs['run_id'], 'people app run differs')
        record = model.strict_json(_hazard_proof.decode(model.strict_json(originals[digest])['record'], 64 << 10))
        model.require(record['scope']['deployment_generation'] == runner.tasks['a/people']['generation'], 'people origin generation differs')
        body = model.strict_json(_hazard_proof.decode(record['publication']['payload'], 32 << 10))
        proofs = {p['reference']['envelope_sha256']: p for p in origin['inputs']}
        members = row['payload']['current'] + row['payload'].get('previous', [])
        model.require(body['inputs'] == list(dict((model.json_bytes(m['proof']).decode(), m['proof']) for m in members).values()),
                      'track generic proof order differs')
        for member in row['payload']['current']:
            sensor, ref = member['sensor_id'], member['proof']
            model.require(sensor in model.PEOPLE_CAMERAS and instant(member['sample_time']) == model.ANCHOR + ns,
                          'track must use current shared people pixels')
            proof = proofs[ref['envelope_sha256']]
            raw = _hazard_proof.decode(proof['envelope_json'])
            signed = model.strict_json(raw); inner = _hazard_proof.decode(signed['record'], 64 << 10)
            original = model.strict_json(inner)
            model.require(proof['reference'] == ref and model.digest(raw) == ref['envelope_sha256'] and
                          model.digest(inner) == ref['record_sha256'] and
                          ref['version'] == {k: original[k] for k in ('epoch', 'sequence')}, 'people original proof differs')
            scope, pub = original['scope'], original['publication']
            expected = {'tenant': runner.pair['tenant'], 'site': runner.scene['site'],
                        'device': runner.pair['nodes'][0]['public']['principal'], 'app': model.APP,
                        'service': sensor, 'execution_domain': 'simulation',
                        'deployment_generation': runner.tasks['a/' + sensor]['generation']}
            model.require(all(scope[k] == value for k, value in expected.items()) and
                          pub['component_type'] == model.CAPTURE and pub['stream'] == sensor and
                          pub['frame'] == runner.scene['frame'] and not pub['tombstone'], 'people capture owner differs')
            capture = model.strict_json(_hazard_proof.decode(pub['payload'], 32 << 10))
            sample = model.strict_json(_hazard_proof.decode(capture['sample_json'], 32 << 10))
            model.require(capture == captures[sensor]['capture'] and member['sample_id'] == sample['id'] and
                          member['image'] == sample['evidence'] and member['scene_boot_id'] == runner.scene['boot_id'] and
                          member['captured_unix_ns'] == capture['captured_unix_ns'] and
                          member['simulation_anchor_unix_ns'] == model.ANCHOR and
                          member['calibration_revision'] == sample['calibration_revision'] and
                          all(member[key] == runner.inputs['model'][key] for key in runner.inputs['model']),
                          'track original image/model/calibration differs')
            until, physical, _ = _hazard_proof.deadlines(pub, sample, capture)
            model.require(instant(member['valid_until']) <= until and instant(member['physical_until']) <= physical,
                          'track member extends original lifetime')
            used[ref['envelope_sha256']] = {'reference': ref, 'sensor_id': sensor, 'image': member['image']}
            caps.append(until)
    return {'version': 1, 'track_count': len(tracks), 'original_envelope_sha256': sorted(originals),
            'current_inputs': list(used.values()), 'sample_valid_until_unix_ns': caps,
            'exact_original_bytes_matched': True, 'independent_crypto_reverification': False}


def execute(runner):
    a, b = runner.nodes['a'], runner.nodes['b']
    runner.step('source-readiness')
    for kind, service in (('hazard', 'reader'), ('people', 'people-reader')):
        runner.save(service + '-readiness.json', wait_reader(runner, 0, kind, False, 15))
    for service in ('fusion', 'people'):
        runner.save(service + '-readiness.json', wait_value(a, service, lambda row:
            row.get('run_id') == runner.inputs['run_id'] and row.get('status') in ('waiting', 'insufficient_evidence'), 10))
    rows, ns, first = [], 1_000_000_000, {}
    for index, name in enumerate(('shared-observations', 'original-expiry', 'fresh-restoration'), 1):
        present, prefix = index != 2, f'steps/{index:04d}'
        sensors = list(model.SENSORS) if present else []
        runner.step('sample-' + str(index))
        request = {'simulation_time_ns': ns, 'sensors': sensors}
        response = model.strict_json(a.scene_request('/v1/step', request))
        model.require(response == {'sequence': index, **request}, 'source step differs; no retry')
        runner.save(prefix + '-control.json', {'request': request, 'response': response, 'source_clock_shared': True,
                    'clock_domain': runner.scene['clock_domain'], 'anchor_unix_ns': model.ANCHOR, 'no_mutation_retry': True})
        # Save original source bytes promptly. Reader history can then recover a
        # successful short-lived observation without relabeling it as current.
        captures, media, source_values = retain_captures(runner, prefix, sensors, ns)
        presentation = a.scene_request('/v1/presentation')
        model.require(model.strict_json(presentation, 4 << 20)['simulation_time_ns'] == ns, 'presentation step differs')
        libraries.stage.write_new(runner.out / (prefix + '-presentation.json'), presentation)
        readers, digests, origin_paths, checks = {}, {}, {}, {}
        for kind, service, origin_service, script, field in (
                ('hazard', 'reader', 'fusion', control.MATCH_PUBLICATIONS, 'hazards'),
                ('people', 'people-reader', 'people', PEOPLE_PUBLICATIONS, 'tracks')):
            readers[kind] = wait_reader(runner, ns, kind, present)
            runner.save(prefix + '-' + service + '.json', readers[kind])
            digests[kind] = [item['envelope_sha256'] for item in readers[kind][field]]
            if not present:
                continue
            selected = a.python(script, a.service_volume(origin_service, 'proofs') + '/stdout.json', model.json_bytes(digests[kind]).decode())
            origins = model.strict_json(selected.stdout, 4 << 20)
            origin_paths[kind] = prefix + '-' + origin_service + '-origin.json'
            runner.save(origin_paths[kind], origins)
            if kind == 'hazard':
                remote = {}
                for sensor in model.REMOTE_SENSORS:
                    refs = [p['reference'] for p in next(iter(origins.values()))['publication']['inputs']
                            if p['reference']['key']['source']['stream'] == sensor]
                    model.require(len(refs) == 1, 'one remote capture reference required')
                    receipt = b.python(control.MATCH_CAPTURE, b.service_volume(sensor, 'proofs') + '/stdout.json', refs[0]['envelope_sha256'])
                    remote[sensor] = model.strict_json(receipt.stdout, 384 << 10)
                    runner.save(prefix + '-' + sensor + '-origin.json', remote[sensor])
                checks[kind] = _hazard_proof.correspondence(readers[kind], origins, remote, runner, ns)
            else:
                checks[kind] = people_correspondence(readers[kind], origins, source_values, runner, ns)
            runner.save(prefix + '-' + kind + '-correspondence.json', checks[kind])
            if index == 3:
                model.require(first[kind].isdisjoint(digests[kind]), 'restoration reused old derived originals')
        row = {'name': name, 'simulation_time_ns': ns, 'control': prefix + '-control.json',
               'presentation': prefix + '-presentation.json', 'captures': captures, 'media': media,
               'hazard_reader': prefix + '-reader.json', 'people_reader': prefix + '-people-reader.json',
               'origins': origin_paths, 'original_envelope_sha256': digests,
               'hazards': len(readers['hazard']['hazards']), 'tracks': len(readers['people']['tracks']),
               'reader_observation_semantics': 'live_at_each_original_reader_tick_not_at_host_collection'}
        runner.save(prefix + '.json', row); rows.append(row)
        if index == 1:
            first = {key: set(value) for key, value in digests.items()}
            caps = [*checks['hazard']['remote_sample_valid_until_unix_ns'].values(),
                    *checks['people']['sample_valid_until_unix_ns'],
                    *(instant(h['payload']['valid_until']) for h in readers['hazard']['hazards']),
                    *(instant(t['payload']['valid_until']) for t in readers['people']['tracks'])]
            ns = max(caps) - model.ANCHOR + 100_000_000
        else:
            ns += 100_000_000
    runner.save('playback-steps.json', {'version': 1, 'steps': rows})
    return {'steps': rows, 'same_scene_frame_package_clock': True, 'independent_reader_clocks_retained': True,
            'hazard_and_people_originals_matched': True, 'sample_expiry_observed': True,
            'fresh_restoration_observed': True, 'origin_attested_inference': True,
            'receiver_raw_inputs_independently_verified': False, 'physical_hardware_verified': False}

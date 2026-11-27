"""Unsigned structural fixtures test correspondence, not cryptography or runtime."""
from copy import deepcopy
from pathlib import Path
import tempfile
import unittest

from integration.remotemediafusion import model, playback, proof
from integration.remotemediafusion.test_proof import fixture, signed, request
from integration.hazard.test_playback import encoded, instant


def step_fixture(index, ns, present=True):
    reader, origins, receipts, runner, _ = fixture()
    publication = next(iter(origins.values()))['publication']
    hazard = reader['hazards'][0]['payload']
    captures, media = {}, {}
    for item, original in zip(hazard['inputs'], publication['inputs']):
        record = model.strict_json(proof.decode(model.strict_json(proof.decode(original['envelope_json']))['record']))
        pub = record['publication']; sensor = item['sensor_id']
        capture = model.strict_json(proof.decode(pub['payload']))
        sample = model.strict_json(proof.decode(capture['sample_json']))
        sample.update(sample_ns=ns, sequence=index, id=runner.scene['run_id'] + '/' + sensor + '/' + str(index))
        capture['sample_json'] = encoded(model.json_bytes(sample))
        capture['captured_unix_ns'] += ns - 1_000_000_000
        pub['payload'] = encoded(model.json_bytes(capture))
        pub['timing']['sample_time'] = instant(ns)
        record['sequence'] = index
        raw, inner = signed(record)
        ref = deepcopy(original['reference'])
        ref.update(version={'epoch': 1, 'sequence': index}, envelope_sha256=model.digest(raw), record_sha256=model.digest(inner))
        original.update(reference=ref, envelope_json=encoded(raw))
        until, _, start = proof.deadlines(pub, sample, capture)
        item.update(sample_id=sample['id'], sample_time=instant(ns), version=ref['version'],
                    capture_start=instant(start - model.ANCHOR), valid_until=instant(until - model.ANCHOR))
        item['capture']['proof_sha256'] = ref['envelope_sha256']
        if sensor != 'lidar-atlas':
            item['sample'] = sample
        fetched = next((row for row in publication['media'] if row['source']['service'] == sensor), None)
        data = proof.decode(fetched['data']) if fetched else b''
        captures[sensor] = {'capture': capture, 'media': encoded(data)}
        if data:
            media[sensor] = data
        if sensor in receipts:
            receipts[sensor].update(capture_id=sample['id'], native_sequence=index, sample_ns=ns,
                captured_unix_ns=capture['captured_unix_ns'], request_json=encoded(model.json_bytes(request(pub))),
                signed_record_json=encoded(raw), proof_sha256=ref['envelope_sha256'], record_sha256=ref['record_sha256'])
    hazard.update(confidence=.8, uncertainty_m=.4, gas_value_ppm=80, audio={'tone_fraction': .9},
                  cameras=[{'sensor_id': c, 'red_fraction': .1} for c in model.CAMERAS],
                  valid_until=min(hazard['inputs'], key=lambda i: proof.instant(i['valid_until']))['valid_until'])
    generic = {'version': 1, 'result_schema': 'wendy.hazard.v1', 'result_json': encoded(model.json_bytes(hazard)),
               'inputs': [p['reference'] for p in publication['inputs']]}
    record = model.strict_json(proof.decode(model.strict_json(proof.decode(publication['signed_record_json']))['record']))
    record['sequence'] = index
    start = min(proof.instant(item['capture_start']) for item in hazard['inputs'])
    record['publication'].update(payload=encoded(model.json_bytes(generic)), ttl_ns=proof.instant(hazard['valid_until']) - start)
    record['publication']['timing']['sample_time'] = instant(start - model.ANCHOR)
    raw, inner = signed(record); digest = model.digest(raw)
    publication.update(signed_record_json=encoded(raw), proof_sha256=digest, record_sha256=model.digest(inner))
    origins = {digest: {'publication': publication}}
    reader.update(tick=index + 4, sample_time=instant(ns), hazards=[{'envelope_json': encoded(raw),
        'envelope_sha256': digest, 'record_sha256': model.digest(inner), 'version': {'epoch': 2, 'sequence': index}, 'payload': hazard}])
    checked = proof.correspondence(reader, origins, receipts, runner, ns)
    sensors = model.SENSORS if present else model.LOCAL_SENSORS
    if not present:
        reader['hazards'] = []; origins = {}; receipts = {}; checked = None
        captures = {k: v for k, v in captures.items() if k in sensors}
        media = {k: v for k, v in media.items() if k in sensors}
    req = {'simulation_time_ns': ns, 'sensors': list(sensors)}
    presentation = {'simulation_time_ns': ns, 'geometry': {'schema': 'wendy.macos-demo.geometry.v1',
        'run_id': runner.scene['run_id'], 'model_sha256': 'b' * 64, 'geoms': [], 'lights': [],
        'coordinate_frame': {'up': 'Z', 'units': 'metres', 'handedness': 'right'}}, 'dynamic_geoms': [],
        'sensor_sources': [{'id': s, 'enabled': s in sensors} for s in model.SENSORS],
        'camera_sources': [{'id': s, 'enabled': s in sensors} for s in model.CAMERAS]}
    return runner, {'name': playback.NAMES[index - 1], 'simulation_time_ns': ns, 'reader': reader,
        'origins': origins, 'remote_origins': receipts, 'correspondence': checked, 'captures': captures, 'media': media,
        'presentation': presentation, 'control': {'request': req, 'response': {'sequence': index, **req},
            'no_mutation_retry': True, 'source_clock_shared': True, 'clock_domain': runner.scene['clock_domain'],
            'anchor_unix_ns': model.ANCHOR}}


def complete_fixture():
    runner, first = step_fixture(1, 1_000_000_000)
    _, middle = step_fixture(2, 4_100_000_000, False)
    _, last = step_fixture(3, 4_200_000_000)
    return runner, [first, middle, last]


def write_run(root, runner, steps):
    def save(name, data):
        path = root / name; path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data if isinstance(data, bytes) else model.json_bytes(data))
        return name
    save('result.json', {'passed': True, 'gate_completed': True, 'cleanup': {'complete': True}})
    save('pair-public.json', runner.pair); save('scene-identity.json', runner.scene)
    for key, value in runner.tasks.items():
        node, service = key.split('/')
        save('task-' + node + '-' + service + '.json', {**value, 'node': node, 'service': service, 'app': model.APP,
            'device': runner.pair['nodes'][0 if node == 'a' else 1]['public']['principal']})
    rows = []
    for i, step in enumerate(steps, 1):
        prefix = f'steps/{i:04d}'
        row = {k: step[k] for k in ('name', 'simulation_time_ns')}
        row.update({k: save(prefix + '-' + k + '.json', step[k]) for k in ('presentation', 'control', 'reader')})
        row['origin'] = save(prefix + '-origin.json', step['origins']) if step['origins'] else None
        row['remote_origins'] = {k: save(prefix + '-' + k + '-origin.json', v) for k, v in step['remote_origins'].items()}
        row['correspondence'] = save(prefix + '-correspondence.json', step['correspondence']) if step['correspondence'] else None
        row['captures'] = {k: save(prefix + '-' + k + '.json', v) for k, v in step['captures'].items()}
        row['media'] = {k: save(prefix + '-' + k + '.bin', v) for k, v in step['media'].items()}
        row.update(hazards=len(step['reader']['hazards']), original_envelope_sha256=[h['envelope_sha256'] for h in step['reader']['hazards']],
                   reader_observation_semantics='live_at_original_tick_not_at_host_collection')
        rows.append(row)
    save('playback-steps.json', {'version': 1, 'steps': rows})


class PlaybackTests(unittest.TestCase):
    def test_original_split_device_chain_and_no_rebasing(self):
        runner, steps = complete_fixture()
        geometry, frames, originals, assets = playback.build_timeline(runner, steps)
        self.assertEqual(len(originals), 2)
        self.assertEqual(len(frames), 7)
        self.assertEqual({f['state']['snapshot']['time_s'] for f in frames}, {1, 4.1, 4.2})
        first = frames[0]['state']['snapshot']['fusion']
        self.assertEqual(first['position'], [1, -2, .7])
        self.assertEqual(len(first['observations']), 4)
        self.assertEqual(first['original_envelope_sha256'], steps[0]['reader']['hazards'][0]['envelope_sha256'])
        refs = {r['source']['stream']: r for r in first['native_evidence']}
        self.assertEqual(set(refs), {'camera-a', 'camera-b', 'audio-1'})
        for used in steps[0]['reader']['hazards'][0]['payload']['inputs']:
            if used['sensor_id'] in refs:
                ref = refs[used['sensor_id']]
                self.assertEqual(ref['proof_envelope_sha256'], used['capture']['proof_sha256'])
                self.assertEqual(ref['sha256'], used['evidence']['sha256'])
                self.assertEqual(ref['sample_time'], used['sample_time'])
                self.assertEqual(ref['source']['device'], used['source']['device'])
        self.assertEqual(refs['audio-1']['content_type'], 'audio/wav')
        self.assertIn('fetched by A', next(r['label'] for r in first['observations'] if r['id'] == 'camera-b'))
        empty = next(f for f in frames if f['source_step']['name'] == 'original-expiry')
        self.assertEqual(empty['assets'], {})
        self.assertEqual(empty['state']['snapshot']['fusion']['observations'], [])
        self.assertIn('A camera, gas and LiDAR remain enabled', empty['state']['snapshot']['fusion']['basis'])
        self.assertEqual({a['target'] for f in frames for a in f['actions']}, {'Floor plan', 'Overview', 'Hazards', 'Observations'})
        for step in (steps[0], steps[2]):
            for digest, row in step['origins'].items():
                self.assertEqual(originals[digest], proof.decode(row['publication']['signed_record_json']))

    def test_both_remote_originals_and_fetched_media_required(self):
        for kind in ('camera-b', 'audio-1', 'fetched-audio', 'correspondence'):
            with self.subTest(kind=kind):
                runner, steps = complete_fixture()
                first = steps[0]
                if kind in model.REMOTE_SENSORS:
                    del first['remote_origins'][kind]
                elif kind == 'fetched-audio':
                    pub = next(iter(first['origins'].values()))['publication']
                    next(m for m in pub['media'] if m['source']['service'] == 'audio-1')['data'] = encoded(b'changed')
                else:
                    first['correspondence']['remote_sample_valid_until_unix_ns']['audio-1'] += 1
                with self.assertRaises(model.Refused):
                    playback.build_timeline(runner, steps)

    def test_source_media_and_capture_must_match_actual_a_input(self):
        for kind in ('bytes', 'capture'):
            runner, steps = complete_fixture()
            if kind == 'bytes':
                steps[0]['media']['camera-b'] += b'changed'
            else:
                steps[0]['captures']['camera-b']['capture']['captured_unix_ns'] += 1
            with self.assertRaises(model.Refused):
                playback.build_timeline(runner, steps)

    def test_absence_waits_for_both_remote_original_deadlines(self):
        runner, steps = complete_fixture()
        # The result/audio expire first; camera-b still has its original TTL.
        _, steps[1] = step_fixture(2, 3_800_000_000, False)
        with self.assertRaisesRegex(model.Refused, 'camera, audio or hazard expiry'):
            playback.build_timeline(runner, steps)

    def test_reader_time_scope_and_real_source_controls_required(self):
        for kind in ('time', 'generation', 'source-controls'):
            runner, steps = complete_fixture()
            if kind == 'time':
                steps[0]['reader']['sample_time'] = instant(1_100_000_000)
            elif kind == 'generation':
                runner.tasks['b/camera-b']['generation'] += 1
            else:
                steps[1]['presentation']['sensor_sources'][0]['enabled'] = False
            with self.assertRaises(model.Refused):
                playback.build_timeline(runner, steps)

    def test_three_steps_required_and_inputs_unchanged(self):
        runner, steps = complete_fixture(); before = deepcopy(steps)
        with self.assertRaises(model.Refused):
            playback.build_timeline(runner, steps[:2])
        playback.build_timeline(runner, steps)
        self.assertEqual(steps, before)
        from integration.hazard import playback as legacy
        self.assertEqual(legacy.model.APP, 'com.wendy.world.hazard')

    def test_assemble_gate_inventory_and_exact_output(self):
        runner, steps = complete_fixture()
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory); run = root / 'run'; run.mkdir()
            write_run(run, runner, steps)
            out = root / 'playback'
            result = playback.assemble(run, out)
            self.assertEqual(result['recorded_steps'], 3)
            self.assertFalse(result['new_crypto_verification'])
            self.assertFalse(result['sample_deadlines_rebased'])
            self.assertIn('steps/0001-audio-1-origin.json', result['inputs'])
            self.assertIn('steps/0001-audio-1.bin', result['inputs'])
            with self.assertRaises(model.Refused):
                playback.assemble(run, out)
            (run / 'result.json').write_bytes(model.json_bytes({'passed': False, 'gate_completed': True, 'cleanup': {'complete': True}}))
            with self.assertRaises(model.Refused):
                playback.assemble(run, root / 'rejected')
            self.assertFalse((root / 'rejected').exists())


if __name__ == '__main__':
    unittest.main()

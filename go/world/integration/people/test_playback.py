"""Structural replay fixtures only; these dummy envelopes are not crypto tests."""
import base64
import copy
from datetime import datetime, timezone
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest

from integration.people import model, playback
from integration.people.test_model import identity_fixture, pair_fixture


def instant(ns):
    seconds, nanos = divmod(model.ANCHOR + ns, 1_000_000_000)
    return datetime.fromtimestamp(seconds, timezone.utc).strftime('%Y-%m-%dT%H:%M:%S') + '.%09dZ' % nanos


def encoded(value):
    return base64.b64encode(value).decode()


def track_fixture(pair, scene, ns, sequence, motion=False):
    members = [{'sensor_id': camera, 'sample_id': f'{scene["run_id"]}/{camera}/{sequence}',
                'image': {'sha256': str(index + 1) * 64}, 'model_sha256': 'd' * 64,
                'calibration_sha256': 'e' * 64} for index, camera in enumerate(model.CAMERAS)]
    payload = {'schema': 'wendy.person.track.v1', 'track_id': 'anonymous-1', 'revision': sequence,
               'run_id': scene['run_id'], 'clock_domain': scene['clock_domain'], 'frame': scene['frame'],
               'observed_at': instant(ns), 'valid_until': instant(ns + 1_250_000_000),
               'position_xz': [1.25, -.5], 'covariance': [.1, 0, 0, .2], 'ambiguous': False,
               'motion_known': motion, 'current': members}
    if motion:
        payload['velocity_xz'] = [.4, -.2]
    result = {'version': 1, 'result_schema': 'wendy.person.track.v1',
              'result_json': encoded(json.dumps(payload, indent=2).encode()), 'inputs': []}
    record = {'epoch': 3, 'sequence': sequence,
              'scope': {'tenant': pair['tenant'], 'site': scene['site'],
                        'device': pair['nodes'][0]['public']['principal'], 'app': model.APP,
                        'service': 'people', 'execution_domain': 'simulation'},
              'publication': {'component_type': model.RESULT, 'stream': 'tracks',
                  'entity': scene['run_id'] + '/track/anonymous-1', 'frame': scene['frame'],
                  'tombstone': False, 'payload': encoded(model.json_bytes(result)), 'ttl_ns': 1_250_000_000,
                  'timing': {'sample_time': instant(ns), 'clock_domain': scene['clock_domain'],
                             'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
    record_raw = json.dumps(record, indent=3).encode()
    raw = b' \n' + json.dumps({'record': encoded(record_raw), 'signature': encoded(b'not-a-real-signature')}, indent=2).encode() + b'\n'
    digest = model.digest(raw)
    row = {'envelope_json': encoded(raw), 'envelope_sha256': digest, 'record_sha256': model.digest(record_raw),
           'version': {'epoch': 3, 'sequence': sequence}, 'payload': payload}
    origin = {digest: {'publication': {'locally_verified': True, 'signed_record_json': encoded(raw)}}}
    return row, origin, raw


def fixture():
    pair, _ = pair_fixture()
    scene = identity_fixture()
    geometry = {'schema': 'wendy.macos-demo.geometry.v1', 'run_id': scene['run_id'],
                'model_sha256': scene['package_sha256'], 'geoms': []}
    steps = []
    for index, ns in enumerate((100_000_000, 500_000_000, 2_000_000_000, 2_100_000_000), 1):
        cameras = [] if index == 3 else list(model.CAMERAS)
        tracks, origins = [], {}
        if cameras:
            row, origins, _ = track_fixture(pair, scene, ns, index, motion=index == 2)
            tracks.append(row)
        reader = {'version': 1, 'status': 'ready', 'tick': index + 10, 'run_id': scene['run_id'],
                  'reader_generation': 9, 'sample_time': instant(ns), 'tracks': tracks,
                  'absent_track_ids': ['anonymous-1'] if not cameras else [],
                  'origin_attested_inference': True, 'camera_inputs_independently_verified': False}
        request = {'simulation_time_ns': ns, 'cameras': cameras}
        steps.append({'name': playback.NAMES[index - 1], 'simulation_time_ns': ns, 'reader': reader,
                      'origins': origins, 'control': {'request': request, 'response': {'sequence': index, **request},
                          'receiver_clock': {'domain': scene['clock_domain'], 'now': instant(ns)}, 'no_mutation_retry': True},
                      'presentation': {'geometry': geometry, 'simulation_time_ns': ns,
                          'dynamic_geoms': [{'id': 7, 'position': [index, 0, 0], 'rotation': [1, 0, 0, 0, 1, 0, 0, 0, 1]}],
                          'camera_sources': [{'id': c, 'enabled': c in cameras} for c in model.CAMERAS]}})
    return pair, scene, steps


def write_run(directory, pair, scene, steps):
    def write(name, value):
        p = directory / name
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_bytes(model.json_bytes(value))
    write('result.json', {'passed': True, 'gate_completed': True})
    write('pair-public.json', pair)
    write('scene-identity.json', scene)
    write('task-b-reader.json', {'node': 'b', 'app': model.APP, 'service': 'reader',
                               'device': pair['nodes'][1]['public']['principal'], 'generation': 9})
    rows = []
    for index, step in enumerate(steps, 1):
        prefix = 'steps/%04d' % index
        row = {'name': step['name'], 'simulation_time_ns': step['simulation_time_ns']}
        for field in ('reader', 'control', 'presentation'):
            row[field] = prefix + '-' + field + '.json'
            write(row[field], step[field])
        row['origin'] = prefix + '-origin.json' if step['origins'] else None
        if row['origin']:
            write(row['origin'], step['origins'])
        row['original_envelope_sha256'] = list(step['origins'])
        rows.append(row)
    write('playback-steps.json', {'version': 1, 'steps': rows})


class PlaybackTests(unittest.TestCase):
    def setUp(self):
        self.pair, self.scene, self.steps = fixture()

    def build(self, steps=None):
        return playback.build_timeline(self.pair, self.scene, 9, steps or self.steps)

    def test_exact_originals_times_and_unknown_motion_survive_native_mapping(self):
        geometry, frames, originals = self.build()
        self.assertEqual(len(frames), 6)
        self.assertEqual(geometry, self.steps[0]['presentation']['geometry'])
        self.assertEqual([f['at_s'] for f in frames], [0, 5, 6.5, 10, 20, 30])
        first = frames[0]['state']['snapshot']['tracking']
        track = first['tracks'][0]
        expected = self.steps[0]['reader']['tracks'][0]['payload']
        self.assertEqual(track['last_observed'], expected['observed_at'])
        self.assertEqual(track['valid_until'], expected['valid_until'])
        self.assertEqual(track['position_xz'], expected['position_xz'])
        self.assertEqual(track['covariance'], expected['covariance'])
        self.assertFalse(track['motion_known'])
        self.assertNotIn('velocity_xz', track)
        self.assertEqual(first['now_ns'], 100_000_000)
        self.assertEqual(first['cameras'], [])
        self.assertEqual(len(track['members']), 2)
        self.assertEqual(track['members'][0]['evidence'][0]['sha256'], expected['current'][0]['image']['sha256'])
        digest = self.steps[0]['reader']['tracks'][0]['envelope_sha256']
        self.assertEqual(originals[digest], base64.b64decode(self.steps[0]['reader']['tracks'][0]['envelope_json']))
        self.assertTrue(originals[digest].startswith(b' \n'))
        self.assertEqual(frames[3]['state']['snapshot']['tracking']['tracks'][0]['velocity_xz'], [.4, -.2])

    def test_only_recorded_expiry_and_restoration_change_tracks_and_source_cues(self):
        _, frames, _ = self.build()
        expired, restored = frames[4:]
        self.assertEqual(expired['state']['snapshot']['tracking']['tracks'], [])
        self.assertFalse(any(s['enabled'] for s in expired['state']['snapshot']['tracking']['camera_sources']))
        self.assertEqual(expired['pointer']['label'], 'Disable camera sources')
        self.assertEqual(restored['pointer']['label'], 'Restore camera sources')
        self.assertEqual(restored['state']['dynamic_geoms'], self.steps[3]['presentation']['dynamic_geoms'])
        for frame in frames:
            self.assertEqual(frame['assets'], {})
            self.assertEqual(frame['state']['snapshot']['tracking']['cameras'], [])
            for action in frame['actions']:
                self.assertIn(action['kind'], ('layer', 'camera'))
        self.assertEqual(frames[0]['state'], frames[1]['state'])
        self.assertEqual(frames[1]['state'], frames[2]['state'])

    def test_waiting_refusal_and_incomplete_camera_verification_cannot_be_ready_playback(self):
        for patch in ({'status': 'waiting_for_source', 'tracks': [], 'origin_attested_inference': False},
                      {'status': 'failed'}, {'camera_inputs_independently_verified': True},
                      {'reader_generation': 8}, {'run_id': 'other'}):
            steps = copy.deepcopy(self.steps)
            steps[0]['reader'].update(patch)
            with self.subTest(patch=patch), self.assertRaises(model.Refused):
                self.build(steps)

    def test_exact_envelope_ack_record_payload_and_version_binding(self):
        mutations = [lambda s: s['reader']['tracks'][0].update(envelope_sha256='f' * 64),
                     lambda s: s['reader']['tracks'][0].update(record_sha256='f' * 64),
                     lambda s: s['reader']['tracks'][0]['payload'].update(position_xz=[9, 9]),
                     lambda s: s['reader']['tracks'][0]['version'].update(sequence=999),
                     lambda s: s.update(origins={}),
                     lambda s: next(iter(s['origins'].values()))['publication'].update(locally_verified=False),
                     lambda s: next(iter(s['origins'].values()))['publication'].update(signed_record_json=encoded(b'changed'))]
        for change in mutations:
            steps = copy.deepcopy(self.steps)
            change(steps[0])
            with self.subTest(change=change), self.assertRaises(model.Refused):
                self.build(steps)

    def test_original_deadline_and_source_step_cannot_be_rebased(self):
        steps = copy.deepcopy(self.steps)
        later = 1_350_000_000  # Equality with the original track deadline.
        steps[0]['reader']['sample_time'] = instant(later)
        with self.assertRaises(model.Refused):
            playback.receiver_tracks(steps[0]['reader'], steps[0]['origins'], self.pair, self.scene, later, 9)
        for field in ('presentation', 'control'):
            steps = copy.deepcopy(self.steps)
            if field == 'presentation':
                steps[1][field]['simulation_time_ns'] += 1
            else:
                steps[1][field]['receiver_clock']['now'] = instant(500_000_001)
            with self.subTest(field=field), self.assertRaises(model.Refused):
                self.build(steps)
        self.assertEqual(playback.timestamp_ns('2026-01-01T00:00:00.000000001Z'), model.ANCHOR + 1)

    def test_still_live_previous_result_is_not_a_new_measurement(self):
        steps = copy.deepcopy(self.steps)
        steps[1]['reader']['tracks'] = copy.deepcopy(steps[0]['reader']['tracks'])
        steps[1]['origins'] = copy.deepcopy(steps[0]['origins'])
        # The old original is still valid at the receiver's new .5s clock.
        tracks, _ = playback.receiver_tracks(steps[1]['reader'], steps[1]['origins'],
            self.pair, self.scene, 500_000_000, 9)
        self.assertEqual(len(tracks), 1)
        # It cannot substantiate the storyboard's new-observation chapter.
        with self.assertRaises(model.Refused):
            self.build(steps)

    def test_control_and_reference_mismatches_are_not_turned_into_a_demo(self):
        mutations = [lambda s: s['control'].update(no_mutation_retry=False),
                     lambda s: s['control']['response'].update(sequence=100),
                     lambda s: s['presentation']['camera_sources'][0].update(enabled=False),
                     lambda s: s['presentation']['geometry'].update(model_sha256='f' * 64)]
        for change in mutations:
            steps = copy.deepcopy(self.steps)
            change(steps[0])
            with self.subTest(change=change), self.assertRaises(model.Refused):
                self.build(steps)

    def test_assemble_records_input_hashes_and_is_accepted_by_existing_timeline_loader(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            run, out = root / 'run', root / 'playback'
            write_run(run, self.pair, self.scene, self.steps)
            before = {str(p.relative_to(run)): p.read_bytes() for p in run.rglob('*') if p.is_file()}
            report = playback.assemble(run, out)
            self.assertEqual(report['recorded_steps'], 4)
            self.assertFalse(report['sample_deadlines_rebased'])
            self.assertFalse(report['new_crypto_verification'])
            self.assertEqual(report['camera_thumbnails'], 0)
            self.assertEqual(report['inputs']['steps/0001-reader.json']['sha256'], model.digest(before['steps/0001-reader.json']))
            spec = importlib.util.spec_from_file_location('promo_render_for_test', Path(__file__).resolve().parents[2] / 'integration/nativeapp/render.py')
            render = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(render)
            _, _, frames, assets = render.timeline_inputs(out / 'timeline')
            self.assertEqual(len(frames), 6)
            self.assertEqual(assets, {})
            self.assertEqual(before, {str(p.relative_to(run)): p.read_bytes() for p in run.rglob('*') if p.is_file()})
            for digest, pin in report['originals'].items():
                raw = (out / 'originals' / (digest + '.json')).read_bytes()
                self.assertEqual(pin, {'bytes': len(raw), 'sha256': model.digest(raw)})
            with self.assertRaises(model.Refused):
                playback.assemble(run, out)

    def test_failed_run_is_not_renderable_and_does_not_create_output(self):
        with tempfile.TemporaryDirectory() as directory:
            run, out = Path(directory) / 'run', Path(directory) / 'out'
            write_run(run, self.pair, self.scene, self.steps)
            (run / 'result.json').write_bytes(model.json_bytes({'passed': False, 'gate_completed': False}))
            with self.assertRaises(model.Refused):
                playback.assemble(run, out)
            self.assertFalse(out.exists())


if __name__ == '__main__':
    unittest.main()

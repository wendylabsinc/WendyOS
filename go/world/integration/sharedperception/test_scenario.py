"""Structural fixture tests; no signature verification or native runtime claim."""
import base64
from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.sharedperception import model, scenario
from integration.sharedperception.test_model import scene_fixture
from integration.people.test_model import pair_fixture
from integration.hazard.test_scenario import stamp
from integration.remotemediafusion import test_scenario as old


def encode(value): return base64.b64encode(model.json_bytes(value)).decode()


def clock(runner, ns, generation, kind):
    return {'sample_time': stamp(ns), 'sample_clock_domain': runner.scene['clock_domain'],
            'physical_earliest': stamp(100), 'physical_latest': stamp(200 if kind == 'hazard' else 300),
            'server_boot_id': 'same-agent-b', 'deployment_generation': generation}


def people_fixture(runner, ns, sequence=1):
    proofs, members, captures = [], [], {}
    for sensor in model.PEOPLE_CAMERAS:
        image = (sensor + str(ns)).encode()
        sample = {'id': f'{sensor}/{sequence}', 'sensor_id': sensor, 'sample_ns': ns, 'kind': 'camera',
                  'camera': {'calibration_valid_for_ns': 5_000_000_000}, 'calibration_revision': 'shared-cal-v1',
                  'evidence': {'sha256': model.digest(image), 'size': len(image), 'content_type': 'image/png'}}
        capture = {'version': 1, 'scene_boot_id': runner.scene['boot_id'], 'captured_unix_ns': model.ANCHOR + ns,
                   'simulation_anchor_unix_ns': model.ANCHOR, 'sample_json': encode(sample)}
        captures[sensor] = {'capture': capture, 'media': base64.b64encode(image).decode()}
        source = {'device': runner.pair['nodes'][0]['public']['principal'], 'app': model.APP, 'service': sensor, 'stream': sensor}
        key = {'tenant': runner.pair['tenant'], 'site': runner.scene['site'], 'entity': 'sensor/' + sensor,
               'type': model.CAPTURE, 'source': source}
        record = {'epoch': 1, 'sequence': sequence, 'scope': {**{k: v for k, v in source.items() if k != 'stream'},
                  'tenant': runner.pair['tenant'], 'site': runner.scene['site'], 'execution_domain': 'simulation',
                  'deployment_generation': runner.tasks['a/' + sensor]['generation']},
                  'publication': {'entity': key['entity'], 'component_type': model.CAPTURE, 'stream': sensor,
                    'frame': runner.scene['frame'], 'tombstone': False, 'payload': encode(capture), 'ttl_ns': 3_000_000_000,
                    'timing': {'sample_time': stamp(ns), 'clock_domain': runner.scene['clock_domain'],
                               'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
        inner = model.json_bytes(record); envelope = model.json_bytes({'record': base64.b64encode(inner).decode(), 'signature': 'AA=='})
        ref = {'key': key, 'version': {'epoch': 1, 'sequence': sequence}, 'envelope_sha256': model.digest(envelope),
               'record_sha256': model.digest(inner)}
        proofs.append({'reference': ref, 'envelope_json': base64.b64encode(envelope).decode()})
        members.append({'proof': ref, 'sensor_id': sensor, 'sample_id': sample['id'], 'image': sample['evidence'],
                        'sample_time': stamp(ns), 'valid_until': stamp(ns + 1_250_000_000),
                        'physical_until': stamp(ns + 5_000_000_000), 'scene_boot_id': runner.scene['boot_id'],
                        'captured_unix_ns': capture['captured_unix_ns'], 'simulation_anchor_unix_ns': model.ANCHOR,
                        'calibration_revision': sample['calibration_revision'], **runner.inputs['model']})
    payload = {'schema': 'wendy.person.track.v1', 'track_id': 'anonymous-1', 'revision': sequence,
               'run_id': runner.scene['run_id'], 'clock_domain': runner.scene['clock_domain'], 'frame': runner.scene['frame'],
               'observed_at': stamp(ns), 'valid_until': stamp(ns + 1_250_000_000), 'position_xz': [1, 2],
               'covariance': [.1, 0, 0, .1], 'ambiguous': False, 'motion_known': False, 'current': members}
    result = {'version': 1, 'result_schema': 'wendy.person.track.v1', 'result_json': encode(payload),
              'inputs': [p['reference'] for p in proofs]}
    record = {'epoch': 1, 'sequence': sequence, 'scope': {'tenant': runner.pair['tenant'], 'site': runner.scene['site'],
              'device': runner.pair['nodes'][0]['public']['principal'], 'app': model.APP, 'service': 'people',
              'execution_domain': 'simulation', 'deployment_generation': runner.tasks['a/people']['generation']},
              'publication': {'entity': runner.scene['run_id'] + '/track/anonymous-1', 'component_type': model.RESULT,
                 'stream': 'tracks', 'frame': runner.scene['frame'], 'tombstone': False, 'payload': encode(result),
                 'ttl_ns': 1_250_000_000, 'timing': {'sample_time': stamp(ns), 'clock_domain': runner.scene['clock_domain'],
                    'error_known': True, 'error_bound_ns': 0, 'age_hint_ns': 0}}}
    inner = model.json_bytes(record); raw = model.json_bytes({'record': base64.b64encode(inner).decode(), 'signature': 'AA=='})
    digest = model.digest(raw)
    row = {'envelope_json': base64.b64encode(raw).decode(), 'envelope_sha256': digest,
           'record_sha256': model.digest(inner), 'version': {'epoch': 1, 'sequence': sequence}, 'payload': payload}
    origins = {digest: {'tick': sequence, 'run_id': runner.inputs['run_id'], 'inputs': proofs,
                        'publication': {'locally_verified': True, 'signed_record_json': row['envelope_json']}}}
    generation = runner.tasks['b/people-reader']['generation']
    reader = {'version': 1, 'status': 'ready', 'tick': sequence, 'run_id': runner.scene['run_id'],
              'reader_generation': generation, 'sample_time': stamp(ns), 'tracks': [row],
              'origin_attested_inference': True, 'camera_inputs_independently_verified': False,
              'clock': clock(runner, ns, generation, 'people')}
    return reader, origins, captures


class Fixture(old.Fixture):
    def __init__(self, out):
        super().__init__(out)
        self.scene = scene_fixture()
        self.inputs['model'] = {'model_id': 'movenet', 'model_version': '1', 'model_sha256': 'f' * 64}
        self.tasks.update({'a/people': {'generation': 20}, 'b/people-reader': {'generation': 21},
                          'a/people-front': {'generation': 11}, 'a/people-oblique': {'generation': 12}})
        self.nodes = {key: Node(self, key) for key in ('a', 'b')}


class Node(old.Node):
    def latest(self, service):
        if service == 'people': return {'run_id': self.owner.inputs['run_id'], 'status': 'waiting'}
        if service == 'people-reader':
            row = people_fixture(self.owner, self.owner.ns, self.owner.sequence + 1)[0]
            if not self.owner.sensors: row['tracks'] = []
            return row
        row = super().latest(service)
        if service == 'reader': row['clock'] = clock(self.owner, self.owner.ns, 7, 'hazard')
        return row

    def python(self, script, *args, **kwargs):
        if script == scenario.control.READER_OBSERVATIONS:
            service = 'people-reader' if '/people-reader/' in args[0] else 'reader'
            return SimpleNamespace(returncode=0, stdout=model.json_bytes(self.latest(service)))
        if script == scenario.PEOPLE_PUBLICATIONS:
            return SimpleNamespace(stdout=model.json_bytes(people_fixture(self.owner, self.owner.ns, self.owner.sequence + 1)[1]))
        return super().python(script, *args, **kwargs)

    def scene_request(self, path, request=None):
        if path.startswith('/v1/captures/'):
            sensor = path.split('/')[-1]
            if sensor in model.PEOPLE_CAMERAS:
                value = people_fixture(self.owner, self.owner.ns, self.owner.sequence + 1)[2][sensor]
            else:
                value = model.strict_json(super().scene_request(path, request))
            sample = model.strict_json(base64.b64decode(value['capture']['sample_json']))
            sample.update(frame=self.owner.scene['frame'], package_sha256=self.owner.scene['package_sha256'],
                          run_id=self.owner.inputs['run_id'])
            value['capture']['sample_json'] = encode(sample)
            return model.json_bytes(value)
        return super().scene_request(path, request)


class ScenarioTests(unittest.TestCase):
    def test_tracks_bind_original_camera_bytes_model_generation_and_deadlines(self):
        runner = Fixture(Path('/unused')); ns = 1_000_000_000
        reader, origins, captures = people_fixture(runner, ns)
        report = scenario.people_correspondence(reader, origins, captures, runner, ns)
        self.assertEqual(report['track_count'], 1)
        self.assertEqual({p['sensor_id'] for p in report['current_inputs']}, set(model.PEOPLE_CAMERAS))
        for mutation in ('capture', 'ack', 'generation', 'model'):
            r, o, c = deepcopy(reader), deepcopy(origins), deepcopy(captures)
            original_generation = runner.tasks['a/people']['generation']
            original_model = runner.inputs['model']['model_sha256']
            if mutation == 'capture': c['people-front']['capture']['captured_unix_ns'] += 1
            if mutation == 'ack': next(iter(o.values()))['publication']['signed_record_json'] = encode({})
            if mutation == 'generation': runner.tasks['a/people']['generation'] += 1
            if mutation == 'model': runner.inputs['model']['model_sha256'] = 'a' * 64
            with self.subTest(mutation=mutation), self.assertRaises((ValueError, KeyError)):
                scenario.people_correspondence(r, o, c, runner, ns)
            runner.tasks['a/people']['generation'] = original_generation
            runner.inputs['model']['model_sha256'] = original_model

    def test_each_reader_clock_and_history_remain_independent(self):
        runner = Fixture(Path('/unused')); runner.ns, runner.sequence, runner.sensors = 1_000_000_000, 1, list(model.SENSORS)
        hazard, people = runner.nodes['b'].latest('reader'), runner.nodes['b'].latest('people-reader')
        for kind, value in (('hazard', hazard), ('people', people)):
            self.assertTrue(scenario.reader_matches(value, runner, runner.ns, kind, True))
            empty = {**value, 'tick': value['tick'] + 1, 'hazards' if kind == 'hazard' else 'tracks': []}
            self.assertEqual(scenario.recorded_reader_match(model.json_bytes(value) + model.json_bytes(empty), runner, runner.ns, kind, True), value)
        changed = deepcopy(people); changed['clock'] = hazard['clock']
        with self.assertRaises(ValueError): scenario.reader_matches(changed, runner, runner.ns, 'people', True)
        self.assertFalse(scenario.reader_matches({'status': 'waiting_for_source'}, runner, runner.ns, 'people', True))
        with self.assertRaises(ValueError): scenario.recorded_reader_match(b'{}\n' * 513, runner, 0, 'people', False)

    def test_three_steps_keep_one_source_and_both_reader_receipts(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = Fixture(Path(directory))
            with patch.object(scenario._hazard_proof, 'correspondence', side_effect=old.check_selected), \
                 patch.object(scenario, 'people_correspondence', return_value={'sample_valid_until_unix_ns': [model.ANCHOR + 4_000_000_000]}) as people:
                result = scenario.execute(runner)
            self.assertEqual(people.call_count, 2)
            self.assertEqual([r['hazards'] for r in result['steps']], [1, 0, 1])
            self.assertEqual([r['tracks'] for r in result['steps']], [1, 0, 1])
            steps = [e[1] for e in runner.events if e[0] == 'step']
            self.assertEqual(steps, [{'simulation_time_ns': 1_000_000_000, 'sensors': list(model.SENSORS)},
                                    {'simulation_time_ns': 4_100_000_000, 'sensors': []},
                                    {'simulation_time_ns': 4_200_000_000, 'sensors': list(model.SENSORS)}])
            self.assertEqual(set(result['steps'][0]['media']), {'camera-a', 'camera-b', 'audio-1', *model.PEOPLE_CAMERAS})
            self.assertEqual(result['steps'][1]['origins'], {})
            self.assertNotEqual(runner.saved['steps/0001-reader.json']['clock'], runner.saved['steps/0001-people-reader.json']['clock'])

    def test_unknown_source_step_is_not_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = Fixture(Path(directory)); runner.fail_step = True
            with self.assertRaises(ValueError): scenario.execute(runner)
            self.assertEqual(len([e for e in runner.events if e[0] == 'step']), 1)


if __name__ == '__main__': unittest.main()

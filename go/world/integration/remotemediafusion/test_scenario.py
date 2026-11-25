"""Offline scenario ordering with structural receipts and an injected proof checker."""
import base64
from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.remotemediafusion import control, model, scenario
from integration.hazard.test_model import identity_fixture
from integration.hazard.test_scenario import stamp, encoded
from integration.people.test_model import pair_fixture


def observation(owner):
    a, b = [node['public']['principal'] for node in owner.pair['nodes']]
    proofs, remote = [], {}
    for sensor in model.SENSORS:
        digest = model.digest((sensor + str(owner.ns)).encode())
        source = {'device': b if sensor in model.REMOTE_SENSORS else a, 'app': model.APP,
                  'service': sensor, 'stream': sensor}
        ref = {'key': {'source': source}, 'envelope_sha256': digest}
        proofs.append({'reference': ref})
        if sensor in model.REMOTE_SENSORS:
            remote[sensor] = {'proof_sha256': digest, 'sample_ns': owner.ns}
    payload = {'inputs': [{'sensor_id': sensor, 'sample_time': stamp(owner.ns)} for sensor in model.SENSORS],
               'valid_until': stamp(owner.ns + 2_750_000_000)}
    inner = model.json_bytes({'epoch': 1, 'sequence': owner.sequence,
        'publication': {'payload': encoded({'result_schema': 'wendy.hazard.v1', 'result_json': encoded(payload)})}})
    raw = model.json_bytes({'record': base64.b64encode(inner).decode(), 'signature': 'AA=='})
    digest = model.digest(raw)
    row = {'envelope_json': base64.b64encode(raw).decode(), 'envelope_sha256': digest,
           'record_sha256': model.digest(inner), 'version': {'epoch': 1, 'sequence': owner.sequence}, 'payload': payload}
    origins = {digest: {'publication': {'status': 'complete', 'locally_verified': True,
        'inputs': proofs, 'signed_record_json': row['envelope_json'], 'record_sha256': row['record_sha256']}}}
    return row, origins, remote


class Fixture:
    def __init__(self, out):
        self.out, self.saved, self.events = out, {}, []
        self.pair, _ = pair_fixture(); self.scene = identity_fixture()
        self.inputs = {'run_id': self.scene['run_id'], 'anchor_unix_ns': model.ANCHOR}
        self.tasks = {'a/fusion': {'generation': 6}, 'b/reader': {'generation': 7},
                      'b/camera-b': {'generation': 5}, 'b/audio-1': {'generation': 8}}
        self.ns, self.sequence, self.sensors, self.fail_step = 0, 0, [], False
        self.nodes = {key: Node(self, key) for key in ('a', 'b')}

    def save(self, name, value): self.saved[name] = value
    def step(self, name): self.events.append(('phase', name))
    def current(self): return observation(self)


class Node:
    def __init__(self, owner, key): self.owner, self.key = owner, key
    def service_volume(self, service, kind='config'): return '/test/' + self.key + '/' + service + '/' + kind

    def latest(self, service):
        if service == 'fusion': return {'status': 'waiting', 'run_id': self.owner.inputs['run_id']}
        hazards = [self.owner.current()[0]] if all(s in self.owner.sensors for s in model.REMOTE_SENSORS) else []
        return {'version': 1, 'status': 'ready', 'tick': self.owner.sequence + 1,
                'run_id': self.owner.inputs['run_id'], 'reader_generation': 7,
                'hazards': hazards, 'sample_time': stamp(self.owner.ns), 'origin_attested_inference': True,
                'raw_inputs_independently_verified': False}

    def cli(self, *args, **kwargs): return SimpleNamespace(returncode=0)

    def python(self, script, *args, **kwargs):
        if script == control.READER_OBSERVATIONS:
            return SimpleNamespace(returncode=0, stdout=model.json_bytes(self.latest('reader')))
        if script == control.MATCH_PUBLICATIONS:
            return SimpleNamespace(stdout=model.json_bytes(self.owner.current()[1]))
        if script == control.MATCH_CAPTURE:
            sensor = next(s for s in model.REMOTE_SENSORS if '/' + s + '/' in args[0])
            value = self.owner.current()[2][sensor]
            assert self.key == 'b' and args[1] == value['proof_sha256']
            return SimpleNamespace(stdout=model.json_bytes(value))
        raise AssertionError('unmocked runtime operation')

    def scene_request(self, path, request=None):
        if path == '/v1/step':
            self.owner.events.append(('step', deepcopy(request)))
            if self.owner.fail_step: raise model.Refused('unknown source write')
            self.owner.sequence += 1; self.owner.ns = request['simulation_time_ns']; self.owner.sensors = request['sensors']
            return model.json_bytes({'sequence': self.owner.sequence, **request})
        if path == '/v1/presentation': return model.json_bytes({'simulation_time_ns': self.owner.ns})
        sensor = path.removeprefix('/v1/captures/')
        sample = {'sample_ns': self.owner.ns, 'sensor_id': sensor, 'run_id': self.owner.inputs['run_id']}
        data = b''
        if sensor in model.CAMERAS or sensor == 'audio-1':
            data = (sensor + str(self.owner.ns)).encode()
            sample['evidence'] = {'sha256': model.digest(data), 'size': len(data),
                                  'content_type': 'audio/wav' if sensor == 'audio-1' else 'image/png'}
        return model.json_bytes({'capture': {'sample_json': encoded(sample)}, 'media': base64.b64encode(data).decode()})


def check_selected(reader, origins, remote, runner, ns):
    # The independently owned proof.py tests cryptographic-byte correspondence.
    # Here we check that scenario selection passes both exact B receipts to it.
    assert set(remote) == set(model.REMOTE_SENSORS)
    for sensor, row in remote.items():
        assert row['sample_ns'] == ns
        assert scenario.remote_reference(origins, sensor)['envelope_sha256'] == row['proof_sha256']
    return {'remote_sample_valid_until_unix_ns': {
        'camera-b': model.ANCHOR + ns + 3_000_000_000,
        'audio-1': model.ANCHOR + ns + 2_750_000_000}}


class ScenarioTests(unittest.TestCase):
    def test_selects_both_b_receipts_and_expires_only_remote_sources(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory))
            with patch.object(scenario, 'correspondence', side_effect=check_selected) as check:
                result = scenario.execute(owner)
            self.assertEqual(check.call_count, 2)
            self.assertTrue(result['b_camera_audio_ack_and_media_to_a_input_to_a_hazard_to_b_reader_matched'])
            self.assertEqual([s['hazards'] for s in result['steps']], [1, 0, 1])
            requests = [e[1] for e in owner.events if e[0] == 'step']
            self.assertEqual(requests[1], {'simulation_time_ns': 4_100_000_000, 'sensors': list(model.LOCAL_SENSORS)})
            self.assertEqual(requests[2]['simulation_time_ns'], 4_200_000_000)
            self.assertEqual(set(result['steps'][0]['remote_origins']), set(model.REMOTE_SENSORS))
            self.assertEqual(result['steps'][1]['remote_origins'], {})
            self.assertEqual(set(result['steps'][1]['captures']), set(model.LOCAL_SENSORS))
            self.assertEqual(set(result['steps'][1]['media']), {'camera-a'})
            self.assertEqual(set(result['steps'][0]['media']), {'camera-a', 'camera-b', 'audio-1'})
            self.assertTrue(owner.saved['steps/0002-control.json']['source_clock_shared'])
            self.assertNotIn('receiver_clock', owner.saved['steps/0002-control.json'])
            self.assertTrue(all(s['reader_observation_semantics'] == 'live_at_original_tick_not_at_host_collection' for s in result['steps']))

    def test_original_positive_read_survives_later_empty_poll_without_restamping(self):
        owner = Fixture(Path('/unused')); owner.ns, owner.sequence = 1_000_000_000, 1
        owner.sensors = list(model.SENSORS)
        positive = owner.nodes['b'].latest('reader')
        empty = {**positive, 'hazards': [], 'tick': positive['tick'] + 1}
        raw = model.json_bytes(positive) + model.json_bytes(empty)
        self.assertEqual(scenario.recorded_reader_match(raw, owner, owner.ns, True), positive)
        self.assertEqual(scenario.recorded_reader_match(raw, owner, owner.ns, False), empty)
        self.assertIsNone(scenario.recorded_reader_match(raw, owner, owner.ns + 1, True))
        with self.assertRaises(model.Refused):
            scenario.recorded_reader_match(b'{}\n' * 513, owner, owner.ns, True)

    def test_unknown_source_step_is_not_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory)); owner.fail_step = True
            with patch.object(scenario, 'correspondence') as check:
                with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertEqual(len([event for event in owner.events if event[0] == 'step']), 1)
            check.assert_not_called()

    def test_missing_second_deadline_cannot_advance_expiry(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory))
            with patch.object(scenario, 'correspondence', return_value={
                'remote_sample_valid_until_unix_ns': {'camera-b': model.ANCHOR + 4_000_000_000}}):
                with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertEqual(len([event for event in owner.events if event[0] == 'step']), 1)

    def test_duplicate_or_wrong_remote_input_refuses_selection(self):
        owner = Fixture(Path('/unused')); owner.ns, owner.sequence = 1_000_000_000, 1
        _, origins, _ = owner.current()
        for sensor in ('camera-b', 'audio-1'):
            ref = scenario.remote_reference(origins, sensor)
            self.assertEqual(ref['key']['source']['device'], owner.pair['nodes'][1]['public']['principal'])
        changed = deepcopy(origins)
        rows = next(iter(changed.values()))['publication']['inputs']
        rows.append(deepcopy(next(row for row in rows if row['reference']['key']['source']['stream'] == 'camera-b')))
        with self.assertRaises(model.Refused): scenario.remote_reference(changed, 'camera-b')
        with self.assertRaises(model.Refused): scenario.remote_reference(origins, 'gas-1')


if __name__ == '__main__':
    unittest.main()

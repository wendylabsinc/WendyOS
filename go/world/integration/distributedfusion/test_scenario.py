"""Structural fake acknowledgments test fixture correspondence, not signatures."""
import base64
from copy import deepcopy
from pathlib import Path
import subprocess
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import patch

from integration.distributedfusion import control, model, scenario
from integration.hazard.test_model import identity_fixture
from integration.hazard.test_scenario import stamp, encoded
from integration.people.test_model import pair_fixture


def observation(owner, ns, sequence=1):
    a, b = [n['public']['principal'] for n in owner.pair['nodes']]
    proofs, gas, typed = [], None, []
    for sensor in sorted(model.SENSORS):
        device = b if sensor == 'gas-1' else a
        source = {'device': device, 'app': model.APP, 'service': sensor, 'stream': sensor}
        sample = {'id': owner.inputs['run_id'] + '/' + sensor + '/' + str(sequence),
                  'sequence': sequence, 'sensor_id': sensor, 'sample_ns': ns, 'run_id': owner.inputs['run_id']}
        capture = {'scene_boot_id': owner.scene['boot_id'], 'simulation_anchor_unix_ns': model.ANCHOR,
                   'captured_unix_ns': 1794400000000000000 + ns, 'sample_json': encoded(sample)}
        publication = {'entity': 'sensor/' + sensor, 'component_type': model.CAPTURE, 'stream': sensor,
                       'frame': owner.scene['frame'], 'tombstone': False, 'payload': encoded(capture), 'ttl_ns': 3_000_000_000,
                       'timing': {'sample_time': stamp(ns), 'error_bound_ns': 0, 'age_hint_ns': 0}}
        inner = model.json_bytes({'scope': {'tenant': owner.pair['tenant'], 'site': owner.scene['site'],
                   'device': device, 'app': model.APP, 'service': sensor, 'deployment_generation': 5},
                   'epoch': 1, 'sequence': sequence, 'publication': publication})
        raw = model.json_bytes({'record': base64.b64encode(inner).decode(), 'signature': 'AA=='})
        ref = {'key': {'tenant': owner.pair['tenant'], 'site': owner.scene['site'], 'entity': 'sensor/' + sensor,
                     'type': model.CAPTURE, 'source': source}, 'version': {'epoch': 1, 'sequence': sequence},
               'envelope_sha256': model.digest(raw), 'record_sha256': model.digest(inner)}
        proofs.append({'reference': ref, 'envelope_json': base64.b64encode(raw).decode()})
        typed.append({'sensor_id': sensor, 'sample_time': stamp(ns), 'source': source})
        if sensor == 'gas-1':
            gas = {'signed_record_json': base64.b64encode(raw).decode(), 'proof_sha256': model.digest(raw),
                   'record_sha256': model.digest(inner), 'sample_ns': ns, 'scene_boot_id': capture['scene_boot_id'],
                   'captured_unix_ns': capture['captured_unix_ns'], 'capture_id': sample['id'], 'native_sequence': sequence}
    # The result expires before the gas, just as the original audio interval can.
    payload = {'inputs': typed, 'valid_until': stamp(ns + 2_750_000_000)}
    generic = {'result_schema': 'wendy.hazard.v1', 'inputs': [p['reference'] for p in proofs], 'result_json': encoded(payload)}
    inner = model.json_bytes({'scope': {'device': a, 'app': model.APP, 'service': 'fusion', 'deployment_generation': 6},
                              'epoch': 1, 'sequence': sequence, 'publication': {'payload': encoded(generic)}})
    raw = model.json_bytes({'record': base64.b64encode(inner).decode(), 'signature': 'AA=='})
    digest = model.digest(raw)
    item = {'envelope_json': base64.b64encode(raw).decode(), 'envelope_sha256': digest,
            'record_sha256': model.digest(inner), 'version': {'epoch': 1, 'sequence': sequence}, 'payload': payload}
    origins = {digest: {'publication': {'status': 'complete', 'locally_verified': True, 'inputs': proofs,
                'signed_record_json': item['envelope_json'], 'record_sha256': item['record_sha256'], 'proof_sha256': digest}}}
    return item, origins, gas


class Fixture:
    def __init__(self, out):
        self.out, self.saved, self.events = out, {}, []
        self.pair, _ = pair_fixture(); self.scene = identity_fixture()
        self.inputs = {'run_id': self.scene['run_id'], 'anchor_unix_ns': model.ANCHOR}
        self.tasks = {'a/fusion': {'generation': 6}, 'b/reader': {'generation': 7}, 'b/gas-1': {'generation': 5}}
        self.ns, self.sequence, self.sensors, self.fail_step = 0, 0, [], False
        self.nodes = {key: Node(self, key) for key in ('a', 'b')}
    def save(self, name, value): self.saved[name] = value
    def step(self, name): self.events.append(('phase', name))
    def current(self): return observation(self, self.ns, self.sequence)


class Node:
    def __init__(self, owner, key): self.owner, self.key = owner, key
    def service_volume(self, service, kind='config'): return '/test/' + self.key + '/' + service + '/' + kind
    def latest(self, service):
        if service == 'fusion': return {'status': 'waiting', 'run_id': self.owner.inputs['run_id']}
        hazards = [self.owner.current()[0]] if 'gas-1' in self.owner.sensors else []
        return {'version': 1, 'status': 'ready', 'run_id': self.owner.inputs['run_id'], 'reader_generation': 7,
                'hazards': hazards, 'sample_time': stamp(self.owner.ns), 'origin_attested_inference': True,
                'raw_inputs_independently_verified': False}
    def cli(self, *args, **kwargs): return SimpleNamespace(returncode=0)
    def python(self, script, *args, **kwargs):
        if script == control.READER_OBSERVATIONS:
            return SimpleNamespace(returncode=0, stdout=model.json_bytes(self.latest('reader')))
        index = 1 if script == control.MATCH_PUBLICATIONS else 2
        return SimpleNamespace(stdout=model.json_bytes(self.owner.current()[index]))
    def scene_request(self, path, request=None):
        if path == '/v1/step':
            self.owner.events.append(('step', deepcopy(request)))
            if self.owner.fail_step: raise model.Refused('unknown source write')
            self.owner.sequence += 1; self.owner.ns = request['simulation_time_ns']; self.owner.sensors = request['sensors']
            return model.json_bytes({'sequence': self.owner.sequence, **request})
        if path == '/v1/presentation': return model.json_bytes({'simulation_time_ns': self.owner.ns})
        sensor = path.removeprefix('/v1/captures/')
        sample = {'sample_ns': self.owner.ns, 'sensor_id': sensor, 'run_id': self.owner.inputs['run_id']}
        return model.json_bytes({'capture': {'sample_json': encoded(sample)}, 'media': ''})


class ScenarioTests(unittest.TestCase):
    def test_reader_file_helper_emits_only_complete_original_rows(self):
        with tempfile.TemporaryDirectory() as name:
            path = Path(name) / 'stdout.json'
            path.write_bytes(b'{"tick":34}\n{"tick":35}\n{"tick":36')
            # Production runs as root. Exercise its fixed receiver source with
            # the host test user's UID; all file and byte guards stay present.
            self.assertEqual(control.READER_OBSERVATIONS.count('s.st_uid==0'), 1)
            source = control.READER_OBSERVATIONS.replace('s.st_uid==0', 's.st_uid==os.geteuid()')
            result = subprocess.run([sys.executable, '-c', source, str(path)],
                                    capture_output=True, timeout=5)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(result.stdout, b'{"tick":34}\n{"tick":35}\n')

    def test_recorded_positive_survives_later_empty_before_host_poll(self):
        owner = Fixture(Path('/unused')); owner.ns, owner.sequence = 4_200_000_000, 2
        owner.sensors = list(model.SENSORS)
        original = {**owner.nodes['b'].latest('reader'), 'tick': 34}
        empty = {**original, 'tick': 35, 'hazards': []}
        raw = model.json_bytes(original) + model.json_bytes(empty)
        node = owner.nodes['b']
        with patch.object(node, 'python', return_value=SimpleNamespace(returncode=0, stdout=raw)) as read:
            selected = scenario.wait_reader_observation(node, owner, owner.ns, True)
        self.assertEqual(selected, original)
        self.assertEqual(selected['hazards'][0]['version']['sequence'], 2)
        self.assertEqual(read.call_count, 1)
        self.assertIsNone(scenario.recorded_reader_match(raw, owner, owner.ns + 1, True))
        wrong_scope = {**original, 'reader_generation': 99}
        with self.assertRaises(ValueError):
            scenario.recorded_reader_match(model.json_bytes(wrong_scope), owner, owner.ns, True)

    def test_recorded_scan_requires_complete_bounded_rows(self):
        owner = Fixture(Path('/unused')); owner.ns, owner.sequence = 1_000_000_000, 1
        owner.sensors = list(model.SENSORS)
        original = owner.nodes['b'].latest('reader')
        empty = {**original, 'hazards': []}
        raw = model.json_bytes(empty) + model.json_bytes(original).rstrip(b'\n')
        self.assertIsNone(scenario.recorded_reader_match(raw, owner, owner.ns, True))
        self.assertEqual(scenario.recorded_reader_match(raw + b'\n', owner, owner.ns, True), original)
        with self.assertRaises(ValueError):
            scenario.recorded_reader_match(b'{}\n' * 513, owner, owner.ns, True)
        with self.assertRaises(ValueError):
            scenario.recorded_reader_match(b' ' * ((2 << 20) + 1) + b'\n', owner, owner.ns, True)

    def test_three_steps_match_chain_and_expire_remote_gas_only(self):
        with tempfile.TemporaryDirectory() as name:
            owner = Fixture(Path(name)); result = scenario.execute(owner)
            self.assertTrue(result['b_gas_ack_to_a_input_to_a_hazard_to_b_reader_matched'])
            self.assertEqual([s['hazards'] for s in result['steps']], [1, 0, 1])
            steps = [e[1] for e in owner.events if e[0] == 'step']
            self.assertEqual(steps[1], {'simulation_time_ns': 4_100_000_000, 'sensors': list(model.LOCAL_SENSORS)})
            self.assertEqual(steps[2]['simulation_time_ns'], 4_200_000_000)
            evidence = owner.saved['steps/0001-correspondence.json']
            self.assertEqual(evidence['gas_envelope_sha256'], evidence['a_input_envelope_sha256'])
            self.assertEqual(evidence['source_devices']['gas-1'], owner.pair['nodes'][1]['public']['principal'])
            self.assertTrue(owner.saved['steps/0002-control.json']['source_clock_shared'])
            self.assertNotIn('receiver_clock', owner.saved['steps/0002-control.json'])

    def test_unknown_step_is_not_retried(self):
        with tempfile.TemporaryDirectory() as name:
            owner = Fixture(Path(name)); owner.fail_step = True
            with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertEqual(len([e for e in owner.events if e[0] == 'step']), 1)

    def test_changed_gas_acknowledgment_or_generation_refuses(self):
        owner = Fixture(Path('/unused')); owner.ns, owner.sequence = 1_000_000_000, 1
        item, origins, gas = owner.current()
        owner.sensors = list(model.SENSORS)
        reader = owner.nodes['b'].latest('reader')
        for field, value in [('signed_record_json', encoded({'other': 'bytes'})), ('captured_unix_ns', 1), ('sample_ns', 2)]:
            changed = deepcopy(gas); changed[field] = value
            with self.subTest(field=field), self.assertRaises(model.Refused):
                scenario.correspondence(reader, origins, changed, owner, owner.ns)
        owner.tasks['b/gas-1']['generation'] += 1
        with self.assertRaises(model.Refused): scenario.correspondence(reader, origins, gas, owner, owner.ns)


if __name__ == '__main__':
    unittest.main()

"""Harness ordering/byte matching with structural fixtures, not crypto proof."""
import base64
from copy import deepcopy
from pathlib import Path
import tempfile
from types import SimpleNamespace
import unittest

from integration.hazard import control, model, scenario


def stamp(ns):
    return f'2026-01-01T00:00:{ns // 1_000_000_000:02d}.{ns % 1_000_000_000:09d}Z'


def encoded(value):
    return base64.b64encode(model.json_bytes(value)).decode()


def observation(ns, sequence=1):
    payload = {'inputs': [{'sensor_id': sensor, 'sample_time': stamp(ns)} for sensor in model.SENSORS],
               'valid_until': stamp(ns + 3_000_000_000)}
    generic = {'result_schema': 'wendy.hazard.v1', 'result_json': encoded(payload)}
    record = model.json_bytes({'epoch': 1, 'sequence': sequence,
                               'publication': {'payload': encoded(generic)}})
    envelope = model.json_bytes({'record': base64.b64encode(record).decode()})
    digest = model.digest(envelope)
    item = {'envelope_json': base64.b64encode(envelope).decode(), 'envelope_sha256': digest,
            'record_sha256': model.digest(record), 'version': {'epoch': 1, 'sequence': sequence},
            'payload': payload}
    origin = {digest: {'tick': sequence, 'run_id': 'hazard-test', 'publication': {
        'status': 'complete', 'locally_verified': True, 'signed_record_json': item['envelope_json'],
        'record_sha256': item['record_sha256'], 'proof_sha256': digest}}}
    return item, origin


def reader(ns, items=()):
    return {'version': 1, 'tick': 1, 'run_id': 'hazard-test', 'reader_generation': 7,
            'status': 'ready', 'sample_time': stamp(ns), 'hazards': list(items),
            'origin_attested_inference': True, 'raw_inputs_independently_verified': False}


class Fixture:
    def __init__(self, out):
        self.out, self.events, self.saved = out, [], {}
        self.inputs = {'run_id': 'hazard-test', 'anchor_unix_ns': model.ANCHOR}
        self.tasks = {'b/reader': {'generation': 7}}
        self.ns, self.sequence, self.sensors = 0, 0, []
        self.fail_step = self.fail_clock = self.stale_restore = False
        self.nodes = {'a': Node(self, 'a'), 'b': Node(self, 'b')}

    def step(self, value):
        self.events.append(('phase', value))

    def save(self, path, value):
        self.saved[path] = deepcopy(value)
        target = self.out / path
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes(model.json_bytes(value))

    def current(self):
        ns = 1_000_000_000 if self.stale_restore and self.sequence == 3 else self.ns
        return observation(ns, 1 if self.stale_restore else self.sequence)


class Node:
    def __init__(self, owner, key):
        self.owner, self.key = owner, key
        self.clock_raw = control.sample_clock('hazard-test', model.ANCHOR, 0)

    def latest(self, service):
        self.owner.events.append(('read', service, self.owner.ns))
        if service == 'fusion':
            return {'tick': 1, 'run_id': 'hazard-test', 'status': 'waiting'}
        items = [self.owner.current()[0]] if self.owner.sensors else []
        return reader(self.owner.ns, items)

    def service_volume(self, service, kind):
        return '/proofs/' + service

    def python(self, script, *args, **kwargs):
        if script == control.MATCH_PUBLICATIONS:
            self.owner.events.append(('match-origin', self.owner.ns))
            return SimpleNamespace(stdout=model.json_bytes(self.owner.current()[1]))
        # A fake stopped app bounds the intentionally stale restoration test.
        return SimpleNamespace(returncode=1)

    def cli(self, *args, **kwargs):
        return SimpleNamespace(returncode=0)

    def atomic_write(self, path, raw, *, previous):
        self.owner.events.append(('clock', self.owner.ns))
        if self.owner.fail_clock:
            raise model.Refused('clock write unknown')
        if previous != self.clock_raw:
            raise model.Refused('old clock differs')

    def scene_request(self, path, request=None):
        if path == '/v1/step':
            self.owner.events.append(('source-step', request['simulation_time_ns']))
            if self.owner.fail_step:
                raise model.Refused('step outcome unknown')
            self.owner.sequence += 1
            self.owner.ns, self.owner.sensors = request['simulation_time_ns'], request['sensors']
            return model.json_bytes({'sequence': self.owner.sequence, **request})
        if path == '/v1/presentation':
            return model.json_bytes({'simulation_time_ns': self.owner.ns})
        sensor = path.removeprefix('/v1/captures/')
        sample = {'sample_ns': self.owner.ns, 'sensor_id': sensor, 'run_id': 'hazard-test'}
        media = b''
        if sensor in (*model.CAMERAS, 'audio-1'):
            media = ('structural-fixture-' + sensor).encode()
            sample['evidence'] = {'sha256': model.digest(media), 'size': len(media),
                'content_type': 'audio/wav' if sensor == 'audio-1' else 'image/png'}
        return model.json_bytes({'capture': {'sample_json': encoded(sample)},
                                 'media': base64.b64encode(media).decode()})


class ScenarioTests(unittest.TestCase):
    def test_reader_requires_current_inputs_and_exact_trust_labels(self):
        owner = Fixture(Path('/unused'))
        old, _ = observation(1_000_000_000)
        self.assertFalse(scenario.reader_matches(reader(2_000_000_000, [old]), owner, 2_000_000_000, True))
        self.assertTrue(scenario.reader_matches(reader(1_000_000_000, [old]), owner, 1_000_000_000, True))
        value = reader(1_000_000_000, [old])
        value['raw_inputs_independently_verified'] = True
        with self.assertRaises(model.Refused): scenario.reader_matches(value, owner, 1_000_000_000, True)
        self.assertFalse(scenario.reader_matches({'status': 'waiting_for_source'}, owner, 0, False))

    def test_original_bytes_version_and_typed_payload_must_match(self):
        item, origins = observation(1_000_000_000)
        self.assertEqual(scenario.original_matches(reader(1_000_000_000, [item]), origins), [item['envelope_sha256']])
        for field, value in (('record_sha256', '0'*64), ('version', {'epoch': 2, 'sequence': 1}),
                             ('payload', {'inputs': [], 'valid_until': stamp(4_000_000_000)})):
            changed = deepcopy(item); changed[field] = value
            with self.subTest(field=field), self.assertRaises(model.Refused):
                scenario.original_matches(reader(1_000_000_000, [changed]), origins)
        changed = deepcopy(origins)
        changed[item['envelope_sha256']]['publication']['locally_verified'] = False
        with self.assertRaises(model.Refused): scenario.original_matches(reader(1_000_000_000, [item]), changed)

    def test_three_steps_keep_exact_originals_and_expire_before_restore(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory))
            result = scenario.execute(owner)
            rows = result['steps']
            self.assertEqual([r['hazards'] for r in rows], [1, 0, 1])
            self.assertEqual([r['simulation_time_ns'] for r in rows], [1_000_000_000, 4_100_000_000, 4_200_000_000])
            self.assertTrue(set(rows[0]['original_envelope_sha256']).isdisjoint(rows[2]['original_envelope_sha256']))
            self.assertEqual([len(r['captures']) for r in rows], [5, 0, 5])
            self.assertEqual([len(r['media']) for r in rows], [3, 0, 3])
            self.assertFalse(result['raw_inputs_independently_verified_at_receiver'])
            for ns in (1_000_000_000, 4_100_000_000, 4_200_000_000):
                self.assertLess(owner.events.index(('source-step', ns)), owner.events.index(('clock', ns)))
                self.assertLess(owner.events.index(('clock', ns)), owner.events.index(('read', 'reader', ns)))

    def test_uncertain_source_mutation_is_not_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory)); owner.fail_step = True
            with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertEqual(len([e for e in owner.events if e[0] == 'source-step']), 1)
            self.assertFalse(any(e[0] == 'clock' for e in owner.events))
            self.assertNotIn('playback-steps.json', owner.saved)

    def test_partial_clock_update_stops_without_rollback_or_next_command(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory)); owner.fail_clock = True
            with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertEqual([e[0] for e in owner.events if e[0] in ('clock', 'source-step')], ['source-step', 'clock'])
            self.assertNotIn('steps/0001-reader.json', owner.saved)

    def test_restoration_cannot_accept_still_live_older_measurement(self):
        with tempfile.TemporaryDirectory() as directory:
            owner = Fixture(Path(directory)); owner.stale_restore = True
            with self.assertRaises(model.Refused): scenario.execute(owner)
            self.assertIn('steps/0002-reader.json', owner.saved)
            self.assertNotIn('playback-steps.json', owner.saved)


if __name__ == '__main__':
    unittest.main()

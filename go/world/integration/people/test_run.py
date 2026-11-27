import base64
from pathlib import Path
import subprocess
import os
import tempfile
import sys
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import control
import model
import run
import scenario


class PeopleRunTests(unittest.TestCase):
    def test_receiver_clock_preserves_anchor_and_increases(self):
        first = model.strict_json(control.sample_clock('test', model.ANCHOR, 100_000_000))
        last = model.strict_json(control.sample_clock('test', model.ANCHOR, 2_000_000_000))
        self.assertEqual(first['domain'], 'simulation:test')
        self.assertLess(scenario.instant(first['now']), scenario.instant(last['now']))
        self.assertEqual(first['now'], '2026-01-01T00:00:00.100000000Z')

    def test_reader_waiting_never_becomes_empty_success(self):
        runner = SimpleNamespace(inputs={'run_id': 'test', 'anchor_unix_ns': model.ANCHOR}, tasks={'b/reader': {'generation': 9}})
        self.assertFalse(scenario.reader_matches({'status': 'waiting_for_source', 'tracks': [], 'reader_generation': 0}, runner, 0, False))
        ready = {'version': 1, 'status': 'ready', 'run_id': 'test', 'reader_generation': 9, 'tracks': [], 'sample_time': '2026-01-01T00:00:00Z', 'origin_attested_inference': True, 'camera_inputs_independently_verified': False}
        self.assertTrue(scenario.reader_matches(ready, runner, 0, False))
        self.assertFalse(scenario.reader_matches(ready, runner, 100_000_000, False))
        ready['reader_generation'] = 8
        with self.assertRaises(ValueError): scenario.reader_matches(ready, runner, 0, False)

    def test_exact_origin_bytes_not_payload_equivalence(self):
        record = b'{"version":1}'
        envelope = model.json_bytes({'record': base64.b64encode(record).decode()})
        digest = model.digest(envelope)
        reader = {'tracks': [{'envelope_json': base64.b64encode(envelope).decode(), 'envelope_sha256': digest, 'record_sha256': model.digest(record)}]}
        origins = {digest: {'publication': {'locally_verified': True, 'signed_record_json': base64.b64encode(envelope).decode()}}}
        self.assertEqual(scenario.original_matches(reader, origins), [digest])
        origins[digest]['publication']['signed_record_json'] = base64.b64encode(envelope + b' ').decode()
        with self.assertRaises(ValueError): scenario.original_matches(reader, origins)

    def test_service_deploy_is_selective_and_no_restart(self):
        runner = object.__new__(run.Runner); runner.tasks = {}; runner.save = lambda *a: None
        calls = []
        node = SimpleNamespace(key='a', cli=lambda *a: calls.append(a), task=lambda service: {'service': service})
        runner.deploy_service(node, 'camera-b')
        self.assertIn('--no-restart', calls[0])
        self.assertEqual(calls[0][-2:], ('--service', 'camera-b'))

    def test_node_state_volume_does_not_shadow_service_paths(self):
        owner = SimpleNamespace(name='world-people-test', inputs={'run_id':'test'})
        node = run.Node(owner, 'a')
        self.assertEqual(node.volume, 'world-people-test-a-state')
        self.assertTrue(node.service_volume('scene').endswith('-scene-config'))

    def test_empty_gate_uses_closed_stdin_and_same_exclusive_receiver(self):
        calls = []
        def call(argv, *, input_data):
            calls.append((argv, input_data))
            command = [sys.executable, *argv[argv.index('python3') + 1:]]
            if input_data is None:
                return subprocess.run(command, stdin=subprocess.DEVNULL, capture_output=True, timeout=3)
            return subprocess.run(command, input=input_data, capture_output=True, timeout=3)
        owner = SimpleNamespace(name='world-people-test', call=call)
        node = run.Node(owner, 'a')
        with tempfile.TemporaryDirectory(dir=run.stage.ROOT / '.local') as directory:
            gate, config = Path(directory) / 'go', Path(directory) / 'config'
            empty = node.write(str(gate), b'')
            self.assertEqual(empty.returncode, 0, empty.stderr)
            self.assertEqual(empty.stdout, b'file received\n')
            self.assertNotIn('-i', calls[-1][0])
            self.assertIsNone(calls[-1][1])
            self.assertEqual(gate.read_bytes(), b'')
            self.assertEqual(gate.stat().st_mode & 0o777, 0o600)
            value = node.write(str(config), b'bounded config')
            self.assertEqual(value.returncode, 0, value.stderr)
            self.assertIn('-i', calls[-1][0])
            self.assertEqual(calls[-1][1], b'bounded config')
            self.assertEqual(config.read_bytes(), b'bounded config')
            self.assertNotEqual(node.write(str(gate), b'').returncode, 0)
            self.assertEqual(gate.read_bytes(), b'')

    def test_old_live_track_does_not_prove_a_new_observation(self):
        runner = SimpleNamespace(inputs={'run_id':'test','anchor_unix_ns':model.ANCHOR}, tasks={'b/reader':{'generation':9}})
        row = {'version':1,'status':'ready','run_id':'test','reader_generation':9,'sample_time':'2026-01-01T00:00:00.5Z',
               'origin_attested_inference':True,'camera_inputs_independently_verified':False,
               'tracks':[{'payload':{'observed_at':'2026-01-01T00:00:00.1Z'}}]}
        self.assertFalse(scenario.reader_matches(row,runner,500_000_000,True))
        row['tracks'][0]['payload']['observed_at']='2026-01-01T00:00:00.5Z'
        self.assertTrue(scenario.reader_matches(row,runner,500_000_000,True))

    def test_failure_always_runs_cleanup(self):
        runner = object.__new__(run.Runner)
        runner.runtime_state = 'setup'; saved = {}; cleaned = []
        runner.save = lambda name, value: saved.update({name: value})
        def failed(): raise model.Refused('test fixture')
        runner.setup = failed
        runner.cleanup = lambda: cleaned.append(True) or {'complete': True, 'resources': []}
        with self.assertRaises(ValueError): runner.execute()
        self.assertEqual(cleaned, [True])
        self.assertFalse(saved['result.json']['passed'])

    def test_steps_use_original_expiry_and_explicit_empty(self):
        self.assertEqual([s[0] for s in scenario.STEPS], sorted(set(s[0] for s in scenario.STEPS)))
        self.assertGreater(scenario.STEPS[2][0] - scenario.STEPS[1][0], 1_250_000_000)
        self.assertEqual(scenario.STEPS[2][1], ())
        self.assertEqual(scenario.STEPS[3][1], ('camera-a', 'camera-b'))

    def test_atomic_writer_is_unchanged_reviewed_primitive(self):
        self.assertEqual(control.ATOMIC_WRITE, run.libraries.control.ATOMIC_WRITE)
        self.assertIn('len(names)==2', control.VOLUME_DIRECTORIES)
        self.assertIn('world-people-', control.VOLUME_DIRECTORIES)
        compile(control.VOLUME_DIRECTORIES, 'volumes', 'exec')

    def test_actual_two_volume_creation_under_umask_zero(self):
        with tempfile.TemporaryDirectory(dir=run.stage.ROOT / '.local') as directory:
            root = Path(directory) / 'wendy' / 'volumes'
            names = ['world-people-test-a-scene-config', 'world-people-test-a-scene-proofs']
            script = control.VOLUME_DIRECTORIES.replace("if __name__=='__main__':", 'if False:') + "\nos.umask(0);print(json.dumps(prepare(sys.argv[1],sys.argv[2:])))\n"
            argv = [sys.executable, '-c', script, str(root), *names]
            value = model.strict_json(subprocess.run(argv, capture_output=True, check=True).stdout)
            self.assertEqual(len(value['directories']), 4)
            for name in names:
                self.assertEqual((root / name).stat().st_mode & 0o777, 0o700)
            self.assertNotEqual(subprocess.run(argv, capture_output=True).returncode, 0)


if __name__ == '__main__': unittest.main()

"""Portable control-admission tests, not a replacement for the actual ROS gate."""
import importlib.util
from pathlib import Path
import unittest

HERE = Path(__file__).resolve().parent
SPEC = importlib.util.spec_from_file_location('patrol_gate', HERE/'test-patrol-ros-runtime.py')
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)
ROOT = HERE.parents[1]


class Admission(unittest.TestCase):
    def test_expert_storage_contract_matches_all_seven_locked_files(self):
        import json
        manifest = json.loads((HERE/'fixtures/g1-coke/expert-ci-input.json').read_text())
        lock = json.loads((ROOT/'Examples/G1CokeDemo/assets.lock.json').read_text())
        files = {value['path']: {'bytes': value['bytes'], 'sha256': value['sha256']}
                 for name, value in lock['files'].items() if name.startswith('expert/')}
        self.assertEqual(len(files), 7)
        self.assertEqual(manifest['files'], files)
        self.assertEqual(manifest['total_bytes'], sum(value['bytes'] for value in files.values()))
        self.assertIsNone(manifest['fetch_url'])

    def test_controls_apply_to_exact_production_and_remain_valid_python(self):
        source = (ROOT/'go/simulator/go2/examples/patrol/ros_app.py').read_text()
        rows = GATE.plans(source)
        self.assertEqual(len(rows), 4)
        for name, _, code, _ in rows:
            compile(code, name, 'exec')
        with self.assertRaisesRegex(RuntimeError, 'source control drift'):
            GATE.plans(source.replace('allow_scan_gaps=args.allow_scan_gaps', 'allow_scan_gaps=True'))

    def test_normal_needs_all_actual_cases_and_success_marker(self):
        record = {'status':'passed', 'cases':[{'case':case} for case in GATE.CASES]}
        self.assertTrue(GATE.accept('normal', 0, record, 'PATROL_ROS_TRANSPORT_VERIFIED'))
        self.assertFalse(GATE.accept('normal', 0, record, ''))
        record['cases'].pop()
        self.assertFalse(GATE.accept('normal', 0, record, 'PATROL_ROS_TRANSPORT_VERIFIED'))

    def test_controls_reject_setup_teardown_and_timeout_failures(self):
        for name, _, _, _, marker in GATE.CONTROLS:
            failure = {'assertion':marker, 'subscriptions':[1,1], 'commands':[[1,{'x':.55}]]}
            record = {'status':'failed', 'assertion':marker}
            self.assertTrue(GATE.accept(name, 1, record, '', failure))
            for code in (0, 2, 124, 137, 143):
                self.assertFalse(GATE.accept(name, code, record, '', failure))
            for reason in ('ImportError', 'PATROL_DDS_DISCOVERY', 'PATROL_CHILD_DRAIN'):
                altered = dict(record, assertion=reason)
                self.assertFalse(GATE.accept(name, 1, altered, '', failure))
            altered = dict(failure, subscriptions=[0,1])
            self.assertFalse(GATE.accept(name, 1, record, '', altered))

    def test_permissive_stamp_control_requires_actual_nonzero_wire_command(self):
        marker = 'PATROL_ARRIVAL_REJECTS_ZERO'
        record = {'status':'failed', 'assertion':marker}
        failure = {'assertion':marker, 'subscriptions':[1,1], 'commands':[[1,{'x':0.0}]]}
        self.assertFalse(GATE.accept('skip-stamp-admission', 1, record, '', failure))
        failure['commands'].append([2,{'x':.55}])
        self.assertTrue(GATE.accept('skip-stamp-admission', 1, record, '', failure))


if __name__ == '__main__':
    unittest.main()

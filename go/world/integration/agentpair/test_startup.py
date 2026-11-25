import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import Mock

import model
import run
import startup
from test_run import runner_fixture


# Same fixed message/error fields observed in actual run A. Other original
# fields and private log contents are deliberately absent from this fixture.
RECORDED_FAILURE = b'{"level":"warn","msg":"Private World publication unavailable","error":"fresh authenticated quorum required"}\n'


class StartupTests(unittest.TestCase):
    def test_recorded_quorum_failure_has_only_fixed_classification(self):
        report = startup.classify(RECORDED_FAILURE)
        self.assertEqual(report, {'known_startup_failure': True, 'reasons': ['publication_fresh_quorum_missing'],
                                  'world_ready': False, 'scope_behavior_check_pending': True})

    def test_other_publication_and_replication_errors_never_export_raw_text(self):
        for message, reason in (('Private World publication unavailable', 'publication_unavailable'),
                                ('Private World replication unavailable', 'replication_unavailable')):
            raw = json.dumps({'level': 'warn', 'msg': message, 'error': 'do-not-export-this-value'}).encode()
            report = startup.classify(raw)
            self.assertEqual(report['reasons'], [reason])
            self.assertNotIn('do-not-export', json.dumps(report))

    def test_unrelated_and_empty_logs_are_unknown_readiness(self):
        for raw in (b'', b'private unrelated non-json text\n', b'{"msg":"apps list succeeded"}\n'):
            report = startup.classify(raw)
            self.assertFalse(report['known_startup_failure'])
            self.assertFalse(report['world_ready'])
            self.assertTrue(report['scope_behavior_check_pending'])

    def test_malformed_recognized_messages_and_bounds_refuse(self):
        for raw in (RECORDED_FAILURE[:-3],
                    b'{"msg":"ignored","msg":"Private World publication unavailable"}',
                    b'x' * ((1 << 20) + 1)):
            with self.assertRaises(ValueError):
                startup.classify(raw)
        compile(startup.PROBE_SCRIPT, '<sanitized-startup-probe>', 'exec')

    def test_deploy_failure_saves_reason_before_any_deployment_side_effect(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory)
            runner.runtime_state = 'commission'
            runner.current_container_id = Mock(return_value='a' * 64)
            report = {**startup.classify(RECORDED_FAILURE), 'inspection_complete': True, 'config_hash_matches': True}
            runner.call = Mock(return_value=subprocess.CompletedProcess([], 0, model.json_bytes(report), b''))
            for node in runner.nodes.values():
                node.agent_phase = 'commissioned'; node.agent_pid = 42; node.world_config_sha256 = 'b' * 64
                node.cli = Mock(); node.inside = Mock(); node.write = Mock(); node.python = Mock()
            with self.assertRaises(model.Refused):
                runner.deploy()
            self.assertEqual(runner.call.call_count, 1)
            argv = runner.call.call_args.args[0]
            self.assertEqual(argv[:3], ['docker', 'exec', 'a' * 64])
            self.assertIn('b' * 64, argv)
            for node in runner.nodes.values():
                node.cli.assert_not_called(); node.inside.assert_not_called()
                node.write.assert_not_called(); node.python.assert_not_called()
            saved = json.loads((Path(directory) / 'world-startup-a-1.json').read_text())
            self.assertTrue(saved['config_hash_matches'])
            self.assertEqual(saved['reasons'], ['publication_fresh_quorum_missing'])
            self.assertFalse(saved['world_ready'])

    def test_incomplete_or_unpinned_inspection_does_not_authorize_deploy(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            node.agent_phase = 'commissioned'; node.agent_pid = 42
            runner.call = Mock()
            with self.assertRaises(model.Refused): node.check_world_startup()
            runner.call.assert_not_called()
            node.world_config_sha256 = 'b' * 64
            runner.current_container_id = Mock(return_value='a' * 64)
            for change in ({'inspection_complete': False}, {'config_hash_matches': False}, {'reasons': ['startup_inspection_refused']}):
                report = {**startup.classify(b''), 'inspection_complete': True, 'config_hash_matches': True, **change}
                runner.call.return_value = subprocess.CompletedProcess([], 0, model.json_bytes(report), b'')
                with self.assertRaises(model.Refused): node.check_world_startup()

    def test_enrollment_start_remains_no_world_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            runner.call = Mock(); node.inside = Mock(); node.wait = Mock()
            node.check_world_startup = Mock()
            node.start_agent('enrollment')
            node.check_world_startup.assert_not_called()
            self.assertEqual(node.agent_phase, 'enrollment')
            shell = runner.call.call_args.args[0]
            self.assertTrue(shell[shell.index('-c') + 1].startswith('umask 077;'))


if __name__ == '__main__':
    unittest.main()

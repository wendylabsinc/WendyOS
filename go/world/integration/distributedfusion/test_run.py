from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from integration.distributedfusion import run, model, stage, scenario
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class RunnerTests(unittest.TestCase):
    def test_reused_lifecycle_has_private_bindings(self):
        self.assertIs(run.base.base.model, model)
        self.assertIs(run.base.base.stage, stage)
        for name in ('setup', 'enroll', 'cleanup', 'current_container_id', 'snapshot_outputs'):
            self.assertIs(getattr(run.Runner, name), getattr(run.base.base.Runner, name))
        node = run.base.base.Node(SimpleNamespace(name='owned', inputs={'run_id': 'split-test'}), 'b')
        self.assertIn('world-distributedfusion-split-test-b-gas-1', node.service_volume('gas-1'))

    def test_gates_after_all_tasks_and_b_gas_uses_read_relay(self):
        runner = object.__new__(run.Runner)
        runner.inputs = {'run_id': 'hazard-test'}; runner.pair, _ = pair_fixture()
        runner.scene, runner.scene_url = identity_fixture(), 'http://10.20.0.5:8091'
        runner.remote_scene_url = 'http://172.20.0.2:8092'
        runner.tasks, events = {'a/scene': {}}, []
        runner.nodes = {key: SimpleNamespace(key=key, check_world_startup=lambda: None,
            service_volume=lambda service, key=key: '/volumes/' + key + '/' + service,
            write=lambda path, raw, key=key: events.append(('write', key, path, raw))) for key in ('a', 'b')}
        def deploy(node, service):
            row = {'generation': len(runner.tasks) + 1, 'socket_directory': '/socket/' + node.key + '/' + service}
            runner.tasks[node.key + '/' + service] = row; events.append(('deploy', node.key, service)); return row
        runner.deploy_service, runner.step = deploy, Mock()
        runner.deploy()
        deploys = [(x[1], x[2]) for x in events if x[0] == 'deploy']
        self.assertIn(('b', 'gas-1'), deploys); self.assertNotIn(('a', 'gas-1'), deploys)
        self.assertEqual(len(deploys), 7)
        configs = {x[2]: model.strict_json(x[3]) for x in events if x[0] == 'write' and x[2].endswith('config.json')}
        self.assertEqual(configs['/volumes/b/gas-1/config.json']['scene_url'], runner.remote_scene_url)
        self.assertEqual(configs['/volumes/a/camera-a/config.json']['scene_url'], runner.scene_url)
        gates = [i for i, x in enumerate(events) if x[0] == 'write' and x[2].endswith('/go')]
        self.assertEqual(len(gates), 7)
        self.assertTrue(all(x[0] == 'write' and x[2].endswith('/go') for x in events[min(gates):]))

    def test_failed_phase_still_cleans_exact_owned_resources(self):
        runner = object.__new__(run.Runner)
        for name in ('setup', 'enroll', 'projects', 'source_scene', 'commission', 'deploy'):
            setattr(runner, name, Mock())
        runner.runtime_state, runner.save = 'sample-1', Mock()
        runner.cleanup = Mock(return_value={'complete': True})
        with patch.object(scenario, 'execute', side_effect=model.Refused('unknown outcome')):
            with self.assertRaises(model.Refused): runner.execute()
        runner.cleanup.assert_called_once()
        self.assertFalse(runner.save.call_args.args[1]['passed'])


if __name__ == '__main__':
    unittest.main()

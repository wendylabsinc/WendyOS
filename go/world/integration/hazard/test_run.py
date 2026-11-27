from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from integration.hazard import run, model, stage, scenario
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class RunnerTests(unittest.TestCase):
    def test_existing_lifecycle_uses_isolated_hazard_bindings(self):
        self.assertIs(run.base.model, model)
        self.assertIs(run.base.stage, stage)
        for method in ('setup', 'enroll', 'commission', 'cleanup', 'current_container_id', 'snapshot_outputs'):
            self.assertIs(getattr(run.Runner, method), getattr(run.base.Runner, method))
        owner = SimpleNamespace(name='owned-fixture', inputs={'run_id': 'hazard-test'})
        node = run.base.Node(owner, 'a')
        self.assertEqual(node.service_volume('lidar-atlas'), '/var/lib/wendy/volumes/world-hazard-hazard-test-a-lidar-atlas-config')
        self.assertEqual(run.libraries.people_stage.SERVICES['a'], ('scene', 'camera-a', 'camera-b', 'people'))

    def test_deploy_gates_only_after_all_seven_distinct_scoped_tasks(self):
        runner = object.__new__(run.Runner)
        runner.inputs = {'run_id': 'hazard-test'}
        runner.pair, _ = pair_fixture()
        runner.scene, runner.scene_url = identity_fixture(), 'http://10.47.0.5:8091'
        runner.tasks, events = {'a/scene': {}}, []
        nodes = {}
        for key in ('a', 'b'):
            nodes[key] = SimpleNamespace(key=key, check_world_startup=lambda: None,
                service_volume=lambda service, key=key: '/volumes/' + key + '/' + service,
                write=lambda path, data, key=key: events.append(('write', key, path, data)))
        runner.nodes, runner.step = nodes, Mock()
        def deploy(node, service):
            task = {'generation': len(runner.tasks) + 1, 'socket_directory': '/socket/' + node.key + '/' + service}
            runner.tasks[node.key + '/' + service] = task
            events.append(('deploy', node.key, service)); return task
        runner.deploy_service = deploy
        runner.deploy()
        self.assertEqual(len([x for x in events if x[0] == 'deploy']), 7)
        gates = [i for i, x in enumerate(events) if x[0] == 'write' and x[2].endswith('/go')]
        self.assertEqual(len(gates), 7)
        self.assertTrue(all(x[0] == 'write' and x[2].endswith('/go') for x in events[min(gates):]))
        configs = {x[2]: model.strict_json(x[3]) for x in events if x[0] == 'write' and x[2].endswith('/config.json')}
        self.assertEqual(configs['/volumes/a/lidar-atlas/config.json']['sensor_id'], 'lidar-atlas')
        self.assertEqual(configs['/volumes/a/fusion/config.json']['request_timeout_ms'], 3000)
        self.assertEqual(configs['/volumes/b/reader/config.json']['source']['service'], 'fusion')

    def test_scenario_failure_still_runs_owned_cleanup(self):
        runner = object.__new__(run.Runner)
        for method in ('setup', 'enroll', 'projects', 'source_scene', 'commission', 'deploy'):
            setattr(runner, method, Mock())
        runner.runtime_state = 'sample-1'
        runner.cleanup = Mock(return_value={'complete': True, 'resources': []})
        runner.save = Mock()
        with patch.object(scenario, 'execute', side_effect=model.Refused('unknown mutation outcome')):
            with self.assertRaises(model.Refused): runner.execute()
        runner.cleanup.assert_called_once()
        result = runner.save.call_args.args[1]
        self.assertFalse(result['passed'])
        self.assertFalse(result['gate_completed'])
        self.assertEqual(result['failed_phase'], 'sample-1')


if __name__ == '__main__':
    unittest.main()

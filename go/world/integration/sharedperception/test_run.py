from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

from integration.sharedperception import model, run
from integration.sharedperception.test_model import scene_fixture
from integration.people.test_model import pair_fixture


class RunnerTests(unittest.TestCase):
    def test_reuses_lifecycle_and_binds_exact_combined_modules(self):
        self.assertIs(run.base.base.base.model, model)
        for name in ('setup', 'enroll', 'source_scene', 'commission', 'deploy', 'cleanup', 'snapshot_outputs'):
            self.assertIs(getattr(run.Runner, name), getattr(run.base.Runner, name))
        node = run.base.base.base.Node(SimpleNamespace(name='owned', inputs={'run_id': 'shared-test'}), 'b')
        self.assertIn('world-sharedperception-shared-test-b-people-reader', node.service_volume('people-reader'))
        self.assertIn('/v1/captures/people-front', run.control.HTTP)
        self.assertIn('world-sharedperception-', run.control.VOLUME_DIRECTORIES)

    def test_all_eleven_configs_then_gates_and_two_independent_reader_generations(self):
        runner = object.__new__(run.Runner)
        runner.scene = scene_fixture(); runner.pair, _ = pair_fixture()
        runner.inputs = {'run_id': runner.scene['run_id'], 'model': {'model_id': 'pose', 'model_version': 'v1', 'model_sha256': 'a' * 64}}
        runner.scene_url, runner.remote_scene_url = 'http://10.1.0.1:8091', 'http://172.20.0.1:8092'
        runner.tasks, events = {'a/scene': {}}, []
        runner.nodes = {key: SimpleNamespace(key=key, check_world_startup=Mock(),
            service_volume=lambda service, key=key: '/' + key + '/' + service,
            write=lambda path, raw: events.append(('write', path, raw))) for key in ('a', 'b')}
        def deploy(node, service):
            task = {'generation': len(runner.tasks) + 10, 'socket_directory': '/socket/' + node.key + '/' + service}
            runner.tasks[node.key + '/' + service] = task; events.append(('deploy', node.key, service)); return task
        runner.deploy_service, runner.step = deploy, Mock()
        runner.deploy()
        configs = {event[1]: model.strict_json(event[2]) for event in events if event[0] == 'write' and event[1].endswith('/config.json')}
        self.assertEqual(len(configs), 11)
        self.assertEqual(configs['/a/people-front/config.json']['scene_url'], runner.scene_url)
        self.assertEqual(configs['/b/camera-b/config.json']['scene_url'], runner.remote_scene_url)
        for service in ('reader', 'people-reader'):
            self.assertEqual(configs['/b/' + service + '/config.json']['expected_generation'], runner.tasks['b/' + service]['generation'])
        self.assertNotEqual(configs['/b/reader/config.json']['expected_generation'], configs['/b/people-reader/config.json']['expected_generation'])
        self.assertEqual([s['sensor_id'] for s in configs['/a/people/config.json']['sources']], list(model.PEOPLE_CAMERAS))
        gates = [index for index, event in enumerate(events) if event[0] == 'write' and event[1].endswith('/go')]
        self.assertEqual(len(gates), 11)
        self.assertEqual([event[0] for event in events[min(gates):]], ['write'] * 11)

    def test_observation_failure_uses_original_unconditional_cleanup(self):
        runner = object.__new__(run.Runner)
        for name in ('setup', 'enroll', 'projects', 'source_scene', 'commission', 'deploy'):
            setattr(runner, name, Mock())
        runner.runtime_state, runner.save = 'sample-1', Mock()
        runner.cleanup = Mock(return_value={'complete': True})
        runner.observe = Mock(side_effect=model.Refused('missing second reader'))
        with self.assertRaises(model.Refused): runner.execute()
        runner.cleanup.assert_called_once()
        self.assertFalse(runner.save.call_args.args[1]['passed'])


if __name__ == '__main__': unittest.main()

from types import SimpleNamespace
from pathlib import Path
from unittest.mock import Mock, patch
import unittest

from integration.nativesensorfusion import model, run, scenario, stage
from integration.hazard.test_model import identity_fixture
from integration.people.test_model import pair_fixture


class RunnerTests(unittest.TestCase):
    def test_pair_and_status_use_the_existing_commissioned_agent_socket(self):
        from integration.agentvm.verify import ENV
        self.assertEqual(ENV['WENDY_AGENT_SOCKET'], '/var/lib/wendy/agent-control/agent.sock')
        runner = object.__new__(run.Runner)
        node = SimpleNamespace(inside=Mock(return_value='native reply'))
        runner.nodes = {'a': node}
        for args in [('pair', '--principal', 'source', '--address', '127.0.0.1:50060'), ('status',)]:
            self.assertEqual(runner.native_pair(*args), 'native reply')
            node.inside.assert_called_with('env', 'WENDY_AGENT_SOCKET=' + ENV['WENDY_AGENT_SOCKET'],
                                          model.TOOLS + '/native-pair', *args, timeout=8)

    def test_cleanup_retains_only_known_native_frames_and_distinguishes_missing(self):
        runner = object.__new__(run.Runner)
        runner.out = Path('/retained-run')
        runner.current_container_id = Mock(return_value='owned-id')
        runner.save = Mock()
        def inspect(script, path, check):
            missing = 'frame-000002' in path
            return SimpleNamespace(returncode=0, stdout=b'not_created\n' if missing else b'present\n')
        def capture(source, destination, limit, container):
            self.assertEqual(container, 'owned-id')
            return {'status': 'invalid' if source.endswith('000001.json') else 'captured'}
        node = SimpleNamespace(key='a', name='owned-a', python=Mock(side_effect=inspect),
                               snapshot_file=Mock(side_effect=capture))
        with patch.object(run.base.Runner, 'snapshot_outputs'):
            runner.snapshot_outputs(node, 'owned-id')
        saved = runner.save.call_args.args[1]
        self.assertEqual(set(saved), {'ready.json', 'stdout.json', 'stderr.log', 'exit.json',
            'frame-000001.jpg', 'frame-000001.json', 'frame-000002.jpg', 'frame-000002.json'})
        self.assertEqual(saved['frame-000002.jpg'], {'status': 'not_created'})
        self.assertEqual(saved['frame-000002.json'], {'status': 'not_created'})
        self.assertEqual(saved['frame-000001.json'], {'status': 'invalid'})
        copied = {Path(call.args[0]).name: call.args[2] for call in node.snapshot_file.call_args_list}
        self.assertEqual(copied['frame-000001.jpg'], 8 << 20)
        self.assertEqual(copied['frame-000001.json'], 64 << 10)
        self.assertNotIn('frame-000002.jpg', copied)

    def test_native_pair_response_requires_exact_principal_and_camera(self):
        principal = pair_fixture()[0]['nodes'][0]['public']['principal']
        value = {'pairing': {'source_principal_uri': principal, 'source_asset_id': 0,
            'transport': 'tcp', 'sensor_allowlist': ['camera-b'], 'connected': False}}
        run.validate_pairing(value, principal)
        for field, bad in [('source_asset_id', 8), ('transport', 'grpc'), ('sensor_allowlist', ['camera-a']),
                           ('source_principal_uri', 'other'), ('connected', True)]:
            altered = {'pairing': {**value['pairing'], field: bad}}
            with self.subTest(field=field), self.assertRaises(model.Refused):
                run.validate_pairing(altered, principal)

    def test_deploy_keeps_native_bridge_gated_until_first_scene_step(self):
        runner = object.__new__(run.Runner)
        runner.inputs = {'run_id': 'hazard-test'}
        runner.pair, _ = pair_fixture()
        runner.scene, runner.scene_url = identity_fixture(), 'http://10.47.0.5:8091'
        runner.source_id = model.native_source_id(runner.pair['nodes'][0]['public']['principal'])
        runner.metadata_url = 'http://172.21.0.2:50061'
        runner.tasks, events = {'a/scene': {}}, []
        runner.nodes = {key: SimpleNamespace(key=key, check_world_startup=lambda: None,
            service_volume=lambda s, key=key: '/volumes/' + key + '/' + s,
            write=lambda path, data, key=key: events.append((key, path, data))) for key in ('a', 'b')}
        runner.step = Mock()
        def deploy(node, service):
            task = {'generation': len(runner.tasks)+1, 'socket_directory': '/sockets/'+node.key+'/'+service}
            runner.tasks[node.key+'/'+service] = task
            return task
        runner.deploy_service = deploy
        runner.deploy()
        gates = [path for _, path, _ in events if path.endswith('/go')]
        self.assertEqual(len(gates), 6)
        self.assertNotIn('/volumes/a/camera-b/go', gates)
        configs = {path: model.strict_json(raw) for _, path, raw in events if path.endswith('/config.json')}
        camera = configs['/volumes/a/camera-b/config.json']
        self.assertEqual(camera['source_id'], runner.source_id)
        self.assertNotIn('scene_url', camera)
        self.assertEqual(configs['/volumes/a/gas-1/config.json']['sensor_id'], 'gas-1')

    def test_failed_native_pairing_always_reaches_owned_cleanup(self):
        runner = object.__new__(run.Runner)
        for method in ('setup', 'enroll', 'projects', 'source_scene', 'commission', 'deploy'):
            setattr(runner, method, Mock())
        runner.native_source = Mock(side_effect=model.Refused('pairing refused'))
        runner.runtime_state = 'native-source'
        runner.cleanup = Mock(return_value={'complete': True, 'resources': []})
        runner.save = Mock()
        with self.assertRaises(model.Refused):
            runner.execute()
        runner.cleanup.assert_called_once()
        runner.deploy.assert_not_called()
        self.assertFalse(runner.save.call_args.args[1]['gate_completed'])

    def test_existing_lifecycle_remains_bound_to_new_fixture_only(self):
        self.assertIs(run.base.model, model)
        self.assertIs(run.base.stage, stage)
        for method in ('setup', 'enroll', 'commission', 'cleanup', 'current_container_id'):
            self.assertIs(getattr(run.Runner, method), getattr(run.base.Runner, method))
        from integration.hazard import model as previous
        self.assertEqual(previous.APP, 'com.wendy.world.hazard')

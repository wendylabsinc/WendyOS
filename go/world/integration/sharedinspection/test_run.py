from pathlib import Path
from copy import deepcopy
import importlib.util
import os
import stat
from types import SimpleNamespace
import tempfile
import unittest

from integration.sharedinspection import model, run, stage
from integration.sharedperception.test_model import scene_fixture
from integration.people.test_model import pair_fixture
from integration.sharedinspection.test_model import viewer_fixture


class Node:
    def __init__(self, key): self.key, self.writes, self.commands = key, [], []
    def check_world_startup(self): pass
    def service_volume(self, service): return '/owned/' + service
    def write(self, path, data): self.writes.append((path, data))


class RunnerTests(unittest.TestCase):
    def test_actual_volume_creator_and_receipt_accept_only_gateway_state(self):
        run_id = scene_fixture()['run_id']
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            script = Path(name) / 'volume_creator.py'
            script.write_text(run.control.VOLUME_DIRECTORIES)
            spec = importlib.util.spec_from_file_location('sharedinspection_volume_test', script)
            creator = importlib.util.module_from_spec(spec)
            spec.loader.exec_module(creator)
            root = Path(name) / 'wendy' / 'volumes'
            for service in ('camera-a', 'mission-gateway'):
                names = list(model.volume_names(run_id, 'a', service).values())
                actual = creator.prepare(str(root), names)
                self.assertEqual(len(actual['directories']), 2 + len(names))
                for volume in names:
                    info = (root / volume).stat()
                    self.assertEqual(info.st_uid, os.geteuid())
                    self.assertEqual(stat.S_IMODE(info.st_mode), 0o700)
                    self.assertEqual(list((root / volume).iterdir()), [])
                # The same actual filesystem receipt goes through the runtime
                # parser with only this non-root host test's owner mapped to 0.
                receipt = deepcopy(actual)
                for row in receipt['directories']: row['uid'] = 0
                self.assertEqual(run.control.volume_receipt(model.json_bytes(receipt), names), receipt)
                with self.assertRaises(FileExistsError): creator.prepare(str(root), names)
                broken = deepcopy(receipt); broken['directories'][-1]['mode'] = 0o777
                with self.assertRaises(ValueError): run.control.volume_receipt(model.json_bytes(broken), names)
            bad = ['world-sharedinspection-invalid-a-fusion-' + kind for kind in ('config', 'proofs', 'state')]
            with self.assertRaises(AssertionError): creator.prepare(str(root), bad)
            with self.assertRaises(ValueError): run.control.volume_receipt(b'{}', bad)
            self.assertFalse(any((root / item).exists() for item in bad))
        from integration.sharedperception import run as original
        self.assertIn('assert len(names)==2 and len(set(names))==2', original.control.VOLUME_DIRECTORIES)

    def test_twelve_world_configs_install_viewer_public_trust_before_any_world_gate(self):
        runner = object.__new__(run.Runner); runner.nodes = {key: Node(key) for key in ('a', 'b')}
        runner.tasks = {'a/scene': {'socket_directory': '', 'generation': 0}}
        runner.pair, _ = pair_fixture(); runner.scene = scene_fixture()
        runner.inputs = {'run_id': runner.scene['run_id']}
        viewer, public = viewer_fixture(runner.pair, runner.scene)
        runner.save = lambda *args: None
        events = []; runner.step = events.append
        def deploy(node, service):
            value = viewer if service == 'viewer' else {'socket_directory': '/sockets/' + node.key + '/' + service, 'generation': 1}
            runner.tasks[node.key + '/' + service] = value; return value
        runner.deploy_service = deploy
        runner.service_config = lambda node, service, task: run.Runner.service_config(runner, node, service, task) if service == 'viewer' else {'service': service, 'generation': task['generation']}
        def commissioned():
            self.assertFalse(any(path.endswith('/go') or path == '/owned/viewer/config.json'
                                 for node in runner.nodes.values() for path, _ in node.writes))
            events.append('mission-config-before-world-gates')
            runner.mission_directory = '/owned/mission-gateway/commissioned'
        runner.commission_missions = commissioned
        def public_read(code, path):
            self.assertEqual(code, run.VIEWER_PUBLIC)
            self.assertEqual(path, runner.mission_directory)
            return SimpleNamespace(stdout=model.json_bytes(public))
        runner.nodes['a'].python = public_read
        runner.deploy()
        self.assertEqual(len(runner.tasks), 13)
        self.assertEqual(events, ['apps', 'mission-config-before-world-gates'])
        for node in runner.nodes.values():
            self.assertTrue(all('mission-gateway' not in path and 'atlas-executor' not in path for path, _ in node.writes))
        self.assertEqual(sum(path.endswith('/go') for node in runner.nodes.values() for path, _ in node.writes), 12)
        writes = runner.nodes['b'].writes
        viewer_index = next(i for i, (path, _) in enumerate(writes) if path == '/owned/viewer/config.json')
        self.assertTrue(all(i > viewer_index for i, (path, _) in enumerate(writes) if path.endswith('/go')))
        self.assertEqual(model.strict_json(writes[viewer_index][1]),
                         model.viewer_config(runner.pair, runner.scene['run_id'], runner.scene, viewer, public))
        # A failed public trust binding never opens an ordinary app gate.
        for node in runner.nodes.values(): node.writes.clear()
        public['tenant'] = 'wrong-tenant'
        with self.assertRaises(ValueError): runner.deploy()
        self.assertFalse(any(path.endswith('/go') for node in runner.nodes.values() for path, _ in node.writes))

    def test_commissioning_keeps_keys_and_tokens_inside_a_then_checks_signed_catalog(self):
        pair, _ = pair_fixture(); scene = scene_fixture(); scene['frame']['revision'] = 'shared-office-' + 'b' * 16
        presentation = {'geometry': {'run_id': scene['run_id'], 'model_sha256': 'b' * 64}, 'simulation_time_ns': 10}
        runner = object.__new__(run.Runner); a = Node('a'); runner.nodes = {'a': a}
        runner.tasks = {'b/viewer': viewer_fixture(pair, scene)[0]}; runner.pair, runner.scene = pair, scene
        runner.inputs = {'run_id': scene['run_id']}; runner.scene_url = 'http://10.0.0.3:8091'
        runner.step = lambda value: None; saved = {}; runner.save = lambda name, value: saved.update({name: value})
        def deploy(node, service):
            value = {'service': service, 'generation': 0, 'socket_directory': '', 'ipv4': '10.0.0.4'}
            runner.tasks['a/' + service] = value; return value
        runner.deploy_service = deploy
        a.wait = lambda predicate, timeout: self.assertTrue(predicate())
        a.python = lambda code, *args, **kw: SimpleNamespace(returncode=0, stdout=model.json_bytes(presentation) if code == run.control.HTTP else b'installed')
        def inside(*args, **kw):
            a.commands.append(args)
            return SimpleNamespace(returncode=0, stdout=model.json_bytes({'assignee': 'atlas/navigation', 'domain': 'simulation',
                'run_id': scene['run_id'], 'types': [{'type': 'wendy.nav.inspect.v1', 'class': 'MOVE'}]}))
        a.inside = inside
        with tempfile.TemporaryDirectory(dir=stage.ROOT / '.local') as name:
            runner.source = Path(name); p = runner.source / 'a/project/services/scene/source/examples/collaborative-office/scene.json'
            p.parent.mkdir(parents=True); p.write_bytes(b'existing office test metadata')
            runner.commission_missions()
        self.assertEqual([args[1] for args in a.commands], ['commission', 'catalog'])
        self.assertTrue(a.commands[1][-1].endswith('/commissioned/executor.json'))
        self.assertEqual([path for path, _ in a.writes if path.endswith('/go')], ['/owned/mission-gateway/go'])
        runner.start_inspection_executor(); runner.start_inspection_executor()
        self.assertEqual([path for path, _ in a.writes if path.endswith('/go')], ['/owned/mission-gateway/go', '/owned/atlas-executor/go'])
        receipt = saved['mission-commission.json']
        self.assertFalse(receipt['motion_requested']); self.assertFalse(receipt['mission_signed'])
        self.assertFalse(receipt['operator_endpoint_configured'])
        self.assertEqual(receipt['input']['viewer_url'], 'http://10.0.0.8:8094')
        self.assertTrue(receipt['executor_gated_until_source_start'])
        self.assertNotIn('private_key', model.json_bytes(receipt).decode())

    def test_actual_node_task_hook_accepts_only_explicit_nonworld_services(self):
        pair, _ = pair_fixture()
        owner = SimpleNamespace(name='isolated-test', pair=pair)
        node = run.base.base.base.Node(owner, 'a')
        for service in ('mission-gateway', 'atlas-executor', 'fusion'):
            cid = model.APP + '_' + service
            labels = {'sh.wendy/app.id': model.APP, 'sh.wendy/service': service}
            def inside(*args, **kw):
                if args[:3] == ('ctr', 'containers', 'info'):
                    data = model.json_bytes({'Labels': labels})
                elif args[:3] == ('ctr', 'tasks', 'list'):
                    data = ('TASK PID STATUS\n' + cid + ' 42 RUNNING\n').encode()
                else:
                    data = model.json_bytes([{'addr_info': [{'family': 'inet', 'local': '10.0.0.4'}]}])
                return SimpleNamespace(stdout=data)
            node.inside = inside
            node.python = lambda *a, **k: SimpleNamespace(stdout=model.json_bytes({'pid': 42, 'start_ticks': 1, 'netns_inode': 2, 'cgroup': 'owned'}))
            if service == 'fusion':
                with self.assertRaises(ValueError): node.task(service)
            else:
                self.assertEqual(node.task(service)['generation'], 0)
                labels['sh.wendy/world.deployment-generation'] = '1'
                with self.assertRaises(ValueError): node.task(service)


if __name__ == '__main__': unittest.main()

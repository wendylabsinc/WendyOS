"""No worker, Docker, ROS or privilege mutations; filesystem and fake-child tests."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import stat
import tempfile
import threading
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('world_boot', Path(__file__).resolve().parents[2] / 'deploy/go2-sim/boot.py')
boot = importlib.util.module_from_spec(spec)
spec.loader.exec_module(boot)


class Child:
    def __init__(self, name, status=None):
        self.name = name
        self.status = status
    def poll(self):
        return self.status


class Steps:
    def __init__(self, calls=3):
        self.calls = calls
    def is_set(self):
        return self.calls <= 0
    def wait(self, _):
        self.calls -= 1
        return self.is_set()


class BootTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.data = self.root / 'world-go2'
        self.data.mkdir(mode=0o700)
        self.digest = 'a' * 64
        self.cfg = {'gateway_port': 18885, 'bridge_port': 18886, 'task_id': 'selected-task'}
        self.patch_uid = patch.object(boot, 'UID', os.getuid())
        self.patch_uid.start()
        self.addCleanup(self.patch_uid.stop)

    def test_config_fence_refuses_changes_and_unknown_old_state(self):
        lock = boot.prepare_state(self.digest, self.data, change_owner=False)
        self.assertEqual((self.data / 'configuration.sha256').read_text(), self.digest + '\n')
        self.assertEqual(stat.S_IMODE((self.data / 'configuration.sha256').stat().st_mode), 0o600)
        with self.assertRaises(BlockingIOError):
            boot.prepare_state(self.digest, self.data, change_owner=False)
        os.close(lock)
        with self.assertRaises(boot.BootError):
            boot.prepare_state('b' * 64, self.data, change_owner=False)
        self.assertEqual((self.data / 'configuration.sha256').read_text(), self.digest + '\n')
        (self.data / 'configuration.sha256').unlink()
        (self.data / 'world.tasks').write_bytes(b'existing state')
        with self.assertRaises(boot.BootError):
            boot.prepare_state(self.digest, self.data, change_owner=False)
        self.assertEqual((self.data / 'world.tasks').read_bytes(), b'existing state')

    def test_parent_directory_sync_failure_refuses_initialization(self):
        self.data.rmdir()
        calls = []
        def fail_parent(path):
            calls.append(path)
            raise OSError('parent directory sync failed')
        with patch.object(boot, 'sync_directory', side_effect=fail_parent), self.assertRaises(OSError):
            boot.prepare_state(self.digest, self.data, change_owner=False)
        self.assertEqual(calls, [self.root])
        self.assertFalse((self.data / 'launch-once.json').exists())
        self.assertFalse((self.data / 'configuration.sha256').exists())

    def test_launch_persists_once_and_partial_marker_never_retries(self):
        self.assertTrue(boot.claim_launch(self.digest, self.data))
        self.assertFalse(boot.claim_launch(self.digest, self.data))
        self.assertEqual(stat.S_IMODE((self.data / 'launch-once.json').stat().st_mode), 0o600)
        (self.data / 'launch-once.json').write_bytes(b'{')
        with self.assertRaises(boot.BootError):
            boot.claim_launch(self.digest, self.data)

    def test_fsync_failure_never_starts_worker_or_removes_marker(self):
        children = []
        def spawn(args):
            children.append(Child(args[0]))
            return children[-1]
        with patch.object(boot.os, 'fsync', side_effect=OSError('injected')), self.assertRaises(OSError):
            boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, lambda *_: True, lambda _: None)
        self.assertEqual(len(children), 1)
        self.assertTrue((self.data / 'launch-once.json').exists())
        self.assertFalse(boot.claim_launch(self.digest, self.data))

    def test_start_order_marker_and_restart_reporting_only(self):
        launched, stopped, readiness = [], [], []
        def spawn(args):
            if launched:
                self.assertTrue((self.data / 'launch-once.json').is_file())
            launched.append(args)
            return Child(args[0])
        def ready(child, port, _timeout, _closing):
            readiness.append(port)
            return True
        self.assertEqual(boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, ready, lambda p: stopped.append(p.name) if p else None), 0)
        self.assertEqual(readiness, [18885, 18886])
        self.assertEqual([a[0] for a in launched], ['/opt/world/worldd', 'python3', '/opt/world/world-go2-executor'])
        self.assertIn('--task-id', launched[-1])
        self.assertIn('selected-task', launched[-1])
        self.assertEqual(launched[-1].count('--fault-snapshot'), 1)
        self.assertEqual(launched[-1][launched[-1].index('--fault-snapshot') + 1], '/data/world-go2/first-fault.json')
        self.assertEqual(launched[-1].count('--fault-snapshot-log'), 1)
        self.assertEqual(launched[-1].count('--observation-log'), 1)
        self.assertEqual(stopped[-3:], ['/opt/world/world-go2-executor', 'python3', '/opt/world/worldd'])
        launched.clear()
        boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, ready, lambda _: None)
        self.assertEqual(len(launched), 1)

    def test_executor_death_stops_bridge_keeps_gateway_until_shutdown(self):
        launched, stopped, events = [], [], []
        def spawn(args):
            child = Child(args[0], 1 if len(launched) == 2 else None)
            launched.append(child)
            return child
        with patch.object(boot, 'emit', side_effect=events.append):
            boot.supervise(self.cfg, self.digest, Steps(4), self.data, spawn, lambda *_: True, lambda p: stopped.append(p.name) if p else None)
        self.assertEqual(stopped, ['/opt/world/world-go2-executor', 'python3', '/opt/world/worldd'])
        self.assertIn('motion_worker_exited_reporting_only', events)
        self.assertEqual(len(launched), 3)
        # A retained diagnostic never becomes a reason to retry or re-arm.
        artifact = self.data / 'first-fault.json'
        artifact.write_bytes(b'{"synthetic_diagnostic":"retained"}\n')
        before = artifact.read_bytes()
        with patch.object(boot, 'emit', side_effect=events.append):
            boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, lambda *_: True, lambda _: None)
        self.assertEqual(len(launched), 4)
        self.assertEqual(launched[-1].name, '/opt/world/worldd')
        self.assertEqual(artifact.read_bytes(), before)

    def test_gateway_failure_no_marker_and_bridge_failure_consumes_marker(self):
        spawned = []
        def spawn(args):
            spawned.append(Child(args[0]))
            return spawned[-1]
        with self.assertRaises(boot.BootError):
            boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, lambda *_: False, lambda _: None)
        self.assertFalse((self.data / 'launch-once.json').exists())
        spawned.clear()
        boot.supervise(self.cfg, self.digest, Steps(), self.data, spawn, lambda _, port, *_a: port == 18885, lambda _: None)
        self.assertEqual(len(spawned), 2)
        self.assertFalse(boot.claim_launch(self.digest, self.data))

    def test_private_real_state_and_bounded_input(self):
        regular = self.root / 'input'
        regular.write_bytes(b'0123')
        self.assertEqual(boot.read_regular(regular, 4), b'0123')
        with self.assertRaises(boot.BootError):
            boot.read_regular(regular, 3)
        link = self.root / 'link'
        link.symlink_to(regular)
        with self.assertRaises(boot.BootError):
            boot.read_regular(link)
        fifo = self.root / 'fifo'
        os.mkfifo(fifo)
        with self.assertRaises(boot.BootError):
            boot.read_regular(fifo)
        self.data.chmod(0o755)
        with self.assertRaises(boot.BootError):
            boot.prepare_state(self.digest, self.data, change_owner=False)
        self.data.chmod(0o700)
        self.data.rmdir()
        self.data.symlink_to(self.root, target_is_directory=True)
        with self.assertRaises(boot.BootError):
            boot.prepare_state(self.digest, self.data, change_owner=False)

    def test_group_cleanup_includes_descendants_after_main_exits(self):
        child = unittest.mock.Mock()
        child.pid = 123456
        with patch.object(boot.os, 'killpg') as kill:
            boot.stop_child(child)
        self.assertEqual(kill.call_args_list, [unittest.mock.call(123456, boot.signal.SIGTERM), unittest.mock.call(123456, boot.signal.SIGKILL)])
        self.assertEqual(child.wait.call_count, 2)

    def test_ros_entrypoint_restores_environment_after_setup(self):
        script = (Path(__file__).resolve().parents[2] / 'deploy/go2-sim/mission-entrypoint.sh').read_text()
        setup = script.index('source /opt/ros/humble/setup.bash')
        self.assertLess(setup, script.index('set -u'))
        for setting in ('export CYCLONEDDS_URI=file:///opt/world/cyclonedds.xml',
                        'export ROS_DOMAIN_ID=0 RMW_IMPLEMENTATION=rmw_cyclonedds_cpp ROS_LOCALHOST_ONLY=0',
                        'export ROS_LOG_DIR=/tmp/ros-log'):
            self.assertGreater(script.index(setting), setup)
            self.assertLess(script.index(setting), script.index('exec python3'))

    def test_privilege_drop_order(self):
        events = []
        with patch.object(boot.os, 'setgroups', side_effect=lambda x: events.append(('groups', x))), patch.object(boot.os, 'setgid', side_effect=lambda x: events.append(('gid', x))), patch.object(boot.os, 'setuid', side_effect=lambda x: events.append(('uid', x))), patch.object(boot.os, 'geteuid', return_value=os.getuid()), patch.object(boot.os, 'getegid', return_value=os.getuid()):
            boot.drop_privileges()
        self.assertEqual(events, [('groups', []), ('gid', os.getuid()), ('uid', os.getuid())])

    def test_complete_staged_hash_inventory(self):
        app = self.root / 'app'
        app.mkdir()
        files = {}
        for name in boot.FILES:
            path = app / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_bytes(name.encode())
            files[name] = {'sha256': hashlib.sha256(name.encode()).hexdigest(), 'bytes': len(name)}
        cfg = dict(self.cfg, schema='wendy.world.go2-boot.v1', run_id='test', files=files)
        (app / 'boot.json').write_text(json.dumps(cfg))
        observed, digest = boot.checked_config(app)
        self.assertEqual(observed, cfg)
        self.assertEqual(len(digest), 64)
        (app / 'profile.json').write_bytes(b'changed')
        with self.assertRaises(boot.BootError):
            boot.checked_config(app)
        cfg['files']['../escape'] = files['node.json']
        (app / 'boot.json').write_text(json.dumps(cfg))
        with self.assertRaises(boot.BootError):
            boot.checked_config(app)


if __name__ == '__main__':
    unittest.main(verbosity=2)

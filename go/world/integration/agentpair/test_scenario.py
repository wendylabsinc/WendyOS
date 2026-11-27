"""Offline orchestration checks; synthetic receipts confer no cryptographic trust."""
import base64
import copy
from pathlib import Path
import subprocess
import tempfile
from types import SimpleNamespace
import unittest
from unittest.mock import Mock, patch

import control
import model
import scenario
from test_run import runner_fixture
from test_proof import await_fixture, encoded


def make(directory):
    runner = runner_fixture(directory, 'replication')
    runner.inputs['binaries'] = {'proof': {}}
    runner.runtime_state = 'deploy'; runner.addresses = {'a': '172.19.0.3', 'b': '172.19.0.4'}
    runner.current_container_id = lambda name, expected=None: ('a' if name.endswith('-a') else 'b') * 64
    for key, node in runner.nodes.items():
        node.generation = 1
        node.task_state = {'generation': 40 if key == 'a' else 41, 'app': model.APP, 'service': model.SERVICE,
                           'device': runner.pair['nodes'][('a', 'b').index(key)]['public']['principal'],
                           'pid': 20, 'start_ticks': 50, 'socket_directory': '/old'}
        node.clock_raw, _ = control.sample_clock(runner.inputs['run_id'], 0)
        node.mesh_raw = model.json_bytes(model.mesh_configuration(runner.pair, key, runner.addresses))
    return scenario.Scenario(runner)


class ScenarioTests(unittest.TestCase):
    def test_two_clock_readbacks_precede_new_sample_use(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); events = []
            for key, node in s.nodes.items():
                node.atomic_write = lambda path, raw, previous, k=key: events.append(('write', k, model.digest(previous)))
                node.read = lambda path, k=key: events.append(('read', k)) or s.nodes[k].clock_raw
            s.advance(31)
            self.assertEqual([x[:2] for x in events], [('write', 'a'), ('write', 'b'), ('read', 'a'), ('read', 'b')])
            self.assertEqual(s.elapsed, 31)
            self.assertEqual(s.nodes['a'].clock_raw, s.nodes['b'].clock_raw)
            with self.assertRaises(model.Refused): s.advance(31)
            with self.assertRaises(model.Refused): s.advance(0)

    def test_partial_clock_change_stops_without_rollback_or_read_commands(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); before = s.sample_raw
            s.nodes['a'].atomic_write = Mock()
            s.nodes['b'].atomic_write = Mock(side_effect=model.Refused('write uncertain'))
            s.nodes['a'].read = s.nodes['b'].read = Mock(side_effect=AssertionError('no subsequent reads'))
            with self.assertRaises(model.Refused): s.advance(31)
            self.assertNotEqual(s.nodes['a'].clock_raw, before)
            self.assertEqual(s.nodes['b'].clock_raw, before)
            self.assertEqual(s.elapsed, 0)
            s.nodes['a'].atomic_write.assert_called_once(); s.nodes['b'].atomic_write.assert_called_once()
            with self.assertRaises(model.Refused): s.advance(31)
            s.nodes['a'].atomic_write.assert_called_once()

    def test_query_binds_exact_source_sample_generation_and_original(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory)
            inputs = await_fixture(); receipt = inputs['received_receipt']
            s.runner.inputs['run_id'] = inputs['run_id']
            s.nodes['b'].task_state = inputs['received_task']
            s.nodes['b'].command = Mock(return_value=receipt)
            digest = receipt['originals'][0]['envelope_sha256']
            self.assertEqual(s.query('b', 'present', digest), receipt)
            changes = [lambda r: r['clock'].update(deployment_generation=99),
                       lambda r: r['clock'].update(sample_unix_ns=s.sample_ns + 1),
                       lambda r: r.update(reply_json=encoded({'signedRecordJson': [base64.b64encode(b'altered').decode()]})),
                       lambda r: r.update(request_json=encoded({'sources': [], 'limit': 8})),
                       lambda r: r.update(rpc_code='PermissionDenied'),
                       lambda r: r.update(await_elapsed_ns=10_000_000_001)]
            for change in changes:
                wrong = copy.deepcopy(receipt); change(wrong); s.nodes['b'].command.return_value = wrong
                with self.subTest(change=change), self.assertRaises(model.Refused): s.query('b', 'present', digest)

    def test_empty_read_is_explicit_and_never_exact_transfer(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); r = await_fixture()['received_receipt']
            s.runner.inputs['run_id'] = r['run_id']
            r.update(originals=[], reply_json=encoded({}))
            s.nodes['b'].command = Mock(return_value=r)
            self.assertEqual(s.query('b', 'absent')['originals'], [])
            with self.assertRaises(model.Refused): s.query('b', 'present', 'a' * 64)
            self.assertFalse(s.summary['durable_remote_tombstone_proven'])
            self.assertFalse(s.summary['durable_remote_floor_proven'])

    def test_restart_refuses_old_mount_before_removal_and_fresh_deployment(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); node = s.nodes['b']; node.agent_pid = 10; s.boots = {'a': 'a' * 32, 'b': 'b' * 32}
            events = []; old = copy.deepcopy(node.task_state)
            node.stop_agent = lambda: events.append('stop')
            def start(phase): events.append('restart'); node.agent_pid = 11
            node.start_agent = start
            node.observe_task = lambda: events.append('observe-old') or old
            def refused(action, **fields):
                events.append('old-query'); self.assertEqual((action, fields), ('query', {'expect_refusal': True}))
                return {'sequence': 5, 'error': 'socket_or_clock_refused'}
            node.command = refused
            node.cli = lambda *args, **kw: events.append(('cli', args))
            def deploy(actual):
                events.append('deploy'); actual.task_state = {**old, 'generation': 42, 'socket_directory': '/new', 'pid': 30, 'start_ticks': 60}
            s.runner.deploy_node = deploy
            s.native = lambda label: events.append('native')
            def query(key, expect, **kwargs):
                events.append(('query', key, kwargs)); s.boots['b'] = 'c' * 32
            s.query = query
            s.restart_receiver()
            self.assertEqual(events[:6], ['stop', 'restart', 'observe-old', 'old-query',
                                         ('cli', ('device', 'apps', 'remove', model.APP, '--force')), 'deploy'])
            self.assertEqual(events[-2:], [('query', 'b', {'new_boot': True}), ('query', 'a', {})])
            self.assertTrue((Path(directory) / 'restart-old-reader.json').exists())
            self.assertFalse(model.strict_json((Path(directory) / 'restart-new-reader.json').read_bytes())['stores_reinitialized'])

    def test_failed_old_mount_check_prevents_remove_and_redeploy(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); node = s.nodes['b']; s.boots['b'] = 'b' * 32; node.agent_pid = 10
            node.stop_agent = Mock(); node.start_agent = lambda phase: setattr(node, 'agent_pid', 11)
            node.observe_task = lambda: node.task_state
            node.command = Mock(side_effect=model.Refused('old mount returned data'))
            node.cli = Mock(); s.runner.deploy_node = Mock()
            with self.assertRaises(model.Refused): s.restart_receiver()
            node.cli.assert_not_called(); s.runner.deploy_node.assert_not_called()

    def test_full_observation_order_and_incomplete_durable_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            s = make(directory); events = []
            s.native = lambda label: events.append(('native', label))
            s.query = lambda key, expect, envelope=None: events.append(('query', key, expect)) or {'sequence': 7}
            def publish(key, value, epoch, sequence, label):
                events.append(('publish', key, epoch, sequence)); s.publications[key] = ({'originals': [{'envelope_sha256': key * 64}]}, epoch, sequence)
            s.publish = publish
            s.transfer = lambda key, label: events.append(('transfer', key, label))
            s.cut = lambda: events.append(('cut',))
            s.advance = lambda value: events.append(('advance', value))
            s.restore = lambda: events.append(('restore',))
            s.clock = Mock(); s.verify = Mock()
            s.restart_receiver = lambda: events.append(('restart',))
            for key, node in s.nodes.items():
                node.command = lambda action, k=key, **fields: events.append((action, k)) or {'originals': [{'envelope_sha256': 'f' * 64}]}
            result = s.execute()
            self.assertLess(events.index(('cut',)), events.index(('publish', 'a', 1, 2)))
            self.assertLess(events.index(('publish', 'b', 1, 2)), events.index(('restore',)))
            self.assertLess(events.index(('transfer', 'a', 'repaired-original')), events.index(('tombstone', 'a')))
            self.assertLess(events.index(('restart',)), events.index(('publish', 'b', 2, 1)))
            self.assertTrue(result['scalar_transfer']); self.assertTrue(result['receiver_restart_fresh_transfer'])
            self.assertFalse(result['retained_remote_original_after_restart_proven'])
            self.assertFalse(result['full_native_repair_gate_complete'])
            self.assertFalse(result['durable_remote_tombstone_proven'])

    def test_partition_cannot_be_bypassed_by_an_earlier_accept(self):
        row = control.partition_rules('world-agentpair-' + 'a' * 12, '172.19.0.4')[0]
        valid = ('-P INPUT ACCEPT\n-A INPUT -j ' + row['chain'] + '\n-A INPUT -p udp -j ACCEPT\n').encode()
        control.validate_jump(valid, row)
        with self.assertRaises(model.Refused): control.validate_jump(b'-P INPUT ACCEPT\n-A INPUT -j ACCEPT\n' + valid.split(b'\n', 1)[1], row)
        with self.assertRaises(model.Refused): control.validate_jump(valid + ('-A INPUT -j ' + row['chain'] + '\n').encode(), row)


if __name__ == '__main__': unittest.main()

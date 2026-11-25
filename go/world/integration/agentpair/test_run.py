import base64
import copy
import json
from pathlib import Path
import subprocess
import tempfile
import types
import unittest
from unittest.mock import Mock, patch

import model
import run
from test_model import pair_fixture


def runner_fixture(directory, phase='deploy'):
    runner = run.Runner.__new__(run.Runner)
    runner.out = Path(directory).resolve()
    runner.name = 'world-agentpair-test'; runner.phase = phase
    runner.inputs = {'run_id': 'unit-test'}
    runner.nodes = {name: run.Node(runner, name) for name in ('a', 'b')}
    runner.created = []; runner.count = 0; runner.trace_rpc = False
    runner.pair, runner.peers = pair_fixture()
    runner.started = run.time.monotonic(); runner.runtime_state = 'new'
    runner.call = Mock(side_effect=AssertionError('runtime invocation requires an explicit test mock'))
    return runner


class RunnerTests(unittest.TestCase):
    def test_existing_library_methods_import_and_fixed_scripts_compile(self):
        self.assertIs(run.Runner.call, run.library.Runner.call)
        self.assertIs(run.Node.wait, run.library.Runner.wait)
        self.assertIs(run.Node.snapshot_file, run.library.Runner.snapshot_file)
        for source in (run.WRITE, run.PAIR_SEND, run.PAIR_RECEIVE, run.control.ATOMIC_WRITE, run.control.VOLUME_DIRECTORIES):
            compile(source, '<fixed-runtime-script>', 'exec')

    def test_private_runtime_and_networkless_issuer_arguments(self):
        image = 'sha256:' + 'a' * 64
        args = run.runtime_args(image, 'node', 'owned-network', 'owned-volume', 'owner')
        self.assertIn('wendy.world.agentpair=owner', args)
        self.assertIn('/dev/net/tun:/dev/net/tun', args)
        self.assertIn('--cgroupns=private', args)
        self.assertIn('--read-only', args)
        for key, value in run.IPV6.items():
            self.assertIn(key + '=' + value, args)
        for forbidden in ('--privileged', '--network=host', '--pid=host', '-p', '/var/run/docker.sock'):
            self.assertNotIn(forbidden, args)
        issuer = run.issuer_args(image, 'issuer', 'issuer-volume', 'owner')
        self.assertEqual(issuer[issuer.index('--network') + 1], 'none')
        self.assertNotIn('--cap-add', issuer)
        self.assertNotIn('/dev/net/tun:/dev/net/tun', issuer)

    def test_invalid_inputs_never_create_output_or_process(self):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory).resolve(); source = root / 'inputs.json'
            source.write_text('{"version":1}')
            with patch.object(run.subprocess, 'Popen') as process, self.assertRaises(model.Refused):
                run.Runner(source, root / 'output')
            process.assert_not_called()
            self.assertFalse((root / 'output').exists())

    def test_gate_order_and_cleanup_on_each_phase_failure(self):
        for fail in (None, 'setup', 'enroll', 'commission', 'deploy'):
            with self.subTest(fail=fail), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory)
                calls = []
                def phase(name):
                    def apply():
                        calls.append(name); runner.runtime_state = name
                        if fail == name:
                            raise model.Refused('injected')
                    return apply
                for name in ('setup', 'enroll', 'commission', 'deploy'):
                    setattr(runner, name, phase(name))
                runner.cleanup = lambda: calls.append('cleanup') or {'complete': True}
                if fail:
                    with self.assertRaises(model.Refused): runner.execute()
                    expected = ['setup', 'enroll', 'commission', 'deploy']
                    self.assertEqual(calls, expected[:expected.index(fail)+1] + ['cleanup'])
                else:
                    result = runner.execute()
                    self.assertEqual(calls, ['setup', 'enroll', 'commission', 'deploy', 'cleanup'])
                    self.assertTrue(result['passed']); self.assertFalse(result['replication_acceptance'])
                saved = json.loads((Path(directory) / 'result.json').read_text())
                self.assertEqual(saved['passed'], fail is None)

    def test_enrollment_gate_never_commissions_or_deploys(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory, 'enrollment')
            runner.setup = Mock(); runner.enroll = Mock()
            runner.commission = Mock(); runner.deploy = Mock()
            runner.cleanup = lambda: {'complete': True}
            runner.execute()
            runner.setup.assert_called_once(); runner.enroll.assert_called_once()
            runner.commission.assert_not_called(); runner.deploy.assert_not_called()

    def test_cleanup_includes_ambiguous_create_and_never_deletes_foreign_resource(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory)
            runner.call = Mock(side_effect=model.Refused('unknown create outcome'))
            with self.assertRaises(model.Refused):
                runner.create('container', 'unknown', ['docker', 'run'])
            self.assertEqual(runner.created, [('container', 'unknown')])
            runner.created += [('volume', 'foreign'), ('network', 'absent')]
            before = {'unknown': 'owned', 'foreign': 'foreign', 'absent': 'absent'}
            removed = set()
            runner.resource_state = lambda kind, name: 'absent' if name in removed else before[name]
            runner.current_container_id = lambda name, expected=None: 'a' * 64
            runner.call = lambda argv, **kw: removed.add('unknown' if argv[-1] == 'a' * 64 else argv[-1])
            result = runner.cleanup()
            self.assertEqual(removed, {'unknown'})
            self.assertFalse(result['complete'])

    def test_same_name_replacement_after_snapshot_is_never_removed(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            runner.created = [('container', node.name)]
            runner.resource_state = lambda *args: 'owned'
            old = {'Id': 'a' * 64, 'Config': {'Labels': {run.LABEL: runner.name}}}
            foreign = {'Id': 'b' * 64, 'Config': {'Labels': {run.LABEL: 'someone-else'}}}
            replies = iter((old, foreign)); removed = []
            def call(argv, **kw):
                if argv[:2] == ['docker', 'inspect']:
                    return subprocess.CompletedProcess(argv, 0, model.json_bytes([next(replies)]), b'')
                removed.append(argv)
            runner.call = call
            observed = []
            runner.snapshot_outputs = lambda actual, identity: observed.append((actual.key, identity))
            result = runner.cleanup()
            self.assertEqual(observed, [('a', 'a' * 64)])
            self.assertEqual(removed, [])
            self.assertFalse(result['complete'])
            self.assertEqual(result['resources'][0]['after'], 'unknown')

    def test_diagnostics_use_fresh_owned_id_and_stop_after_replacement(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            checked = []
            def current(name, expected=None):
                checked.append((name, expected))
                if len(checked) == 2:
                    raise model.Refused('replacement')
                return expected
            runner.current_container_id = current
            node.snapshot_file = Mock(return_value={'status': 'captured'})
            with self.assertRaises(model.Refused):
                runner.snapshot_outputs(node, 'a' * 64)
            self.assertEqual(node.snapshot_file.call_count, 1)
            self.assertEqual(node.snapshot_file.call_args.kwargs['container'], 'a' * 64)
            self.assertEqual(checked, [(node.name, 'a' * 64)] * 2)

    def test_telemetry_is_opt_in_and_uses_private_collector_with_exact_bounds(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            runner.current_container_id = Mock(return_value='a' * 64)
            node.snapshot_file = Mock(return_value={'status': 'unavailable'})
            with patch.object(run.library.Runner, 'call') as collect:
                runner.snapshot_outputs(node, 'a' * 64)
            collect.assert_not_called()
            self.assertFalse((runner.out / 'private-runtime-telemetry').exists())

        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); runner.trace_rpc = True; node = runner.nodes['a']
            runner.current_container_id = Mock(return_value='a' * 64)
            node.snapshot_file = Mock(return_value={'status': 'unavailable'})
            private_bytes = b'{"body":"World peer repair incomplete","attributes":{"error":"secret-value"}}\n'
            calls = []
            def collect(namespace, argv, **kwargs):
                runner.current_container_id.assert_called_once_with(node.name, 'a' * 64)
                calls.append((namespace, argv, kwargs))
                self.assertEqual(namespace.out, runner.out / 'private-runtime-telemetry/a')
                self.assertEqual(namespace.count, 0)
                self.assertEqual(namespace.out.stat().st_mode & 0o777, 0o700)
                self.assertEqual(namespace.out.parent.stat().st_mode & 0o777, 0o700)
                run.library.stage.write_new(namespace.out / '001.json', model.json_bytes({'argv': argv, 'exit_code': 0}))
                run.library.stage.write_new(namespace.out / '001.stdout', private_bytes)
                run.library.stage.write_new(namespace.out / '001.stderr', b'private-cli-warning\n')
                return subprocess.CompletedProcess(argv, 0, private_bytes, b'private-cli-warning\n')
            with patch.object(run.library.Runner, 'call', side_effect=collect):
                runner.snapshot_outputs(node, 'a' * 64)
            self.assertEqual(len(calls), 1)
            _, argv, kwargs = calls[0]
            env = [item for key, value in run.library.base.ENV.items() for item in ('-e', key + '=' + value)]
            self.assertEqual(argv, ['docker', 'exec', *env, 'a' * 64, model.TOOLS + '/cli', '--json',
                                   'device', 'logs', '--service', 'wendy-agent', '--level', 'debug',
                                   '--tail', '512', '--no-follow', '--read-only'])
            self.assertEqual(kwargs, {'timeout': 15, 'check': False, 'max_output': 1 << 20})
            runner.call.assert_not_called()
            self.assertEqual(runner.count, 0)
            self.assertNotIn(node.name, argv)
            self.assertNotIn('WENDY_DEBUG=1', argv)
            saved = json.loads((runner.out / 'snapshots-a.json').read_bytes())['telemetry']
            self.assertEqual(set(saved), {'status', 'event_completeness', 'files'})
            self.assertEqual(saved['status'], 'captured')
            self.assertEqual(saved['event_completeness'], 'unknown')
            self.assertEqual(saved['files']['001.stdout'], {'size': len(private_bytes), 'sha256': model.digest(private_bytes)})
            for value in saved['files'].values():
                self.assertEqual(set(value), {'size', 'sha256'})
            public = (runner.out / 'snapshots-a.json').read_bytes()
            for private in (b'secret-value', b'private-cli-warning', b'World peer repair incomplete'):
                self.assertNotIn(private, public)
            self.assertEqual(sorted(p.name for p in runner.out.iterdir()), ['private-runtime-telemetry', 'snapshots-a.json'])

    def test_telemetry_refuses_replaced_container_before_exec(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']
            runner.current_container_id = Mock(side_effect=model.Refused('private replacement reason'))
            with patch.object(run.library.Runner, 'call') as collect:
                result = runner.snapshot_telemetry(node, 'a' * 64)
            collect.assert_not_called()
            runner.current_container_id.assert_called_once_with(node.name, 'a' * 64)
            self.assertEqual(result, {'status': 'unknown', 'event_completeness': 'unknown', 'files': {}})

    def test_telemetry_real_collector_retains_empty_timeout_and_overflow_only_privately(self):
        # Exercise the actual bounded collector with a local test child; no
        # Docker or Agent process is started. The wrapper swaps only Popen argv.
        for case in ('empty', 'timeout', 'overflow'):
            with self.subTest(case=case), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory); node = runner.nodes['a']
                runner.current_container_id = Mock(return_value='a' * 64)
                popen = run.library.subprocess.Popen
                def child(argv, **kwargs):
                    if case == 'overflow':
                        script = 'import sys; sys.stdout.buffer.write(b"private-data" * 110000); sys.stdout.flush()'
                    elif case == 'timeout':
                        script = 'import time; print("private-before-timeout", flush=True); time.sleep(60)'
                    else:
                        script = 'pass'
                    process = popen([run.sys.executable, '-c', script], **kwargs)
                    if case == 'timeout':
                        wait = process.wait
                        def limited_wait(timeout=None):
                            return wait(timeout=0.1 if timeout == 15 else timeout)
                        process.wait = limited_wait
                    return process
                with patch.object(run.library.subprocess, 'Popen', side_effect=child):
                    result = runner.snapshot_telemetry(node, 'a' * 64)
                self.assertEqual(result['status'], 'captured' if case == 'empty' else 'unknown')
                self.assertEqual(result['event_completeness'], 'unknown')
                self.assertEqual(set(result['files']), {'001.json', '001.stdout', '001.stderr'})
                private = runner.out / 'private-runtime-telemetry/a'
                receipt = json.loads((private / '001.json').read_bytes())
                self.assertEqual(receipt['exit_code'], {'empty': 0, 'timeout': 124, 'overflow': 125}[case])
                self.assertEqual(receipt['timeout'], case == 'timeout')
                self.assertEqual(receipt['output_limited'], case == 'overflow')
                self.assertLessEqual((private / '001.stdout').stat().st_size, 1 << 20)
                self.assertLessEqual((private / '001.stderr').stat().st_size, 1 << 20)
                self.assertEqual((private / '001.stdout').stat().st_size,
                                 result['files']['001.stdout']['size'])
                self.assertNotIn('private-data', model.json_bytes(result).decode())
                runner.call.assert_not_called()

    def test_capture_failure_never_blocks_snapshots_or_owned_cleanup(self):
        for failure in ('timeout', 'exception', 'interrupted', 'private_path'):
            with self.subTest(failure=failure), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory); runner.trace_rpc = True; node = runner.nodes['a']
                runner.created = [('container', node.name)]
                removed = []
                runner.resource_state = lambda *args: 'absent' if removed else 'owned'
                runner.current_container_id = Mock(return_value='a' * 64)
                node.snapshot_file = Mock(return_value={'status': 'unavailable'})
                runner.call = lambda argv, **kwargs: removed.append(argv) or subprocess.CompletedProcess(argv, 0, b'', b'')
                if failure == 'private_path':
                    (runner.out / 'private-runtime-telemetry').symlink_to(runner.out, target_is_directory=True)
                def failed_collect(*args, **kwargs):
                    if failure == 'exception':
                        raise OSError('sensitive collection failure')
                    if failure == 'interrupted':
                        raise run.library.InterruptedRun(run.signal.SIGTERM)
                    return subprocess.CompletedProcess([], 124, b'', b'')
                with patch.object(run.library.Runner, 'call', side_effect=failed_collect) as collect:
                    result = runner.cleanup()
                self.assertEqual(collect.call_count, 0 if failure == 'private_path' else 1)
                self.assertTrue(result['complete'])
                self.assertEqual(removed, [['docker', 'rm', '-f', 'a' * 64]])
                self.assertEqual(node.snapshot_file.call_count, 3)
                saved = json.loads((runner.out / 'snapshots-a.json').read_bytes())
                self.assertEqual(saved['telemetry']['status'], 'unknown')
                self.assertNotIn('sensitive', (runner.out / 'snapshots-a.json').read_text())

    def test_public_commission_requires_both_enrollments_before_mutation(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); runner.peers = None
            runner.call = Mock()
            with self.assertRaises(model.Refused): runner.commission()
            runner.call.assert_not_called()
            self.assertEqual(list(Path(directory).iterdir()), [])

    def test_issuer_parent_is_protected_before_binary_or_secret_creation(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory)
            runner.network = runner.name + '-net'; runner.issuer = runner.name + '-issuer'
            runner.issuer_volume = runner.issuer + '-state'
            runner.inputs['runtime_image'] = 'sha256:' + 'a' * 64
            calls = []
            runner.call = lambda argv, **kwargs: calls.append(argv) or subprocess.CompletedProcess(argv, 0, b'', b'')
            runner.upload_binary = Mock(side_effect=model.Refused('stop before tool execution'))
            with self.assertRaises(model.Refused): runner.setup()
            creations = [argv for argv in calls if len(argv) > 6 and argv[:3] == ['docker', 'exec', runner.issuer]]
            self.assertEqual(creations, [
                ['docker', 'exec', runner.issuer, 'python3', '-c', run.library.CREATE_STAGE, '/var/lib/agentpair'],
                ['docker', 'exec', runner.issuer, 'python3', '-c', run.library.CREATE_STAGE, model.TOOLS]])
            runner.upload_binary.assert_called_once_with(runner.issuer, 'pair-fixture')

    def test_agent_start_requires_fresh_pid_and_successful_rpc(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']; node.agent_pid = 40
            runner.call = Mock(return_value=subprocess.CompletedProcess([], 0, b'', b''))
            node.python = Mock(side_effect=[subprocess.CompletedProcess([], 0, b'41\n', b''), subprocess.CompletedProcess([], 0, b'42\n', b''), subprocess.CompletedProcess([], 0, b'', b'')])
            node.cli = Mock(side_effect=[subprocess.CompletedProcess([], 1, b'', b''), subprocess.CompletedProcess([], 0, b'', b'')])
            node.check_world_startup = Mock()
            observations = []
            def wait(ready, seconds):
                observations.extend([ready(), ready()])
            node.wait = wait
            node.start_agent('restart')
            self.assertEqual(observations, [False, True])
            self.assertEqual(node.agent_pid, 42)
            self.assertIn("p!=int(sys.argv[1])", node.python.call_args_list[0].args[0])
            self.assertEqual(node.cli.call_count, 2)
            node.check_world_startup.assert_called_once()

    def test_unknown_publication_saved_and_never_retried(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']; node.generation = 1
            node.task_state = {'generation': 18446744073709551614}
            writes = []
            node.atomic_write = lambda path, body: writes.append((path, body))
            node.wait = lambda *args: None
            def receipt(path, limit):
                command = writes[-1][1]
                return model.json_bytes({'version': 1, 'run_id': 'unit-test', 'sequence': 1, 'action': 'publish',
                                         'status': 'unknown_outcome', 'command_json': base64.b64encode(command).decode(),
                                         'command_sha256': model.digest(command), 'validation': 'socket_scope_and_structure_only'})
            node.read = receipt
            with self.assertRaises(model.Refused): node.command('publish', value=7, ttl_ms=3000)
            self.assertEqual(len(writes), 1); self.assertEqual(node.command_sequence, 1)
            self.assertEqual(json.loads((Path(directory) / 'probe/a/000001.json').read_text())['status'], 'unknown_outcome')

    def test_scope_generation_and_original_command_bytes_are_checked(self):
        for mutation in ('generation', 'command', 'run'):
            with self.subTest(mutation=mutation), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory); node = runner.nodes['a']; node.generation = 1
                node.task_state = {'generation': 7}; writes = []
                node.atomic_write = lambda path, body: writes.append(body); node.wait = lambda *args: None
                def receipt(path, limit):
                    command = writes[-1]
                    value = {'version': 1, 'run_id': 'unit-test', 'sequence': 1, 'action': 'query', 'status': 'complete',
                             'command_json': base64.b64encode(command).decode(), 'command_sha256': model.digest(command),
                             'validation': 'socket_scope_and_structure_only', 'clock': {'deployment_generation': 7, 'sample_clock_domain': 'simulation:unit-test'}}
                    if mutation == 'generation': value['clock']['deployment_generation'] = 8
                    if mutation == 'command': value['command_json'] = base64.b64encode(b'{}').decode()
                    if mutation == 'run': value['run_id'] = 'other'
                    return model.json_bytes(value)
                node.read = receipt
                with self.assertRaises(model.Refused): node.command('query')
                self.assertEqual(len(writes), 1)

    def test_old_mount_refusal_uses_real_error_field_and_never_accepts_empty_success(self):
        for status, error, okay in (('failed', 'socket_or_clock_refused', True), ('failed', 'deadline_exceeded', True),
                                    ('failed', 'command_refused', False), ('complete', None, False)):
            with self.subTest(status=status, error=error), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory); node = runner.nodes['b']; node.generation = 1
                node.task_state = {'generation': 7}; writes = []
                node.atomic_write = lambda path, raw: writes.append(raw); node.wait = lambda *args: None
                def receipt(*args):
                    value = {'version': 1, 'run_id': 'unit-test', 'sequence': 1, 'action': 'query', 'status': status,
                             'command_json': base64.b64encode(writes[0]).decode(), 'command_sha256': model.digest(writes[0]),
                             'originals': [], 'validation': 'socket_scope_and_structure_only'}
                    if error: value['error'] = error
                    return model.json_bytes(value)
                node.read = receipt
                if okay:
                    self.assertEqual(node.command('query', expect_refusal=True)['error'], error)
                else:
                    with self.assertRaises(model.Refused): node.command('query', expect_refusal=True)
                self.assertEqual(len(writes), 1)
                with self.assertRaises(model.Refused): node.command('publish', expect_refusal=True, value=1, ttl_ms=1000)

    def test_replication_failure_runs_cleanup_and_never_marks_full_gate(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory, 'replication')
            calls = []
            for name in ('setup', 'enroll', 'commission', 'deploy'):
                setattr(runner, name, lambda n=name: calls.append(n))
            runner.replication = Mock(side_effect=model.Refused('unknown write'))
            runner.cleanup = lambda: calls.append('cleanup') or {'complete': True}
            with self.assertRaises(model.Refused): runner.execute()
            self.assertEqual(calls, ['setup', 'enroll', 'commission', 'deploy', 'cleanup'])
            runner.replication.assert_called_once()
            result = json.loads((Path(directory) / 'result.json').read_text())
            self.assertFalse(result['passed']); self.assertFalse(result['replication_acceptance'])

    def test_await_query_and_generation_receipts_do_not_overwrite(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']; node.generation = 2
            node.task_state = {'generation': 9}; writes = []
            node.atomic_write = lambda path, raw: writes.append((path, raw)); node.wait = lambda *args: None
            def receipt(*args):
                raw = writes[-1][1]
                return model.json_bytes({'version': 1, 'run_id': 'unit-test', 'sequence': 1, 'action': 'await_query',
                                         'status': 'complete', 'command_json': base64.b64encode(raw).decode(),
                                         'command_sha256': model.digest(raw), 'validation': 'socket_scope_and_structure_only',
                                         'clock': {'deployment_generation': 9, 'sample_clock_domain': 'simulation:unit-test'}})
            node.read = receipt
            node.command('await_query', expect='present', envelope_sha256='a' * 64)
            self.assertTrue((runner.out / 'probe/a/generation-2/000001.json').exists())
            self.assertEqual(node.command_counts, {2: 1})
            self.assertIn('-2-control/', writes[0][0])

    def test_atomic_receiver_uses_fresh_pinned_id_and_never_retries_failure(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['a']; node.runtime_identity = 'b' * 64
            runner.current_container_id = Mock(return_value='b' * 64)
            runner.call = Mock(side_effect=model.Refused('uncertain stdin'))
            with self.assertRaises(model.Refused): node.atomic_write('/private/000001.json', b'{}')
            runner.call.assert_called_once(); runner.current_container_id.assert_called_once_with(node.name, 'b' * 64)
            self.assertEqual(runner.call.call_args.args[0][3], 'b' * 64)

    def test_restart_background_launch_uses_pinned_id(self):
        with tempfile.TemporaryDirectory() as directory:
            runner = runner_fixture(directory); node = runner.nodes['b']; node.runtime_identity = 'b' * 64
            runner.current_container_id = Mock(return_value='b' * 64)
            runner.call = Mock(return_value=subprocess.CompletedProcess([], 0, b'', b''))
            node.inside = Mock(); node.wait = Mock(); node.check_world_startup = Mock()
            node.start_agent('restart')
            argv = runner.call.call_args.args[0]
            self.assertIn('b' * 64, argv); self.assertNotIn(node.name, argv)
            runner.current_container_id.assert_called_once_with(node.name, 'b' * 64)

    def test_volume_metadata_is_checked_and_saved_before_config_or_deployment(self):
        for unsafe in (False, True):
            with self.subTest(unsafe=unsafe), tempfile.TemporaryDirectory() as directory:
                runner = runner_fixture(directory); node = runner.nodes['a']
                node.check_world_startup = Mock()
                names = list(model.volume_names('unit-test', 'a', 1).values())
                metadata = {'version': 1, 'directories': [{'component': name, 'uid': 0,
                              'mode': 0o777 if unsafe and i == 1 else 0o700, 'created': True}
                              for i, name in enumerate(['wendy', 'volumes', *names])]}
                node.python = Mock(return_value=subprocess.CompletedProcess([], 0, model.json_bytes(metadata), b''))
                def stop(path, raw, *args):
                    self.assertTrue((runner.out / 'volume-directories-a.json').exists())
                    raise model.Refused('stop after metadata before normal deployment')
                node.write = Mock(side_effect=stop); node.cli = Mock()
                with self.assertRaises(model.Refused): runner.deploy_node(node)
                node.python.assert_called_once_with(run.control.VOLUME_DIRECTORIES, *names)
                node.cli.assert_not_called()
                self.assertEqual(node.write.call_count, 0 if unsafe else 1)
                self.assertEqual((runner.out / 'volume-directories-a.json').exists(), not unsafe)


if __name__ == '__main__':
    unittest.main()

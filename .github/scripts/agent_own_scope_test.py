"""Portable admission/ownership checks; these do not execute Agent or Docker."""
import contextlib
import importlib.util
import io
import json
import os
from pathlib import Path
import signal
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location('scope_driver', Path(__file__).with_name('test-agent-own-scope.py'))
DRIVER = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(DRIVER)


class StartupDriverTests(unittest.TestCase):
    def exercise(self, interrupt=False, foreign=False):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            agent = root / 'agent'
            agent.write_bytes(b'inert binary identity')
            output = root / 'output'
            containers, removed, starts = {}, [], []
            before = {sig: signal.getsignal(sig) for sig in (signal.SIGINT, signal.SIGTERM)}

            def command(argv, _timeout, *, cwd, env, stdout):
                if argv[:2] in (['go', 'test'], ['go', 'build']):
                    Path(argv[argv.index('-o') + 1]).write_bytes(b'inert built identity')
                    return 0
                if argv[:3] == ['docker', 'image', 'inspect']:
                    stdout.write('sha256:' + 'a' * 64 + '\n')
                    return 0
                if argv[:2] == ['docker', 'create']:
                    name = argv[argv.index('--name') + 1]
                    label = argv[argv.index('--label') + 1].split('=', 1)[1]
                    case = next(x.split('=', 1)[1] for x in argv if x.startswith('WENDY_SCOPE_CASE='))
                    containers[name] = (label, case, any('permissive-agent' in x for x in argv))
                    # Actual command must keep host services/filesystems private.
                    for pair in (['--network', 'none'], ['--cap-drop', 'ALL'], ['--security-opt', 'no-new-privileges']):
                        self.assertEqual(argv[argv.index(pair[0]):argv.index(pair[0]) + 2], pair)
                    self.assertNotIn('--privileged', argv)
                    self.assertTrue(all(x.endswith(',readonly') for x in argv if x.startswith('type=bind,')))
                    return 0
                if argv[:3] == ['docker', 'start', '--attach']:
                    label, case, control = containers[argv[-1]]
                    starts.append((case, control))
                    if control:
                        stdout.write('UNKNOWN_SCOPE_STARTUP_EXPOSED actual mTLS Agent RPC succeeded\n--- FAIL: TestAgentOwnScopeStartup (0.1s)\n')
                        return 1
                    stdout.write(f'AGENT_OWN_SCOPE_VERIFIED case={case} refused=true\n--- PASS: TestAgentOwnScopeStartup (0.1s)\n')
                    return 0
                if argv[:3] == ['docker', 'container', 'inspect']:
                    label = containers[argv[-1]][0]
                    stdout.write(json.dumps([{'Config': {'Labels': {'wendy.scope-fixture': 'foreign' if foreign else label}}}]))
                    return 0
                if argv[:3] == ['docker', 'rm', '--force']:
                    removed.append(argv[-1])
                    del containers[argv[-1]]
                    if interrupt:
                        os.kill(os.getpid(), signal.SIGTERM)
                    return 0
                self.fail(f'unexpected command {argv}')

            with patch.object(DRIVER.sys, 'platform', 'linux'), patch.object(DRIVER.OWNER, 'run', side_effect=command), patch.object(DRIVER.sys, 'argv', ['driver', '--agent', str(agent), '--output', str(output)]), contextlib.redirect_stdout(io.StringIO()):
                if interrupt:
                    with self.assertRaises(InterruptedError):
                        DRIVER.main()
                elif foreign:
                    with self.assertRaisesRegex(RuntimeError, 'unmatched container'):
                        DRIVER.main()
                else:
                    DRIVER.main()
            self.assertEqual(before, {sig: signal.getsignal(sig) for sig in before})
            evidence = json.loads((output / 'evidence.json').read_text())
            if foreign:
                self.assertEqual(removed, [])
                self.assertEqual(len(containers), 1)
                self.assertTrue(evidence['cleanup_errors'])
            else:
                self.assertEqual(containers, {})
                self.assertEqual(len(removed), 1 if interrupt else 7)
                self.assertEqual(len(starts), 1 if interrupt else 7)
                self.assertEqual(evidence['cleanup_errors'], [])

    def test_seven_cases_and_exact_owned_cleanup(self):
        self.exercise()

    def test_signal_during_cleanup_drains_and_cannot_pass(self):
        self.exercise(interrupt=True)

    def test_foreign_container_is_never_deleted(self):
        self.exercise(foreign=True)

    def test_non_linux_refuses_before_output(self):
        with tempfile.TemporaryDirectory() as temporary:
            output = Path(temporary) / 'never-created'
            args = type('Args', (), {'agent': Path('/bin/false'), 'output': output})()
            with patch.object(DRIVER.sys, 'platform', 'darwin'):
                with self.assertRaisesRegex(RuntimeError, 'existing Linux'):
                    DRIVER.execute(args, [])
            self.assertFalse(output.exists())


if __name__ == '__main__':
    unittest.main()

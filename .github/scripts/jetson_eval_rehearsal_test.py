"""Offline preparation and actual local HTTP timeout-report plumbing; no GPU/device."""
import importlib.util
import json
import os
from pathlib import Path
import shutil
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[2]
MANUAL = ROOT / '.github/regression-coverage/manual'
HARNESS = ROOT / 'evals/agent-experience'


def module(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    result = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(result)
    return result


PLAN = module('jetson_eval_plan', MANUAL / 'jetson-eval-rehearsal.py')
CONTROL = module('jetson_eval_control', MANUAL / 'jetson-eval-controls.py')


class JetsonEvalPlans(unittest.TestCase):
    def fixture(self, root):
        source = root / 'source'
        shutil.copytree(HARNESS, source / 'evals/agent-experience', ignore=shutil.ignore_patterns('__pycache__', '*.pyc', '.stage'))
        (source / '.github/scripts').mkdir(parents=True)
        shutil.copyfile(ROOT / '.github/scripts/run-owned-media-test.py', source / '.github/scripts/run-owned-media-test.py')
        skills = source / 'plugins/wendy-agentic-coding/skills'
        skills.mkdir(parents=True)
        (skills / 'fixture.txt').write_text('synthetic offline treatment')
        record = root / 'record.txt'
        record.write_text('synthetic offline record, not hardware evidence')
        digest = PLAN.artifact(record)['sha256']
        raw = {'trial': 'a' * 32, 'source_commit': 'b' * 40, 'source_root': str(source),
               'host_cli': str(record), 'staged_cli': str(record), 'host_cli_sha256': digest,
               'staged_cli_sha256': digest, 'agent_sha256': 'c' * 64,
               'reserved_disposable': True, 'empty_apps_and_volumes': True,
               'board': {'device_type': 'jetson-agx-thor', 'soc': 'tegra264',
                         **{key: 'synthetic' for key in ('serial', 'device_tree_serial', 'ecid', 'root_disk_serial',
                                                       'boot_id', 'owner', 'os_build', 'firmware')},
                         'boot_id': '00000000-0000-4000-8000-000000000001',
                         'address': '192.0.2.20:50051'},
               'records': {name: str(record) for name in ('identity', 'authorization', 'recovery',
                                                         'agent_build', 'cli_build', 'resource_budget')}}
        path = root / 'inventory.json'
        path.write_text(json.dumps(raw))
        return path, raw

    def test_generation_never_executes_and_hashes_all_outputs(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            inventory, raw = self.fixture(root)
            with patch.object(PLAN, 'source_identity') as source, patch.object(PLAN.subprocess, 'run') as run:
                evidence = PLAN.generate(inventory, root / 'plan')
            run.assert_not_called()
            source.assert_called_once_with(Path(raw['source_root']).resolve(), raw['source_commit'])
            self.assertEqual(evidence['hardware_execution'], 'not_run')
            self.assertTrue(all(case['status'] == 'not_run' for case in evidence['cases']))
            for item in evidence['sources'] + evidence['generated']:
                self.assertEqual(item, PLAN.artifact(item['path'], allow_empty=True))
            project = root / 'plan/project'
            for name in ('Dockerfile', 'on_device.py', 'on-device-init.sh', 'benchmark.py', 'eval_wendy.py'):
                self.assertEqual((project / name).read_bytes(), (HARNESS / name).read_bytes())
            cfg = json.loads((project / 'wendy.json').read_text())
            self.assertEqual(cfg['appId'], evidence['app_id'])
            self.assertTrue(all(raw['trial'] in name for name in evidence['volumes']))
            commands = json.loads((root / 'plan/commands.json').read_text())
            self.assertIn('EVAL_MODEL=qwen3-coder-30b-a3b', commands['deploy'])
            self.assertIn('EVAL_LIMIT=1', commands['deploy'])
            self.assertIn('--skip-cloud-registration', commands['deploy'])
            self.assertIn('WENDY_SECRET_STORE=file', commands['deploy'])
            self.assertIn('240s', commands['controls'])
            self.assertIn('600s', commands['verify'])
            self.assertEqual(commands['export'][-6:], ['tar', '-C', '/results', '-cf', '-', '.'])

    def test_refuses_wrong_board_unreserved_secrets_or_unbound_binary(self):
        for fault in ('board', 'reserved', 'apps', 'endpoint', 'hash', 'secret'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                inventory, raw = self.fixture(root)
                if fault == 'board': raw['board']['soc'] = 'tegra234'
                if fault == 'reserved': raw['reserved_disposable'] = False
                if fault == 'apps': raw['empty_apps_and_volumes'] = False
                if fault == 'endpoint': raw['board']['address'] = 'cloud://foreign'
                if fault == 'hash': raw['staged_cli_sha256'] = 'd' * 64
                if fault == 'secret': raw['token'] = 'must not be copied'
                inventory.write_text(json.dumps(raw))
                with patch.object(PLAN, 'source_identity'), self.assertRaises(ValueError):
                    PLAN.generate(inventory, root / 'plan')
                self.assertFalse((root / 'plan').exists())

    def test_source_commit_and_dirty_tree_refused_before_copy(self):
        from types import SimpleNamespace
        with patch.object(PLAN.subprocess, 'run', return_value=SimpleNamespace(stdout='c' * 40 + '\n')):
            with self.assertRaisesRegex(ValueError, 'differs'):
                PLAN.source_identity(Path('/unused'), 'b' * 40)
        with patch.object(PLAN.subprocess, 'run', side_effect=[SimpleNamespace(stdout='b' * 40 + '\n'),
                                                             SimpleNamespace(stdout=' M evals/agent-experience/on_device.py')]):
            with self.assertRaisesRegex(ValueError, 'clean'):
                PLAN.source_identity(Path('/unused'), 'b' * 40)

    def test_foreign_app_refuses_before_reading_device(self):
        with patch.dict(os.environ, {'WENDY_APP_ID': 'foreign'}), patch.object(CONTROL.Path, 'read_text') as read:
            with self.assertRaisesRegex(RuntimeError, 'foreign app'):
                CONTROL.admit({'app_id': 'owned'})
            read.assert_not_called()

    def test_actual_http_timeout_enters_production_trial_and_report(self):
        # This is a controlled process, not a Wendy/model/physical acceptance claim.
        # It proves the helper's real HTTP barrier and persisted report assertions.
        sys.path.insert(0, str(HARNESS))
        try:
            import benchmark as b
            import scenarios as sc
            import eval_wendy
            with tempfile.TemporaryDirectory() as temp:
                root = Path(temp)
                cli = root / 'controlled-cli'
                cli.write_text('#!/usr/bin/env python3\n# WENDY_CHAT_MCP_COMMAND\n'
                               'import json,sys,urllib.request\n'
                               'url=sys.argv[sys.argv.index("--base-url")+1]+"/chat/completions"\n'
                               'req=urllib.request.Request(url,data=json.dumps({"messages":[{"role":"user","content":"controlled request"}]}).encode(),headers={"Content-Type":"application/json"})\n'
                               'urllib.request.urlopen(req,timeout=20).read()\n')
                cli.chmod(0o755)
                tools = root / 'tools.json'; tools.write_text('[]')
                shim = eval_wendy.write_shim(root)
                with patch.object(CONTROL, 'HERE', HARNESS), patch.dict(os.environ, {
                        'HOME': str(root), 'WENDY_CONFIG_DIR': str(root / 'config'),
                        'WENDY_AGENT_SOCKET': str(root / 'absent.sock')}):
                    result = CONTROL.timeout_trial(b, sc, {}, root, str(cli), shim, tools, agent_seconds=2)
                self.assertTrue(result['real_cli_http_request_observed'])
                saved = json.loads(next(root.glob('chat-*/result.json')).read_text())
                self.assertEqual(saved['status'], 'timeout')
                self.assertEqual(json.loads((root / 'summary.json').read_text())[0]['unscored'], 1)
        finally:
            sys.path.remove(str(HARNESS))


if __name__ == '__main__':
    unittest.main()

"""Offline admission only: synthetic bundle bytes, no app signing or execution."""
import importlib.util
import json
from pathlib import Path
import plistlib
import tempfile
import unittest
from unittest.mock import patch
import zipfile

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT/'.github/regression-coverage/manual/mac-agent-lifecycle-rehearsal.py'
spec = importlib.util.spec_from_file_location('mac_lifecycle_plan', SCRIPT)
PLAN = importlib.util.module_from_spec(spec); spec.loader.exec_module(PLAN)


class MacLifecyclePlans(unittest.TestCase):
    def fixture(self, root):
        record = root/'synthetic-record'; record.write_text('offline synthetic record; no physical/signature evidence')
        raw = {'trial': 'a'*32, 'source_commit': 'b'*40, 'origin_source_commit': 'c'*40,
               'cli_binary': str(record), 'installed_bundle': '/Users/reserved_mac/Applications/WendyAgentMac.app',
               'endpoint': '127.0.0.1:50051', 'reserved_account_empty': True,
               'mac': {'serial': 'synthetic', 'hardware_uuid': 'synthetic-uuid', 'os_build': 'fixture-build',
                       'owner': 'fixture owner', 'account': 'reserved_mac', 'uid': 503},
               'records': {name: str(record) for name in ('identity', 'empty_account', 'authorization',
                           'signing', 'origin_build', 'candidate_build', 'recovery')}}
        for name in ('origin', 'candidate'):
            bundle = root/name/'WendyAgentMac.app'
            for directory in ('Contents/MacOS', 'Contents/Library/LaunchAgents', 'Contents/_CodeSignature'):
                (bundle/directory).mkdir(parents=True)
            (bundle/'Contents/Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier': PLAN.LABEL,
                'CFBundleExecutable': 'WendyAgentMac', 'WLWendyAgentVersion': 'same-version'}))
            (bundle/'Contents/MacOS/WendyAgentMac').write_bytes(name.encode())
            (bundle/'Contents/_CodeSignature/CodeResources').write_text('synthetic seal, not a signature')
            (bundle/'Contents/Library/LaunchAgents'/f'{PLAN.LABEL}.plist').write_bytes((ROOT/PLAN.SOURCES[2]).read_bytes())
            archive = root/(name+'.zip')
            self.archive(bundle, archive)
            raw[name+'_bundle'], raw[name+'_zip'] = str(bundle), str(archive)
        inventory = root/'inventory.json'; inventory.write_text(json.dumps(raw))
        return inventory, raw

    def archive(self, bundle, archive):
        with zipfile.ZipFile(archive, 'w') as out:
            for path in bundle.rglob('*'):
                if path.is_file(): out.write(path, bundle.name+'/'+str(path.relative_to(bundle)))

    def test_actual_commands_and_controls_stay_offline_not_run(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp); inventory, raw = self.fixture(root)
            with patch.object(PLAN.COMMON.subprocess, 'run') as run, patch.object(PLAN.COMMON.subprocess, 'check_output') as spawn:
                evidence = PLAN.generate(inventory, root/'plan')
            run.assert_not_called(); spawn.assert_not_called()
            self.assertEqual(evidence['hardware_execution'], 'not_run')
            self.assertEqual({case['status'] for case in evidence['cases']}, {'not_run'})
            self.assertNotEqual(evidence['candidate']['executable']['sha256'], evidence['origin']['executable']['sha256'])
            commands = json.loads((root/'plan/commands.json').read_text())
            self.assertEqual(commands['update'][-5:], ['--json', 'device', 'update', '--binary', str(Path(raw['candidate_zip']).resolve())])
            self.assertEqual(commands['crash-owned-agent'], ['/bin/launchctl', 'kill', 'SIGABRT', 'gui/503/'+PLAN.LABEL])
            self.assertEqual(commands['remove-heartbeat'][-2:], [evidence['app_id'], '--force'])
            self.assertIn('WENDY_SECRET_STORE=file', commands['update'])
            for item in evidence['sources']+evidence['generated']:
                self.assertEqual(PLAN.COMMON.artifact(item['path']), item)
            source = (root/'plan/controls/AppDelegate.swift').read_text()
            self.assertNotIn('await self?.performUpdateQuit()', source)
            self.assertEqual(source.count('await self?.performQuit()'), 1)
            control = plistlib.loads((root/'plan/controls'/f'{PLAN.LABEL}.plist').read_bytes())
            self.assertIs(control['KeepAlive'], False); self.assertIs(control['RunAtLoad'], True)
            self.assertIn('"$i" -lt 900', (root/'plan/heartbeat/launch.sh').read_text())
            guide = (root/'plan/procedure.txt').read_text()
            self.assertNotIn('@CANDIDATE@', guide)
            self.assertIn('Current device info does not expose binary_sha256', guide)
            self.assertIn('Until qualified control packages are built', guide)

    def test_rejects_nonreserved_or_foreign_target_and_secrets(self):
        for fault in ('existing', 'installed', 'uid', 'endpoint', 'source', 'secret'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp); inventory, raw=self.fixture(root)
                if fault=='existing': raw['reserved_account_empty']=False
                if fault=='installed': raw['installed_bundle']='/Applications/WendyAgentMac.app'
                if fault=='uid': raw['mac']['uid']=0
                if fault=='endpoint': raw['endpoint']='192.0.2.1:50051'
                if fault=='source': raw['source_commit']='main'
                if fault=='secret': raw['private_key']='never record'
                inventory.write_text(json.dumps(raw))
                with self.assertRaises(ValueError): PLAN.generate(inventory, root/'plan')
                self.assertFalse((root/'plan').exists())

    def test_rejects_zip_mismatch_identical_executable_and_missing_supervision(self):
        for fault in ('zip', 'same', 'plist'):
            with self.subTest(fault=fault), tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp); inventory, raw=self.fixture(root)
                bundle=Path(raw['candidate_bundle'])
                if fault in ('zip', 'same'):
                    (bundle/'Contents/MacOS/WendyAgentMac').write_bytes(b'changed' if fault=='zip' else b'origin')
                if fault=='plist':
                    path=bundle/'Contents/Library/LaunchAgents'/f'{PLAN.LABEL}.plist'
                    value=plistlib.loads(path.read_bytes()); value['KeepAlive']=False; path.write_bytes(plistlib.dumps(value))
                if fault!='zip': self.archive(bundle, Path(raw['candidate_zip']))
                with self.assertRaises(ValueError): PLAN.generate(inventory, root/'plan')
                self.assertFalse((root/'plan').exists())


if __name__ == '__main__': unittest.main()

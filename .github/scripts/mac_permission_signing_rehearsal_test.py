"""Offline synthetic artifact admission; never sign, install, request TCC or load an extension."""
import hashlib
import importlib.util
import json
from pathlib import Path
import plistlib
import shutil
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path);module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module);return module
PLAN=load('permission_plan',ROOT/'.github/regression-coverage/manual/mac-permission-signing-rehearsal.py')
OLD=load('permission_lifecycle_tests',Path(__file__).with_name('mac_agent_lifecycle_rehearsal_test.py'))

class PermissionPlans(unittest.TestCase):
    def fixture(self,root):
        helper=OLD.MacLifecyclePlans();inventory,data=helper.fixture(root)
        data.update(reserved_machine_empty=True,installed_bundle='/Applications/WendyAgentMac.app')
        record=root/'synthetic-record'
        for key in ('empty_machine','notarization','signer_der','extension_peer','reserved_cloud_profile'):data['records'][key]=str(record)
        app=Path(data['candidate_bundle']);info=plistlib.loads((app/'Contents/Info.plist').read_bytes());info.update(NSLocationWhenInUseUsageDescription='Synthetic usage',CFBundleVersion='1');(app/'Contents/Info.plist').write_bytes(plistlib.dumps(info))
        ext=app/'Contents/Library/SystemExtensions'/f'{PLAN.EXT}.systemextension/Contents';(ext/'MacOS').mkdir(parents=True)
        (ext/'Info.plist').write_bytes(plistlib.dumps({'CFBundleIdentifier':PLAN.EXT,'CFBundleExecutable':'NetProxy','CFBundleVersion':'1'}));(ext/'MacOS/NetProxy').write_bytes(b'synthetic extension')
        (ext/'embedded.provisionprofile').write_bytes(b'synthetic profile');(app/'Contents/embedded.provisionprofile').write_bytes(b'synthetic host profile')
        for kind,key in [('host','host_entitlements'),('extension','extension_entitlements')]:
            values={'com.apple.developer.networking.networkextension':['app-proxy-provider-systemextension','packet-tunnel-provider-systemextension']}
            if kind=='host':values.update({'com.apple.developer.system-extension.install':True,'com.apple.security.virtualization':True})
            path=root/(key+'.plist');path.write_bytes(plistlib.dumps(values));data['records'][key]=str(path)
        helper.archive(app,Path(data['candidate_zip']));inventory.write_text(json.dumps(data));return inventory,data

    def test_generation_preserves_disabled_release_and_pending_hardware(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);inventory,data=self.fixture(root)
            with patch.object(PLAN.COMMON.subprocess,'run') as run,patch.object(PLAN.COMMON.subprocess,'check_output') as native:
                evidence=PLAN.generate(inventory,root/'plan')
            run.assert_not_called();native.assert_not_called()
            self.assertEqual({case['status'] for case in evidence['cases']},{'not_run'})
            self.assertIsNone(evidence['validation_build'])
            self.assertIn('meshVPNIsEnabled = true',(root/'plan/validation-AppDelegate.swift').read_text())
            self.assertIn('meshVPNIsEnabled = false',(ROOT/'swift/WendyAgentMac/Sources/AppDelegate.swift').read_text())
            commands=json.loads((root/'plan/commands.json').read_text())
            self.assertNotIn('activation',commands)
            self.assertEqual(commands['release-validation'][-1],hashlib.sha1(Path(data['records']['signer_der']).read_bytes()).hexdigest().upper())
            self.assertIn('instrumented',(root/'plan/procedure.txt').read_text())

    def test_rejects_shared_host_missing_usage_and_wrong_extension_bytes(self):
        for fault in ('machine','account','path','usage','extension','zip','entitlement'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);inventory,data=self.fixture(root);app=Path(data['candidate_bundle'])
                if fault=='machine':data['reserved_machine_empty']=False
                elif fault=='account':data['reserved_account_empty']=False
                elif fault=='path':data['installed_bundle']='/Users/reserved_mac/Applications/WendyAgentMac.app'
                elif fault=='usage':
                    file=app/'Contents/Info.plist';info=plistlib.loads(file.read_bytes());info.pop('NSLocationWhenInUseUsageDescription');file.write_bytes(plistlib.dumps(info));OLD.MacLifecyclePlans().archive(app,Path(data['candidate_zip']))
                elif fault=='extension':
                    file=app/'Contents/Library/SystemExtensions'/f'{PLAN.EXT}.systemextension/Contents/Info.plist';info=plistlib.loads(file.read_bytes());info['CFBundleIdentifier']='foreign';file.write_bytes(plistlib.dumps(info));OLD.MacLifecyclePlans().archive(app,Path(data['candidate_zip']))
                elif fault=='zip':(app/'Contents/Library/SystemExtensions'/f'{PLAN.EXT}.systemextension/Contents/MacOS/NetProxy').write_bytes(b'changed after archive')
                elif fault=='entitlement':Path(data['records']['extension_entitlements']).write_bytes(plistlib.dumps({'com.apple.developer.networking.networkextension':['packet-tunnel-provider']}))
                inventory.write_text(json.dumps(data))
                with self.assertRaises((ValueError,PLAN.VALIDATOR.ValidationError)):PLAN.generate(inventory,root/'plan')
                self.assertFalse((root/'plan').exists())


    def test_optional_validation_artifact_binds_actual_overlay_and_new_versions(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);inventory,data=self.fixture(root)
            candidate=Path(data['candidate_bundle']);validation=root/'validation/WendyAgentMac.app'
            shutil.copytree(candidate,validation)
            (validation/'Contents/MacOS/WendyAgentMac').write_bytes(b'synthetic validation host')
            for relative in ('Contents/Info.plist','Contents/Library/SystemExtensions/'+PLAN.EXT+'.systemextension/Contents/Info.plist'):
                path=validation/relative;info=plistlib.loads(path.read_bytes());info['CFBundleVersion']='2';path.write_bytes(plistlib.dumps(info))
            archive=root/'validation.zip';OLD.MacLifecyclePlans().archive(validation,archive)
            source=(ROOT/'swift/WendyAgentMac/Sources/AppDelegate.swift').read_text().replace('private let meshVPNIsEnabled = false','private let meshVPNIsEnabled = true')
            build=root/'validation-build.json';build.write_text(json.dumps({'base_source_commit':data['source_commit'],'overlay_sha256':hashlib.sha256(source.encode()).hexdigest()}))
            data.update(validation_bundle=str(validation),validation_zip=str(archive));data['records']['validation_build']=str(build);inventory.write_text(json.dumps(data))
            evidence=PLAN.generate(inventory,root/'plan')
            self.assertNotEqual(evidence['candidate']['executable']['sha256'],evidence['validation_build']['executable']['sha256'])
            self.assertEqual({case['status'] for case in evidence['cases']},{'not_run'})
            self.assertIn('validation-release-check',json.loads((root/'plan/commands.json').read_text()))
            build.write_text(json.dumps({'base_source_commit':data['source_commit'],'overlay_sha256':'0'*64}))
            with self.assertRaisesRegex(ValueError,'overlay/base'):PLAN.generate(inventory,root/'wrong-plan')

if __name__=='__main__':unittest.main()

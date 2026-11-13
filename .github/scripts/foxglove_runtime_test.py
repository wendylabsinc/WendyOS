"""Offline evidence admission, never real bridge/ROS acceptance."""
import importlib.util
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('test-foxglove-runtime.py')
SPEC = importlib.util.spec_from_file_location('foxglove_runtime_gate', SCRIPT)
GATE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(GATE)
TEST = 'TestFoxgloveGeneratedAppDeploysAndCarriesROS '


class FoxgloveRuntimeAdmissionTests(unittest.TestCase):
    def evidence(self, root):
        (root/'image-digest.txt').write_text('sha256:'+'a'*64+'\n')
        (root/'installed-packages.txt').write_text('ros-humble-foxglove-bridge 3.0\nros-humble-rmw-cyclonedds-cpp 1.0\nsocat 1.7\n')
        (root/'Dockerfile').write_text('synthetic admission fixture, no image build\n')
        (root/'wendy.json').write_text('{}\n')

    def logs(self, name):
        if name == 'normal':
            return '--- PASS: '+TEST+'\nFOXGLOVE_RUNTIME_VERIFIED actual bytes\n'
        text = '--- FAIL: '+TEST+'\nFOXGLOVE_DEPLOYED_IMAGE actual digest\n'
        if name == 'named-service':
            return text+'FOXGLOVE_ROS_BYTES_VERIFIED actual bytes\nforwarding cancellation changed deployed bridge or left legacy services\n'
        return text+'Foxglove real deployment/readiness failed\nwaiting for foxglove_bridge WebSocket\nFOXGLOVE_EXEC_ERROR loading sh.wendy.foxglovebridge_foxglove: not found\n'

    def test_accepts_only_complete_normal_and_specific_control_evidence(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);self.evidence(root)
            for name in ('normal','named-service','named-forward'):
                GATE.validate_case(name,0 if name=='normal' else 1,self.logs(name),root)
            for name,code,text in (
                ('normal',0,self.logs('normal')+'--- SKIP: missing runtime\n'),
                ('normal',1,self.logs('normal')),
                ('normal',0,'--- PASS: '+TEST),
                ('named-service',1,self.logs('named-service').replace('FOXGLOVE_ROS_BYTES_VERIFIED','missing')),
                ('named-forward',1,self.logs('named-forward').replace('sh.wendy.foxglovebridge_foxglove','other')),
                ('named-forward',1,'--- FAIL: '+TEST+'\nFoxglove real deployment/readiness failed: build error'),
                ('named-forward',124,self.logs('named-forward')),
                ('named-forward',0,self.logs('named-forward')),
                ('unknown',1,self.logs('named-forward')),
            ):
                with self.subTest(case=name,code=code,text=text),self.assertRaises((ValueError,RuntimeError)):
                    GATE.validate_case(name,code,text,root)

    def test_rejects_missing_foreign_or_unresolved_artifact_provenance(self):
        for fault in ('missing','symlink','digest','package'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as temp:
                root=Path(temp);self.evidence(root)
                if fault=='missing':(root/'Dockerfile').unlink()
                elif fault=='symlink':
                    (root/'wendy.json').rename(root/'foreign.json');(root/'wendy.json').symlink_to(root/'foreign.json')
                elif fault=='digest':(root/'image-digest.txt').write_text('latest\n')
                else:(root/'installed-packages.txt').write_text('ros-humble-foxglove-bridge\n')
                with self.assertRaises(ValueError):GATE.validate_case('normal',0,self.logs('normal'),root)

    def test_non_hosted_entrypoint_refuses_before_creating_work(self):
        with tempfile.TemporaryDirectory() as temp:
            output=Path(temp)/'must-not-exist'
            result=subprocess.run([sys.executable,str(SCRIPT),'--output',str(output)],capture_output=True,text=True,timeout=5,env={**os.environ,'GITHUB_ACTIONS':'false','RUNNER_ENVIRONMENT':'local'})
            self.assertEqual(result.returncode,2,result.stderr)
            self.assertFalse(output.exists())
            self.assertIn('disposable GitHub-hosted Linux runner',result.stderr)


if __name__ == '__main__':
    unittest.main()

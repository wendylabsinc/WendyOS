"""Offline evidence admission, never real bridge/ROS acceptance."""
import hashlib
import importlib.util
import json
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
CLIENT_SPEC = importlib.util.spec_from_file_location('foxglove_docker_client', SCRIPT.with_name('foxglove-docker-client.py'))
CLIENT = importlib.util.module_from_spec(CLIENT_SPEC)
CLIENT_SPEC.loader.exec_module(CLIENT)


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


class FoxgloveDockerClientTests(unittest.TestCase):
    def fixture(self, root):
        namespace=root/'pinned-net';namespace.touch()
        docker=root/'docker-real'
        docker.write_text('#!'+sys.executable+'\nimport json,os,sys\nprint(json.dumps({"argv":sys.argv[1:],"home":os.environ["HOME"]}))\n')
        nsenter=root/'nsenter'
        nsenter.write_text('#!'+sys.executable+'\nimport os,sys\nassert sys.argv[1].startswith("--net=") and sys.argv[2]=="--"\nos.execv(sys.argv[3],sys.argv[3:])\n')
        for path in (docker,nsenter):path.chmod(0o700)
        environment={
            'WENDY_FOXGLOVE_PARENT_net':f'net:[{namespace.stat().st_ino}]',
            'WENDY_FOXGLOVE_PARENT_mnt':'mnt:[100]',
            'WENDY_FOXGLOVE_PARENT_pid':'pid:[200]',
            'WENDY_FOXGLOVE_HOST_NET':str(namespace),
            'WENDY_FOXGLOVE_REAL_DOCKER':str(docker),
            'WENDY_FOXGLOVE_DOCKER_SHA256':hashlib.sha256(docker.read_bytes()).hexdigest(),
            'WENDY_FOXGLOVE_NSENTER':str(nsenter),
            'WENDY_FOXGLOVE_NSENTER_SHA256':hashlib.sha256(nsenter.read_bytes()).hexdigest(),
        }
        identity=lambda path:Path(path).name+':[999]'
        return environment,identity

    def test_pinned_command_preserves_literal_argv_and_private_home(self):
        # Fake executables test the actual child argv/env, never setns or Docker.
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();env,identity=self.fixture(root)
            arguments=['buildx','build','--output','type=oci,dest=/tmp/a b','$(touch must-not-exist)','a;echo wrong']
            command=CLIENT.command(arguments,env,identity)
            self.assertEqual(command[:4],[env['WENDY_FOXGLOVE_NSENTER'],'--net='+env['WENDY_FOXGLOVE_HOST_NET'],'--',env['WENDY_FOXGLOVE_REAL_DOCKER']])
            result=subprocess.run(command,capture_output=True,text=True,timeout=5,check=True,env={**os.environ,'HOME':str(root/'private-home')})
            self.assertEqual(json.loads(result.stdout),{'argv':arguments,'home':str(root/'private-home')})

    def test_rejects_changed_executable_or_namespace_before_launch(self):
        for fault in ('docker-bytes','nsenter-bytes','docker-symlink','namespace-symlink','namespace-inode','relative-path','digest'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as temp:
                root=Path(temp).resolve();env,identity=self.fixture(root)
                if fault.endswith('-bytes'):
                    key='WENDY_FOXGLOVE_REAL_DOCKER' if fault=='docker-bytes' else 'WENDY_FOXGLOVE_NSENTER'
                    Path(env[key]).write_text('changed')
                elif fault.endswith('-symlink'):
                    key='WENDY_FOXGLOVE_REAL_DOCKER' if fault=='docker-symlink' else 'WENDY_FOXGLOVE_HOST_NET'
                    path=Path(env[key]);other=path.with_suffix('.other');path.rename(other);path.symlink_to(other)
                elif fault=='namespace-inode':env['WENDY_FOXGLOVE_PARENT_net']='net:[1]'
                elif fault=='relative-path':env['WENDY_FOXGLOVE_HOST_NET']='relative'
                else:env['WENDY_FOXGLOVE_NSENTER_SHA256']='a'*64
                with self.assertRaises(ValueError):CLIENT.command(['version'],env,identity)

    def test_rejects_call_outside_private_namespaces(self):
        with tempfile.TemporaryDirectory() as temp:
            env,identity=self.fixture(Path(temp).resolve())
            for kind in ('net','mnt','pid'):
                with self.subTest(kind=kind),self.assertRaises(ValueError):
                    CLIENT.command(['version'],env,lambda path:env['WENDY_FOXGLOVE_PARENT_'+kind] if path.endswith('/'+kind) else identity(path))
            for kind in ('net','mnt','pid'):
                with self.subTest(malformed=kind),self.assertRaises(ValueError):
                    CLIENT.command(['version'],{**env,'WENDY_FOXGLOVE_PARENT_'+kind:'unknown'},identity)

    def test_helper_refuses_non_hosted_execution_without_executing_docker(self):
        result=subprocess.run([sys.executable,str(SCRIPT.with_name('foxglove-docker-client.py')),'version'],capture_output=True,text=True,timeout=5,env={**os.environ,'GITHUB_ACTIONS':'false'})
        self.assertNotEqual(result.returncode,0)
        self.assertIn('requires the owned hosted fixture',result.stderr)


if __name__ == '__main__':
    unittest.main()

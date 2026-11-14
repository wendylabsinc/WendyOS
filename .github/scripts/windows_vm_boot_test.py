"""Portable checks for the hosted Windows VM gate; no emulator or install runs."""
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

SCRIPT=Path(__file__).with_name('test-windows-vm-boot.py')
spec=importlib.util.spec_from_file_location('windows_vm_boot',SCRIPT)
VM=importlib.util.module_from_spec(spec);spec.loader.exec_module(VM)


class WindowsVMBootTests(unittest.TestCase):
    def test_missing_tool_control_requires_exact_structured_launch_error(self):
        import json
        error={'code':'tool_not_found','exit':6,'retryable':False,'message':'starting qemu-system-aarch64: exec: "qemu-system-aarch64": executable file not found in %PATH%','next_steps':[]}
        VM.validate_path_only_control(6,json.dumps({'error':error}))
        for code,record in ((1,error),(124,error),(6,{**error,'code':'config_invalid'}),(6,{**error,'exit':1}),
                            (6,{**error,'retryable':True}),(6,{**error,'message':'starting another-tool: executable file not found in %PATH%'}),
                            (6,{**error,'message':'starting qemu-system-aarch64: firmware is missing'})):
            with self.subTest(code=code,record=record),self.assertRaises(ValueError):
                VM.validate_path_only_control(code,json.dumps({'error':record}))
        for malformed in ('not JSON','{}','{"error":null}','{"error":[]}'):
            with self.assertRaises(ValueError):VM.validate_path_only_control(6,malformed)

    def test_actual_admitted_subprocess_runs_only_after_job_assignment(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);receipt=root/'receipt';events=[]
            class Job:
                def assign(self,child):
                    if receipt.exists() or list(root.glob('*.permit')):raise AssertionError('child escaped assignment barrier')
                    events.append('assigned')
                def drain(self):events.append('drained')
                def close(self):events.append('closed')
            owner=VM.Owned(root,Job(),lambda:None)
            try:
                command=[sys.executable,'-c',f'from pathlib import Path; Path({str(receipt)!r}).write_text("actual owned process")']
                owner.run('child',command,timeout=10)
                self.assertEqual(receipt.read_text(),'actual owned process')
            finally:owner.close()
            self.assertEqual(events,['assigned','drained','closed'])

    def test_assignment_failure_or_cancellation_never_releases_child(self):
        for fault in ('assignment','cancellation','drain'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);events=[];cancelled=False
                class Child:
                    stdin=None
                    def kill(self):events.append('kill')
                    def wait(self,timeout):events.append('wait');return 0
                class Job:
                    def assign(self,child):
                        nonlocal cancelled
                        if fault=='assignment':raise RuntimeError('assignment failed')
                        cancelled=True
                    def drain(self):
                        events.append('drain')
                        if fault=='drain':raise RuntimeError('unproven drain')
                    def close(self):events.append('close')
                def check():
                    if cancelled:raise KeyboardInterrupt()
                owner=VM.Owned(root,Job(),check,lambda *a,**k:Child())
                with self.assertRaises(RuntimeError if fault=='assignment' else KeyboardInterrupt):owner.start('child',['inert'])
                self.assertFalse(list(root.glob('*.permit')))
                if fault=='drain':
                    with self.assertRaisesRegex(RuntimeError,'unproven drain'):owner.close()
                else:owner.close()
                self.assertLess(events.index('drain'),events.index('close'))
                if fault=='assignment':self.assertEqual(events[:2],['kill','wait'])

    def test_download_requires_exact_body_and_hash_without_redirect_or_proxy(self):
        for body in (b'exact payload',b'short',b'exact payload EXTRA',b'wrong payload'):
            with self.subTest(body=body),tempfile.TemporaryDirectory() as tmp:
                target=Path(tmp)/'artifact'
                class Response:
                    status=200
                    def __init__(self):self.pending=body
                    def __enter__(self):return self
                    def __exit__(self,*a):pass
                    def read(self,n):value=self.pending;self.pending=b'';return value
                class Opener:
                    def open(self,url,timeout):return Response()
                pin={'url':'https://fixture.invalid/owned','size':13,'sha256':hashlib.sha256(b'exact payload').hexdigest()}
                with patch.dict(VM.PINS,fixture=pin),patch.object(VM.urllib.request,'build_opener',return_value=Opener()) as build:
                    if body==b'exact payload':VM.download('fixture',target)
                    else:
                        with self.assertRaises(ValueError):VM.download('fixture',target)
                    handlers=build.call_args.args
                    self.assertEqual(handlers[0].proxies,{})
                    self.assertIsInstance(handlers[1],VM.NoRedirect)
                if body==b'exact payload':self.assertEqual(target.read_bytes(),body)
        with self.assertRaises(ValueError):VM.NoRedirect().redirect_request(None,None,302,'',{},'https://foreign.invalid/')

    def test_collision_and_restart_require_saved_bounded_port(self):
        VM.validate_lifecycle_port('collision',50053,{'agentPort':50053},None)
        VM.validate_lifecycle_port('restart',50053,{'agentPort':50053},{'agentPort':50053})
        for phase,port,meta,first in (
            ('collision',50051,{'agentPort':50051},None),
            ('collision',50054,{'agentPort':50054},None),
            ('collision',50067,{'agentPort':50067},None),
            ('collision',50053,{'agentPort':50051},None),
            ('restart',50051,{'agentPort':50051},{'agentPort':50053}),
            ('restart',50053,{'agentPort':50053},None),
            ('unknown',50053,{'agentPort':50053},None),
        ):
            with self.subTest(phase=phase,port=port),self.assertRaises(ValueError):
                VM.validate_lifecycle_port(phase,port,meta,first)

    def test_guest_and_listener_cannot_accept_an_unrelated_host_endpoint(self):
        guest={'cpuArchitecture':'arm64','deviceType':'vm-arm64','os':'wendyos','osVersion':'WendyOS-0.21.0','version':'2026.11.12-135023'}
        VM.validate_guest(guest)
        for key in guest:
            with self.subTest(key=key),self.assertRaises(ValueError):VM.validate_guest(dict(guest,**{key:''}))
        qemu=Path('/owned/Private Program Files/qemu/qemu-system-aarch64.exe')
        state={'pid':122,'agentPort':12345,'accel':'tcg','netMode':'user'}
        listener={'pid':122,'port':12345,'path':str(qemu)}
        VM.validate_listener(listener,state,qemu,12345)
        for key,value in (('pid',999),('port',54321),('path','/foreign/qemu.exe')):
            with self.subTest(key=key),self.assertRaises(ValueError):VM.validate_listener(dict(listener,**{key:value}),state,qemu,12345)

    def test_identity_uses_actual_rpc_and_must_match_the_owned_cli_guest(self):
        # Fields match actual collision-info-38.log from job131729254510.
        # The public CLI hardware JSON does not contain hostname/publicKey.
        guest={'cpuArchitecture':'arm64','deviceType':'vm-arm64','os':'wendyos',
               'osVersion':'WendyOS-0.21.0','version':'2026.11.12-135023'}
        VM.validate_guest(guest)
        nonce='0123456789abcdef0123456789abcdef'
        identity=dict(guest,address='127.0.0.1:50053',nonce=nonce,hostname='owned-vm',bootId='11111111-2222-4333-8444-555555555555',bootTimeNs=1000000000)
        VM.validate_identity(identity,guest,50053,nonce)
        for key,value in (('address','127.0.0.1:50051'),('nonce','other'),('hostname',''),('bootId',''),('bootTimeNs',0),('bootTimeNs',True),
                          ('version','other'),('cpuArchitecture','amd64'),('deviceType','foreign'),
                          ('os','linux'),('osVersion','WendyOS-0.21.1')):
            with self.subTest(key=key),self.assertRaises(ValueError):
                VM.validate_identity(dict(identity,**{key:value}),guest,50053,nonce)
        for value in ('0.21.0','0.21.1','WendyOS-0.21.0-extra'):
            with self.subTest(version=value),self.assertRaises(ValueError):
                VM.validate_guest(dict(guest,osVersion=value))

    def test_restart_requires_same_disk_hostname_and_a_new_kernel_boot(self):
        disk={'path':'owned/disk.img','device':1,'inode':17,'size':16*1024**3}
        before={'hostname':'owned-vm','bootId':'11111111-2222-4333-8444-555555555555'}
        after=dict(before,bootId='22222222-2222-4333-8444-555555555555')
        initial={'startedAt':'first'};restarted={'startedAt':'second'}
        VM.validate_restart(restarted,after,disk,initial,before,disk)
        for state,identity,current_disk in (
            (initial,after,disk),({},after,disk),(restarted,before,disk),
            (restarted,dict(after,hostname='foreign-vm'),disk),
            (restarted,after,dict(disk,inode=18)),
            (restarted,after,dict(disk,path='foreign/disk.img')),
            (restarted,after,dict(disk,size=8*1024**3)),
        ):
            with self.subTest(state=state,identity=identity,disk=current_disk),self.assertRaises(ValueError):
                VM.validate_restart(state,identity,current_disk,initial,before,disk)

    def test_private_environment_removes_case_variants_and_keeps_host_tools_unchanged(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            with patch.dict(os.environ,{'PROGRAMFILES':'foreign','ProgramFiles':'other','ProgramW6432':'foreign64','ProgramFiles(x86)':'foreign32','SystemRoot':str(root/'system'),'PATH':'foreign','Path':'other','WENDY_CONFIG_DIR':'user','Wendy_Token':'private'},clear=True):
                env=VM.private_environment(root,root/'private')
                self.assertEqual([key for key in env if key.upper()=='PROGRAMFILES'],['PROGRAMFILES'])
                self.assertEqual([key for key in env if key.upper()=='PATH'],['PATH'])
                for name in ('PROGRAMFILES','PROGRAMFILES(X86)','PROGRAMW6432'):
                    self.assertEqual([key for key in env if key.upper()==name],[name])
                    self.assertEqual(env[name],str(root/'private'))
                self.assertNotIn('Wendy_Token',env)
                self.assertEqual(env['WENDY_CONFIG_DIR'],str(root/'config'))
                self.assertEqual(os.environ['WENDY_CONFIG_DIR'],'user')

    def test_original_launch_control_preserves_source_and_has_one_changed_site(self):
        source=VM.ROOT/'go/internal/cli/vm/spec.go';before=source.read_bytes()
        with tempfile.TemporaryDirectory() as tmp:
            overlay=VM.mutant_overlay(Path(tmp));mapping=json.loads(overlay.read_text())['Replace']
            self.assertEqual(list(mapping),[str(source)])
            changed=Path(mapping[str(source)]).read_text()
            self.assertNotIn('if path, err := FindQEMU(runtime.GOOS); err == nil {',changed)
            self.assertIn('return "qemu-system-aarch64"',changed)
            self.assertEqual(source.read_bytes(),before)

if __name__=='__main__':unittest.main()

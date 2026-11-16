#!/usr/bin/env python3
"""Portable admission/log tests and actual private flock/QMP cleanup behavior."""
import importlib.util
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import unittest

SPEC=importlib.util.spec_from_file_location('vm_lifecycle',Path(__file__).with_name('test-macos-vm-lifecycle.py'))
M=importlib.util.module_from_spec(SPEC);SPEC.loader.exec_module(M)


class LifecycleTests(unittest.TestCase):
    def test_hosted_admission_does_not_spoof_acceleration(self):
        host={'system':'Darwin','uid':501,'machine':'x86_64'}
        self.assertEqual(M.admit_host(host,env={'GITHUB_ACTIONS':'true','RUNNER_ENVIRONMENT':'github-hosted'}),'hosted_tcg')
        for env in ({},{'GITHUB_ACTIONS':'true'}):
            with self.assertRaises(ValueError):M.admit_host(host,env=env)
        with self.assertRaises(ValueError):M.admit_host({**host,'machine':'arm64'},env={'GITHUB_ACTIONS':'true','RUNNER_ENVIRONMENT':'github-hosted'})
        with self.assertRaises(ValueError):M.admit_host({**host,'uid':0},env={'GITHUB_ACTIONS':'true','RUNNER_ENVIRONMENT':'github-hosted'})

    def test_reserved_host_requires_exact_boot_and_owner(self):
        identity={'system':'Darwin','uid':501,'machine':'arm64','platform_uuid':'physical','boot':'reserved-boot','hostname':'reserved'}
        reservation={'identity':identity,'owner_confirmation':'I reserve this bare-metal Mac for the owned WendyOS VM lifecycle','hardware_execution':'not_run'}
        self.assertEqual(M.admit_host(identity,reservation,env={}), 'reserved_hvf')
        with self.assertRaises(ValueError):M.admit_host({**identity,'boot':'new-boot'},reservation,env={})
        with self.assertRaises(ValueError):M.admit_host(identity,{**reservation,'owner_confirmation':''},env={})

    def test_control_requires_actual_forwarding_failure_and_drain(self):
        normal='VM_CANCEL_RECOVERY_VERIFIED VM_APP_FORWARD_VERIFIED VM_RESTART_FORWARD_VERIFIED VM_LIFECYCLE_VERIFIED\n--- PASS: TestVMOwnedLifecycle (2s)\nVM_OWNED_DRAINED'
        M.validate_log(normal,0)
        control='VM_GUEST_APP_STARTED VM_CANCEL_RECOVERY_VERIFIED\nVM_APP_FORWARD: actual guest application never became reachable: connection refused\n--- FAIL: TestVMOwnedLifecycle (2s)\nVM_OWNED_DRAINED'
        M.validate_log(control,1,True)
        for text,code in ((control,124),(control.replace('VM_OWNED_DRAINED','VM_DRAIN_UNKNOWN'),1),(normal,0),(control+'\npanic: broken',1)):
            with self.assertRaises(ValueError):M.validate_log(text,code,True)

    def test_actual_lock_prevents_cleanup_until_qmp_quit(self):
        with tempfile.TemporaryDirectory(prefix='vmt-',dir='/tmp') as directory:
            config=Path(directory);nonce='a'*32;name='vm-'+nonce[:16];vm=config/'vms'/name;vm.mkdir(parents=True)
            (config/'fixture-owner').write_text(nonce);(vm/'state.json').write_text('{}')
            child=subprocess.Popen([sys.executable,'-c',
                'import fcntl,sys; f=open(sys.argv[1],"w"); fcntl.flock(f,fcntl.LOCK_EX); print("locked",flush=True); sys.stdin.readline()',str(vm/'run.lock')],stdin=subprocess.PIPE,stdout=subprocess.PIPE,text=True)
            self.assertEqual(child.stdout.readline().strip(),'locked')
            self.assertFalse(M.drain(config,name,nonce,timeout=.05))
            server=socket.socket(socket.AF_UNIX);server.bind(str(vm/'qmp.sock'));server.listen(1);server.settimeout(3)
            seen=[];errors=[]
            def peer():
                try:
                    conn,_=server.accept()
                    with conn,conn.makefile('rb') as reader:
                        conn.sendall(b'{"QMP":{}}\n')
                        for expected in ('qmp_capabilities','quit'):
                            seen.append(json.loads(reader.readline())['execute']);self.assertEqual(seen[-1],expected);conn.sendall(b'{"return":{}}\n')
                        child.stdin.write('\n');child.stdin.flush();child.wait(timeout=3)
                except BaseException as error:errors.append(error)
            thread=threading.Thread(target=peer);thread.start()
            try:
                self.assertTrue(M.drain(config,name,nonce,timeout=3));thread.join(3)
                self.assertFalse(thread.is_alive());self.assertEqual(errors,[]);self.assertEqual(seen,['qmp_capabilities','quit'])
            finally:
                server.close()
                if child.poll() is None:child.kill();child.wait(timeout=3)
                child.stdin.close();child.stdout.close();thread.join(3)

    def test_qmp_async_event_flood_is_bounded(self):
        with tempfile.TemporaryDirectory(prefix='vmt-',dir='/tmp') as directory:
            path=Path(directory)/'qmp.sock';server=socket.socket(socket.AF_UNIX);server.bind(str(path));server.listen(1);server.settimeout(2)
            def peer():
                try:
                    conn,_=server.accept()
                    with conn:
                        conn.settimeout(2)
                        conn.sendall(b'{"QMP":{}}\n')
                        conn.recv(1024)
                        conn.sendall(b'{"event":"noise"}\n'*5000)
                except OSError:pass
            thread=threading.Thread(target=peer);thread.start()
            try:
                with self.assertRaisesRegex(ValueError,'oversized'):M.qmp(path,'quit')
            finally:server.close();thread.join(3)
            self.assertFalse(thread.is_alive())

    def test_cleanup_rejects_wrong_nonce_and_symlink(self):
        with tempfile.TemporaryDirectory(prefix='vmt-') as directory:
            root=Path(directory);(root/'fixture-owner').write_text('a'*32)
            with self.assertRaises(ValueError):M.drain(root,'vm-owned','b'*32)
            (root/'vms').mkdir();(root/'vms/vm-owned').symlink_to(root,target_is_directory=True)
            with self.assertRaises(ValueError):M.drain(root,'vm-owned','a'*32)

if __name__=='__main__':unittest.main()

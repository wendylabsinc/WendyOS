#!/usr/bin/env python3
"""Injected installs in temporary directories; never run apt, mount or host policy."""
import importlib.util
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time
import unittest
from unittest import mock

SCRIPT=Path(__file__).with_name('install-usb-profile-dependencies.py')
spec=importlib.util.spec_from_file_location('usb_deps',SCRIPT)
MODULE=importlib.util.module_from_spec(spec);spec.loader.exec_module(MODULE)

class USBDependencyPolicyTests(unittest.TestCase):
    def test_download_precedes_policy_and_isolated_install(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);policy=root/'policy';events=[]
            def download(log):
                self.assertFalse(policy.exists())
                events.append('download')
                log.write('archives fetched with the resolver still available\n')
                return 0
            def install(plan,log):
                self.assertEqual(events,['download'])
                self.assertEqual(policy.read_bytes(),MODULE.POLICY)
                events.append('install')
                return 0,True
            MODULE.provision(policy,root/'result',install,work_parent=root,download=download)
            self.assertEqual(events,['download','install'])
            self.assertFalse(policy.exists())
            result=json.loads((root/'result/result.json').read_text())
            self.assertEqual(result['download_exit_code'],0)
            self.assertEqual(result['status'],'passed')

    def test_download_failure_never_prepares_policy_or_starts_installer(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);policy=root/'policy';install=mock.Mock()
            with self.assertRaisesRegex(RuntimeError,'dependency download failed: 100'):
                MODULE.provision(policy,root/'result',install,work_parent=root,download=lambda _:100)
            install.assert_not_called()
            self.assertFalse(policy.exists())
            result=json.loads((root/'result/result.json').read_text())
            self.assertEqual(result['download_exit_code'],100)
            self.assertFalse(result['cleanup_errors'])

    def test_existing_regular_and_symlink_policy_are_never_replaced(self):
        for symlink in (False,True):
            with self.subTest(symlink=symlink),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);policy=root/'policy';target=root/'original'
                target.write_bytes(b'#!/bin/sh\nexit 0\n');target.chmod(0o751)
                if symlink:policy.symlink_to(target)
                else:target.rename(policy)
                before=MODULE.snapshot(policy)
                def install(plan,log):
                    data=json.loads(plan.read_text())
                    self.assertEqual(MODULE.snapshot(policy),before)
                    self.assertEqual(Path(data['stub']).read_bytes(),MODULE.POLICY)
                    self.assertEqual(Path(data['policy']),policy.resolve())
                    return 0,True
                MODULE.provision(policy,root/'result',install,work_parent=root)
                self.assertEqual(MODULE.snapshot(policy),before)
                self.assertEqual(json.loads((root/'result/result.json').read_text())['status'],'passed')

    def test_absent_policy_is_owned_temporarily_and_removed_on_success_or_failure(self):
        for status in (0,17):
            with self.subTest(status=status),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);policy=root/'policy'
                def install(plan,log):
                    self.assertEqual(policy.read_bytes(),MODULE.POLICY)
                    return status,True
                if status:
                    with self.assertRaisesRegex(RuntimeError,'installation failed'):
                        MODULE.provision(policy,root/'result',install,work_parent=root)
                else:MODULE.provision(policy,root/'result',install,work_parent=root)
                self.assertFalse(policy.exists())
                result=json.loads((root/'result/result.json').read_text())
                self.assertEqual(result['install_exit_code'],status)
                self.assertFalse(result['cleanup_errors'])

    def test_foreign_replacement_or_modified_policy_is_never_deleted(self):
        for replace in (False,True):
            with self.subTest(replace=replace),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);policy=root/'policy'
                def install(plan,log):
                    if replace:policy.rename(root/'owned-old-policy')
                    policy.write_bytes(b'unrelated replacement policy')
                    return 17,True
                with self.assertRaisesRegex(RuntimeError,'refusing to remove unrelated state'):
                    MODULE.provision(policy,root/'result',install,work_parent=root)
                self.assertEqual(policy.read_bytes(),b'unrelated replacement policy')
                self.assertTrue(json.loads((root/'result/result.json').read_text())['cleanup_errors'])

    def test_existing_policy_is_preserved_on_injected_install_error(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);policy=root/'policy';policy.write_bytes(b'original');before=MODULE.snapshot(policy)
            with self.assertRaisesRegex(OSError,'injected'):
                MODULE.provision(policy,root/'result',lambda *_:(_ for _ in ()).throw(OSError('injected')),work_parent=root)
            self.assertEqual(MODULE.snapshot(policy),before)

    def test_actual_signal_cleans_only_temporary_policy(self):
        for existing in (False,True):
            with self.subTest(existing=existing),tempfile.TemporaryDirectory() as directory:
                root=Path(directory);policy=root/'policy';ready=root/'ready'
                if existing:policy.write_bytes(b'original')
                before=MODULE.snapshot(policy) if existing else None
                driver=root/'driver.py'
                driver.write_text(f'''import importlib.util,signal,time
from pathlib import Path
spec=importlib.util.spec_from_file_location('fixture',{str(SCRIPT)!r});m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
def stop(*_):raise InterruptedError('actual TERM')
signal.signal(signal.SIGTERM,stop)
def install(plan,log):
 Path({str(ready)!r}).write_text('ready')
 time.sleep(5)
m.provision(Path({str(policy)!r}),Path({str(root/'result')!r}),install,work_parent={str(root)!r})
''')
                child=subprocess.Popen([sys.executable,str(driver)],stdout=subprocess.PIPE,stderr=subprocess.PIPE,text=True)
                try:
                    until=time.monotonic()+3
                    while not ready.exists():
                        if time.monotonic()>until:self.fail('injected installer did not start')
                        time.sleep(.01)
                    child.terminate()
                    _,err=child.communicate(timeout=3)
                    self.assertNotEqual(child.returncode,0,err)
                finally:
                    if child.poll() is None:child.kill();child.wait(timeout=2)
                if existing:self.assertEqual(MODULE.snapshot(policy),before)
                else:self.assertEqual(policy.read_bytes(),MODULE.POLICY)
                cleanup=json.loads((root/'result/result.json').read_text())['cleanup_errors']
                self.assertEqual(bool(cleanup),not existing)
                if cleanup:self.assertIn('namespace drain unproven',cleanup[0])

    def test_unproven_timeout_retains_policy_even_after_group_signals(self):
        spec=importlib.util.spec_from_file_location('owned_dependency_test',SCRIPT.with_name('run-owned-media-test.py'))
        owned=importlib.util.module_from_spec(spec);spec.loader.exec_module(owned)
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);policy=root/'policy';ticks=root/'ticks';child=root/'installer.py'
            child.write_text("import signal,time\n"
                             "signal.signal(signal.SIGTERM,signal.SIG_IGN)\n"
                             f"with open({str(ticks)!r},'w',buffering=1) as log:\n"
                             " end=time.monotonic()+8\n"
                             " while time.monotonic()<end:\n"
                             "  log.write('tick\\n');time.sleep(.03)\n")
            def install(plan,log):
                status=owned.run([sys.executable,str(child)],.2,stdout=log)
                self.assertTrue(policy.exists(),'policy removed before installer ownership drained')
                return status,False
            with self.assertRaisesRegex(RuntimeError,'namespace drain unproven'):
                MODULE.provision(policy,root/'result',install,work_parent=root)
            self.assertEqual(policy.read_bytes(),MODULE.POLICY)
            observed=ticks.read_bytes();time.sleep(.1)
            self.assertEqual(ticks.read_bytes(),observed,'installer survived policy cleanup')

    def test_inside_rejects_parent_namespace_without_mounting(self):
        with tempfile.TemporaryDirectory() as directory:
            plan=Path(directory)/'plan.json';plan.write_text(json.dumps({'parent_mnt':'same','parent_pid':'same'}))
            with mock.patch.object(MODULE.os,'readlink',return_value='same'),mock.patch.object(MODULE.subprocess,'run') as run:
                with self.assertRaisesRegex(RuntimeError,'private mnt'):MODULE.inside(plan)
                run.assert_not_called()

    def test_local_invocation_refuses_before_installation(self):
        result=subprocess.run([sys.executable,str(SCRIPT),'/not-created'],env=dict(os.environ,GITHUB_ACTIONS='',RUNNER_ENVIRONMENT=''),capture_output=True,text=True,timeout=3)
        self.assertEqual(result.returncode,2)
        self.assertIn('disposable hosted Linux runner',result.stderr)

if __name__=='__main__':unittest.main()

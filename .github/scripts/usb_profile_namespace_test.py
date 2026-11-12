#!/usr/bin/env python3
"""Offline namespace admission; never starts NetworkManager or changes links."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT=Path(__file__).with_name('test-usb-profile-namespace.sh')

class USBProfileAdmissionTests(unittest.TestCase):
    def test_nonhosted_execution_refuses_every_entry(self):
        for args in (['out'],['--namespace','unused','out'],['--inside','unused','out']):
            with self.subTest(args=args):
                result=subprocess.run(['bash',str(SCRIPT),*args],env=dict(os.environ,GITHUB_ACTIONS='',RUNNER_ENVIRONMENT=''),capture_output=True,text=True,timeout=3)
                self.assertEqual(result.returncode,2)
                self.assertIn('disposable GitHub-hosted Linux runner',result.stderr)

    def test_shared_namespace_refuses_before_host_mutations(self):
        with tempfile.TemporaryDirectory() as directory:
            root=Path(directory);marker=root/'unexpected'
            for name,body in {'uname':'echo Linux','readlink':'echo same-namespace',**{name:f'touch "{marker}"; exit 97' for name in ('mount','ip','sysctl','NetworkManager','dbus-daemon')}}.items():
                command=root/name;command.write_text('#!/bin/sh\n'+body+'\n');command.chmod(0o700)
            env=dict(os.environ,PATH=str(root)+os.pathsep+os.environ['PATH'],GITHUB_ACTIONS='true',RUNNER_ENVIRONMENT='github-hosted',**{'WENDY_USB_PARENT_'+kind:'same-namespace' for kind in ('mnt','net','pid','uts')})
            result=subprocess.run(['bash',str(SCRIPT),'--inside','unused','out'],env=env,capture_output=True,text=True,timeout=3)
            self.assertNotEqual(result.returncode,0)
            self.assertFalse(marker.exists())

if __name__=='__main__':unittest.main()

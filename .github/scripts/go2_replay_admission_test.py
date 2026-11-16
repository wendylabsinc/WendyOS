"""Admission checks only; no VM, device or kernel operation is executed."""
import importlib.util
import os
from pathlib import Path
import subprocess
import unittest
from unittest import mock

spec=importlib.util.spec_from_file_location('admission',Path(__file__).with_name('go2_replay_admission.py'))
A=importlib.util.module_from_spec(spec);spec.loader.exec_module(A)
BOOT='11111111-1111-4111-8111-111111111111'
PRODUCT='22222222-2222-4222-8222-222222222222'


class AdmissionTests(unittest.TestCase):
    def probe(self, *, boot=BOOT, product=PRODUCT, actual_boot=BOOT, vm='kvm', container='none', uid=0):
        env={'WENDY_GO2_VM_BOOT_ID':boot,'WENDY_GO2_VM_PRODUCT_UUID':product}
        def read(path):return actual_boot if str(path).endswith('/boot_id') else PRODUCT
        def run(argv,**kwargs):
            value=container if argv[-1]=='--container' else vm
            return subprocess.CompletedProcess(argv,1 if value=='none' else 0,value+'\n','')
        with mock.patch.dict(os.environ,env,clear=True),mock.patch.object(A.sys,'platform','linux'),mock.patch.object(A.os,'geteuid',return_value=uid),mock.patch.object(Path,'read_text',read),mock.patch.object(A.subprocess,'run',side_effect=run):
            return A.admit()

    def test_exact_vm_confirmation_is_required(self):
        self.assertEqual(self.probe(),{'kind':'kvm','boot_id':BOOT,'product_uuid':PRODUCT})
        for kwargs in ({'boot':''},{'boot':PRODUCT},{'product':BOOT},{'actual_boot':PRODUCT},{'vm':'none'},{'vm':'docker'},{'container':'docker'},{'uid':1000},{'product':'00000000-0000-0000-0000-000000000000'}):
            with self.subTest(kwargs=kwargs),self.assertRaises(ValueError):self.probe(**kwargs)

    def test_failure_to_observe_virtualization_is_not_acceptance(self):
        with mock.patch.dict(os.environ,{'WENDY_GO2_VM_BOOT_ID':BOOT,'WENDY_GO2_VM_PRODUCT_UUID':PRODUCT},clear=True),mock.patch.object(A.sys,'platform','linux'),mock.patch.object(A.os,'geteuid',return_value=0),mock.patch.object(Path,'read_text',side_effect=[BOOT,PRODUCT]),mock.patch.object(A.subprocess,'run',side_effect=subprocess.TimeoutExpired('systemd-detect-virt',5)):
            with self.assertRaises(subprocess.TimeoutExpired):A.admit()

if __name__=='__main__':unittest.main()

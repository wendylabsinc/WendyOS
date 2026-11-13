#!/usr/bin/env python3
"""Offline generation checks. No serial device, CLI or firmware process runs."""
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import tempfile
import unittest

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('serial_plan',ROOT/'.github/regression-coverage/manual/serial-flash-rehearsal.py')
PLAN=importlib.util.module_from_spec(spec);spec.loader.exec_module(PLAN)

class SerialPlanTests(unittest.TestCase):
    def setUp(self):
        self.temp=tempfile.TemporaryDirectory();self.addCleanup(self.temp.cleanup);self.root=Path(self.temp.name)
        def file(name,text):
            p=self.root/name;p.write_text(text);return str(p)
        cli=file('candidate-cli','not an executable binary; never invoked\n');os.chmod(cli,0o700)
        firmware=file('firmware.bin','synthetic offline payload');raw=Path(firmware).read_bytes()
        project=self.root/'project';project.mkdir();(project/'wendy.json').write_text('{"appId":"serial-fixture"}');(project/'main.c').write_text('/* reviewed inert offline fixture */')
        main=file('master.json',json.dumps({'firmware':{'esp32s3_native':{'latest':'0.20.2','manifest_path':'fw.json'}}}))
        fm=file('fw.json',json.dumps({'firmware_id':'esp32s3_native','versions':{'0.20.2':{'path':'firmware/esp32s3.bin','size_bytes':len(raw),'checksum':hashlib.sha256(raw).hexdigest()}}}))
        self.data={'host':{'os':'macos','identity':'reserved Mac','os_build':'test-build','owner':'owner','uid':502,'reserved_account':True},'board':{'reserved':True,'serial':'board-serial','hardware_id':'factory-mac','owner':'owner','model':'dual connector S3','recovery_instructions':'reviewed exact record','catalog_board':'esp32s3_generic_native','chip':'esp32s3','device_name':'serial-fixture','cp210x':{'path':'/dev/cu.fixture-uart','vid_pid':'10c4:ea60','usb_serial':'bridge-serial','location':'hub1-port1'},'native':{'path':'/dev/cu.fixture-native','vid_pid':'303a:1001','usb_serial':'native-serial','location':'hub1-port2'},'native_replug_path':'/dev/cu.fixture-replug'},'cli_source_commit':'a'*40,'cli_binary':cli,'main_manifest':main,'firmware_manifest':fm,'firmware_binary':firmware,'project':str(project)}
        for key in ('identity_record','recovery_record','blank_state_record','reinstall_state_record','app_review_record'):self.data[key]=file(key+'.txt','independent offline test record '+key)
    def generate(self,data=None,name='output'):
        p=self.root/(name+'.json');p.write_text(json.dumps(data or self.data));return PLAN.generate(p,self.root/name)
    def test_offline_provenance_and_real_argv(self):
        result=self.generate();self.assertEqual(result['hardware_execution'],'not_run');self.assertTrue(all(c['status']=='not_run' for c in result['cases']))
        commands=json.loads((self.root/'output/commands.json').read_text());install=commands['install'];run=commands['reinstall']
        self.assertIn('--no-wifi',install);self.assertIn('--pre-enroll=false',install);self.assertIn('--prefix',run);self.assertIn('--skip-cloud-registration',run)
        for flag in ('--force','--yes','--binary','--device-type','--version'):self.assertNotIn(flag,install)
        self.assertEqual(Path(result['firmware']['cache']).read_bytes(),Path(self.data['firmware_binary']).read_bytes())
        self.assertEqual(len(result['app_sources']),2);self.assertEqual(len(result['generated']),2)
        self.assertIn('--read-only',commands['identity_after']);self.assertIn('wendy-lite:/dev/cu.fixture-replug',commands['identity_after'])
        self.assertEqual((self.root/'output').stat().st_mode & 0o777,0o700)
    def test_identity_and_payload_refusal(self):
        changes=[lambda d:d['host'].update(reserved_account=False),lambda d:d['board'].update(reserved=False),lambda d:d['board'].update(chip='esp32c6'),lambda d:d['board']['cp210x'].update(vid_pid='ffff:ffff'),lambda d:d['board'].update(native_replug_path=d['board']['native']['path'])]
        for i,change in enumerate(changes):
            d=copy.deepcopy(self.data);change(d)
            with self.assertRaises(ValueError):self.generate(d,'bad'+str(i))
        Path(self.data['firmware_binary']).write_text('changed firmware')
        with self.assertRaisesRegex(ValueError,'firmware bytes mismatch'):self.generate(name='changed')
    def test_linux_and_source_paths(self):
        self.data['host']['os']='linux';result=self.generate()
        self.assertIn('/cache/wendy/',result['firmware']['cache'])
        commands=json.loads((self.root/'output/commands.json').read_text());self.assertEqual(commands['usb_inventory'][0],'udevadm')
        (Path(self.data['project'])/'foreign').symlink_to(self.root/'candidate-cli')
        with self.assertRaisesRegex(ValueError,'symlinks'):self.generate(name='linked')

if __name__=='__main__':unittest.main()

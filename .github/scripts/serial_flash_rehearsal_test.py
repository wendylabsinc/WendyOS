#!/usr/bin/env python3
"""Offline generation checks. No serial device, CLI or firmware process runs."""
import copy
import hashlib
import importlib.util
import json
import os
import signal
import subprocess
import sys
import time
import types
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[2]
spec=importlib.util.spec_from_file_location('serial_plan',ROOT/'.github/regression-coverage/manual/serial-flash-rehearsal.py')
PLAN=importlib.util.module_from_spec(spec);spec.loader.exec_module(PLAN)
spec=importlib.util.spec_from_file_location('serial_windows',ROOT/'.github/regression-coverage/manual/serial-windows-run.py')
RUN=importlib.util.module_from_spec(spec);spec.loader.exec_module(RUN)

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

    def windows(self):
        self.data['host'].update(os='windows',sid='S-1-5-21-1-2-3-1001',machine_uuid='11111111-2222-3333-4444-555555555555')
        self.data['powershell_binary']=self.data['cli_binary']
        self.data['board'].update(native_replug_path='COM9',native_replug_location='usb-second-location')
        for name,port in [('cp210x','COM7'),('native','COM8')]:
            item=self.data['board'][name];vid,pid=item['vid_pid'].split(':')
            item.update(path=port,instance_id='USB\\VID_'+vid.upper()+'&PID_'+pid.upper()+'\\'+item['usb_serial'],driver_version='10.0.1',driver_provider='reviewed provider',driver_inf='oem42.inf')
        return self.data

    def snapshot(self,transport='native'):
        d=self.data;host=d['host'];b=d['board'];item=b['native' if transport=='replug' else transport]
        port={key:item[key] for key in ('instance_id','driver_version','driver_provider','driver_inf')}
        port.update(port=b['native_replug_path'] if transport=='replug' else item['path'],locations=[b['native_replug_location'] if transport=='replug' else item['location']],status='OK',error_code=0,driver_signed=True)
        return {**{key:host[key] for key in ('machine_uuid','sid','os_build')},'elevated':False,'ports':[port]}

    def test_windows_plan_and_identity_admission(self):
        self.windows();result=self.generate();self.assertEqual(result['hardware_execution'],'not_run')
        actions=result['windows']['actions'];self.assertEqual(actions['install_cp210x']['timeout'],900)
        self.assertEqual(actions['identity_after']['argv'][4],'device')
        self.assertEqual(result['windows']['environment']['LOCALAPPDATA'],str((self.root/'output/cache').resolve()))
        for transport in ('native','cp210x','replug'):RUN.admit(self.snapshot(transport),self.data,transport)
        absent=self.snapshot();absent['ports']=[];RUN.admit(absent,self.data,'absent')
        for fault in ('sid','elevated','driver','location','port','instance','other'):
            snap=self.snapshot()
            if fault=='sid':snap['sid']='other'
            elif fault=='elevated':snap['elevated']=True
            elif fault=='driver':snap['ports'][0]['driver_version']='changed'
            elif fault=='location':snap['ports'][0]['locations']=['other']
            elif fault=='port':snap['ports'][0]['port']='COM100'
            elif fault=='instance':snap['ports'][0]['instance_id']+='other'
            else:snap['ports']*=2
            with self.subTest(fault=fault),self.assertRaises(ValueError):RUN.admit(snap,self.data,'native')
        commands=json.loads((self.root/'output/commands.json').read_text())
        self.assertIn('--confirm-board',commands['reinstall']);self.assertNotIn('run-owned-media-test.py',' '.join(commands['reinstall']))

    def test_real_launcher_cannot_start_before_job_permit(self):
        permit=self.root/'permit';marker=self.root/'marker';command=self.root/'command.json'
        command.write_text(json.dumps([sys.executable,'-c','from pathlib import Path; import sys; Path(sys.argv[1]).write_text("started")',str(marker)]))
        child=subprocess.Popen([sys.executable,str(Path(RUN.__file__)),'--child',str(permit),'trial',str(command)])
        try:
            until=time.monotonic()+5
            while not Path(str(permit)+'.ready').exists():
                if time.monotonic()>until:self.fail('owned child did not reach permit barrier')
                time.sleep(.02)
            self.assertEqual(Path(str(permit)+'.ready').read_text(),'trial\n')
            self.assertIsNone(child.poll());self.assertFalse(marker.exists())
            permit.write_text('trial\n');self.assertEqual(child.wait(timeout=5),0);self.assertEqual(marker.read_text(),'started')
        finally:
            if child.poll() is None:child.kill()
            child.wait(timeout=5)

    def test_cancel_during_windows_job_drain_cannot_complete(self):
        self.windows();evidence=self.generate();events=[];snapshot=self.snapshot()
        real_load=RUN.load
        owner=real_load('serial_test_owner',evidence['windows']['supervisor'])
        class Job:
            def assign(self,child):events.append('assign')
            def drain(self):events.append('drain');signal.raise_signal(signal.SIGTERM)
            def close(self):events.append('close')
        class Child:
            def wait(self,timeout):return 0
            def kill(self):events.append('kill')
        def spawn(argv,env,stdout=None):
            if stdout:stdout.write(json.dumps(snapshot));stdout.flush()
            return Child()
        def helper(name,path):return types.SimpleNamespace(Job=Job) if name=='serial_job_type' else owner
        handlers={sig:signal.getsignal(sig) for sig in (signal.SIGINT,signal.SIGTERM)}
        with patch.object(RUN,'windows_host',return_value=True),patch.object(RUN,'load',side_effect=helper),patch.object(RUN.subprocess,'Popen',side_effect=spawn),self.assertRaises(InterruptedError):
            RUN.run(self.root/'output/evidence.json','identity_before',self.root/'action',self.data['board']['serial'])
        self.assertEqual(events,['assign','drain','close'])
        self.assertEqual(handlers,{sig:signal.getsignal(sig) for sig in handlers})
        report=json.loads((self.root/'action/result.json').read_text());self.assertEqual(report['status'],'failed');self.assertEqual(report['hardware_acceptance'],'pending_operator_review')

if __name__=='__main__':unittest.main()

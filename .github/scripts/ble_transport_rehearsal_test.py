"""Offline physical BLE plan/ownership checks. No radio, WinRT or TCC execution."""
import importlib.util
import json
import os
import shlex
import signal
import sys
import time
import types
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

MANUAL=Path(__file__).resolve().parents[1]/'regression-coverage/manual'

def load(name):
    spec=importlib.util.spec_from_file_location(name,MANUAL/(name+'.py'))
    module=importlib.util.module_from_spec(spec);spec.loader.exec_module(module)
    return module

PLAN=load('ble-transport-rehearsal')
RUN=load('ble-transport-run')

class BLEPhysicalTests(unittest.TestCase):
    def fixture(self,root,platform='darwin'):
        dummy=root/'inert-artifact';dummy.write_text('synthetic offline artifact, never hardware provenance')
        data={key:str(dummy) for key in ('probe_binary','cli_binary','firmware_binary','identity_record','build_record','authorization_record','permission_record','recovery_record')}
        data.update(source_commit='a'*40,firmware_source_commit='b'*40,trial='c'*32,
                    host={'platform':platform,'identity':'fixture host','owner':'fixture operator','os_version':'fixture OS','adapter_identity':'fixture adapter','radio_firmware':'fixture radio firmware','permission_record':'not observed'},
                    peripheral={'serial':'fixture board','device_id':'fixture-device-id','board_model':'ESP32 fixture','owner':'fixture owner','power_connection':'reserved fixture connector','recovery_record':'fixture recovery','address':'12345678-1234-1234-1234-123456789ABC' if platform=='darwin' else 'AA:BB:CC:DD:EE:FF','gatt_profile':'wendy-lite-info-and-provisioning-status'})
        inventory=root/'inventory.json';inventory.write_text(json.dumps(data));return inventory,data

    def test_three_platform_plans_remain_not_run_and_bind_every_file(self):
        for platform,system in (('darwin','Darwin'),('linux','Linux'),('windows','Windows')):
            with self.subTest(platform=platform),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,_=self.fixture(root,platform)
                with patch.object(PLAN.platform,'system',return_value=system),patch.object(RUN.subprocess,'Popen') as spawn:
                    evidence=PLAN.generate(inventory,root/'out')
                spawn.assert_not_called()
                self.assertEqual(evidence['hardware_execution'],'not_run')
                self.assertEqual({c['status'] for c in evidence['cases']},{'not_run'})
                for record in [evidence['inventory_record'],*evidence['records'].values(),*evidence['sources'],*evidence['generated']]:RUN.verify(record)
                config=json.loads(Path(evidence['probe_plan']).read_text())
                for key in ('RestoreFile','PermitFile','SuccessFile'):self.assertFalse(Path(config[key]).exists())
                self.assertEqual(config['Platform'],platform)
                self.assertEqual(evidence['test'],'TestPhysicalWinRTScanRestart' if platform=='windows' else 'TestPhysicalLiteGATTDisconnect')
                self.assertEqual(bool(evidence['unsupported']),platform=='windows')
                guide=(root/'out/procedure.txt').read_text()
                self.assertIn('GATT/L2CAP is explicitly unimplemented',guide)
                self.assertIn('Both pending calls must',guide)
                self.assertNotIn('@TRIAL@',guide)
                Path(evidence['probe_plan']).write_text('{}')
                with patch.object(RUN.subprocess,'Popen') as execute,self.assertRaisesRegex(ValueError,'changed physical plan artifact'):
                    RUN.run(root/'out/evidence.json')
                execute.assert_not_called()

    def test_refuses_changed_host_or_ambiguous_peripheral_before_output(self):
        for fault in ('platform','address','source','profile','secret','trial'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,data=self.fixture(root)
                if fault=='platform':data['host']['platform']='windows'
                elif fault=='address':data['peripheral']['address']='cached display name'
                elif fault=='source':data['source_commit']='main'
                elif fault=='profile':data['peripheral']['gatt_profile']='invented fixture'
                elif fault=='secret':data['password']='never record'
                elif fault=='trial':data['trial']='reused'
                inventory.write_text(json.dumps(data))
                with patch.object(PLAN.platform,'system',return_value='Darwin'),self.assertRaises(ValueError):PLAN.generate(inventory,root/'out')
                self.assertFalse((root/'out').exists())

    def test_windows_job_owns_before_permit_and_drains_before_close(self):
        for fault in ('none','assign','wait','interrupt','drain'):
            with self.subTest(fault=fault),tempfile.TemporaryDirectory() as tmp:
                path=Path(tmp)/'permit';events=[]
                class Child:
                    def kill(self):events.append('child-kill')
                    def wait(self,timeout):
                        events.append('wait'+str(timeout))
                        if timeout!=5:
                            self.assert_permitted()
                            if fault=='wait':raise subprocess.TimeoutExpired('fixture',timeout)
                            if fault=='interrupt':raise KeyboardInterrupt()
                        return 0
                    def assert_permitted(self):
                        if path.read_text()!='a'*32+'\n':raise AssertionError('wrong permit')
                class Job:
                    def assign(self,child):
                        events.append('assign')
                        if path.exists():raise AssertionError('permit preceded job assignment')
                        if fault=='assign':raise RuntimeError('assignment refused')
                    def drain(self):
                        events.append('drain')
                        if fault=='drain':raise RuntimeError('unproven drain')
                    def close(self):events.append('close')
                def spawn(*args,**kwargs):events.append('spawn');return Child()
                if fault=='none':self.assertEqual(RUN.windows_probe(['inert'],{},path,'a'*32,Job,spawn,timeout=.01),0)
                else:
                    expected=KeyboardInterrupt if fault=='interrupt' else (subprocess.TimeoutExpired if fault=='wait' else RuntimeError)
                    with self.assertRaises(expected):RUN.windows_probe(['inert'],{},path,'a'*32,Job,spawn,timeout=.01)
                self.assertEqual(events[:2],['spawn','assign'])
                if fault=='assign':self.assertFalse(path.exists());self.assertIn('child-kill',events)
                self.assertLess(events.index('drain'),events.index('close'))

    def test_windows_signals_preserve_assignment_barrier_and_cleanup(self):
        for stage in ('assign','drain'):
            with self.subTest(stage=stage),tempfile.TemporaryDirectory() as tmp:
                root=Path(tmp);inventory,_=self.fixture(root,'windows');events=[]
                with patch.object(PLAN.platform,'system',return_value='Windows'):
                    evidence=PLAN.generate(inventory,root/'out')
                class Child:
                    def wait(self,timeout):events.append('wait');return 0
                    def kill(self):events.append('kill')
                class Job:
                    def assign(self,child):
                        events.append('assign')
                        if stage=='assign':signal.raise_signal(signal.SIGTERM)
                    def drain(self):
                        events.append('drain-start')
                        if stage=='drain':
                            signal.raise_signal(signal.SIGTERM)
                            signal.raise_signal(signal.SIGTERM)
                        events.append('drain-finished')
                    def close(self):events.append('close')
                handlers={sig:signal.getsignal(sig) for sig in (signal.SIGTERM,signal.SIGINT)}
                with patch.object(RUN,'load_helper',return_value=types.SimpleNamespace(Job=Job)),patch.object(RUN.subprocess,'Popen',return_value=Child()),self.assertRaises(KeyboardInterrupt):
                    RUN.run(root/'out/evidence.json')
                self.assertLess(events.index('drain-start'),events.index('drain-finished'))
                self.assertLess(events.index('drain-finished'),events.index('close'))
                self.assertEqual(handlers,{sig:signal.getsignal(sig) for sig in handlers})
                if stage=='assign':self.assertFalse((root/'out/supervisor.nonce').exists())
                self.assertFalse((root/'out/completed.nonce').exists())

    def test_generated_restore_command_writes_exact_nonce(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);inventory,_=self.fixture(root)
            with patch.object(PLAN.platform,'system',return_value='Darwin'):
                evidence=PLAN.generate(inventory,root/'out')
            command=(root/'out/procedure.txt').read_text().strip().splitlines()[-1]
            subprocess.run(shlex.split(command),check=True,timeout=5)
            config=json.loads(Path(evidence['probe_plan']).read_text())
            self.assertEqual(Path(config['RestoreFile']).read_text(),config['Trial']+'\n')

    @unittest.skipUnless(os.name=='posix','Unix guardian ownership check')
    def test_real_supervisor_term_drains_only_inert_owned_probe(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp);inventory,data=self.fixture(root)
            ready=root/'inert-ready';drained=root/'inert-drained'
            probe=root/'inert-probe'
            probe.write_text('#!'+sys.executable+'\nimport signal, time\nfrom pathlib import Path\n'
                             +'def stop(*_):\n Path('+repr(str(drained))+').write_text("drained")\n raise SystemExit(0)\n'
                             +'signal.signal(signal.SIGTERM,stop)\nPath('+repr(str(ready))+').write_text("ready")\n'
                             +'while True: time.sleep(0.05)\n')
            probe.chmod(0o700);data['probe_binary']=str(probe);inventory.write_text(json.dumps(data))
            with patch.object(PLAN.platform,'system',return_value='Darwin'):
                evidence=PLAN.generate(inventory,root/'out')
            child=subprocess.Popen([sys.executable,str(root/'out/ble-transport-run.py'),str(root/'out/evidence.json')])
            try:
                deadline=time.monotonic()+5
                while not ready.exists() and time.monotonic()<deadline:time.sleep(0.02)
                self.assertTrue(ready.exists(),'inert probe did not start')
                child.send_signal(signal.SIGTERM)
                self.assertEqual(child.wait(timeout=10),143)
                self.assertEqual(drained.read_text(),'drained')
                self.assertFalse(Path(json.loads(Path(evidence['probe_plan']).read_text())['SuccessFile']).exists())
            finally:
                if child.poll() is None:
                    child.terminate();child.wait(timeout=10)

    def test_zero_exit_without_completed_nonce_cannot_pass(self):
        with tempfile.TemporaryDirectory() as tmp:
            path=Path(tmp)/'completed';plan={'SuccessFile':str(path),'Trial':'d'*32}
            with self.assertRaises(FileNotFoundError):RUN.completed(0,plan)
            path.write_text('foreign\n')
            with self.assertRaises(ValueError):RUN.completed(0,plan)
            path.write_text('d'*32+'\n')
            self.assertEqual(RUN.completed(0,plan),0)
            self.assertEqual(RUN.completed(124,plan),124)

if __name__=='__main__':unittest.main()

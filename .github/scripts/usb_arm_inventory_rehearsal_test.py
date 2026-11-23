#!/usr/bin/env python3
"""Offline plan/observation checks; no USB, Agent, serial or hardware acceptance."""
import contextlib
import copy
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / '.github/regression-coverage/manual/usb-arm-inventory-rehearsal.py'
SPEC = importlib.util.spec_from_file_location('usb_arm_rehearsal', SCRIPT)
M = importlib.util.module_from_spec(SPEC); SPEC.loader.exec_module(M)


class USBArmRehearsalTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix='wendy2246-offline-')
        self.addCleanup(self.temp.cleanup); self.root = Path(self.temp.name)

    def inventory(self, platform='linux', serial='owned-adapter'):
        target = self.root / (platform + ('-serial' if serial else '-location'))
        target.mkdir()
        port = '/dev/ttyACM0' if platform == 'linux' else '/dev/cu.fixture0'
        location = '1-2.3.4' if platform == 'linux' else '0x12340000'
        adapter = dict(vendor_id='1a86', product_id='55d3', serial_number=serial,
                       usb_interface='1', usb_location=location, device_path=port,
                       physical_label='reserved offline fixture')
        kind, key = ('s', serial) if serial else ('l', location)
        adapter['id'] = f'usb:1a86:55d3:{kind}:{key.encode().hex()}:1'
        device = {k: v for k, v in adapter.items() if k != 'physical_label'}
        device.update(identity_kind='usb_serial' if serial else 'usb_location',
                      configured=False, verified=False, connected=True,
                      connection_status='present', access_status='unknown', name='', model='')
        raw = dict(platform=platform, hostname='offline-host', target='offline-host:50051',
                   os_version='fixture', agent_version='fixture', owner='offline reviewer',
                   captured_at='2026-11-13T03:00:00Z', adapter=adapter,
                   nested_hubs=[dict(physical_label='hub'+str(i), native_location='hub-location'+str(i),
                                     vendor_id='1234', product_id='5678') for i in range(2)])
        for key in ('cli', 'agent_binary', 'native_inventory', 'authorization', 'restart_procedure'):
            p=target/key; p.write_text('offline provenance only\n'); raw[key]=str(p)
        p=target/'baseline.json'; p.write_text(json.dumps({'devices':[device]}));raw['baseline_list']=str(p)
        inventory=target/'inventory.json';inventory.write_text(json.dumps(raw))
        return inventory, raw, device, target

    def snapshot(self, target, rows):
        p=target/'observation.json'; p.write_text(json.dumps({'devices':rows}));return p

    def check(self, plan, phase, snapshot):
        with contextlib.redirect_stdout(io.StringIO()): M.verify(plan, phase, snapshot)

    def test_all_four_native_identity_plans_are_unexecuted_and_scoped(self):
        for platform in ('linux', 'macos'):
            for serial in ('owned-adapter', ''):
                inventory, raw, device, target=self.inventory(platform,serial)
                p=M.generate(inventory,target/'plan')
                self.assertEqual(p['hardware_execution'],'not_run')
                self.assertTrue(all(c['status']=='not_run' for c in p['cases']))
                self.assertEqual(p['commands']['forget'][-1],raw['adapter']['id'])
                self.assertEqual('--allow-location-binding' in p['commands']['bind'],not bool(serial))
                self.assertNotIn('--arm-name',p['commands']['rebind'])
                self.assertIn(raw['target'],p['commands']['list'])
                self.assertEqual(json.loads(Path(raw['baseline_list']).read_text())['devices'],[device])
                self.assertFalse(any('@'+x+'@' in (target/'plan/procedure.txt').read_text() for x in ('ID','NAME','HOST','TARGET','PORT','PLATFORM')))

    def test_refuses_ambiguous_existing_or_mismatched_target_before_output(self):
        inventory, raw, d, target=self.inventory()
        for mutation in ('bound','duplicate','wrong-id','no-hubs'):
            candidate=copy.deepcopy(raw); rows=[copy.deepcopy(d)]
            if mutation=='bound': rows[0]['configured']=True
            elif mutation=='duplicate': rows.append(copy.deepcopy(d))
            elif mutation=='wrong-id': candidate['adapter']['id']='foreign'
            else: candidate['nested_hubs']=candidate['nested_hubs'][:1]
            Path(raw['baseline_list']).write_text(json.dumps({'devices':rows}))
            inventory.write_text(json.dumps(candidate));out=target/mutation
            with self.assertRaises(ValueError):M.generate(inventory,out)
            self.assertFalse(out.exists())

    def test_actual_snapshot_contract_rejects_stale_tty_name_and_unrelated_mutation(self):
        inventory, raw, d, target=self.inventory();p=M.generate(inventory,target/'plan');plan=target/'plan/plan.json'
        bound=dict(d,configured=True,name=p['saved_name'],model='so101')
        self.check(plan,'bound',self.snapshot(target,[bound]))
        disconnected=dict(bound,connected=False,connection_status='disconnected',device_path='')
        self.check(plan,'disconnected',self.snapshot(target,[disconnected]))
        moved=dict(bound,device_path='/dev/ttyACM7',usb_location='1-2.3.5')
        self.check(plan,'renumbered',self.snapshot(target,[moved]))
        self.check(plan,'restarted',self.snapshot(target,[moved]))
        self.check(plan,'forgotten',self.snapshot(target,[dict(moved,configured=False,name='',model='')]))
        cases=[('disconnected',bound),('bound',dict(bound,name='SO-101')),
               ('bound',dict(bound,verified=True)),('renumbered',bound),
               ('renumbered',dict(moved,serial_number='replacement'))]
        for phase,bad in cases:
            with self.assertRaises(ValueError):self.check(plan,phase,self.snapshot(target,[bad]))
        with self.assertRaises(ValueError):
            self.check(plan,'bound',self.snapshot(target,[bound,dict(bound,id='foreign',configured=True)]))

    def test_location_move_stays_unbound_and_baseline_cannot_be_substituted(self):
        inventory,raw,d,target=self.inventory(serial='');p=M.generate(inventory,target/'plan');plan=target/'plan/plan.json'
        old=dict(d,configured=True,name=p['saved_name'],model='so101',connected=False,device_path='',connection_status='disconnected')
        new=dict(d,id='usb:1a86:55d3:l:'+b'1-2.3.9'.hex()+':1',usb_location='1-2.3.9',device_path='/dev/ttyACM8')
        self.check(plan,'relocated-location',self.snapshot(target,[old,new]))
        with self.assertRaises(ValueError):self.check(plan,'relocated-location',self.snapshot(target,[old,dict(new,configured=True)]))
        for field,value in [('id','arbitrary'),('device_path',''),('identity_kind','usb_serial'),('connection_status','ambiguous'),('access_status','ready')]:
            with self.assertRaises(ValueError):self.check(plan,'relocated-location',self.snapshot(target,[old,dict(new,**{field:value})]))
        Path(raw['baseline_list']).write_text(json.dumps({'devices':[]}))
        with self.assertRaises(ValueError):self.check(plan,'relocated-location',self.snapshot(target,[old,new]))


if __name__ == '__main__': unittest.main()

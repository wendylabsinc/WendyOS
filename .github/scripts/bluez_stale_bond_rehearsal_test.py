#!/usr/bin/env python3
"""Offline synthetic inventory and private-file checks, never Bluetooth access."""
import importlib.util
import json
from pathlib import Path
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]

def load(name,path):
    spec=importlib.util.spec_from_file_location(name,path)
    module=importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module

GEN=load('bluez_plan',ROOT/'regression-coverage/manual/bluez-stale-bond-rehearsal.py')
OBS=load('bluez_observer',ROOT/'regression-coverage/manual/bluez-input-observer.py')


class BluezPhysicalTests(unittest.TestCase):
    def inventory(self,root):
        def file(name,value):
            p=root/name;p.write_text(json.dumps(value));return str(p)
        cli=root/'inert-cli';cli.write_text('#!/bin/sh\nexit 97\n');cli.chmod(0o700)
        data={'cli_binary':str(cli),'agent_binary':file('agent','SYNTHETIC NOT AN AGENT'),'agent_source_commit':'a'*40,
              'host':{'serial':'SYNTHETIC-HOST','owner':'fixture','management_selector':'fixture.invalid:50051',
                      'os_release':'synthetic Linux','kernel':'fixture','bluez_version':'fixture','architecture':'arm64',
                      'adapter_mac':'02:00:00:00:00:01','adapter_index':7},
              'peripheral':{'model':'fixture keyboard','serial':'SYNTHETIC-KEYBOARD','address':'02:00:00:00:00:02',
                            'profile':'le-hid','action_labels':['KEY_A','KEY_B']},'records':{}}
        for key in ('host_identity','adapter_identity','peripheral_identity','original_state','recovery'):
            data['records'][key]=file(key,{'synthetic':True})
        data['records']['peripheral_bond_reset']=file('vendor-recipe',{
            'peripheral_serial':'SYNTHETIC-KEYBOARD','model':'fixture keyboard','owner_approved':True,
            'scope':'peripheral-bond-only','vendor_url':'https://fixture.invalid/manual','slot':'reserved test slot',
            'max_seconds':30,'steps':['SYNTHETIC instructions, never execute'],'expected_confirmation':'SYNTHETIC'})
        path=root/'inventory.json';path.write_text(json.dumps(data));return path,data

    def test_generation_has_no_side_effect_commands_or_hardware_success(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);inventory,data=self.inventory(root)
            with patch.object(subprocess,'Popen',side_effect=AssertionError('executed device operation')):
                evidence=GEN.generate(inventory,root/'plan')
            self.assertEqual(evidence['hardware_execution'],'not_run')
            self.assertTrue(all(case['status']=='not_run' for case in evidence['cases']))
            commands=json.loads((root/'plan/commands.json').read_text())
            self.assertIn('--pair=true',commands['repair_preserving_live_trust'])
            self.assertIn('--trust=false',commands['repair_preserving_live_trust'])
            self.assertIn('--pair=false',commands['no_pair_control'])
            self.assertTrue(all('forget' not in argv and 'remove' not in argv for argv in commands.values()))
            self.assertIn(data['peripheral']['address'],commands['disconnect_target'])
            self.assertEqual(len(set(evidence['challenge'][:2])),2)
            self.assertTrue((root/'plan/bluez-input-observer.py').is_file())
            with self.assertRaises(FileExistsError):GEN.generate(inventory,root/'plan')

    def test_wrong_target_or_host_reset_recipe_refused(self):
        for key,value in [('scope','host-controller-reset'),('owner_approved',False),('peripheral_serial','OTHER'),('max_seconds',121)]:
            with self.subTest(key=key),tempfile.TemporaryDirectory() as d:
                root=Path(d);inventory,data=self.inventory(root)
                recipe=Path(data['records']['peripheral_bond_reset']);doc=json.loads(recipe.read_text());doc[key]=value;recipe.write_text(json.dumps(doc))
                with self.assertRaisesRegex(ValueError,'exact-target'):GEN.generate(inventory,root/'plan')
                self.assertFalse((root/'plan').exists())

    def test_invalid_transport_and_controller_identity_refused(self):
        for group,key,value in [('host','management_selector','cloud://unreserved'),('host','adapter_index',-1),
                                ('peripheral','profile','audio'),('peripheral','address','02:00:00:00:00:01')]:
            with self.subTest(key=key),tempfile.TemporaryDirectory() as d:
                root=Path(d);inventory,data=self.inventory(root);data[group][key]=value;inventory.write_text(json.dumps(data))
                with self.assertRaises(ValueError):GEN.generate(inventory,root/'plan')

    def test_registry_matches_only_exact_peripheral_and_observes_removal(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d)
            for name,address in [('input1','AA:BB:CC:DD:EE:01'),('input2','AA:BB:CC:DD:EE:02')]:
                p=root/name;p.mkdir();(p/'uniq').write_text(address+'\n');(p/'event3').mkdir()
            self.assertEqual(len(OBS.snapshot('aa:bb:cc:dd:ee:01',root)),1)
            (root/'input1/uniq').unlink()
            self.assertEqual(OBS.snapshot('AA:BB:CC:DD:EE:01',root),[])

    def test_registry_permission_failure_cannot_claim_input_absent(self):
        with tempfile.TemporaryDirectory() as d:
            root=Path(d);(root/'input1').mkdir();(root/'input1/uniq').write_text('fixture')
            with patch.object(Path,'read_text',side_effect=PermissionError('fixture denial')):
                with self.assertRaises(PermissionError):OBS.snapshot('AA:BB:CC:DD:EE:01',root)

    def test_observer_new_file_bound_and_timestamp_order(self):
        with tempfile.TemporaryDirectory() as d:
            output=Path(d)/'observations.jsonl'
            with patch.object(OBS,'snapshot',return_value=[]):OBS.record('AA:BB:CC:DD:EE:01',.01,output)
            rows=[json.loads(line) for line in output.read_text().splitlines()]
            self.assertEqual(len(rows),2)
            self.assertLess(rows[0]['monotonic'],rows[1]['monotonic'])
            with self.assertRaises(FileExistsError):OBS.record('AA:BB:CC:DD:EE:01',.01,output)


if __name__=='__main__':unittest.main()

#!/usr/bin/env python3
"""Offline provenance and bounded recording checks; no ROS/hardware calls."""
import importlib.util
import json
import os
from pathlib import Path
import struct
import stat
from types import SimpleNamespace
import tempfile
import unittest
from unittest import mock

ROOT=Path(__file__).resolve().parents[2]
def load(name):
    spec=importlib.util.spec_from_file_location(name,ROOT/'.github/regression-coverage/manual'/f'{name}.py')
    m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m);return m
plan=load('go2-camera-rehearsal');record=load('go2-record-cdr')
spec=importlib.util.spec_from_file_location('owned_go2_cleanup',Path(__file__).with_name('cleanup-owned-go2-camera.py'))
cleanup=importlib.util.module_from_spec(spec);spec.loader.exec_module(cleanup)

class Go2PhysicalProcedure(unittest.TestCase):
    def inventory(self,root):
        binary=root/'candidate';binary.write_text('inert fixture only');binary.chmod(0o700)
        record=root/'observation';record.write_text('inert operator record')
        listing=root/'cameras.json';listing.write_text(json.dumps([{'id':128,'stable_id':'ros2:observed'}]))
        data={'cli_binary':str(binary),'agent_binary':str(binary),'source_commit':'a'*40,
              'consumer_image':'example.invalid/reviewed-media@sha256:'+'b'*64,
              'target':{'selector':'reserved-companion:50052','serial':'COMPANION-ONLY','owner':'operator','board':'Orin','kernel':'pinned','camera_stable_id':'ros2:observed'},
              'robot':{'serial':'ROBOT-ONLY','model':'Unitree Go2','firmware':'pinned firmware','firmware_record':str(record),'wired_interface':'eth0','topic':'/frontvideostream','resolution':360},
              'records':{k:str(record) for k in ['device_info','installed_agent','camera_list','kernel_module','ros_interfaces','baseline_apps','recovery']}}
        data['records']['camera_list']=str(listing)
        source=root/'inventory.json';source.write_text(json.dumps(data));return source,data

    def test_generation_never_calls_devices_and_all_cases_stay_not_run(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);source,data=self.inventory(root)
            with mock.patch('subprocess.Popen',side_effect=AssertionError('no execution permitted')):
                result=plan.generate(source,root/'plan')
            self.assertTrue(all(v=='not_run' for v in result['cases'].values()))
            self.assertEqual(result['target']['node'],'/dev/video128')
            capture=(root/'plan/capture-cli.sh').read_text()
            self.assertNotIn('--json',capture);self.assertIn('2> physical.stderr',capture)
            self.assertIn('--stable-id ros2:observed',capture)
            commands=json.loads((root/'plan/commands.json').read_text())
            self.assertEqual(commands['remove_owned_consumer'][-1],'--force')
            self.assertNotIn('--cleanup',str(commands));self.assertNotIn('--delete-volumes',str(commands))
            manifest=json.loads((root/'plan/consumer/wendy.json').read_text())
            self.assertEqual(manifest['entitlements'],[{'type':'camera'}])
            self.assertEqual(manifest['env']['GO2_CAMERA_NODE'],'/dev/video128')
            guide=(root/'plan/procedure.txt').read_text()
            self.assertIn('Recorded service replay remains pending',guide)
            # Nothing that changes the target may be listed as read-only.
            read_only,changing=guide.split('\nExact read-only commands:\n')[1].split('\nExact commands that change the target')
            self.assertEqual([l.split(': ')[0] for l in read_only.splitlines()],['device_info','cameras','apps_before_and_after','owned_consumer_logs'])
            for word in (' run ',' remove ','--force','--yes','--detach'):self.assertNotIn(word,read_only)
            self.assertEqual([l.split(': ')[0] for l in changing.splitlines()[1:]],['deploy_owned_consumer','remove_owned_consumer'])

    def test_wrong_camera_ambiguous_identity_and_unpinned_image_refuse(self):
        for case in ['wrong-camera','ambiguous-camera','image']:
            with self.subTest(case=case), tempfile.TemporaryDirectory() as temp:
                root=Path(temp);source,data=self.inventory(root)
                if case=='wrong-camera':data['target']['camera_stable_id']='ros2:other'
                elif case=='image':data['consumer_image']='latest'
                else:Path(data['records']['camera_list']).write_text(json.dumps([{'id':128,'stable_id':'ros2:observed'},{'id':129,'stable_id':'ros2:observed'}]))
                source.write_text(json.dumps(data))
                with self.assertRaises(ValueError):plan.generate(source,root/'plan')
                self.assertFalse((root/'plan').exists())

    def payload(self):
        unit=b'\x00\x00\x00\x01\x65'+b'x'*11
        return b'\x00\x01\x00\x01'+struct.pack('<QII',100,360,len(unit))+unit

    def test_raw_record_preserves_exact_bytes_and_provenance(self):
        with tempfile.TemporaryDirectory() as temp:
            out=Path(temp);rows=[];payload=self.payload()
            record.append_sample(out,payload,rows,123456)
            self.assertEqual((out/'0000.cdr').read_bytes(),payload)
            self.assertEqual(rows[0]['received_monotonic_ns'],123456)
            self.assertEqual(rows[0]['firmware_timestamp'],100)
            self.assertEqual(rows[0]['resolution'],360)
            self.assertEqual(len(rows[0]['sha256']),64)

    def test_invalid_or_excess_raw_samples_never_write(self):
        with tempfile.TemporaryDirectory() as temp:
            out=Path(temp)
            for payload in [b'',b'x'*(1024*1024+1),self.payload()[:12]+struct.pack('<I',720)+self.payload()[16:],self.payload()+b'padding-too-long']:
                with self.assertRaises(ValueError):record.append_sample(out,payload,[],1)
            with self.assertRaises(ValueError):record.append_sample(out,self.payload(),[{'bytes':1}]*1800,1)
            with self.assertRaises(ValueError):record.append_sample(out,self.payload(),[{'bytes':64*1024*1024}],1)
            self.assertEqual(list(out.iterdir()),[])

class OwnedGo2NodeCleanup(unittest.TestCase):
    def test_no_node_and_foreign_label_never_open_control(self):
        with tempfile.TemporaryDirectory() as temp, mock.patch.object(cleanup.os,'open') as opened:
            cleanup.cleanup('ci-go2-'+'a'*12,Path(temp),Path(temp))
            with self.assertRaises(ValueError):cleanup.cleanup('another-camera',Path(temp),Path(temp))
            opened.assert_not_called()

    def test_cleanup_uses_exact_label_and_node128(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp);dev=root/'dev';sysfs=root/'sys';dev.mkdir()
            node=dev/'video128';node.write_text('inert stand-in')
            version=sysfs/'module/v4l2loopback/version';version.parent.mkdir(parents=True);version.write_text('0.15.4')
            name=sysfs/'class/video4linux/video128/name';name.parent.mkdir(parents=True);name.write_text('ci-go2-'+'a'*12)
            metadata=SimpleNamespace(st_mode=stat.S_IFCHR,st_ino=123,st_rdev=456)
            with mock.patch.object(Path,'lstat',return_value=metadata), mock.patch.object(cleanup.os,'open',return_value=42) as opened, mock.patch.object(cleanup.os,'close') as closed, mock.patch.object(cleanup.fcntl,'ioctl',side_effect=lambda *args:node.unlink()) as ioctl:
                cleanup.cleanup('ci-go2-'+'a'*12,dev,sysfs)
                ioctl.assert_called_once_with(42,0x40047E02,128)
                self.assertEqual(opened.call_args.args[0],dev/'v4l2loopback')
                closed.assert_called_once_with(42)
            node.write_text('inert stand-in');name.write_text('other-owned-camera')
            with mock.patch.object(Path,'lstat',return_value=metadata), mock.patch.object(cleanup.os,'open') as opened:
                with self.assertRaisesRegex(RuntimeError,'another camera'):cleanup.cleanup('ci-go2-'+'a'*12,dev,sysfs)
                opened.assert_not_called()

if __name__=='__main__':unittest.main()

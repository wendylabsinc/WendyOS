"""Synthetic byte fixtures validate admission only, never physical firmware."""
import hashlib
import importlib.util
import json
from pathlib import Path
import struct
import subprocess
import sys
import tempfile
import unittest

spec=importlib.util.spec_from_file_location('archive',Path(__file__).with_name('go2_recording_archive.py'))
A=importlib.util.module_from_spec(spec);spec.loader.exec_module(A)


class ArchiveTests(unittest.TestCase):
    def fixture(self,root):
        archive=root/'recording';archive.mkdir();provenance=root/'provenance';provenance.mkdir()
        target={'selector':'wired-test','serial':'companion-1','owner':'owner','board':'fixture','kernel':'fixture-kernel','camera_stable_id':'ros2:front','node':'/dev/video128'}
        robot={'model':'Unitree Go2','serial':'robot-1','firmware':'observed-fixture','firmware_record':'original/path','wired_interface':'eth0','topic':'/frontvideostream','resolution':360}
        plan={'schema_version':1,'nonce':'go2-camera-'+'a'*12,'source_commit':'a'*40,'target':target,'robot':robot,'cli':{'sha256':'b'*64},'agent':{'sha256':'c'*64},'records':{}}
        for name in ('firmware_record','device_info','installed_agent','camera_list','kernel_module','ros_interfaces','baseline_apps','recovery'):
            raw=json.dumps([{'stable_id':'ros2:front','id':128}]).encode() if name=='camera_list' else (name+' synthetic provenance only').encode()
            (provenance/name).write_bytes(raw)
            destination=plan if name=='firmware_record' else plan['records']
            destination[name]={'sha256':A.sha(raw),'path':'original/'+name}
        path=root/'plan.json';path.write_text(json.dumps(plan));rows=[]
        for index in range(30):
            unit=b'\x00\x00\x00\x01\x65'+bytes([index])
            raw=b'\x00\x01\x00\x01'+struct.pack('<QII',index,360,len(unit))+unit
            name=f'{index:04d}.cdr';(archive/name).write_bytes(raw)
            rows.append({'file':name,'bytes':len(raw),'sha256':A.sha(raw),'received_monotonic_ns':1+index*100000000,'firmware_timestamp':index,'resolution':360,'h264_bytes':len(unit)})
        report={'schema_version':1,'plan_sha256':A.sha(path.read_bytes()),'robot':robot,'target':target,'samples':rows,'error':None,'acceptance':'not_run','physical_identity':'operator confirmation required'}
        (archive/'recording.json').write_text(json.dumps(report))
        return archive,path,provenance,report

    def test_admits_exact_bytes_without_marking_physical_acceptance(self):
        with tempfile.TemporaryDirectory() as tmp:
            archive,plan,provenance,_=self.fixture(Path(tmp))
            result=A.validate(archive,plan,provenance,'robot-1')
            self.assertEqual(len(result['samples']),30)
            self.assertEqual(result['physical_acceptance'],'not_run')

    def test_prepare_command_copies_and_revalidates_the_complete_corpus(self):
        with tempfile.TemporaryDirectory() as tmp:
            root=Path(tmp)
            archive,plan,provenance,_=self.fixture(root)
            output=root/'prepared'
            argv=[sys.executable,str(Path(__file__).with_name('test-go2-recorded-replay.py')),'prepare',
                  '--archive',str(archive),'--plan',str(plan),'--provenance',str(provenance),
                  '--confirm-robot-serial','robot-1','--output',str(output)]
            result=subprocess.run(argv,text=True,capture_output=True,timeout=10)
            self.assertEqual(result.returncode,0,result.stderr)
            self.assertEqual(json.loads((output/'replay.json').read_text()),A.validate(output/'corpus',output/'plan.json',output/'provenance','robot-1'))
            self.assertEqual((output/'corpus/0001.cdr').read_bytes(),(archive/'0001.cdr').read_bytes())
            repeated=subprocess.run(argv,text=True,capture_output=True,timeout=10)
            self.assertNotEqual(repeated.returncode,0)

    def test_rejects_corruption_identity_reordering_and_partial_recordings(self):
        for failure in ('hash','plan','identity','receipt','path','error','extra','symlink','provenance','serial','duplicate'):
            with self.subTest(failure=failure),tempfile.TemporaryDirectory() as tmp:
                archive,plan,provenance,report=self.fixture(Path(tmp))
                serial='robot-1'
                if failure=='hash':(archive/'0000.cdr').write_bytes(b'corrupt')
                if failure=='plan':plan.write_text(plan.read_text()+' ')
                if failure=='identity':report['robot']['serial']='another robot'
                if failure=='receipt':report['samples'][1]['received_monotonic_ns']=1
                if failure=='path':report['samples'][0]['file']='../0000.cdr'
                if failure=='error':report['error']='interrupted'
                if failure=='extra':(archive/'other.cdr').write_bytes(b'extra')
                if failure=='symlink':
                    (archive/'0000.cdr').unlink();(archive/'0000.cdr').symlink_to(archive/'0001.cdr')
                if failure=='provenance':(provenance/'firmware_record').write_bytes(b'changed')
                if failure=='serial':serial='another robot'
                (archive/'recording.json').write_text(json.dumps(report))
                if failure=='duplicate':(archive/'recording.json').write_text('{"schema_version":1,"schema_version":1}')
                with self.assertRaises((ValueError,KeyError)):A.validate(archive,plan,provenance,serial)

if __name__=='__main__':unittest.main()

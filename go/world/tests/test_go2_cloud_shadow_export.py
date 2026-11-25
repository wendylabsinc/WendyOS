"""Local fake-MCP transfer tests; never invokes Wendy or a VM."""
from contextlib import redirect_stdout
from contextlib import ExitStack
import hashlib
import importlib.util
import io
import json
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import patch

ROOT=Path(__file__).resolve().parents[1]
spec=importlib.util.spec_from_file_location('shadow_export',ROOT/'tests/go2_cloud_shadow_export.py')
export=importlib.util.module_from_spec(spec);spec.loader.exec_module(export)
reader_spec=importlib.util.spec_from_file_location('shadow_report_reader',ROOT/'deploy/go2-cloud-shadow/export_report.py')
reader=importlib.util.module_from_spec(reader_spec);reader_spec.loader.exec_module(reader)
launch_spec=importlib.util.spec_from_file_location('shadow_launcher',ROOT/'deploy/go2-cloud-shadow/launch.py')
launch=importlib.util.module_from_spec(launch_spec);launch_spec.loader.exec_module(launch)

FAKE=r'''
import base64,hashlib,json,os,pathlib,sys
mode,log=sys.argv[1:]
data=b'{"synthetic":"'+b'x'*(128*1024+17)+b'"}\n'
identity=[1,2,len(data),3,4]
metadata={'bytes':len(data),'sha256':hashlib.sha256(data).hexdigest(),'identity':identity}
if mode=='whole-hash':metadata['sha256']='0'*64
if mode=='oversized':metadata['bytes']=48*1024*1024+1
for line in sys.stdin:
 req=json.loads(line)
 with open(log,'a') as stream:stream.write(json.dumps(req)+'\n')
 if 'id' not in req:continue
 if mode=='stderr-storm':
  while True:os.write(2,b'x'*8192)
 if req['method']=='initialize':result={}
 else:
  assert req['method']=='tools/call'
  name=req['params']['name'];a=req['params']['arguments']
  if name=='wendy_status':
   value={'connected':True,'command_target':{'device':'vm:wrong' if mode=='wrong-target' else 'vm:fixture'}}
  else:
   assert name=='container_exec' and a['app_name']=='com.wendy.world.go2.cloud-shadow'
   c=a['command'];assert c[:5]==['python3','/opt/world/cloud-shadow-launch.py','export','--capture','fixture']
   if '--offset' not in c:body=dict(metadata)
   else:
    offset=int(c[-1]);raw=data[offset:offset+128*1024]
    body={'offset':offset,'bytes':len(raw),'sha256':hashlib.sha256(raw).hexdigest(),'identity':identity,'base64':base64.b64encode(raw).decode()}
    if mode=='changed-chunk':body['base64']=base64.b64encode(b'z'+raw[1:]).decode()
    if mode=='changed-identity':body['identity']=[1,9,len(data),3,4]
    if mode=='short-chunk':body['base64']=base64.b64encode(raw[:-1]).decode();body['bytes']=len(raw)-1;body['sha256']=hashlib.sha256(raw[:-1]).hexdigest()
   value={'exit_code':0,'truncated':mode=='truncated','stdout':json.dumps(body)}
  result={'structuredContent':value}
 print(json.dumps({'jsonrpc':'2.0','id':req['id'],'result':result}),flush=True)
'''


class ExportTests(unittest.TestCase):
    def exercise(self,mode,error=None,sidecar=False):
        # Resolve /var symlinks used by macOS temporary directories. Production
        # refuses symlinked output parents and this test preserves that rule.
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();script=root/'fake_mcp.py';script.write_text(FAKE)
            log=root/'requests.jsonl';out=root/'capture.json';children=[]
            receipt=out.with_suffix('.json.transfer.json')
            if sidecar:receipt.write_text('existing receipt')
            original=subprocess.Popen
            def launch(command,**kwargs):
                self.assertEqual(command,['wendy','mcp','serve','--device','vm:fixture'])
                child=original([sys.executable,'-u',str(script),mode,str(log)],**kwargs)
                children.append(child);return child
            stdout=io.StringIO()
            with patch.object(export.subprocess,'Popen',side_effect=launch),redirect_stdout(stdout):
                if error:
                    with self.assertRaisesRegex(ValueError,error):
                        export.main(['--device','vm:fixture','--capture','fixture','--out',str(out)])
                else:export.main(['--device','vm:fixture','--capture','fixture','--out',str(out)])
            for child in children:
                self.assertIsNotNone(child.poll())
                self.assertTrue(all(x.closed for x in (child.stdin,child.stdout,child.stderr)))
            requests=[json.loads(x) for x in log.read_text().splitlines()] if log.exists() else []
            if error:
                self.assertFalse(out.exists());self.assertEqual(stdout.getvalue(),'')
                if sidecar:
                    self.assertEqual(receipt.read_text(),'existing receipt');self.assertEqual(children,[])
                else:self.assertFalse(receipt.exists())
            else:
                expected=b'{"synthetic":"'+b'x'*(128*1024+17)+b'"}\n'
                self.assertEqual(out.read_bytes(),expected);self.assertEqual(out.stat().st_mode&0o777,0o600)
                evidence=json.loads(receipt.read_text());announcement=json.loads(stdout.getvalue())
                self.assertEqual(announcement['sha256'],hashlib.sha256(expected).hexdigest())
                self.assertEqual(evidence['metadata']['sha256'],announcement['sha256'])
                self.assertEqual([x['offset'] for x in evidence['chunks']],[0,128*1024])
                self.assertFalse(evidence['capture_started']);self.assertFalse(evidence['operational_authority']);self.assertTrue(evidence['public_mcp_only'])
                self.assertTrue(all('base64' not in x for x in evidence['chunks']))
            if mode=='stderr-storm':
                self.assertEqual(out.with_suffix('.json.mcp-stderr').stat().st_size,export.MAX_STDERR)
            if mode=='wrong-target':
                self.assertFalse(any(r.get('params',{}).get('name')=='container_exec' for r in requests))
            for r in requests:
                if r.get('method')!='tools/call':continue
                name=r['params']['name'];self.assertIn(name,['wendy_status','container_exec'])
                if name=='container_exec':
                    a=r['params']['arguments'];command=a['command']
                    self.assertEqual(command[:5],['python3','/opt/world/cloud-shadow-launch.py','export','--capture','fixture'])
                    self.assertIn(len(command),(5,7));self.assertEqual(a['max_bytes'],250000);self.assertEqual(a['timeout_seconds'],5)

    def test_exact_chunks_whole_hash_and_no_capture(self):self.exercise('good')
    def test_changed_or_short_chunk_refuses_without_output(self):
        for mode in ('changed-chunk','changed-identity','short-chunk'):
            with self.subTest(mode=mode):self.exercise(mode,'chunk binding')
    def test_whole_hash_mismatch_refuses_without_output(self):self.exercise('whole-hash','complete report binding')
    def test_truncated_or_oversized_transfer_refuses(self):
        self.exercise('truncated','incomplete report transfer');self.exercise('oversized','report metadata')
    def test_wrong_vm_never_reads_container(self):self.exercise('wrong-target','wrong or missing VM connection')
    def test_stderr_storm_is_bounded_and_child_reaped(self):self.exercise('stderr-storm','MCP stderr bound')
    def test_existing_sidecar_is_not_overwritten(self):self.exercise('good','new transfer receipt required',sidecar=True)


class ReportReaderTests(unittest.TestCase):
    def test_metadata_chunk_bounds_and_file_identity(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();path=root/'world-cloud-shadow-fixture.json'
            data=b'x'*(reader.CHUNK+3);path.write_bytes(data)
            with patch.object(reader,'Path',side_effect=lambda v:root if v=='/tmp' else Path(v)):
                before=reader.read('fixture');first=reader.read('fixture',0);last=reader.read('fixture',reader.CHUNK)
                self.assertEqual(before['sha256'],hashlib.sha256(data).hexdigest())
                self.assertEqual(first['identity'],before['identity']);self.assertEqual(last['identity'],before['identity'])
                self.assertEqual(first['bytes'],reader.CHUNK);self.assertEqual(last['bytes'],3)
                for offset in (-1,len(data),True):
                    with self.assertRaisesRegex(ValueError,'offset'):reader.read('fixture',offset)
                with self.assertRaisesRegex(ValueError,'capture_name'):reader.read('../other')

    def test_symlink_directory_and_oversize_refuse(self):
        with tempfile.TemporaryDirectory() as temp:
            root=Path(temp).resolve();path=root/'world-cloud-shadow-fixture.json'
            target=root/'target';target.write_bytes(b'data');path.symlink_to(target)
            with patch.object(reader,'Path',side_effect=lambda v:root if v=='/tmp' else Path(v)):
                with self.assertRaises(OSError):reader.read('fixture')
                path.unlink();path.mkdir()
                with self.assertRaisesRegex(ValueError,'report_bound'):reader.read('fixture')
                path.rmdir();path.write_bytes(b'x'*33)
                with patch.object(reader,'MAX_REPORT',32),self.assertRaisesRegex(ValueError,'report_bound'):
                    reader.read('fixture')


class LaunchTests(unittest.TestCase):
    def exercise(self,operation,identity=None,fail=None,error=None):
        state=dict(identity or {'uid':0,'euid':0,'gid':0,'egid':0,'groups':[0,10]})
        events=[]
        def setter(name,value):
            events.append((name,value))
            if fail==name:raise PermissionError('injected refusal')
            if name=='setgroups':state['groups']=value
            elif name=='setgid':state['gid']=state['egid']=value
            elif name=='setuid':state['uid']=state['euid']=value
        def work(name,*args,**kwargs):
            self.assertEqual([state[k] for k in ('uid','euid','gid','egid')],[65532]*4)
            self.assertTrue(all(g==65532 for g in state['groups']))
            events.append((name,args,kwargs,list(sys.argv)))
        with ExitStack() as stack:
            for name in ('uid','euid','gid','egid','groups'):
                stack.enter_context(patch.object(launch.os,'get'+name,side_effect=lambda n=name:state[n]))
            for name in ('setgroups','setgid','setuid'):
                stack.enter_context(patch.object(launch.os,name,side_effect=lambda v,n=name:setter(n,v)))
            stack.enter_context(patch.object(launch.signal,'pause',side_effect=lambda:work('idle')))
            stack.enter_context(patch.object(launch.runpy,'run_module',side_effect=lambda *a,**k:work('module',*a,**k)))
            stack.enter_context(patch.object(launch.runpy,'run_path',side_effect=lambda *a,**k:work('path',*a,**k)))
            argv=['launcher',operation]+([] if operation=='idle' else ['--capture','fixture'])
            stack.enter_context(patch.object(sys,'argv',argv))
            if error:
                with self.assertRaises(error):launch.main()
                self.assertFalse(any(e[0] in ('idle','module','path') for e in events))
            else:launch.main()
        return events

    def test_root_drop_precedes_all_fixed_routes(self):
        for operation,kind,target in [('idle','idle',None),('capture','module','sim.go2_cloud_shadow'),('export','path','/opt/world/cloud-shadow-export.py')]:
            with self.subTest(operation=operation):
                events=self.exercise(operation)
                self.assertEqual(events[:3],[('setgroups',[]),('setgid',65532),('setuid',65532)])
                self.assertEqual(events[3][0],kind)
                if target:
                    self.assertEqual(events[3][1],(target,));self.assertEqual(events[3][2],{'run_name':'__main__'})
                    self.assertEqual(events[3][3],['launcher','--capture','fixture'])

    def test_each_failed_drop_refuses_before_work(self):
        for call in ('setgroups','setgid','setuid'):
            with self.subTest(call=call):self.exercise('capture',fail=call,error=PermissionError)

    def test_already_correct_identity_and_wrong_identity_or_groups(self):
        good={'uid':65532,'euid':65532,'gid':65532,'egid':65532,'groups':[65532]}
        self.assertEqual(self.exercise('export',good)[0][0],'path')
        for key,value in [('uid',0),('gid',0),('egid',0),('groups',[0,65532])]:
            with self.subTest(key=key):self.exercise('export',{**good,key:value},error=RuntimeError)

    def test_arbitrary_command_and_idle_extra_arguments_refuse_before_drop(self):
        for argv in (['launcher','/bin/sh'],['launcher','idle','extra'],['launcher']):
            with patch.object(sys,'argv',argv),patch.object(launch,'drop_privileges') as drop:
                with self.assertRaises(ValueError):launch.main()
                drop.assert_not_called()


if __name__=='__main__':unittest.main()

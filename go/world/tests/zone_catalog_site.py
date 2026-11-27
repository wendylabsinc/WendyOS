"""Actual signed CLI, three loopback replicas and image-derived zone presence.

Uses existing MuJoCo and MoveNet installations, disposable simulation grants,
modeled HTTP partitions and owned process SIGKILL. No physical devices.
"""
import argparse
import copy
import datetime
import hashlib
import http.client
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import socket
import subprocess
import sys
import tempfile
import threading
import time
ROOT=Path(__file__).resolve().parents[1]
sys.path.insert(0,str(ROOT))
from sim.people_demo import PeopleDemo, JSONProcess
from sim.people_scene import CAMERAS
from sim.zone_presence_demo import ZONES


def run(args):
    args.out.mkdir(parents=True,exist_ok=False,mode=0o700)
    processes={};logs=[];proxies=[];demo=None;extra=[];transcript=[]
    partition=threading.Event();reader_loss=threading.Event()
    run_id='zone-catalog-'+str(time.time_ns())
    for port in range(26200,27200,3):
        held=[]
        try:
            for n in range(3):
                sock=socket.socket();sock.bind(('127.0.0.1',port+n));held.append(sock)
            break
        except OSError:
            for sock in held:sock.close()
    else:raise RuntimeError('no loopback port block')
    for sock in held:sock.close()
    with tempfile.TemporaryDirectory(prefix='world-zone-catalog-') as tmp:
        folder=Path(tmp)
        def write(name,value):
            path=folder/name;path.write_text(json.dumps(value));path.chmod(0o600);return path
        def command(name,*argv,okay=True):
            p=subprocess.run([str(args.bin_dir/name),*map(str,argv)],cwd=ROOT,capture_output=True,timeout=25)
            if okay and p.returncode:raise AssertionError(p.stderr.decode()[:1500])
            if not okay:
                assert p.returncode,'expected refusal';return p.stderr.decode()[:300]
            return json.loads(p.stdout) if name in ('world','zone_catalog_replay') and p.stdout.strip() else None
        def cli(n,verb,*argv,okay=True,credential=None):
            flags=[] if verb=='publish' else ['--trust',folder/'site-trust.json']
            return command('world','zone',verb,'--credentials',credential or folder/f'site-admin-{n}.json',*flags,*argv,'--json',okay=okay)
        def until(fn,predicate=lambda value:bool(value),timeout=35):
            deadline=time.monotonic()+timeout;last=None
            while time.monotonic()<deadline:
                try:
                    last=fn()
                    if predicate(last):return last
                except (AssertionError,RuntimeError) as e:last=str(e)
                time.sleep(.15)
            raise AssertionError(last)
        def proxy(target,blocked):
            class Handler(BaseHTTPRequestHandler):
                def log_message(self,*_):pass
                def do_GET(self):
                    if blocked.is_set():self.send_error(503,'modeled partition');return
                    c=http.client.HTTPConnection('127.0.0.1',target,timeout=3)
                    try:
                        body=self.rfile.read(int(self.headers.get('Content-Length','0')))
                        c.request(self.command,self.path,body=body,headers=dict(self.headers))
                        r=c.getresponse();body=r.read();self.send_response(r.status);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
                    except (OSError,http.client.HTTPException):self.send_error(503,'peer unavailable')
                    finally:c.close()
                do_POST=do_GET
            server=ThreadingHTTPServer(('127.0.0.1',0),Handler);threading.Thread(target=server.serve_forever,daemon=True).start();proxies.append(server);return f'http://127.0.0.1:{server.server_port}'
        def start(n):
            log=(args.out/f'node-{n}.log').open('a');logs.append(log)
            processes[n]=subprocess.Popen([str(args.bin_dir/'worldd'),'--config',str(folder/f'node-{n}.json')],stdout=log,stderr=log)
        def record(stage,**data):
            transcript.append({'stage':stage,**data});(args.out/'progress.json').write_text(json.dumps(transcript,indent=2))
        try:
            command('world-lab','--out',folder,'--base-port',port,'--run-id',run_id)
            configs={n:json.loads((folder/f'node-{n}.json').read_text()) for n in (1,2,3)}
            configs[1]['peers']=[{'url':proxy(port+1,partition),'audience':'node-2'}]
            configs[2]['peers']=[{'url':proxy(port,partition),'audience':'node-1'}]
            configs[3]['peers']=[{'url':f'http://127.0.0.1:{port+1}','audience':'node-2'}]
            for n in (1,2,3):
                configs[n]['sync_interval_ms']=400+n*173
                write(f'node-{n}.json',configs[n]);start(n)
            until(lambda:cli(3,'list'))
            perception=args.out/'perception';perception.mkdir(mode=0o700)
            demo=PeopleDemo(perception,args.pose_python,ROOT/'perception/pose.py',args.model,args.bin_dir/'world-people',run_id)
            original_pose=demo.scene.pose
            def stationary(seconds):original_pose(0.);demo.scene.data.time=seconds
            demo.scene.pose=stationary
            sample=demo.sample
            def scoped_sample(*argv):value=sample(*argv);value['site_id']='synthetic-office';return value
            demo.sample=scoped_sample
            now=datetime.datetime.now(datetime.timezone.utc)
            doc={'schema_version':'wendy.zone.catalog.v1','name':'Office work areas','revision':'operator-edit','site':'synthetic-office','run_id':run_id,'frame':demo.frame,'valid_from':(now-datetime.timedelta(minutes=1)).isoformat(),'valid_until':(now+datetime.timedelta(minutes=20)).isoformat(),'zones':copy.deepcopy(ZONES)}
            path=write('catalog.json',doc);published=cli(1,'publish','--catalog',path);digest=published['publication']['sha256'];source=write('source.json',published['source'])
            def fetch(n,sha=digest):return cli(n,'fetch','--sha256',sha,'--source',source)
            initial=until(lambda:fetch(3));original=write('original-record.json',initial['signed_record'])
            assert initial['publication']==published['publication']
            reader=json.loads((folder/'site-admin-3.json').read_text());reader['url']=proxy(port+2,reader_loss);reader_path=write('reader.json',reader)
            def tracker(sha,label):
                p=JSONProcess([str(args.bin_dir/'world-people'),'--zone-credentials',str(reader_path),'--zone-trust',str(folder/'site-trust.json'),'--zone-source',str(source),'--zone-sha256',sha],args.out/(label+'.log'));extra.append(p);return p
            demo.tracker.close();demo.tracker=tracker(digest,'pinned-first')
            timeline=[];seconds=0
            def tick():
                nonlocal seconds
                seconds+=.25;state,request=demo.tick(seconds);result=state['snapshot']['tracking'];timeline.append({'request':request,'result':result});return result,request
            results=[tick()[0] for _ in range(7)]
            assert any('dwell' in [e['kind'] for e in r['presence']['events']] for r in results),'no image-derived dwell'
            assert any(z['observed']>0 for z in results[-1]['presence']['zones'])
            for camera in CAMERAS:(args.out/(camera+'.png')).write_bytes((demo.images/(camera+'.png')).read_bytes())
            record('published_and_image_presence',digest=digest,source=published['source'],replicas=3,captures=len(results),result=results[-1])
            partition.set();doc['zones'][0]['min_xz']=[-9,-9];doc['zones'][0]['max_xz']=[-8,-8];doc['zones'][1]['min_xz']=[8,8];doc['zones'][1]['max_xz']=[9,9];write('catalog.json',doc)
            edited=cli(1,'publish','--catalog',path);second=edited['publication']['sha256'];assert second!=digest
            assert len(cli(3,'list')['records'])==1
            processes[2].kill();processes[2].wait(timeout=5);start(2);until(lambda:fetch(2));assert fetch(3)['publication']==initial['publication']
            reader_loss.set();cached,_=tick();assert cached['zone_catalog']['state']=='cached_snapshot' and cached['zone_catalog']['current_remote_state']=='unknown'
            record('partition_restart_cached_geometry',new_digest_at_origin=second,remote_revisions=1,cached_current_state='unknown',result=cached)
            reader_loss.clear();partition.clear();until(lambda:cli(3,'list'),lambda r:len(r['records'])==2)
            kept,request=tick();assert kept['zone_catalog']['sha256']==digest
            replacement=tracker(second,'selected-second');changed=replacement.call(request)
            assert all(z['observed']==0 for z in changed['presence']['zones'])
            assert any(z['observed']>0 for z in kept['presence']['zones'])
            record('explicit_new_session_selection',old_digest=digest,new_digest=second,active_session_unchanged=True,old_result=kept,new_result=changed)
            deleted=cli(1,'delete','--sha256',digest,'--source',source);assert deleted['deleted']
            until(lambda:fetch(3),lambda r:r['state']=='deleted_at_replica')
            stopped,_=tick();assert stopped['zone_catalog']['state']=='deleted_at_replica' and not stopped['tracks']
            assert all(z['state']=='unknown' and not z['occupants'] for z in stopped['presence']['zones'])
            replay=command('zone_catalog_replay',folder/'site-admin-1.json',original,okay=False)
            assert 'current epoch' in replay
            assert fetch(1)['state']=='deleted_at_replica'
            processes[2].kill();processes[2].wait(timeout=5);processes[3].kill();processes[3].wait(timeout=5);start(3)
            restored=until(lambda:fetch(3));assert restored['state']=='deleted_at_replica'
            record('delete_replay_and_isolated_restart',deleted=True,replay_result=replay,restarted_without_peer=True,result=stopped)
            # Invalid geometry cannot publish, and a reader cannot gain edit authority.
            broken=copy.deepcopy(doc);broken['zones'][0]['min_xz'][0]=None;bad=write('bad.json',broken)
            refusal=cli(1,'publish','--catalog',bad,okay=False)
            denied=cli(1,'publish','--catalog',path,credential=folder/'client-1.json',okay=False)
            assert 'operator publish and site-publish authority' in denied
            record('invalid_geometry_and_publisher_authority',malformed_refused=bool(refusal),ordinary_publisher_refused=bool(denied),current_second_revision=fetch(3,second)['publication']['sha256'])
            report={'passed':True,'simulation_only':True,'nodes':3,'image_pipeline':'MuJoCo RGB -> existing MoveNet ONNX -> calibrated projection -> anonymous tracks -> signed selected zones','captures':len(timeline),'stages':transcript,'limits':['modeled loopback HTTP partition, not physical radio loss','process SIGKILL, not physical power-loss proof','synthetic people/geometry; no measured camera accuracy','no tasks, robot motion, enrollment or hardware','cached geometry is labeled current state unknown during reader link loss','CLI and JSON stream only; native zone-editing UI is not implemented']}
            (args.out/'timeline.json').write_text(json.dumps(timeline))
            (args.out/'acceptance.json').write_text(json.dumps(report,indent=2));return report
        finally:
            if demo:demo.close()
            for p in extra:
                if p.process.poll() is None:p.close()
            for p in processes.values():
                if p.poll() is None:p.terminate()
            for p in processes.values():
                try:p.wait(timeout=5)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=5)
            for s in proxies:s.shutdown();s.server_close()
            for log in logs:log.close()

if __name__=='__main__':
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--bin-dir',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--pose-python',type=Path,required=True);p.add_argument('--model',type=Path,required=True);a=p.parse_args();os.umask(0o077);r=run(a);print(json.dumps({'passed':r['passed'],'captures':r['captures'],'report':str(a.out/'acceptance.json')}))

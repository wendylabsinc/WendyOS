"""Three actual standalone nodes: read-only mission audit repair and process loss.

No simulator/executor is launched: tasks are accepted, explicitly stopped or
failed on assignee restart. This tests durable decisions, not physical motion.
"""
import argparse
import hashlib
import http.client
import http.server
import json
import os
from pathlib import Path
import subprocess
import threading
import time

p=argparse.ArgumentParser(description=__doc__);p.add_argument('--bin-dir',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--port',type=int,default=19610);a=p.parse_args()
os.umask(0o077);out=a.out.resolve();out.mkdir(parents=True,mode=0o700,exist_ok=False);private=out/'private';processes=[];logs=[];checks=[];partition=threading.Event()
def write(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value,indent=2));tmp.replace(path)
def run(args):return subprocess.check_output([str(x) for x in args],stderr=subprocess.PIPE)
def until(fn,predicate,timeout=12):
    end=time.monotonic()+timeout;last=None
    while time.monotonic()<end:
        try:
            last=fn()
            if predicate(last):return last
        except (subprocess.CalledProcessError,KeyError,ValueError,OSError) as e:last=str(e)
        time.sleep(.06)
    raise AssertionError(str(last))
class Proxy(http.server.BaseHTTPRequestHandler):
    def forward(self):
        if partition.is_set():self.send_error(503,'partition');return
        body=self.rfile.read(int(self.headers.get('Content-Length',0)))
        c=http.client.HTTPConnection('127.0.0.1',a.port,timeout=3)
        c.request(self.command,self.path,body,{'X-World-Authorization':self.headers.get('X-World-Authorization',''),'Content-Type':'application/json'})
        r=c.getresponse();data=r.read(2<<20);c.close();self.send_response(r.status);self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
    do_POST=forward;do_GET=forward
    def log_message(self,*_):pass
proxy=http.server.ThreadingHTTPServer(('127.0.0.1',0),Proxy);thread=threading.Thread(target=proxy.serve_forever,daemon=True);thread.start()
def launch(i):
    log=(out/f'node-{i}.log').open('a');logs.append(log);proc=subprocess.Popen([str(a.bin_dir/'worldd'),'--config',str(private/f'node-{i}.json')],stdout=log,stderr=log);processes.append(proc);return proc
def cli(i,verb,*args,viewer=False):
    credential=private/(f'viewer-{i}.json' if viewer else f'operator-{i}.json')
    return json.loads(run([a.bin_dir/'world','task',verb,'--credentials',credential,'--json',*args]))
def history(i,task=''):
    args=['--origin','node-1','--limit','100']
    if task:args+=['--id',task]
    return cli(i,'history',*args,viewer=True)
def submit(task):
    spec={'id':task,'type':'wendy.nav.go_to.v1','site':'synthetic-office','assignee':'sim-robot/navigation','domain':'simulation','class':'MOVE','mode':'autonomous','frame':{'id':'office-floor','revision':'office-frame-v1'},'not_after_unix_ms':int(time.time()*1000)+120000,'area':{'min':[-2.1,.1,-.1],'max':[2.1,.3,.1]},'required_sensors':['gas','local-obstacles'],'minimum_energy':.2,'fallback':'hold','target':[1.2,.2,0],'speed_mps':.4,'tolerance_m':.03,'stop_speed_mps':.03}
    path=private/(task+'.json');write(path,spec);return cli(1,'send','--spec',path)
def payload(view):return [e['event'] for e in view['events']]
try:
    run([a.bin_dir/'world-lab','--out',private,'--run-id','mission-history-demo','--base-port',a.port,'--missions','--sensors','--task-relay'])
    viewer=json.loads((private/'viewer-2.json').read_text());viewer['url']=f'http://127.0.0.1:{a.port+2}';viewer['audience']='node-3';write(private/'viewer-3.json',viewer)
    for i in (1,2,3):
        path=private/f'node-{i}.json';cfg=json.loads(path.read_text());cfg.pop('task_relay',None)
        cfg['peers']=[] if i==1 else [{'audience':f'node-{i-1}','url':f'http://127.0.0.1:{proxy.server_port if i==2 else a.port+1}'}]
        cfg['task_history']={'path':str(private/f'node-{i}.history'),'export':i==1,'origins':{'node-1':{'principal':'lab/node-1','assignee':'sim-robot/navigation','run_id':'mission-history-demo'}},'max_entries':2 if i==3 else 1000}
        write(path,cfg)
    nodes={i:launch(i) for i in (1,2,3)}
    until(lambda:history(1),lambda x:x['historical'])
    submit('review-inspection');one=until(lambda:history(3),lambda x:len(x['events'])==1)
    assert one['events'][0]['event']['event']=='accepted' and one['events'][0]['event']['status']['Phase']=='accepted', one['events'][0]['event']
    write(out/'01-accepted.json',one);checks.append('actual signed acceptance repaired across two hops')
    partition.set();cli(1,'stop','--id','review-inspection');submit('interrupted-inspection')
    nodes[1].kill();nodes[1].wait();nodes[1]=launch(1)
    full=until(lambda:history(1),lambda x:len(x['events'])>=4);write(out/'02-assignee-after-restart.json',full)
    assert any(e['event']['event']=='restart' for e in full['events']);checks.append('assignee restart failed pending work and preserved audit incarnation')
    assert sum(e['event']['event']=='accepted' and e['event']['status']['Phase']=='accepted' for e in full['events'])==2
    assert payload(history(2))==payload(one);nodes[2].kill();nodes[2].wait();nodes[2]=launch(2)
    retained=until(lambda:history(2),lambda x:len(x['events'])==1);assert payload(retained)==payload(one);write(out/'03-partition-reader-reopened.json',retained);checks.append('partition and reader SIGKILL preserve last-known prefix')
    partition.clear();repaired=until(lambda:history(2),lambda x:payload(x)==payload(full));checks.append('ordered history repaired after reconnect')
    time.sleep(.5);limited=history(3);assert payload(limited)==payload(one) and limited['progress']['through']==1;checks.append('actual quota failure preserves prior rows and cursor atomically');write(out/'04-quota-refused.json',limited)
    nodes[3].kill();nodes[3].wait();path=private/'node-3.json';cfg=json.loads(path.read_text());cfg['task_history']['max_entries']=1000;write(path,cfg);nodes[3]=launch(3)
    terminal=until(lambda:history(3),lambda x:payload(x)==payload(full));write(out/'05-repaired-timeline.json',terminal)
    assert [x['receipt'] for x in repaired['events']]==[x['receipt'] for x in terminal['events']];checks.append('relay preserves exact origin attestations and original operator proofs')
    partition.set();nodes[3].kill();nodes[3].wait();nodes[3]=launch(3)
    reopened=until(lambda:history(3),lambda x:payload(x)==payload(terminal));write(out/'06-offline-timeline.json',reopened);checks.append('terminal history remains readable after isolated reader restart')
    assert not (private/'node-2.tasks').exists() and not (private/'node-3.tasks').exists();assert all(not x['executable'] for x in reopened['events']);checks.append('history-only readers have no task stores or executable events')
    selected=history(3,'review-inspection');assert len(selected['events'])==2 and all(x['event']['task_id']=='review-inspection' for x in selected['events']);write(out/'07-task-selected.json',selected);checks.append('CLI exact-task selection retains original sequence positions')
    if os.environ.get('MISSION_HISTORY_KEEP_RUNNING')=='1':
        write(out/'ready.json',{'private':str(private),'reader':'node-3','task':'review-inspection'})
        end=time.monotonic()+180
        while time.monotonic()<end and not (out/'finish').exists():time.sleep(.1)
    write(out/'acceptance.json',{'passed':True,'scope':'Three actual authenticated standalone worldd processes and world CLI; no executor, simulator or physical motion','checks':checks,'nodes':3,'timeline_sha256':hashlib.sha256(json.dumps(payload(terminal),sort_keys=True,separators=(',',':')).encode()).hexdigest()})
finally:
    for proc in processes:
        if proc.poll() is None:proc.terminate()
    for proc in processes:
        try:proc.wait(timeout=5)
        except subprocess.TimeoutExpired:proc.kill();proc.wait()
    proxy.shutdown();proxy.server_close();thread.join(timeout=2)
    for log in logs:log.close()
    write(out/'cleanup.json',{'all_exited':all(x.poll() is not None for x in processes),'owned_pids':[x.pid for x in processes],'exit_codes':[x.returncode for x in processes]})

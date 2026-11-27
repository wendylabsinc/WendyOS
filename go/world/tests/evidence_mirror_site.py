"""Three real loopback nodes, real CLI, controlled partitions and SIGKILL.
The PNG is explicitly synthetic deterministic noise, not a captured observation.
"""
import argparse, base64, hashlib, http.client, http.server, json, os
from pathlib import Path
import subprocess, threading, time
p=argparse.ArgumentParser(description=__doc__)
p.add_argument('--bin-dir',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--port',type=int,default=19810);a=p.parse_args()
os.umask(0o077);out=a.out.resolve();out.mkdir(parents=True,mode=0o700,exist_ok=False);private=out/'private';processes=[];logs=[];proxies=[];checks=[];transcript=[]
def write(path,value):
    tmp=path.with_suffix('.new');tmp.write_text(json.dumps(value,indent=2,sort_keys=True));tmp.replace(path)
def wait(fn,predicate=lambda x:bool(x),seconds=15):
    end=time.monotonic()+seconds;last=None
    while time.monotonic()<end:
        try:
            last=fn()
            if predicate(last):return last
        except (ValueError,OSError,subprocess.CalledProcessError,AssertionError) as e:last=str(e)
        time.sleep(.04)
    raise AssertionError(str(last))
def start(args,name):
    log=(out/(name+'.log')).open('a');logs.append(log);p=subprocess.Popen([str(x)for x in args],stdout=log,stderr=log);processes.append(p);return p
def stop(p,kill=False):
    if p.poll() is None:
        p.kill() if kill else p.terminate()
        try:p.wait(5)
        except subprocess.TimeoutExpired:p.kill();p.wait()
def node(i):return start([a.bin_dir/'worldd','--config',private/f'node-{i}.json'],f'node-{i}')
def run(args,expected=0):
    p=subprocess.run([str(x)for x in args],capture_output=True,text=True,timeout=30)
    if p.returncode!=expected:raise AssertionError((p.returncode,p.stdout,p.stderr))
    return json.loads(p.stdout) if p.stdout.strip() else None
class Fault:
    def __init__(self,node):
        self.node=node;self.mode='';self.delivered=threading.Event();self.release=threading.Event();self.offsets=[];self.events=[];self.mu=threading.Lock()
        control=self
        class Handler(http.server.BaseHTTPRequestHandler):
            def do_POST(self):
                raw=self.rfile.read(int(self.headers.get('Content-Length',0)));q=json.loads(raw)
                if control.mode=='offline':self.send_error(503,'partition');return
                if self.path.endswith('/get') and q.get('length',0)>1 and control.mode=='download_hold' and q['offset']>=49152:
                    control.delivered.set();control.release.wait(15);return
                if self.path.endswith('/put') and q.get('data'):control.offsets.append(q['offset'])
                c=http.client.HTTPConnection('127.0.0.1',a.port+control.node-1,timeout=10)
                try:
                    c.request('POST',self.path,raw,{'X-World-Authorization':self.headers.get('X-World-Authorization',''),'Content-Type':'application/json'})
                    r=c.getresponse();data=r.read(1<<20);status=r.status
                finally:c.close()
                reply=json.loads(data) if status==200 else {}
                with control.mu:
                    control.events.append({'path':self.path,'request_offset':q.get('offset'),'request_length':q.get('length'),'request_data_bytes':len(base64.b64decode(q.get('data') or '')),'response_status':status,'received':reply.get('received'),'complete':reply.get('complete'),'retain_until_unix_ms':reply.get('retain_until_unix_ms'),'mode':control.mode})
                    write(out/f'fault-node-{control.node}.json',control.events)
                if self.path.endswith('/put') and q.get('data') and status==200 and control.mode in ('upload_hold','final_lost_ack'):
                    complete=json.loads(data).get('complete')
                    if control.mode=='upload_hold' or complete:
                        control.delivered.set();control.release.wait(15);return
                try:self.send_response(status);self.send_header('Content-Length',str(len(data)));self.end_headers();self.wfile.write(data)
                except (BrokenPipeError,ConnectionResetError):pass
            def log_message(self,*_):pass
        self.server=http.server.ThreadingHTTPServer(('127.0.0.1',0),Handler);self.server.daemon_threads=True
        self.thread=threading.Thread(target=self.server.serve_forever,daemon=True);self.thread.start();proxies.append(self)
    def set(self,mode):self.mode=mode;self.delivered.clear();self.release.clear()
    def clear(self):self.mode='';self.release.set()
    def close(self):self.clear();self.server.shutdown();self.server.server_close();self.thread.join(2)
def credential_copy(source,target,port):
    c=json.loads(source.read_text());c['url']=f'http://127.0.0.1:{port}';write(target,c)
def mirror(verb,path,source=True,destination=True,extra=None,expected=0):
    args=[a.bin_dir/'world','evidence','mirror',verb,'--state-dir',path,'--json']
    if source:args+=['--source',private/'source.json']
    if destination:args+=['--destination',private/'destination.json']
    args+=extra or [];value=run(args,expected)
    shown=[str(x).replace(str(private),'$PRIVATE').replace(str(out),'$ARTIFACTS')for x in args]
    transcript.append({'argv':shown,'result':value,'expected_exit':expected});write(out/'transcript.json',transcript);return value
try:
    subprocess.run([str(x)for x in [a.bin_dir/'world-lab','--out',private,'--run-id','evidence-mirror-demo','--base-port',a.port,'--sensors']],check=True,stdout=subprocess.DEVNULL)
    for i in (1,2,3):
        path=private/f'node-{i}.json';cfg=json.loads(path.read_text());cfg['peers']=[]
        if i==3:cfg['evidence']['quota_bytes']=1<<20;cfg['evidence']['max_blob_bytes']=1<<20
        write(path,cfg)
    for i in (2,3):
        run([a.bin_dir/'mirrorfixture','--credentials',private/f'site-admin-{i}.json','--issuer',private/'issuer.json','--bind-writer',f'node-{i}','--out',private/f'writer-{i}.json'])
    nodes={i:node(i)for i in (1,2,3)};src=Fault(1);dst=Fault(2)
    credential_copy(private/'viewer-1.json',private/'source.json',src.server.server_port)
    credential_copy(private/'writer-2.json',private/'destination.json',dst.server.server_port)
    deadline=int(time.time()*1000)+180000
    wait(lambda:run([a.bin_dir/'mirrorfixture','--credentials',private/'site-admin-1.json','--out',out/'reference.json','--until',deadline]))
    reference=json.loads((out/'reference.json').read_text());assert reference['size']>3*49152
    run([a.bin_dir/'mirrorfixture','--credentials',private/'writer-3.json','--out',out/'filler.json','--kind','filler','--until',deadline])
    state=out/'selected-mirror';prepared=mirror('prepare',state,extra=['--id','inspection-copy','--reference',out/'reference.json']);assert prepared['plan']['retain_until_unix_ms']==deadline
    write(out/'01-prepared.json',prepared);checks.append('authoritative original metadata persisted before any destination allocation')
    src.set('download_hold');proc=start([a.bin_dir/'world','evidence','mirror','run','--state-dir',state,'--source',private/'source.json','--destination',private/'destination.json'],'download-interrupted')
    assert src.delivered.wait(10);stop(proc,True);src.clear()
    interrupted=mirror('status',state,False,False);write(out/'02-download-interrupted.json',interrupted);checks.append('actual CLI SIGKILL after a durable source download checkpoint')
    dst.set('upload_hold');proc=start([a.bin_dir/'world','evidence','mirror','run','--state-dir',state,'--source',private/'source.json','--destination',private/'destination.json'],'upload-interrupted')
    assert dst.delivered.wait(10);stop(proc,True);dst.clear();stop(nodes[2],True);nodes[2]=node(2);time.sleep(.2)
    partial=mirror('status',state,False,False);assert partial['source_resumed_bytes']==49152;write(out/'03-upload-ack-unknown.json',partial);checks.append('download resumed exactly49152; destination committed a chunk before lost ACK and process restart')
    dst.offsets=[];dst.set('final_lost_ack');proc=start([a.bin_dir/'world','evidence','mirror','run','--state-dir',state,'--source',private/'source.json','--destination',private/'destination.json'],'completion-interrupted')
    assert dst.delivered.wait(10);assert dst.offsets[0]==49152;stop(proc,True);dst.clear()
    before=mirror('status',state,False,False);assert before['phase']!='verified';write(out/'04-completion-uncertain.json',before);checks.append('final destination commit without ACK remains unverified after CLI SIGKILL')
    complete=mirror('run',state);assert complete['phase']=='verified' and complete['plan']['retain_until_unix_ms']==deadline;write(out/'05-reconciled.json',complete);checks.append('exact retry reconciles complete upload and independently verifies destination digest')
    # A second distinct synthetic object has a genuinely short source deadline.
    short_deadline=int(time.time()*1000)+4000
    run([a.bin_dir/'mirrorfixture','--credentials',private/'site-admin-1.json','--out',out/'short-reference.json','--until',short_deadline,'--seed',1730])
    short_state=out/'expired-mirror'
    mirror('prepare',short_state,extra=['--id','short-copy','--reference',out/'short-reference.json'])
    time.sleep(max(0,(short_deadline-int(time.time()*1000))/1000)+.05)
    expired=mirror('run',short_state,expected=1);assert expired['phase']=='expired' and expired['destination_received']==0
    checks.append('original source hard expiry prevents allocation and is durable in mirror status');write(out/'source-expired.json',expired)
    quota=out/'quota-mirror'
    mirror('prepare',quota,destination=False,extra=['--destination',private/'writer-3.json','--id','quota-copy','--reference',out/'reference.json'])
    denied=mirror('run',quota,destination=False,extra=['--destination',private/'writer-3.json'],expected=1);assert denied['last_error']=='quota';checks.append('third real node quota refuses mirror without deleting unrelated protected blob');write(out/'06-quota.json',denied)
    write(private/'filler-sources.json',{'sources':[{'id':'quota-node','credentials':str(private/'writer-3.json')}]})
    filler=run([a.bin_dir/'world','evidence','fetch','--sources',private/'filler-sources.json','--reference',out/'filler.json','--cache-dir',out/'filler-cache','--out',out/'filler-copy.bin','--json'])
    assert hashlib.sha256((out/'filler-copy.bin').read_bytes()).hexdigest()==json.loads((out/'filler.json').read_text())['sha256']
    # Current authenticated reads, including a fully cached source, remain necessary for writes.
    stop(nodes[1],True)
    run([a.bin_dir/'mirrorfixture','--credentials',private/'source.json','--revoke-node',private/'node-1.json','--issuer',private/'issuer.json'])
    nodes[1]=node(1);time.sleep(.2)
    revoked=mirror('run',state,expected=1);assert revoked['last_error']=='denied' and revoked['verified_unix_ms']==complete['verified_unix_ms']
    checks.append('source grant revocation blocks new writes despite complete local cache');write(out/'source-revoked.json',revoked)
    src.set('offline');stop(nodes[1],True)
    verified=mirror('verify',state,False,True);assert verified['phase']=='verified';write(out/'07-source-offline.json',verified)
    write(private/'fetch-sources.json',{'sources':[{'id':'capture','credentials':str(private/'source.json')},{'id':'mirror','credentials':str(private/'destination.json')}]})
    fetched=run([a.bin_dir/'world','evidence','fetch','--sources',private/'fetch-sources.json','--reference',out/'reference.json','--cache-dir',out/'reader-cache','--out',out/'retrieved.png','--json'])
    assert fetched['source']=='mirror' and hashlib.sha256((out/'retrieved.png').read_bytes()).hexdigest()==reference['sha256'] and fetched['retain_until_unix_ms']==deadline
    checks.append('existing fetch falls back to verified replica while original capture node is stopped');write(out/'08-fallback.json',fetched)
    status=mirror('status',state,False,False);assert not status['current_verification'] and not status['original_capture_owner_claimed']
    checks.append('status labels availability as last checked and never claims capture ownership')
    write(out/'acceptance.json',{'passed':True,'scope':'Actual three loopback World nodes and CLI, synthetic deterministic PNG and quota bytes; no simulator or original capture claim','reference':reference,'original_retain_until_unix_ms':deadline,'checks':checks,'destination_write_offsets_after_restart':dst.offsets,'fixture_sha256':hashlib.sha256((out/'reference.json.bytes').read_bytes()).hexdigest()})
finally:
    for proxy in proxies:proxy.clear()
    for proc in processes:stop(proc)
    for proxy in proxies:proxy.close()
    for log in logs:log.close()
    write(out/'cleanup.json',{'all_exited':all(p.poll()is not None for p in processes),'owned_processes':[{'pid':p.pid,'exit':p.returncode}for p in processes]})

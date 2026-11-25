"""Attach a read-only three-node debrief to an existing local simulated patrol.

The executor and original signed mission are untouched. These loopback fault
proxies change delivery only; media remains at the capture node. Reader folders
can be opened with WendyWorldDemo --patrol-debrief /absolute/reader-east.
"""
from __future__ import annotations
import argparse
import base64
import http.client
import http.server
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import threading
import time


def atomic(path, value):
    path = Path(path)
    temp = path.with_name('.'+path.name+'.pending')
    temp.write_text(json.dumps(value, indent=2)+'\n'); temp.chmod(0o600); temp.replace(path)


def free_ports():
    for base in range(19720, 24720, 11):
        held = []
        try:
            for port in range(base, base+6):
                s=socket.socket(); s.bind(('127.0.0.1', port)); held.append(s)
            return base
        except OSError:
            pass
        finally:
            for s in held: s.close()
    raise RuntimeError('six loopback ports unavailable')


class Proxy(http.server.ThreadingHTTPServer):
    daemon_threads=True
    block_on_close=False
    def __init__(self, port, destination, target, state):
        self.destination,self.target,self.state=destination,target,state
        self.slots=threading.BoundedSemaphore(8)
        super().__init__(('127.0.0.1',port),ProxyRequest)
        self.thread=threading.Thread(target=self.serve_forever,daemon=True);self.thread.start()


class ProxyRequest(http.server.BaseHTTPRequestHandler):
    def log_message(self,*args): pass
    def do_GET(self): self.forward()
    def do_POST(self): self.forward()
    def forward(self):
        if not self.server.slots.acquire(blocking=False): self.send_error(503);return
        conn=None
        try:
            self.connection.settimeout(3)
            length=int(self.headers.get('Content-Length','0'))
            if not 0<=length<=2*1024*1024: self.send_error(413);return
            auth=json.loads(base64.b64decode(self.headers.get('X-World-Authorization',''),validate=True))
            source=auth['grant']['grant']['source']['device']
            if self.server.state['partition'] and (source=='patrol-node-1' or self.server.target=='patrol-node-1'):
                self.server.state['blocked']+=1;self.send_error(503,'deliberate debrief partition');return
            body=self.rfile.read(length)
            conn=http.client.HTTPConnection('127.0.0.1',self.server.destination,timeout=3)
            conn.request(self.command,self.path,body,{k:v for k,v in self.headers.items() if k.lower() not in ('host','connection')})
            response=conn.getresponse();raw=response.read(16*1024*1024+1)
            if len(raw)>16*1024*1024:self.send_error(502);return
            self.send_response(response.status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        except (ValueError,KeyError,TimeoutError,OSError):
            try:self.send_error(502)
            except OSError:pass
        finally:
            if conn:conn.close()
            self.server.slots.release()


class ReplicaSite:
    def __init__(self,run_path,out,bin_dir,base=None):
        self.out=Path(out).resolve();self.bin=Path(bin_dir).resolve();self.processes={};self.logs=[];self.proxies=[];self.state={'partition':False,'blocked':0};self.base=base or free_ports()
        self.run=json.loads(Path(run_path).read_text());self.run_path=Path(run_path).resolve()
        if not self.run['mission_url'].startswith('http://127.0.0.1:'):raise ValueError('local simulation required')
        self.out.mkdir(mode=0o700,parents=True,exist_ok=False)
        try:
            subprocess.run([str(self.bin/'world-patrol-replica'),'setup','--out',str(self.out/'config'),'--status-url',self.run['mission_url']+'/status','--captures',str(self.run_path.parent/'executor/captures'),'--base-port',str(self.base)],check=True,timeout=10)
            for n in range(1,4):
                self.proxies.append(Proxy(self.base+3+n-1,self.base+n-1,f'patrol-node-{n}',self.state))
                self.spawn(f'node-{n}',[str(self.bin/'worldd'),'--config',str(self.out/f'config/patrol-node-{n}.json')])
            self.public=json.loads((self.out/'config/public.json').read_text())
            time.sleep(0.7)
            self.start_publisher()
            for name in ('east','west'):
                self.spawn('reader-'+name,[str(self.bin/'world-patrol-replica'),'read','--config',str(self.out/f'config/reader-{name}.json'),'--out',str(self.out/f'reader-{name}')])
            atomic(self.out/'site.json',{'run_id':self.run['run_id'],'base_port':self.base,'pids':{k:p.pid for k,p in self.processes.items()},'publisher_has_motion_credentials':False,'reader_has_motion_credentials':False,'retention_deadline_unix_ms':self.public['retain_until_unix_ms']})
        except Exception:
            self.close();raise
    def spawn(self,name,argv):
        log=(self.out/(name+'.log')).open('a');self.logs.append(log)
        p=subprocess.Popen(argv,stdout=log,stderr=subprocess.STDOUT);self.processes[name]=p;return p
    def start_publisher(self):
        return self.spawn('publisher',[str(self.bin/'world-patrol-replica'),'publish','--config',str(self.out/'config/publisher.json'),'--out',str(self.out/'publisher')])
    def restart_publisher(self):
        self.stop('publisher');self.start_publisher()
    def stop(self,name):
        p=self.processes.get(name)
        if p and p.poll() is None:
            p.terminate()
            try:p.wait(timeout=3)
            except subprocess.TimeoutExpired:p.kill();p.wait(timeout=3)
    def partition(self,value):
        self.state['partition']=bool(value);atomic(self.out/'partition.json',dict(self.state,changed_unix_ms=int(time.time()*1000)))
    def view(self,name):
        return json.loads((self.out/f'reader-{name}/state.json').read_text())
    def close(self):
        for name in reversed(list(self.processes)):self.stop(name)
        for proxy in self.proxies:proxy.shutdown();proxy.server_close()
        for log in self.logs:log.close()
        if self.out.exists():atomic(self.out/'cleanup.json',{'pids':{k:{'pid':p.pid,'returncode':p.poll()} for k,p in self.processes.items()},'proxies_closed':True,'blocked_requests':self.state['blocked']})


def main():
    parser=argparse.ArgumentParser(description=__doc__);parser.add_argument('--run',type=Path,required=True);parser.add_argument('--out',type=Path,required=True);parser.add_argument('--bin-dir',type=Path,required=True);parser.add_argument('--duration',type=int,default=1800);args=parser.parse_args()
    if not 1<=args.duration<=2700:parser.error('duration must be 1..2700 seconds')
    stop=threading.Event()
    for sig in (signal.SIGINT,signal.SIGTERM):signal.signal(sig,lambda *_:stop.set())
    site=ReplicaSite(args.run,args.out,args.bin_dir)
    try:
        print(json.dumps({'reader_east':str(site.out/'reader-east'),'reader_west':str(site.out/'reader-west'),'site':str(site.out/'site.json')}),flush=True)
        end=time.monotonic()+args.duration
        while not stop.wait(0.5) and time.monotonic()<end:
            control=site.out/'partition-request.json'
            if control.exists():
                value=json.loads(control.read_text());site.partition(value.get('partition') is True);control.unlink()
            if any(p.poll() is not None for p in site.processes.values()):raise RuntimeError('debrief process stopped; inspect bounded private logs')
    finally:site.close()

if __name__=='__main__':main()

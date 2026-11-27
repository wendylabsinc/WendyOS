"""Three real local World nodes, two gas estimators and two independent readers.

Only MuJoCo geometry and rendering are physical simulation. Concentrations use
an analytic fixture, and neither source truth nor actual wind enters estimators.
Loopback fault proxies alter delivery, never signed bytes or authentication.
"""
from __future__ import annotations
import argparse
import base64
import concurrent.futures
import copy
import datetime
import hashlib
import http.client
import http.server
import json
import math
import os
from pathlib import Path
import random
import select
import signal
import socket
import subprocess
import threading
import time
import uuid
from gas_source import GasDemo, atomic, png, scene_xml

class RPC:
    def __init__(self, executable, role, config, log):
        self.log = open(log, 'a')
        self.process = subprocess.Popen([str(executable), '--role', role, '--config', str(config)],
                                       stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=self.log,
                                       text=True, bufsize=1)
    def call(self, req, allow_error=False):
        self.process.stdin.write(json.dumps(req, allow_nan=False, separators=(',', ':'))+'\n')
        self.process.stdin.flush()
        if not select.select([self.process.stdout], [], [], 6)[0]:
            raise RuntimeError('independent process request exceeded six seconds')
        line = self.process.stdout.readline()
        if not line:
            raise RuntimeError('independent process exited: '+str(self.process.poll()))
        value = json.loads(line)
        if 'error' in value and not allow_error:
            raise RuntimeError(value['error'])
        return value
    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try: self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.terminate()
                try: self.process.wait(timeout=2)
                except subprocess.TimeoutExpired:
                    self.process.kill();self.process.wait(timeout=2)
        self.log.close()

class Proxy(http.server.ThreadingHTTPServer):
    daemon_threads = True
    block_on_close = False
    def __init__(self, port, destination, target, state):
        self.destination, self.target, self.state = destination, target, state
        self.slots = threading.BoundedSemaphore(8)
        super().__init__(('127.0.0.1', port), ProxyRequest)
        self.thread = threading.Thread(target=self.serve_forever, daemon=True)
        self.thread.start()

class ProxyRequest(http.server.BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self): self.forward()
    def do_POST(self): self.forward()
    def forward(self):
        if not self.server.slots.acquire(blocking=False):
            self.send_error(503);return
        conn = None
        try:
            self.connection.settimeout(3)
            length = int(self.headers.get('Content-Length', '0'))
            if length < 0 or length > 128*1024: self.send_error(413);return
            auth = json.loads(base64.b64decode(self.headers.get('X-World-Authorization', ''), validate=True))
            source = auth['grant']['grant']['source']['device']
            if (self.server.state['partition'] and (source == 'node-1' or self.server.target == 'node-1')) or (self.server.state.get('mission_partition',False) and (source=='node-3' or self.server.target=='node-3')):
                self.server.state['blocked'] += 1
                self.send_error(503, 'deliberate simulation partition');return
            body = self.rfile.read(length)
            conn = http.client.HTTPConnection('127.0.0.1', self.server.destination, timeout=3)
            conn.request(self.command, self.path, body, {k:v for k,v in self.headers.items() if k.lower() not in ('host','connection')})
            response = conn.getresponse();raw = response.read(16*1024*1024+1)
            if len(raw)>16*1024*1024: self.send_error(502);return
            self.send_response(response.status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        except (ValueError,KeyError,TimeoutError,OSError):
            try: self.send_error(502)
            except OSError: pass
        finally:
            if conn: conn.close()
            self.server.slots.release()

class ReplicaDemo:
    def __init__(self, out, executable, worldd, base, seed=31):
        import mujoco
        self.mj=mujoco;self.out=Path(out);self.executable=executable;self.worldd=worldd;self.base=base
        if self.out.exists() or self.out.is_symlink(): raise ValueError('fresh private output required')
        os.umask(0o077);self.out.mkdir(parents=True,mode=0o700)
        for name in ('captures','ground-truth','sensor-input','logs','viewer-east','viewer-west'):
            (self.out/name).mkdir(mode=0o700)
        for name in ('viewer-east','viewer-west'):
            for sub in ('commands','receipts'): (self.out/name/sub).mkdir(mode=0o700)
        self.xml=scene_xml();(self.out/'scene.xml').write_text(self.xml)
        self.model=mujoco.MjModel.from_xml_string(self.xml);self.data=mujoco.MjData(self.model);mujoco.mj_forward(self.model,self.data)
        self.renderer=mujoco.Renderer(self.model,height=600,width=960)
        self.run='gas-replicas-'+uuid.uuid4().hex[:12];self.anchor=time.time()
        self.source=[-.8,-.4];self.true_wind=.25;self.strength=120;self.rng=random.Random(seed)
        self.enabled={name:True for name in 'abcdefgh'};self.bias=self.duplicates=self.reveal=False
        self.network={'partition':False,'blocked':0};self.paused={'east':False,'west':False}
        self.events=[];self.sequence=0;self.command_sequences={'east':0,'west':0};self.traces={name:{s:[] for s in 'abcdefgh'} for name in ('east','west')}
        sensors=[]
        for name in 'abcdefgh':
            sid=mujoco.mj_name2id(self.model,mujoco.mjtObj.mjOBJ_SITE,name);p=self.data.site_xpos[sid]
            sensors.append(dict(id=name,source=dict(device='gas-sensor-'+name,app='gas',service=name,stream='ppm'),position_xz=[float(p[0]),float(-p[1])],position_uncertainty_m=0,calibration='calibration-1',sensor_type='analytic-scalar-v1'))
        self.config=dict(site='synthetic-gas-laboratory',frame=dict(id='laboratory-floor',revision=hashlib.sha256(self.xml.encode()).hexdigest()[:16]),simulation=dict(run_id=self.run,scenario_version='gas-laboratory-v1',seed=seed,clock_domain='simulation:'+self.run),sensors=sensors,min_xz=[-2,-2],max_xz=[2,2],step_m=.1,background_ppm=2)
        self.assumptions=dict(revision='declared-1',wind_radians=.25,downwind_m=3,upwind_m=1,crosswind_m=1.3)
        public=dict(tenant='lab',anchor=datetime.datetime.fromtimestamp(self.anchor,datetime.timezone.utc).isoformat(),field=self.config,assumptions=self.assumptions,gateways={n:dict(device='gas-estimator-'+n,app='gas',service='estimator',stream='source') for n in ('east','west')})
        atomic(self.out/'public-input.json',public)
        for name in ('east','west'): atomic(self.out/('viewer-'+name)/'catalog.json',self.config)
        self.runtime=self.out/'runtime'
        subprocess.run([str(executable),'--role','setup','--config',str(self.out/'public-input.json'),'--out',str(self.runtime),'--base-port',str(base)],check=True,timeout=10)
        self.proxies=[];self.nodes={};self.node_logs={};self.fusers={};self.readers={};self.sensors=None
        try:
            self.proxies=[Proxy(base+3+i,base+i,'node-'+str(i+1),self.network) for i in range(3)]
            for n in range(1,4): self.start_node(n)
            self.sensors=self.rpc('sensors','sensors')
            self.fusers={name:self.rpc('estimator',name) for name in ('east','west')}
            self.readers={name:self.rpc('reader','viewer-'+name) for name in ('east','west')}
        except Exception: self.close();raise
        self.last={};self.last_fusers={};self.started=time.monotonic()
    def rpc(self,role,name): return RPC(self.executable,role,self.runtime/(name+'.json'),self.out/'logs'/(name+'.log'))
    def start_node(self,n):
        log=open(self.out/'logs'/('node-'+str(n)+'.log'),'a');self.node_logs[n]=log
        p=subprocess.Popen([str(self.worldd),'--config',str(self.runtime/('node-'+str(n)+'.json'))],stdout=log,stderr=log);self.nodes[n]=p
        deadline=time.monotonic()+5
        while time.monotonic()<deadline:
            if p.poll() is not None: raise RuntimeError('worldd exited, see private node log')
            try:
                with socket.create_connection(('127.0.0.1',self.base+n-1),timeout=.1): return
            except OSError: time.sleep(.05)
        raise RuntimeError('worldd listener unavailable')
    def restart_east(self):
        self.fusers['east'].close();p=self.nodes[1];p.terminate();p.wait(timeout=4);self.node_logs[1].close();self.start_node(1);self.fusers['east']=self.rpc('estimator','east')
        # Original sensor processes must acquire fresh epochs from the restored
        # owner; their original sample lifetimes are never carried over.
        self.sensors.close();self.sensors=self.rpc('sensors','sensors')
    def parallel(self,services,req):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures={n:pool.submit(p.call,req) for n,p in services.items()}
            return {n:f.result(timeout=7) for n,f in futures.items()}
    def step(self,archive=False,capture=True):
        if getattr(self,'stop_requested',lambda:False)() or time.monotonic()>getattr(self,'deadline',float('inf')): raise InterruptedError('bounded simulation stopped')
        self.sequence+=1;self.data.mocap_pos[0]=[self.source[0],-self.source[1],.16]
        for _ in range(20): self.mj.mj_step(self.model,self.data)
        self.mj.mj_forward(self.model,self.data)
        # Captured once at generation, in the declared wall-anchored simulation
        # domain. Repair and readers never stamp a newer capture time.
        self.data.time=time.time()-self.anchor
        if self.duplicates: publication=self.sensors.call({'op':'duplicates'})
        elif capture:
            observations=[GasDemo.sample(self,s) for s in self.config['sensors'] if self.enabled[s['id']]]
            for o in observations: o['valid_until_s']=o['captured_s']+4.0
            if archive: atomic(self.out/'sensor-input'/f'{self.sequence:05d}.json',observations)
            publication=self.sensors.call(dict(op='publish',observations=observations))
        else: publication={'captured':False}
        active={n:p for n,p in self.fusers.items() if not self.paused[n]}
        raw_deadline=time.monotonic()+1.0
        while True:
            fusers=self.parallel(active,{'op':'tick'})
            for name,value in fusers.items(): self.last_fusers[name]={**self.last_fusers.get(name,{}),**value}
            # Observe complete batches when connected, or each owner's local
            # catalog under partition. Partial claims remain explicitly partial
            # if real repair cannot finish inside this fixed budget.
            def expected_coverage(name):
                return sum(on and (not self.network['partition'] or (ord(sensor)-ord('a'))%2 == (0 if name=='east' else 1)) for sensor,on in self.enabled.items())
            covered=all(v['result']['coverage']>=expected_coverage(name) for name,v in fusers.items())
            if covered or self.duplicates or not capture or time.monotonic()>=raw_deadline: break
            time.sleep(.1)
        expected={}
        for name,v in fusers.items():
            if self.network['partition'] and name!='west': continue
            if v['result'].get('estimate') is None: expected[name]=None
            elif v.get('published'): expected[name]=v['sha256']
        repair_deadline=time.monotonic()+1.2
        while True:
            self.last=self.parallel(self.readers,{'op':'tick'})
            delivered=all(all(any(c['gateway']==gateway and ((digest is None and c['state']=='unavailable') or c.get('sha256')==digest) for c in view['selection']['candidates']) for gateway,digest in expected.items()) for view in self.last.values())
            if delivered or time.monotonic()>=repair_deadline: break
            time.sleep(.1)
        for sensor in self.config['sensors']:
            name=sensor['id'];gid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_GEOM,'lamp-'+name)
            self.model.geom_rgba[gid]=[.07,.58,.51,1] if self.enabled[name] else [.4,.43,.45,1]
        gid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_GEOM,'source-marker');self.model.geom_rgba[gid,3]=1 if self.reveal else 0
        self.renderer.update_scene(self.data,camera='overview');raw=png(self.renderer.render());image='scene-'+str(self.sequence%2)+'.png';digest=hashlib.sha256(raw).hexdigest()
        for name,view in self.last.items():
            result=view['selection']['result'];traces=self.traces[name]
            for rd in result['readings']:
                o=rd.get('observation');series=traces[rd['sensor']['id']]
                if rd['state']=='live' and o and (not series or series[-1][0]!=o['captured_s']): series.append([o['captured_s'],o['value_ppm']]);del series[:-60]
            network=dict(view['selection'],reader=view['reader'],node=view['node'],partition=self.network['partition'],paused=self.paused,controls=name=='east')
            network.pop('result')
            snap=dict(schema='wendy.gas-demo.v1',run_id=self.run,sequence=self.sequence,last_command_sequence=self.command_sequences[name],result=result,image=image,image_sha256=digest,traces=traces,enabled=self.enabled,partition=self.network['partition'],duplicates=self.duplicates,bias=self.bias,reveal=self.reveal,releasing=self.strength>0,events=self.events[-12:],network=network)
            if self.reveal: snap['evaluation_truth']=dict(position_xz=self.source,wind_radians=self.true_wind)
            folder=self.out/('viewer-'+name);temp=folder/(image+'.tmp');temp.write_bytes(raw);os.replace(temp,folder/image);atomic(folder/'snapshot.json',snap)
            if archive: atomic(self.out/'captures'/f'{self.sequence:05d}-{name}.json',snap)
        if archive:
            atomic(self.out/'captures'/f'{self.sequence:05d}-transport.json',dict(publication=publication,estimators=fusers,readers=self.last,blocked=self.network['blocked']))
            atomic(self.out/'ground-truth'/f'{self.sequence:05d}.json',dict(evaluation_only=True,position_xz=self.source,actual_wind=self.true_wind))
            (self.out/'captures'/f'{self.sequence:05d}.png').write_bytes(raw)
        return self.last
    def command(self,action,target=''):
        if action=='partition': self.network['partition']=not self.network['partition']
        elif action=='pause_estimator' and target in self.paused: self.paused[target]=not self.paused[target]
        elif action=='restart_east': self.restart_east()
        elif action=='duplicates': self.duplicates=not self.duplicates
        elif action=='move_source': self.source=[.6,.4] if self.source[0]<0 else [-.8,-.4]
        elif action=='wind_change': self.true_wind=1.5 if self.true_wind<1 else .25
        elif action=='toggle_sensor' and target in self.enabled: self.enabled[target]=not self.enabled[target]
        elif action=='bias': self.bias=not self.bias
        elif action=='release': self.strength=0 if self.strength else 120
        elif action=='reveal': self.reveal=not self.reveal
        elif action=='reset':
            self.source=[-.8,-.4];self.true_wind=.25;self.strength=120;self.enabled={s:True for s in 'abcdefgh'};self.bias=self.duplicates=self.reveal=False;self.network['partition']=False;self.paused={'east':False,'west':False}
        else: raise ValueError('unknown control or immutable model contract')
        self.events.append(dict(time_s=time.time()-self.anchor,action=action,target=target));self.events=self.events[-12:]
    def poll_commands(self):
        changed=False
        for name in ('east','west'):
            seq=self.command_sequences[name]+1;folder=self.out/('viewer-'+name);path=folder/'commands'/f'{seq:06d}.json'
            if seq>4096 or not path.is_file() or path.is_symlink(): continue
            accepted=False;error=''
            try:
                if name!='east': raise ValueError('this independent operator view is read-only')
                if path.stat().st_size>4096: raise ValueError('command bound')
                q=json.loads(path.read_text())
                if set(q)!={'run_id','sequence','action','target'} or q['run_id']!=self.run or q['sequence']!=seq: raise ValueError('command scope mismatch')
                self.command(q['action'],q['target']);accepted=True
            except (ValueError,TypeError,KeyError) as exc: error=str(exc)
            self.command_sequences[name]=seq;atomic(folder/'receipts'/f'{seq:06d}.json',dict(sequence=seq,accepted=accepted,error=error));changed=True
        return changed
    def close(self):
        for p in list(getattr(self,'readers',{}).values())+list(getattr(self,'fusers',{}).values())+([self.sensors] if getattr(self,'sensors',None) else []): p.close()
        for p in getattr(self,'nodes',{}).values():
            if p.poll() is None:
                p.terminate()
                try: p.wait(timeout=3)
                except subprocess.TimeoutExpired: p.kill();p.wait(timeout=2)
        for proxy in getattr(self,'proxies',[]): proxy.shutdown();proxy.server_close()
        for log in getattr(self,'node_logs',{}).values(): log.close()
        self.renderer.close()

def check(demo):
    checks=[]
    def settle(label,seconds=1.0,capture=True,ready=None):
        start=time.monotonic();end=start+max(seconds,12);observed=[]
        while True:
            views=demo.step(archive=True,capture=capture)
            observed.append({n:[v['selection']['state'],v['selection'].get('gateway')] for n,v in views.items()})
            elapsed=time.monotonic()-start
            if elapsed>=seconds and (ready is None or ready(views)): break
            if time.monotonic()>=end: raise AssertionError(dict(label=label,recovery_budget_s=max(seconds,12),observed=observed))
            time.sleep(.1)
        checks.append(dict(label=label,sequence=demo.sequence,elapsed_s=elapsed,observed=observed,views={n:{k:v['selection'].get(k) for k in ('state','gateway','sha256','version')} for n,v in views.items()}))
        return views
    localized=lambda views:all(v['selection']['state']=='localized' for v in views.values())
    unavailable=lambda views:all(v['selection']['state']=='unavailable' for v in views.values())
    views=settle('two independent readers',2,ready=lambda v:localized(v) and all(len([c for c in x['selection']['candidates'] if c.get('sha256')])==2 for x in v.values()))
    assert all(v['selection']['state']=='localized' for v in views.values()), views
    time.sleep(.3);views=demo.parallel(demo.readers,{'op':'tick'})

    hashes={n:{c['gateway']:c.get('sha256') for c in v['selection']['candidates'] if c.get('sha256')} for n,v in views.items()}
    assert hashes['east']==hashes['west'] and len(hashes['east'])==2, hashes
    checks.append(dict(label='quiescent replicas retain identical hashes per gateway',hashes=hashes))
    original_hash=hashes['east']
    for reader in demo.readers.values(): reader.close()
    demo.readers={name:demo.rpc('reader','viewer-'+name) for name in ('east','west')}
    initialized=demo.parallel(demo.readers,{'op':'tick'})
    assert initialized['east']['selection'].get('sha256')==initialized['west']['selection'].get('sha256') and initialized['east']['selection'].get('sha256'),initialized
    checks.append(dict(label='identical complete initial candidate sets select deterministically',selected={n:v['selection']['sha256'] for n,v in initialized.items()}))
    demo.command('partition');views=settle('partition expires east and selects west',5.2,ready=lambda v:all(x['selection'].get('gateway')=='west' and x['selection']['result']['coverage']==4 for x in v.values()))
    assert all(v['selection']['gateway']=='west' for v in views.values()),views
    assert all(v['selection']['result']['coverage']==4 for v in views.values()),views
    assert demo.network['blocked']>0
    demo.command('partition');settle('repair restores fresh originals',1.2)
    demo.command('pause_estimator','west');views=settle('west stops and east takes over',5,ready=lambda v:all(x['selection'].get('gateway')=='east' for x in v.values()))
    assert all(v['selection']['gateway']=='east' for v in views.values()),views
    before=demo.last_fusers['east']['component']['version']['epoch']
    demo.command('restart_east');views=settle('durable epoch after node and estimator restart',2,ready=localized)
    assert demo.last_fusers['east']['component']['version']['epoch']>before
    demo.command('duplicates');views=settle('original packet retries expire',5,ready=unavailable)
    assert all(v['selection']['state']=='unavailable' and v['selection']['result'].get('estimate') is None for v in views.values()),views
    demo.command('reset');settle('fresh recovery',1.5,ready=localized)
    demo.command('move_source');views=settle('moving source reaches both independent readers',1.4,ready=lambda v:localized(v) and all(math.dist(x['selection']['result']['estimate']['position_xz'],demo.source)<=.2 for x in v.values()))
    assert all(v['selection']['state']=='localized' and math.dist(v['selection']['result']['estimate']['position_xz'],demo.source)<=.2 for v in views.values()),views
    demo.command('wind_change');views=settle('undeclared airflow mismatch withdraws regions',3,ready=unavailable)
    assert all(v['selection']['state']=='unavailable' for v in views.values()),views
    demo.command('reset');settle('scope probe baseline',1.2,ready=localized)
    # Actual node admission with a read-only credential must deny publication.
    probe=demo.rpc('probe','viewer-east')
    try:
        cfg=json.loads((demo.runtime/'viewer-east.json').read_text());raw=probe.call({'op':'query'});c=copy.deepcopy(next(c for c in raw['components'] if c['key']['type']=='wendy.gas.observation.v1'));c['key']['source']=cfg['client']['grant']['grant']['source']
        denied=probe.call(dict(op='publish',component=c),True)
        assert '403' in denied.get('error','') or '401' in denied.get('error',''),denied
        checks.append(dict(label='unauthorized original publisher rejected by real node',receipt=denied))
    finally: probe.close()
    # Authorized but semantically incompatible records are admitted as ordinary
    # World state, then explicitly held out by gas applications.
    sensors_cfg=json.loads((demo.runtime/'sensors.json').read_text())
    atomic(demo.runtime/'probe-a.json',dict(public=sensors_cfg['public'],client=sensors_cfg['sensors']['a']))
    probe=demo.rpc('probe','probe-a');reader=demo.rpc('probe','east')
    try:
        raw=reader.call({'op':'query'})
        c=copy.deepcopy(next(c for c in raw['components'] if c['key']['source']['device']=='gas-sensor-a'))
        epoch=probe.call({'op':'epoch'})['epoch'];c['version']=dict(epoch=epoch,sequence=1);c['payload']['version']=c['version'];c['payload']['calibration']='uncommissioned'
        accepted=probe.call(dict(op='publish',component=c));time.sleep(.4)
        fusers=demo.parallel(demo.fusers,{'op':'tick'})
        assert any(any('calibration' in reason for reason in v['held_out']) for v in fusers.values()),fusers
        wrong_frame=copy.deepcopy(c);wrong_frame['version']['sequence']=2;wrong_frame['frame']['revision']='wrong-frame'
        denied=probe.call(dict(op='publish',component=wrong_frame),True)
        after=reader.call({'op':'query'})
        assert not any(item['key']['source']['device']=='gas-sensor-a' for item in after['components']),after
        checks.append(dict(label='real-node wrong calibration held out and uncommissioned frame invisible',calibration_receipt=accepted,frame_receipt=denied))
    finally: probe.close();reader.close()
    demo.sensors.close();demo.sensors=demo.rpc('sensors','sensors');settle('fresh sensor epoch recovers calibration',1)
    probe=demo.rpc('probe','east')
    try:
        c=copy.deepcopy(demo.last_fusers['east']['component']);c['version']=dict(epoch=probe.call({'op':'epoch'})['epoch'],sequence=1);c['payload']['contract_sha256']='0'*64
        accepted=probe.call(dict(op='publish',component=c));time.sleep(.6)
        views=demo.parallel(demo.readers,{'op':'tick'})
        assert any(any(item['gateway']=='east' and 'incompatible model' in item['reason'] for item in view['selection']['candidates']) for view in views.values()),views
        checks.append(dict(label='signed incompatible model claim held out by independent reader',receipt=accepted))
    finally: probe.close()
    demo.fusers['east'].close();demo.fusers['east']=demo.rpc('estimator','east');settle('fresh estimator epoch recovers model',1)
    # A current root-signed update is installed on every real node. Their query
    # and repair paths must reject the revoked raw source, including after restart.
    update=(demo.runtime/'revoke-first-sensor.json').read_bytes()
    for n in range(3):
        conn=http.client.HTTPConnection('127.0.0.1',demo.base+n,timeout=3);conn.request('POST','/v1/revocations',update,{'Content-Type':'application/json'});response=conn.getresponse();body=response.read();conn.close();assert response.status==200,body
    demo.enabled['a']=False
    views=settle('revoked original removed from queries and repair',1.2)
    probe=demo.rpc('probe','viewer-west')
    try:
        snapshot=probe.call({'op':'query'});assert not any(c['key']['source']['device']=='gas-sensor-a' for c in snapshot['components'])
        rejected=demo.sensors.call({'op':'duplicates'},True);assert '401' in rejected.get('error','') or '403' in rejected.get('error',''),rejected
        checks.append(dict(label='revoked source admission and repair rejection',receipt=rejected))
    finally: probe.close()
    demo.restart_east();views=settle('revocation persists across node restart',1)
    probe=demo.rpc('probe','probe-a')
    try:
        denied=probe.call({'op':'epoch'},True)
        assert '401' in denied.get('error','') or '403' in denied.get('error',''),denied
        checks[-1]['revoked_epoch_receipt']=denied
    finally: probe.close()
    report=dict(passed=True,mujoco_version=demo.mj.__version__,three_world_pids={str(n):p.pid for n,p in demo.nodes.items()},checks=checks,captures=demo.sequence,initial_component_sha256=original_hash,blocked_replication_requests=demo.network['blocked'],limitations=['Synthetic analytic concentrations, not fluid dynamics','Loopback HTTP simulation; authenticated requests and node-verified origin proofs','Nested originals are gateway-attested; model reproduction is internal consistency, not independent sensor authentication'])
    atomic(demo.out/'checks.json',report);print(json.dumps(report,indent=2))

def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--executable',type=Path,required=True);p.add_argument('--worldd',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--base-port',type=int,default=19280);p.add_argument('--duration',type=float,default=180);p.add_argument('--check',action='store_true');args=p.parse_args()
    if not all(x.is_absolute() for x in (args.executable,args.worldd,args.out)) or not 1<=args.duration<=1800 or not 1024<=args.base_port<=65529: p.error('absolute paths, six valid ports and duration 1–1800s required')
    stopped=[False];signal.signal(signal.SIGINT,lambda *_:stopped.__setitem__(0,True));signal.signal(signal.SIGTERM,lambda *_:stopped.__setitem__(0,True))
    demo=ReplicaDemo(args.out,args.executable,args.worldd,args.base_port)
    demo.deadline=demo.started+args.duration;demo.stop_requested=lambda:stopped[0]
    try:
        if args.check: check(demo)
        else:
            while not stopped[0] and time.monotonic()-demo.started<args.duration:
                start=time.monotonic();changed=demo.poll_commands();demo.step(archive=changed or demo.sequence==0);time.sleep(max(0,.2-(time.monotonic()-start)))
    except InterruptedError: pass
    finally: demo.close()
if __name__=='__main__':main()

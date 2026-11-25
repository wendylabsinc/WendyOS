"""Commission an explicit three-node simulated World watch for an office patrol.

The sensor adapter reads original-step /world-samples. No laboratory observation
is relabeled. The model and commissioned fixed sites are retained with the run.
"""
import argparse,base64,concurrent.futures,copy,datetime,hashlib,http.client,http.server,json,os,signal,socket,subprocess,threading,time,urllib.request
from pathlib import Path
import sys
sys.path.insert(0,str(Path(__file__).resolve().parent))
from sim.gas_replicas import RPC
from sim.gas_source import atomic

class Proxy(http.server.ThreadingHTTPServer):
    daemon_threads=True;block_on_close=False
    def __init__(self,port,destination,target,state):
        self.destination,self.target,self.state=destination,target,state
        super().__init__(('127.0.0.1',port),Handler)
        self.thread=threading.Thread(target=self.serve_forever,daemon=True);self.thread.start()
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self,*args):pass
    def do_GET(self):self.forward()
    def do_POST(self):self.forward()
    def forward(self):
        conn=None
        try:
            self.connection.settimeout(2);length=int(self.headers.get('Content-Length','0'))
            if not 0<=length<=128*1024:raise ValueError('request bound')
            auth=json.loads(base64.b64decode(self.headers.get('X-World-Authorization',''),validate=True));source=auth['grant']['grant']['source']['device']
            if self.server.state['partition'] and (source=='node-3' or self.server.target=='node-3'):
                self.server.state['blocked']+=1;self.send_error(503,'simulated robot replica partition');return
            conn=http.client.HTTPConnection('127.0.0.1',self.server.destination,timeout=2)
            conn.request(self.command,self.path,self.rfile.read(length),{k:v for k,v in self.headers.items() if k.lower() not in ('host','connection')})
            response=conn.getresponse();raw=response.read(2<<20)
            self.send_response(response.status);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw)
        except (OSError,ValueError,KeyError):
            try:self.send_error(502)
            except OSError:pass
        finally:
            if conn:conn.close()

def get(url):
    with urllib.request.urlopen(url,timeout=2) as r:return json.load(r)

def ports():
    for base in range(20480,25480,9):
        held=[]
        try:
            for port in range(base,base+7):
                sock=socket.socket();sock.bind(('127.0.0.1',port));held.append(sock)
            return base
        except OSError:pass
        finally:
            for sock in held:sock.close()
    raise ValueError('no loopback port block')

def validate_original(original,scope,boot,catalog,previous=None):
    sample=original.get('sample')
    if not isinstance(sample,dict) or sample.get('run_id')!=scope['run_id'] or sample.get('boot_id')!=boot or sample.get('frame')!=scope['frame'] or sample.get('model_sha256')!=scope['model_sha256'] or sample.get('method')!='original-step-office-analytic-field-v1':
        raise ValueError('world sensor run/boot/frame/model or method changed')
    if original.get('catalog')!=catalog:raise ValueError('commissioned sensor catalog changed')
    readings=sample.get('readings');expected=[s['id'] for s in catalog]
    if not isinstance(readings,list) or len(readings)!=len(expected) or any(not isinstance(r,dict) for r in readings):raise ValueError('exact original sensor set required')
    ids=[r.get('sensor') for r in readings]
    if len(set(ids))!=len(ids) or set(ids)!=set(expected):raise ValueError('duplicate or different sensor IDs')
    for field in ('sequence','simulation_ns','captured_unix_ms'):
        if type(sample.get(field)) is not int or sample[field]<=0:raise ValueError('positive original sequence and clocks required')
    if previous:
        if sample['sequence']<previous['sequence']:raise ValueError('original sequence regressed')
        if sample['sequence']==previous['sequence'] and sample!=previous:raise ValueError('original identity changed')
        if sample['sequence']>previous['sequence'] and (sample['simulation_ns']<=previous['simulation_ns'] or sample['captured_unix_ms']<previous['captured_unix_ms']):raise ValueError('original sensor clock regressed')
    return sample

class Site:
    def __init__(self,out,physics,binaries,deadline):
        self.out,self.physics,self.binaries,self.deadline=Path(out),physics,Path(binaries),deadline
        self.out.mkdir(mode=0o700,parents=True,exist_ok=False);(self.out/'logs').mkdir();(self.out/'samples').mkdir()
        self.nodes={};self.logs=[];self.rpcs=[];self.proxies=[];self.seq=0;self.network=dict(partition=False,blocked=0);self.duplicates=False
        scope=get(physics+'/state');original=get(physics+'/world-samples');sample=original['sample']
        if not sample or sample['run_id']!=scope['run_id'] or sample['frame']!=scope['frame'] or sample['model_sha256']!=scope['model_sha256']:raise ValueError('original physics scope differs')
        self.scope=scope;self.catalog=original['catalog'];self.previous=None
        self.boot=sample['boot_id'];self.run=scope['run_id'];self.anchor=time.time()-5;self.base=ports();self.runtime=self.out/'runtime'
        field=dict(site='studio-01',frame=scope['frame'],simulation=dict(run_id=self.run,scenario_version='office-world-field-v1',seed=31,clock_domain='simulation:'+self.run),sensors=original['catalog'],min_xz=[-7,-3],max_xz=[-1,3],step_m=.2,background_ppm=2)
        self.public=dict(tenant='simulation',anchor=datetime.datetime.fromtimestamp(self.anchor,datetime.timezone.utc).isoformat(),field=field,assumptions=dict(revision='office-analytic-v1',wind_radians=.25,downwind_m=3,upwind_m=1,crosswind_m=1.3),gateways={n:dict(device='gas-estimator-'+n,app='gas',service='estimator',stream='source') for n in ('east','west')})
        atomic(self.out/'public-input.json',self.public);atomic(self.out/'commissioned-original.json',dict(physics_scope=scope,original=original,declared_mapping='MuJoCo [x,y,z] -> World [x,z,-y]',analytic_model='original-step directional Gaussian; not fluid dynamics'))
        subprocess.run([str(self.binaries/'world-gas-replica'),'--role','setup','--config',str(self.out/'public-input.json'),'--out',str(self.runtime),'--base-port',str(self.base)],check=True,timeout=10)
        # The reverse pull is useful for the independent east estimate, but it
        # must not continuously contend with the robot's pull on node 2's one
        # incoming repair slot. Keep original publications and deadlines intact.
        reverse_path=self.runtime/'node-1.json';reverse=json.loads(reverse_path.read_text());prior=reverse['sync_interval_ms'];reverse['sync_interval_ms']=2000;atomic(reverse_path,reverse)
        self.repair_cadence={str(n):json.loads((self.runtime/f'node-{n}.json').read_text())['sync_interval_ms'] for n in range(1,4)}
        atomic(self.out/'repair-cadence.json',dict(sync_interval_ms=self.repair_cadence,previous_node1_interval_ms=prior,original_sensor_ttl_ms=4000,reason='Bound competing reverse repair; same owners, topology, signed identities and originals.'))
        try:
            self.proxies=[Proxy(self.base+3+i,self.base+i,'node-'+str(i+1),self.network) for i in range(3)]
            self.proxies.append(Proxy(self.base+6,self.base+2,'node-3',self.network))
            for i in range(1,4):self.start_node(i)
            def rpc(role,name):
                p=RPC(self.binaries/'world-gas-replica',role,self.runtime/(name+'.json'),self.out/'logs'/(name+'.log'));self.rpcs.append(p);return p
            self.sensors=rpc('sensors','sensors');self.fusers={name:rpc('estimator',name) for name in ('east','west')};self.reader=rpc('reader','viewer-west')
            digest=self.reader.call(dict(op='status'))['contract_sha256'];cfg=json.loads((self.runtime/'viewer-west.json').read_text())['client'];cfg['url']='http://127.0.0.1:'+str(self.base+6);atomic(self.out/'reader.json',cfg)
            self.policy=dict(method='synthetic-source-region-outside-watch-v1',contract=self.public,contract_sha256=digest,reader_principal=cfg['grant']['grant']['principal'],reader_audience=cfg['audience'],min_xz=[-4.5,-1],max_xz=[-2,1],max_age_ms=4000)
            atomic(self.out/'ready.json',dict(policy=self.policy,reader=str(self.out/'reader.json'),node_pids={str(k):v.pid for k,v in self.nodes.items()},deadline_unix=deadline,run_id=self.run))
        except Exception:self.close();raise
    def start_node(self,n):
        log=(self.out/'logs'/('node-'+str(n)+'.log')).open('a');self.logs.append(log)
        p=subprocess.Popen([str(self.binaries/'worldd'),'--config',str(self.runtime/('node-'+str(n)+'.json'))],stdout=log,stderr=log);self.nodes[n]=p
        end=time.time()+5
        while time.time()<end:
            try:
                with socket.create_connection(('127.0.0.1',self.base+n-1),timeout=.1):return
            except OSError:time.sleep(.05)
        raise RuntimeError('World node not listening')
    def command(self):
        path=self.out/'control.json'
        if not path.exists():return
        q=json.loads(path.read_text());seq=q.get('sequence',0)
        if seq<=self.seq:return
        self.seq=seq;action=q['action']
        if action in ('partition','heal'):self.network['partition']=action=='partition'
        elif action in ('duplicates','live'):self.duplicates=action=='duplicates'
        elif action=='revoke-reader':
            p=self.nodes[3];p.terminate();p.wait(timeout=5)
            path=self.runtime/'node-3.json';cfg=json.loads(path.read_text());cfg['revocations']=json.loads((self.runtime/'revoke-watch-reader.json').read_text());atomic(path,cfg);self.start_node(3)
        else:raise ValueError('unknown fixture control')
        atomic(self.out/'control-result.json',dict(sequence=seq,action=action,accepted=True,wall_unix=time.time()))
    def step(self):
        self.command();original=get(self.physics+'/world-samples');sample=validate_original(original,self.scope,self.boot,self.catalog,self.previous);self.previous=copy.deepcopy(sample)
        published=None
        if self.duplicates:published=self.sensors.call(dict(op='duplicates'),allow_error=True)
        elif sample['sequence']!=getattr(self,'last_sample',0):
            self.last_sample=sample['sequence'];readings={v['sensor']:v for v in sample['readings']};observations=[];at=sample['captured_unix_ms']/1000-self.anchor
            for sensor in self.public['field']['sensors']:
                value=readings[sensor['id']];observations.append(dict(id=f"{sensor['id']}/{sample['boot_id']}/{sample['sequence']}",sensor=sensor['id'],source=sensor['source'],version=dict(epoch=1,sequence=sample['sequence']),site='studio-01',frame=sample['frame'],provenance=dict(origin='simulated',app_version='patrol-world-field-v1',simulation=self.public['field']['simulation']),calibration=sensor['calibration'],sensor_type=sensor['sensor_type'],units='ppm',species='synthetic-tracer',captured_s=at,valid_until_s=at+4,value_ppm=value['value_ppm'],uncertainty_ppm=value['uncertainty_ppm']))
            atomic(self.out/'samples'/f"{sample['sequence']:08d}.json",dict(original=original,observations=observations));published=self.sensors.call(dict(op='publish',observations=observations),allow_error=True)
        # Coalesce one original cohort before a single estimator evaluation.
        # The fixed pause leaves ordinary peer repair time to run without an
        # authenticated query storm. It never advances a capture or TTL. Any
        # incomplete/expired cohort still receives the normal model hold.
        repair_started=time.time();time.sleep(.75)
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            futures={n:pool.submit(p.call,dict(op='tick'),True) for n,p in self.fusers.items()};claims={n:f.result(timeout=8) for n,f in futures.items()}
        selected=self.reader.call(dict(op='tick'),allow_error=True)
        receipt=dict(repair_cadence_ms=self.repair_cadence,wall_unix=time.time(),sample_sequence=sample['sequence'],sample_age_ms=time.time()*1000-sample['captured_unix_ms'],repair_wait_s=time.time()-repair_started,publication=published,claims=claims,selection=selected,network=self.network,duplicates=self.duplicates)
        atomic(self.out/'state.json',receipt)
        with (self.out/'steps.jsonl').open('a') as trace:trace.write(json.dumps(receipt,separators=(',',':'))+'\n')

    def close(self):
        for p in self.rpcs:
            try:p.close()
            except Exception:pass
        for p in self.nodes.values():
            if p.poll() is None:p.terminate()
        for p in self.nodes.values():
            try:p.wait(timeout=5)
            except subprocess.TimeoutExpired:p.kill();p.wait(timeout=2)
        for proxy in self.proxies:proxy.shutdown();proxy.server_close()
        for log in self.logs:log.close()
        atomic(self.out/'cleanup.json',dict(node_exit_codes={str(k):p.poll() for k,p in self.nodes.items()},all_nodes_exited=all(p.poll() is not None for p in self.nodes.values())))

def main():
    p=argparse.ArgumentParser();p.add_argument('--out',required=True,type=Path);p.add_argument('--physics',required=True);p.add_argument('--binaries',required=True,type=Path);p.add_argument('--deadline',required=True,type=float);args=p.parse_args()
    os.umask(0o077);stopped=False
    def stop(*_):
        nonlocal stopped;stopped=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    site=None
    try:
        site=Site(args.out,args.physics,args.binaries,args.deadline)
        while not stopped and time.time()<args.deadline:
            site.step();time.sleep(.15)
    finally:
        if site:site.close()
if __name__=='__main__':main()

"""Shared MuJoCo visual fixture and analytic gas, with three actual World nodes.

Rendered pixels and actual ray returns enter the visual app. Gas source truth
stays in evaluation files. Local authenticated HTTP is simulation transport.
"""
from __future__ import annotations
import struct,zlib
import argparse,base64,concurrent.futures,copy,datetime,hashlib,json,math,os,random,signal,socket,subprocess,time,uuid
from pathlib import Path
import xml.etree.ElementTree as ET
from gas_source import GasDemo,atomic,png,scene_xml as gas_scene
from gas_replicas import RPC,Proxy

def camera_png(rgb):
    height,width,channels=rgb.shape
    if (width,height,channels)!=(160,120,3):raise ValueError('camera image dimensions')
    def chunk(k,b):return struct.pack('>I',len(b))+k+b+struct.pack('>I',zlib.crc32(k+b)&0xffffffff)
    raw=b''.join(b'\0'+row.tobytes() for row in rgb)
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',width,height,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(raw))+chunk(b'IEND',b'')

def scene_xml():
    root=ET.fromstring(gas_scene());world=root.find('worldbody')
    fixture=ET.SubElement(world,'body',name='visual-fixture',mocap='true',pos='-.8 .4 .6')
    ET.SubElement(fixture,'geom',name='red-indicator',type='box',size='.16 .16 .16',rgba='.95 .025 .015 1',contype='0',conaffinity='0')
    for name,pos in [('camera-a','-3 -4 2.8'),('camera-b','3 -4 2.8')]:ET.SubElement(world,'camera',name=name,pos=pos,mode='targetbody',target='center',fovy='55')
    ET.SubElement(world,'site',name='lidar-site',pos='0 -4 1.3',size='.001',rgba='0 0 0 0')
    return ET.tostring(root,encoding='unicode')

class IncidentDemo:
    def __init__(self,out,executable,gas_executable,worldd,base,seed=31,inspection=False):
        import mujoco,numpy as np
        self.mj,self.np=mujoco,np;self.out=Path(out);self.executable=executable;self.gas_executable=gas_executable;self.worldd=worldd;self.base=base
        if self.out.exists() or self.out.is_symlink():raise ValueError('fresh private output required')
        os.umask(0o077);self.out.mkdir(parents=True,mode=0o700)
        for n in ('captures','ground-truth','sensor-input','logs','viewer-east','viewer-west'):(self.out/n).mkdir(mode=0o700)
        for n in ('viewer-east','viewer-west'):
            for sub in ('commands','receipts'):(self.out/n/sub).mkdir(mode=0o700)
        self.xml=self.build_scene();(self.out/'scene.xml').write_text(self.xml);self.model=mujoco.MjModel.from_xml_string(self.xml);self.data=mujoco.MjData(self.model);mujoco.mj_forward(self.model,self.data)
        self.renderer=mujoco.Renderer(self.model,height=600,width=960);self.camera_renderer=mujoco.Renderer(self.model,height=120,width=160)
        self.anchor=time.time();self.run='incident-'+uuid.uuid4().hex[:12];self.source=[-.8,-.4];self.visual_position=[-.8,-.4];self.true_wind=.25;self.strength=120;self.rng=random.Random(seed);self.sequence=0
        self.enabled={n:True for n in 'abcdefgh'};self.bias=False;self.gas_enabled=True;self.visual_enabled=True;self.duplicates=False;self.network={'partition':False,'blocked':0};self.events=[];self.command_sequences={'east':0,'west':0};self.last={};self.last_visual=None
        sensors=[]
        for name in 'abcdefgh':
            p=self.data.site_xpos[mujoco.mj_name2id(self.model,mujoco.mjtObj.mjOBJ_SITE,name)]
            sensors.append(dict(id=name,source=dict(device='gas-sensor-'+name,app='gas',service=name,stream='ppm'),position_xz=[float(p[0]),float(-p[1])],position_uncertainty_m=0,calibration='calibration-1',sensor_type='analytic-scalar-v1'))
        digest=hashlib.sha256(self.xml.encode()).hexdigest();self.config=dict(site='synthetic-gas-laboratory',frame=dict(id='laboratory-floor',revision=digest[:16]),simulation=dict(run_id=self.run,scenario_version='gas-laboratory-v1',seed=seed,clock_domain='simulation:'+self.run),sensors=sensors,min_xz=[-2,-2],max_xz=[2,2],step_m=.1,background_ppm=2)
        self.assumptions=dict(revision='declared-1',wind_radians=.25,downwind_m=3,upwind_m=1,crosswind_m=1.3)
        gas=dict(tenant='lab',anchor=datetime.datetime.fromtimestamp(self.anchor,datetime.timezone.utc).isoformat(),field=self.config,assumptions=self.assumptions,gateways={n:dict(device='gas-estimator-'+n,app='gas',service='estimator',stream='source') for n in ('east','west')})
        visual=dict(tenant='lab',site=self.config['site'],frame=self.config['frame'],anchor=gas['anchor'],simulation=self.config['simulation'],package_sha256=digest,calibration_revision='mujoco-calibration-1',sources={n:dict(device='visual-adapter',app='sensors',service=n,stream='samples') for n in ('camera-a','camera-b','lidar-site')})
        visual['sensor_models']={n:'mujoco-render-v1' if n.startswith('camera') else 'mujoco-ray-v1' for n in visual['sources']}
        self.contract=dict(gas=gas,visual=visual,visual_publisher=dict(device='visual-estimator',app='incident',service='visual',stream='fixture'),readers={n:dict(device='incident-'+n,app='incident',service='association',stream='state') for n in ('east','west')})
        atomic(self.out/'public-input.json',self.contract)
        for name in ('east','west'):atomic(self.out/('viewer-'+name)/'catalog.json',self.contract)
        self.runtime=self.out/'runtime';subprocess.run([str(executable),'--role','setup','--config',str(self.out/'public-input.json'),'--out',str(self.runtime),'--base-port',str(base)]+(['--inspection']if inspection else []),check=True,timeout=10)
        self.nodes={};self.node_logs={};self.proxies=[];self.fusers={};self.readers={};self.sensors=None;self.visual=None
        try:
            self.proxies=[Proxy(base+3+i,base+i,'node-'+str(i+1),self.network) for i in range(3)]
            for n in range(1,4):self.start_node(n)
            self.sensors=self.rpc(gas_executable,'sensors','sensors');self.fusers={n:self.rpc(gas_executable,'estimator',n) for n in ('east','west')}
            self.visual=self.rpc(executable,'visual','incident-visual');self.readers={n:self.rpc(executable,'reader','incident-'+n) for n in ('east','west')}
        except Exception:self.close();raise
        self.started=time.monotonic()
    def build_scene(self):return scene_xml()
    def rpc(self,exe,role,name):return RPC(exe,role,self.runtime/(name+'.json'),self.out/'logs'/(name+'.log'))
    def start_node(self,n):
        log=open(self.out/'logs'/f'node-{n}.log','a');self.node_logs[n]=log;p=subprocess.Popen([str(self.worldd),'--config',str(self.runtime/f'node-{n}.json')],stdout=log,stderr=log);self.nodes[n]=p
        end=time.monotonic()+5
        while time.monotonic()<end:
            if p.poll() is not None:raise RuntimeError('node exited')
            try:
                with socket.create_connection(('127.0.0.1',self.base+n-1),timeout=.1):return
            except OSError:time.sleep(.05)
        raise RuntimeError('node did not listen')
    def parallel(self,services,q):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2)as pool:
            fs={n:pool.submit(p.call,q)for n,p in services.items()};return {n:f.result(timeout=7)for n,f in fs.items()}
    def capture_visual(self):
        np=self.np;mj=self.mj;samples=[];images={};camera_bytes={};ns=round(self.data.time*1e9);v=self.contract['visual'];axes=np.array([[1.,0,0],[0,0,1],[0,-1,0]])
        for name in ('camera-a','camera-b','lidar-site'):
            camera=name.startswith('camera');kind='camera' if camera else 'lidar'
            sample=dict(schema_version='wendy.sensor.sample.v1',id=f'{self.run}/{name}/{self.sequence}',site_id=v['site'],frame=v['frame'],sensor_id=name,kind=kind,sequence=self.sequence,sample_ns=ns,run_id=self.run,scenario_version=v['simulation']['scenario_version'],seed=v['simulation']['seed'],package_sha256=v['package_sha256'],sensor_model='mujoco-render-v1' if camera else 'mujoco-ray-v1',uncertainty_m=.02,calibration_revision=v['calibration_revision'])
            if camera:
                cid=mj.mj_name2id(self.model,mj.mjtObj.mjOBJ_CAMERA,name);position=axes@self.data.cam_xpos[cid];rotation=axes@self.data.cam_xmat[cid].reshape(3,3)@np.diag([1.,-1.,-1.]);transform=np.eye(4);transform[:3,:3]=rotation;transform[:3,3]=position
                focal=60/math.tan(math.radians(float(self.model.cam_fovy[cid]))/2)
                sample['camera']=dict(width=160,height=120,fx=focal,fy=focal,cx=80,cy=60,camera_to_site=transform.reshape(-1).tolist(),calibration_valid_for_ns=4_000_000_000)
                self.camera_renderer.update_scene(self.data,camera=name);raw=camera_png(self.camera_renderer.render());sha=hashlib.sha256(raw).hexdigest();sample['evidence']=dict(sha256=sha,content_type='image/png',size=len(raw));images[sha]=base64.b64encode(raw).decode();camera_bytes[name]=raw
            else:
                sid=mj.mj_name2id(self.model,mj.mjtObj.mjOBJ_SITE,name);origin=self.data.site_xpos[sid].copy();position=axes@origin;points=[]
                # Fixed angular sampling, independent of source/indicator truth.
                for x in np.linspace(-2.8,2.8,21):
                    for z in np.linspace(.25,1.15,9):
                        target=np.array([x,0.,z]);direction=target-origin;direction/=np.linalg.norm(direction);gid=np.array([-1],dtype=np.int32);distance=mj.mj_ray(self.model,self.data,origin,direction,None,True,-1,gid)
                        if 0<distance<10:points.append((axes@(origin+direction*distance)).tolist())
                sample['lidar']=dict(points_m=points,max_range_m=10)
            sample['position_m']=position.tolist();samples.append(sample)
        return dict(op='capture',samples=samples,images=images,ttl_ms=4000),camera_bytes
    def step(self,archive=False):
        if time.monotonic()>getattr(self,'deadline',float('inf')):raise InterruptedError('bounded simulation ended')
        self.sequence+=1;self.data.mocap_pos[0]=[self.source[0],-self.source[1],.16];self.data.mocap_pos[1]=[self.visual_position[0],-self.visual_position[1],.6];self.mj.mj_forward(self.model,self.data);self.data.time=time.time()-self.anchor
        if self.duplicates:
            gaspub=self.sensors.call({'op':'duplicates'},True);visualpub=self.visual.call({'op':'duplicates'},True);camera_bytes={}
        else:
            observations=[GasDemo.sample(self,s)for s in self.config['sensors']]if self.gas_enabled else []
            for o in observations:o['valid_until_s']=o['captured_s']+4
            gaspub=self.sensors.call(dict(op='publish',observations=observations))
            if self.visual_enabled:
                q,camera_bytes=self.capture_visual();self.last_visual=q;visualpub=self.visual.call(q)
                if archive:atomic(self.out/'sensor-input'/f'{self.sequence:05d}-visual.json',q)
            else:visualpub={'paused':True};camera_bytes={}
        fusers=self.parallel(self.fusers,{'op':'tick'})
        end=time.monotonic()+1.0
        while all(self.gas_enabled and not self.network['partition'] and f['result']['coverage']<8 for f in fusers.values())and time.monotonic()<end:
            time.sleep(.1);fusers=self.parallel(self.fusers,{'op':'tick'})
        views=self.parallel(self.readers,{'op':'tick'});end=time.monotonic()+1.1
        want=visualpub.get('sha256')
        while want and not self.network['partition'] and any(v['result']['visual'].get('sha256')!=want for v in views.values()) and time.monotonic()<end:
            time.sleep(.1);views=self.parallel(self.readers,{'op':'tick'})
        self.last=views;self.renderer.update_scene(self.data,camera='overview');raw=png(self.renderer.render());image=f'scene-{self.sequence%2}.png';digest=hashlib.sha256(raw).hexdigest()
        for name,view in views.items():
            folder=self.out/('viewer-'+name);(folder/image).write_bytes(raw)
            result=view['result'];visual=result['visual'];camera_files=[]
            if visual.get('claim'):
                for c in visual['claim']['result']['inputs']:
                    sample=c['payload']
                    if sample.get('camera'):
                        sha=sample['evidence']['sha256'];blob=base64.b64decode(visual['claim']['images'][sha],validate=True);file=f"{sample['sensor_id']}-{self.sequence%2}.png";(folder/file).write_bytes(blob);camera_files.append(dict(name=sample['sensor_id'],file=file,sha256=sha))
            snap=dict(schema='wendy.incident-demo.v1',run_id=self.run,sequence=self.sequence,last_command_sequence=self.command_sequences[name],reader=name,node=view['node'],result=result,image=image,image_sha256=digest,cameras=camera_files,controls=name=='east',partition=self.network['partition'],visual_enabled=self.visual_enabled,gas_enabled=self.gas_enabled,duplicates=self.duplicates,displaced=self.visual_position!=self.source,wind_mismatch=self.true_wind!=self.assumptions['wind_radians'],events=self.events[-8:])
            atomic(folder/'snapshot.json',snap)
            if archive:atomic(self.out/'captures'/f'{self.sequence:05d}-{name}.json',snap)
        if archive:
            atomic(self.out/'captures'/f'{self.sequence:05d}-receipts.json',dict(gas=gaspub,visual=visualpub,estimators=fusers,readers=views));atomic(self.out/'ground-truth'/f'{self.sequence:05d}.json',dict(evaluation_only=True,gas_source=self.source,visual_fixture=self.visual_position,actual_wind=self.true_wind));(self.out/'captures'/f'{self.sequence:05d}.png').write_bytes(raw)
        return views
    def command(self,action,target=''):
        if action=='displace':self.visual_position=[.8,.5]if self.visual_position==self.source else self.source.copy()
        elif action=='partition':self.network['partition']=not self.network['partition']
        elif action=='visual':self.visual_enabled=not self.visual_enabled
        elif action=='gas':self.gas_enabled=not self.gas_enabled
        elif action=='duplicates':self.duplicates=not self.duplicates
        elif action=='wind':self.true_wind=1.5 if self.true_wind<1 else .25
        elif action=='reset':self.visual_position=self.source.copy();self.network['partition']=False;self.visual_enabled=self.gas_enabled=True;self.duplicates=False;self.true_wind=.25
        else:raise ValueError('unknown simulation control')
        self.events.append(dict(action=action,time_s=time.time()-self.anchor))
    def poll_commands(self):
        changed=False
        for name in ('east','west'):
            seq=self.command_sequences[name]+1;folder=self.out/('viewer-'+name);path=folder/'commands'/f'{seq:06d}.json'
            if seq>4096 or not path.is_file()or path.is_symlink():continue
            ok=False;error=''
            try:
                if name!='east':raise ValueError('this independent operator view is read-only')
                if path.stat().st_size>4096:raise ValueError('command exceeds bound')
                q=json.loads(path.read_text())
                if set(q)!={'run_id','sequence','action','target'}or q['run_id']!=self.run or q['sequence']!=seq:raise ValueError('command scope mismatch')
                self.command(q['action'],q['target']);ok=True
            except(ValueError,KeyError,TypeError)as e:error=str(e)
            self.command_sequences[name]=seq;atomic(folder/'receipts'/f'{seq:06d}.json',dict(sequence=seq,accepted=ok,error=error));changed=True
        return changed
    def close(self):
        for p in list(getattr(self,'readers',{}).values())+list(getattr(self,'fusers',{}).values())+([self.sensors]if getattr(self,'sensors',None)else [])+([self.visual]if getattr(self,'visual',None)else []):p.close()
        for p in getattr(self,'nodes',{}).values():
            if p.poll()is None:
                p.terminate()
                try:p.wait(timeout=3)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=2)
        for proxy in getattr(self,'proxies',[]):proxy.shutdown();proxy.server_close()
        for log in getattr(self,'node_logs',{}).values():log.close()
        self.renderer.close();self.camera_renderer.close()

def check(demo):
    checks=[]
    def settle(label,state,minimum=0):
        start=time.monotonic();observed=[]
        while True:
            views=demo.step(archive=True);states={n:v['result']['state']for n,v in views.items()};observed.append(states)
            if all(s==state for s in states.values())and time.monotonic()-start>=minimum:break
            if time.monotonic()-start>16:raise AssertionError(dict(label=label,want=state,observed=observed,reasons={n:v['result']['reason']for n,v in views.items()},visual={n:v['result']['visual']['reason']for n,v in views.items()}))
            time.sleep(.1)
        checks.append(dict(label=label,sequence=demo.sequence,states=states,observed=observed,elapsed_s=time.monotonic()-start));return views
    views=settle('actual images and analytic gas share one frame','consistent')
    for v in views.values():
        p=v['result']['visual']['claim']['result']['spatial']['position_m'];assert math.dist([p[0],p[2]],demo.visual_position)<.25,p
    for position in ([.6,.4],[-1.0,.8]):
        demo.source=list(position);demo.visual_position=list(position);views=settle('held-out placement '+str(position),'consistent')
        for view in views.values():
            p=view['result']['visual']['claim']['result']['spatial']['position_m'];assert math.dist([p[0],p[2]],position)<.25,p
    demo.source=[-.8,-.4];demo.visual_position=demo.source.copy();settle('baseline placement restored','consistent')
    demo.command('displace');settle('displaced visual fixture conflicts without averaging','conflicting')
    demo.command('reset');settle('matching evidence recovers','consistent')
    demo.command('visual');settle('visual expiry preserves gas only','gas_only',4.1)
    demo.command('reset');settle('fresh visual recovers','consistent')
    demo.command('gas');settle('gas expiry preserves visual only','visual_only',4.1)
    demo.command('reset');settle('fresh gas recovers','consistent')
    demo.command('wind');settle('undeclared airflow mismatch withholds gas association','visual_only')
    demo.command('reset');settle('model agreement recovers','consistent')
    demo.command('partition');settle('partition retains local gas and expires remote visual','gas_only',4.1);assert demo.network['blocked']>0
    demo.command('partition');settle('repair restores compatible association','consistent')
    demo.command('duplicates');settle('duplicate originals cannot renew either support','unavailable',4.1)
    demo.command('reset');settle('new captures recover both readers','consistent')
    # Real CLI rejects incompatible camera evidence before publication.
    for name,mutate in [('frame',lambda q:q['samples'][0]['frame'].update(revision='other')),('run',lambda q:q['samples'][0].update(run_id='other')),('calibration',lambda q:q['samples'][0].update(calibration_revision='other')),('sensor model',lambda q:q['samples'][0].update(sensor_model='other'))]:
        q=copy.deepcopy(demo.last_visual);mutate(q);response=demo.visual.call(q,True);assert response.get('state')=='unavailable'or'error'in response,response;checks.append(dict(label='CLI rejects incompatible '+name,response=response))
        settle('fresh recovery after '+name,'consistent')
    # Local hysteresis may retain different incumbents after different histories.
    # First establish the same live candidate set, then initialize fresh readers.
    def candidate_set(view):
        r=view['result'];return dict(gas={c['gateway']:c.get('sha256') for c in r['gas']['candidates'] if c.get('sha256') and c['remaining_s']>.5},visual=r['visual'].get('sha256'))
    end=time.monotonic()+16
    while True:
        views=demo.step(archive=True);comparison_end=time.monotonic()+.8
        while time.monotonic()<comparison_end:
            sets={n:candidate_set(v) for n,v in views.items()}
            if sets['east']==sets['west'] and len(sets['east']['gas'])==2 and sets['east']['visual']:break
            time.sleep(.1);views=demo.parallel(demo.readers,{'op':'tick'})
        if sets['east']==sets['west'] and len(sets['east']['gas'])==2 and sets['east']['visual']:break
        if time.monotonic()>end:raise AssertionError(dict(reason='same complete current candidate set unavailable',candidates=sets))
    checks.append(dict(label='same replicated candidate hashes with local histories retained',candidates=sets,incumbents={n:v['result']['gas'].get('gateway') for n,v in views.items()}))
    def instant(value):return datetime.datetime.fromisoformat(value.replace('Z','+00:00')).timestamp()
    original_deadlines={}
    for view in views.values():
        r=view['result'];at=instant(r['at'])
        for c in r['gas']['candidates']:
            if c.get('sha256'):original_deadlines[c['sha256']]=at+c['remaining_s']
        if r['visual'].get('claim'):original_deadlines[r['visual']['sha256']]=instant(r['visual']['claim']['result']['valid_until'])
    before_epochs={}
    for name,reader in demo.readers.items():
        snapshot=reader.call({'op':'query'})
        own=[c for c in snapshot['components'] if c['key']['type']=='wendy.incident.association.v1' and c['key']['source']==demo.contract['readers'][name]]
        assert len(own)==1,dict(reason='reader publication unavailable before restart',reader=name)
        before_epochs[name]=own[0]['version']['epoch']
    for reader in demo.readers.values():reader.close()
    demo.readers={n:demo.rpc(demo.executable,'reader','incident-'+n)for n in ('east','west')}
    views=demo.parallel(demo.readers,{'op':'tick'})
    after_epochs={}
    for name,view in views.items():
        after_epochs[name]=view['publication']['component']['version']['epoch']
        assert after_epochs[name]>before_epochs[name],dict(reason='reader restart did not allocate a new durable epoch',reader=name)
        for support in view['result']['supports']:
            assert support['sha256'] in original_deadlines and instant(support['valid_until'])<=original_deadlines[support['sha256']]+.000001,dict(reason='reader restart extended original support',reader=name,support=support)
    checks.append(dict(label='reader restarts advance durable epochs without renewing source deadlines',before=before_epochs,after=after_epochs,original_deadlines=original_deadlines))

    digests={n:v['result']['input_digest']for n,v in views.items()}
    assert digests['east']==digests['west'],dict(reason='fresh-reader deterministic selection differs',digests=digests,candidates={n:candidate_set(v)for n,v in views.items()})
    checks.append(dict(label='fresh independent readers select deterministically from complete candidate set',digest=digests['east']))
    report=dict(passed=True,mujoco_version=demo.mj.__version__,node_pids={str(n):p.pid for n,p in demo.nodes.items()},reader_pids={n:p.process.pid for n,p in demo.readers.items()},checks=checks,captures=demo.sequence,limitations=['Analytic synthetic gas, not physical dispersion validation','Rendered red fixture association is not camera detection of gas or leak causality','Local authenticated loopback nodes; nested evidence is gateway-attested'])
    atomic(demo.out/'checks.json',report);print(json.dumps(report,indent=2))
def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--executable',type=Path,required=True);p.add_argument('--gas-executable',type=Path,required=True);p.add_argument('--worldd',type=Path,required=True);p.add_argument('--out',type=Path,required=True);p.add_argument('--base-port',type=int,default=19480);p.add_argument('--duration',type=float,default=300);p.add_argument('--check',action='store_true');p.add_argument('--seed',type=int,default=31);a=p.parse_args()
    if not all(x.is_absolute()for x in (a.executable,a.gas_executable,a.worldd,a.out))or not 1<=a.duration<=1800 or not 1024<=a.base_port<=65529 or not 0<=a.seed<2**32:p.error('absolute paths and bounded duration/ports required')
    stopped=[False];signal.signal(signal.SIGINT,lambda *_:stopped.__setitem__(0,True));signal.signal(signal.SIGTERM,lambda *_:stopped.__setitem__(0,True));demo=IncidentDemo(a.out,a.executable,a.gas_executable,a.worldd,a.base_port,a.seed);demo.deadline=demo.started+a.duration
    try:
        if a.check:check(demo)
        else:
            while not stopped[0]and time.monotonic()<demo.deadline:
                changed=demo.poll_commands();demo.step(archive=changed or demo.sequence==0);time.sleep(.1)
    except InterruptedError:pass
    finally:demo.close()
if __name__=='__main__':main()

"""Exact-witness rising incident across three real nodes and two native readers.
Actual MuJoCo rendered pixels/rays; analytic synthetic gas; phased delivery.
"""
from __future__ import annotations
import struct,zlib
import argparse,base64,concurrent.futures,copy,datetime,hashlib,json,math,os,random,signal,socket,subprocess,time,uuid
from pathlib import Path
from incident_association import IncidentDemo,scene_xml
from gas_source import GasDemo,atomic,png
from gas_replicas import RPC,Proxy
def camera_png(rgb):
    height,width,channels=rgb.shape
    if (width,height,channels)!=(96,72,3):raise ValueError('camera image dimensions')
    def chunk(kind,raw):return struct.pack('>I',len(raw))+kind+raw+struct.pack('>I',zlib.crc32(kind+raw)&0xffffffff)
    raw=b''.join(b'\0'+row.tobytes() for row in rgb)
    return b'\x89PNG\r\n\x1a\n'+chunk(b'IHDR',struct.pack('>IIBBBBB',width,height,8,2,0,0,0))+chunk(b'IDAT',zlib.compress(raw))+chunk(b'IEND',b'')

class ChangeIncidentDemo(IncidentDemo):
    def __init__(self,out,executable,worldd,base,incident_executable,trend_executable,seed=31,linux_worldd=None,linux_image=None,inspection=False):
        import mujoco,numpy as np
        self.linux_worldd,self.linux_image=linux_worldd,linux_image;self.linux_nodes=[]
        self.mj,self.np=mujoco,np;self.out=Path(out);self.executable=executable;self.worldd=worldd;self.base=base;self.incident_executable=incident_executable;self.trend_executable=trend_executable
        if self.out.exists() or self.out.is_symlink():raise ValueError('fresh private output required')
        os.umask(0o077);self.out.mkdir(parents=True,mode=0o700)
        for n in ('captures','ground-truth','sensor-input','logs','viewer-east','viewer-west'):(self.out/n).mkdir(mode=0o700)
        for n in ('viewer-east','viewer-west'):
            for sub in ('commands','receipts'):(self.out/n/sub).mkdir(mode=0o700)
        self.xml=self.build_scene();(self.out/'scene.xml').write_text(self.xml);self.model=mujoco.MjModel.from_xml_string(self.xml);self.data=mujoco.MjData(self.model);mujoco.mj_forward(self.model,self.data)
        self.renderer=mujoco.Renderer(self.model,height=600,width=960);self.camera_renderer=mujoco.Renderer(self.model,height=72,width=96)
        self.anchor=time.time();self.run='incident-'+uuid.uuid4().hex[:12];self.source=[-.8,-.4];self.visual_position=[-.8,-.4];self.true_wind=.25;self.strength=120;self.rng=random.Random(seed);self.sequence=0
        self.enabled={n:True for n in 'abcdefgh'};self.bias=False;self.gas_enabled=True;self.visual_enabled=True;self.duplicates=False;self.network={'partition':False,'blocked':0};self.events=[];self.command_sequences={'east':0,'west':0};self.last={};self.last_visual=None
        sensors=[]
        for name in 'adeh':
            p=self.data.site_xpos[mujoco.mj_name2id(self.model,mujoco.mjtObj.mjOBJ_SITE,name)]
            sensors.append(dict(id=name,source=dict(device='gas-sensor-'+name,app='gas',service=name,stream='ppm'),position_xz=[float(p[0]),float(-p[1])],position_uncertainty_m=0,calibration='calibration-1',sensor_type='analytic-scalar-v1'))
        digest=hashlib.sha256(self.xml.encode()).hexdigest();self.config=dict(site='synthetic-gas-laboratory',frame=dict(id='laboratory-floor',revision=digest[:16]),simulation=dict(run_id=self.run,scenario_version='gas-laboratory-v1',seed=seed,clock_domain='simulation:'+self.run),sensors=sensors,min_xz=[-2,-2],max_xz=[2,2],step_m=.1,background_ppm=2)
        self.assumptions=dict(revision='declared-1',wind_radians=.25,downwind_m=3,upwind_m=1,crosswind_m=1.3)
        gas=dict(tenant='lab',anchor=datetime.datetime.fromtimestamp(self.anchor,datetime.timezone.utc).isoformat(),field=self.config,assumptions=self.assumptions,gateways={n:dict(device='gas-estimator-'+n,app='gas',service='estimator',stream='source') for n in ('east','west')})
        visual=dict(tenant='lab',site=self.config['site'],frame=self.config['frame'],anchor=gas['anchor'],simulation=self.config['simulation'],package_sha256=digest,calibration_revision='mujoco-calibration-1',sources={n:dict(device='visual-adapter',app='sensors',service=n,stream='samples') for n in ('camera-a','camera-b','lidar-site')})
        visual['sensor_models']={n:'mujoco-render-v1' if n.startswith('camera') else 'mujoco-ray-v1' for n in visual['sources']}
        self.contract=dict(gas=gas,visual=visual,visual_publisher=dict(device='visual-estimator',app='incident',service='visual',stream='fixture'),readers={n:dict(device='incident-'+n,app='incident',service='association',stream='state') for n in ('east','west')})
        self.join_contract=dict(spatial=self.contract,temporal=dict(public=gas,sensors=['a','d','e','h'],min_span_s=.6,max_gap_s=1.5,rate_ppm_s=1))
        atomic(self.out/'public-input.json',self.join_contract)
        for name in ('east','west'):atomic(self.out/('viewer-'+name)/'catalog.json',self.contract)
        self.runtime=self.out/'runtime';setup=[str(executable),'--role','setup','--config',str(self.out/'public-input.json'),'--out',str(self.runtime),'--base-port',str(base)];setup+=['--inspection'] if inspection else [];subprocess.run(setup,check=True,timeout=10)
        self.nodes={};self.node_logs={};self.proxies=[];self.fusers={};self.readers={};self.sensors=None;self.visual=None;self.trends={};self.native={};self.native_logs={};self.qa_seq={n:0 for n in ('east','west')}
        try:
            self.proxies=[Proxy(base+3+i,base+i,'node-'+str(i+1),self.network) for i in range(3)]
            for n in range(1,4):self.start_node(n)
            self.sensors=self.rpc(trend_executable,'sensors','sensors');self.fusers={n:self.rpc(executable,'estimator','estimate-'+n) for n in ('east','west')}
            self.visual=self.rpc(incident_executable,'visual','incident-visual');self.readers={n:self.rpc(executable,'reader','change-'+n) for n in ('east','west')}
        except Exception:self.close();raise
        self.started=time.monotonic();self.deadline=self.started+600;self.mode="steady";self.level_at=self.started;self.pause_trend=False;self.dropout=False;self.last_estimators={};self.last_trends={};self.restart_warmup={};self.original_sequence=0;self.last_capture_wall=None;self.visual_pool=concurrent.futures.ThreadPoolExecutor(max_workers=3);self.publication_futures={}
    def start_node(self,n):
        if not self.linux_worldd:return super().start_node(n)
        from incident_change_nodes import LinuxNode
        node=LinuxNode(self.linux_worldd,self.linux_image,self.runtime/f'node-{n}.json',self.runtime,self.out/'logs',n,self.base+n-1)
        self.linux_nodes.append(node);self.nodes[n]=node
        end=time.monotonic()+5
        while time.monotonic()<end:
            if node.poll() is not None:raise RuntimeError('Linux node exited')
            try:
                with socket.create_connection(('127.0.0.1',self.base+n-1),timeout=.1):return
            except OSError:time.sleep(.05)
        raise RuntimeError('Linux node did not listen')
    def capture_visual(self):
        np=self.np;mj=self.mj;samples=[];images={};camera_bytes={};ns=round(self.data.time*1e9);v=self.contract['visual'];axes=np.array([[1.,0,0],[0,0,1],[0,-1,0]])
        for name in ('camera-a','camera-b','lidar-site'):
            camera=name.startswith('camera');kind='camera' if camera else 'lidar'
            sample=dict(schema_version='wendy.sensor.sample.v1',id=f'{self.run}/{name}/{self.sequence}',site_id=v['site'],frame=v['frame'],sensor_id=name,kind=kind,sequence=self.sequence,sample_ns=ns,run_id=self.run,scenario_version=v['simulation']['scenario_version'],seed=v['simulation']['seed'],package_sha256=v['package_sha256'],sensor_model='mujoco-render-v1' if camera else 'mujoco-ray-v1',uncertainty_m=.02,calibration_revision=v['calibration_revision'])
            if camera:
                cid=mj.mj_name2id(self.model,mj.mjtObj.mjOBJ_CAMERA,name);position=axes@self.data.cam_xpos[cid];rotation=axes@self.data.cam_xmat[cid].reshape(3,3)@np.diag([1.,-1.,-1.]);transform=np.eye(4);transform[:3,:3]=rotation;transform[:3,3]=position
                focal=36/math.tan(math.radians(float(self.model.cam_fovy[cid]))/2)
                sample['camera']=dict(width=96,height=72,fx=focal,fy=focal,cx=48,cy=36,camera_to_site=transform.reshape(-1).tolist(),calibration_valid_for_ns=4_000_000_000)
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
    def command(self,action,target=''):
        if action in ('ramp','steady','decline','spike'):self.mode=action
        elif action=='temporal':self.pause_trend=not self.pause_trend
        elif action=='dropout':self.dropout=not self.dropout
        elif action=='restart':
            for name in ('east','west'):
                self.fusers[name].close();self.fusers[name]=self.rpc(self.executable,'estimator','estimate-'+name);self.restart_warmup[name]=self.fusers[name].call({'op':'paired'})
            atomic(self.out/'restart-warmup.json',self.restart_warmup)
        else:super().command(action,target);return
        self.events.append(dict(action=action,time_s=time.time()-self.anchor))
    def step(self,archive=True,capture=True):
        if time.monotonic()>self.deadline:raise TimeoutError('bounded simulation deadline')
        self.poll_commands();self.sequence+=1
        self.data.mocap_pos[0]=[self.source[0],-self.source[1],.16];self.data.mocap_pos[1]=[self.visual_position[0],-self.visual_position[1],.6];self.mj.mj_forward(self.model,self.data)
        gaspub={};visualpub={};bursts=[];phase={};visual_future=None;pipeline_started=time.monotonic()
        if self.duplicates:
            gaspub=self.sensors.call({'op':'duplicates'},True);visualpub=self.visual.call({'op':'duplicates'},True)
        elif capture:
            # Warm only after a genuine capture gap. During a running stream,
            # retain chronological originals and capture once per update instead
            # of stopping delivery to rebuild three samples for every display.
            burst_count=3 if self.last_capture_wall is None or time.monotonic()-self.last_capture_wall>=1.5 else 1
            phase['capture_count']=burst_count
            previous=None
            for burst in range(burst_count):
                if previous is not None:
                    wait=.35-(time.monotonic()-previous)
                    if wait>0:time.sleep(wait)
                previous=time.monotonic();self.last_capture_wall=previous;dt=min(2,previous-self.level_at);self.level_at=previous
                if self.mode=='ramp':self.strength=min(8000,self.strength+100*dt)
                elif self.mode=='decline':self.strength=max(40,self.strength-100*dt)
                self.original_sequence+=1;self.data.time=time.time()-self.anchor
                observations=[GasDemo.sample(self,s) for s in self.config['sensors'] if not(self.dropout and s['id']=='h')] if self.gas_enabled else []
                for o in observations:
                    o.update(id=f"{o['sensor']}/{self.original_sequence}",version=dict(epoch=1,sequence=self.original_sequence),valid_until_s=o['captured_s']+4)
                    if self.mode=='spike':o['value_ppm']+=80
                if burst==burst_count-1 and self.visual_enabled:
                    stage=time.monotonic();q,_=self.capture_visual();phase['camera_render_s']=time.monotonic()-stage;self.last_visual=q
                    visual_future=self.visual_pool.submit(self.visual.call,q)
                    atomic(self.out/'sensor-input'/f'{self.sequence:05d}-visual.json',q)
                stage=time.monotonic();gaspub=self.sensors.call(dict(op='publish',observations=observations));phase['last_raw_publish_s']=time.monotonic()-stage
                atomic(self.out/'sensor-input'/f'{self.sequence:05d}-{burst}.json',observations)
                if self.mode=='spike':self.mode='steady'
                samples={};stage=time.monotonic();raw_end=stage+.45
                if not self.pause_trend:
                    while True:
                        samples=self.parallel(self.fusers,{'op':'paired' if burst==burst_count-1 else 'sample'})
                        captured=observations[0]['captured_s'] if observations else None
                        def current(v):
                            traces={t['sensor']:t for t in v['temporal']['window']['traces']}
                            expected=set(self.join_contract['temporal']['sensors'])-({'h'} if self.dropout else set())
                            return expected.issubset(traces) and all(traces[n]['points'] and abs(traces[n]['points'][-1]['captured_s']-captured)<1e-6 for n in expected)
                        if captured is None or self.network['partition'] or all(current(v) for v in samples.values()) or time.monotonic()>=raw_end:break
                        time.sleep(.03)
                phase["last_estimator_s"]=time.monotonic()-stage
                bursts.append(dict(publication=gaspub,temporal=samples))
        fusers=samples if capture and not self.duplicates and not self.pause_trend else self.parallel(self.fusers,{'op':'spatial' if self.pause_trend else 'paired'});trends={n:v['temporal'] for n,v in fusers.items()};self.last_estimators=fusers;self.last_trends=trends
        if visual_future is not None:visualpub=visual_future.result(timeout=7)
        publications={name:future.result(timeout=7) for name,future in self.publication_futures.items()};self.publication_futures={}
        stage=time.monotonic();repair_end=stage+.5
        while True:
            views=self.parallel(self.readers,{'op':'observe'})
            want='supported_rise' if self.mode=='ramp' else 'non_rising'
            if all(v['result']['state']==want for v in views.values()) or self.network['partition'] or self.duplicates or self.pause_trend or time.monotonic()>=repair_end:break
            time.sleep(.04)
        phase["readers_s"]=time.monotonic()-stage;phase["pipeline_s"]=time.monotonic()-pipeline_started
        self.last=views
        self.model.geom_rgba[self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_GEOM,'source-marker'),3]=0
        self.renderer.update_scene(self.data,camera='overview');raw=png(self.renderer.render());image=f'scene-{self.sequence%2}.png';digest=hashlib.sha256(raw).hexdigest()
        for name,view in views.items():
            folder=self.out/('viewer-'+name);(folder/image).write_bytes(raw);r=view['result'];spatial=r['spatial'];camera_files=[]
            if spatial['visual'].get('claim'):
                for c in spatial['visual']['claim']['result']['inputs']:
                    sample=c['payload']
                    if sample.get('camera'):
                        sha=sample['evidence']['sha256'];blob=base64.b64decode(spatial['visual']['claim']['images'][sha],validate=True);file=f"{sample['sensor_id']}-{self.sequence%2}.png";(folder/file).write_bytes(blob);camera_files.append(dict(name=sample['sensor_id'],file=file,sha256=sha))
            base=dict(schema='wendy.incident-demo.v1',run_id=self.run,sequence=self.sequence,last_command_sequence=self.command_sequences[name],reader=name,node=view['node'],result=spatial,image=image,image_sha256=digest,cameras=camera_files,controls=name=='east',partition=self.network['partition'],visual_enabled=self.visual_enabled,gas_enabled=self.gas_enabled,duplicates=self.duplicates,displaced=self.visual_position!=self.source,wind_mismatch=self.true_wind!=self.assumptions['wind_radians'],events=self.events[-8:])
            snap=dict(schema='wendy.incident-change-demo.v1',run_id=self.run,sequence=self.sequence,reader=name,node=view['node'],written_unix_s=time.time(),result=r,spatial_view=base,mode=self.mode,temporal_paused=self.pause_trend,dropout=self.dropout)
            atomic(folder/'snapshot.json',snap)
            if archive:atomic(self.out/'captures'/f'{self.sequence:05d}-{name}.json',snap)
        if archive:
            atomic(self.out/'captures'/f'{self.sequence:05d}-transport.json',dict(gas=gaspub,visual=visualpub,estimators=fusers,trends=trends,bursts=bursts,readers=views,previous_publications=publications,blocked=self.network['blocked'],visual_envelope_bytes=visualpub.get('signed_bytes'),phase_seconds=phase,capture_to_read_s={n:{p['kind']:(datetime.datetime.fromisoformat(v['result']['at'].replace('Z','+00:00'))-datetime.datetime.fromisoformat(p['captured_at'].replace('Z','+00:00'))).total_seconds() for p in v['result']['spatial']['supports']} for n,v in views.items()}))
            atomic(self.out/'ground-truth'/f'{self.sequence:05d}.json',dict(evaluation_only=True,gas_source=self.source,visual_fixture=self.visual_position,strength=self.strength,actual_wind=self.true_wind));(self.out/'captures'/f'{self.sequence:05d}.png').write_bytes(raw)
        self.publication_futures={name:self.visual_pool.submit(reader.call,{'op':'publish'}) for name,reader in self.readers.items() if not views[name].get('transport_error')}
        return views
    def launch_native(self,app):
        for name in ('east','west'):
            log=open(self.out/'logs'/f'native-{name}.log','a');self.native_logs[name]=log;self.native[name]=subprocess.Popen([str(Path(app)/'Contents/MacOS/WendyWorldDemo'),'--incident-change',str(self.out/('viewer-'+name))],stdout=log,stderr=log)
        time.sleep(.4)
    def qa(self,action,name='east'):
        if action=='capture':self.step()
        self.qa_seq[name]+=1;seq=self.qa_seq[name];folder=self.out/('viewer-'+name);atomic(folder/'native-qa.json',dict(sequence=seq,action=action));end=time.monotonic()+7
        while time.monotonic()<end:
            if action!='capture':self.step()
            path=folder/f'native-receipt-{seq:04d}.json'
            if path.exists():
                value=json.loads(path.read_text());assert value['accepted'],value;return value
            time.sleep(.08)
        raise TimeoutError('native control receipt unavailable '+action)
    def close(self):
        if hasattr(self,'visual_pool'):
            final={}
            for name,future in self.publication_futures.items():
                try:final[name]=future.result(timeout=7)
                except Exception as error:final[name]=dict(error=repr(error))
            atomic(self.out/'final-publications.json',final);self.visual_pool.shutdown(wait=True,cancel_futures=True)
        for p in getattr(self,'trends',{}).values():p.close()
        for p in getattr(self,'native',{}).values():
            if p.poll() is None:
                p.terminate()
                try:p.wait(timeout=3)
                except subprocess.TimeoutExpired:p.kill();p.wait(timeout=2)
        for f in getattr(self,'native_logs',{}).values():f.close()
        try:super().close()
        finally:
            errors=[]
            for node in self.linux_nodes:
                try:node.cleanup()
                except Exception as error:errors.append(repr(error))
            if errors:raise RuntimeError(errors)
def settle_state(d,record,label,want,minimum=1.3,predicate=None):
    """Wait for the complete expected state within the original 14-second bound."""
    start=time.monotonic();seen=[]
    while time.monotonic()-start<14:
        v=d.step();states={n:x['result']['state'] for n,x in v.items()}
        seen.append({n:dict(state=x['result']['state'],spatial=x['result']['spatial']['state'],temporal=x['result']['temporal']['state']) for n,x in v.items()})
        elapsed=time.monotonic()-start
        if elapsed>=14:break
        if all(s==want for s in states.values()) and (predicate is None or predicate(v)) and elapsed>=minimum:
            record(dict(label=label,state=want,sequence=d.sequence,observed=seen));return v
        time.sleep(.08)
    raise AssertionError(dict(label=label,want=want,observed=seen,reasons={n:(v['result']['reason'],v['result']['spatial']['state'],v['result']['temporal']['state']) for n,v in d.last.items()}))

def check(d,app=None):
    checks=[]
    def record(v):checks.append(v);atomic(d.out/'checks-progress.json',dict(checks=checks,captures=d.sequence))
    def settle(label,want,minimum=1.3,predicate=None):return settle_state(d,record,label,want,minimum,predicate)
    settle('steady gas has a spatial association but no sustained rise','non_rising')
    if app:d.launch_native(app);d.qa('capture');d.qa('capture','west')
    def control(action):
        if app:d.qa(action)
        else:d.command(action)
    control('ramp');v=settle('sustained rise joins exact originals with actual visual geometry','supported_rise',2)
    for x in v.values():assert len(x['result']['witnesses'])==4 and len(x['result']['supports'])==3
    for name,x in v.items():
        pos=x['result']['spatial']['visual']['claim']['result']['spatial']['position_m'];assert math.dist([pos[0],pos[2]],d.visual_position)<.25,dict(reader=name,inferred=pos,evaluation_only=d.visual_position)
    record(dict(label='actual96x72 image-derived geometry within0.25m of withheld evaluation truth',positions={n:x['result']['spatial']['visual']['claim']['result']['spatial']['position_m'] for n,x in v.items()}))
    # Compare the same immutable candidate set without creating new captures.
    def hashes(value):return {p['sensor']:p['sha256'] for p in value['result']['witnesses']}
    compare_end=time.monotonic()+.55
    while hashes(v['east'])!=hashes(v['west']) and time.monotonic()<compare_end:
        time.sleep(.04)
        for future in d.publication_futures.values():future.result(timeout=7)
        d.publication_futures={};v=d.parallel(d.readers,{'op':'observe'})
    assert hashes(v['east'])==hashes(v['west']) and len(hashes(v['east']))==4,dict(comparison={n:hashes(x) for n,x in v.items()})
    record(dict(label='independent readers join the same exact original witness set',hashes=hashes(v['east']),gateways={n:x['result']['spatial']['gas'].get('gateway') for n,x in v.items()},claim_hashes={n:{p['kind']:p['sha256'] for p in x['result']['supports']}for n,x in v.items()}))
    if app:
        for name in ('east','west'):atomic(d.out/('viewer-'+name)/'native-observe.json',dict(sequence=1,seconds=12))
        until=time.monotonic()+16
        paths={name:d.out/('viewer-'+name)/'native-observation-0001.json' for name in ('east','west')}
        while not all(p.exists() for p in paths.values()) and time.monotonic()<until:d.step();time.sleep(.03)
        visibility={}
        for name,path in paths.items():
            measured=json.loads(path.read_text());samples=measured['samples'];seconds=samples[-1]['elapsed_s']-samples[0]['elapsed_s']
            visible=sum(b['elapsed_s']-a['elapsed_s'] for a,b in zip(samples,samples[1:]) if a['state']=='supported_rise')
            sequences=sorted({x['snapshot_sequence'] for x in samples if x['state']=='supported_rise'})
            visibility[name]=dict(measured_seconds=seconds,visible_seconds=visible,fraction=visible/seconds,original_sequences=sequences)
        atomic(d.out/'native-live-visibility.json',visibility)
        assert all(v['fraction']>=.5 and len(v['original_sequences'])>=3 for v in visibility.values()),dict(native_live_visibility=visibility)
        record(dict(label='both actual native readers retain repeatedly current joined support for at least half the live observation window',visibility=visibility))
        accepted={name:[] for name in ('east','west')}
        for path in sorted((d.out/'captures').glob('*-transport.json')):
            for name,reply in json.loads(path.read_text()).get('previous_publications',{}).items():
                if reply.get('published') and reply['result']['state']=='supported_rise':accepted[name].append(dict(sha256=reply['sha256'],signed_bytes=reply['signed_bytes'],input_digest=reply['result']['input_digest']))
        assert all(accepted.values()),dict(actual_publications=accepted)
        record(dict(label='both independent readers publish their exact observed rise within original deadlines',receipts=accepted))
        d.qa('capture');d.qa('capture','west')
    control('steady');settle('stable concentration does not claim rising','non_rising',2)
    control('spike');settle('single transient does not persist as sustained rise','non_rising',2)
    control('decline');settle('falling concentration is non-rising, never clearance','non_rising',2)
    control('ramp');settle('fresh rise restores exact joined support','supported_rise',2)
    control('temporal');v=settle('temporal loss preserves independently current spatial association','unavailable',2,lambda v:all(x['result']['spatial']['state']=='consistent' for x in v.values()))
    assert all(x['result']['spatial']['state']=='consistent' for x in v.values())
    if app:d.qa('capture')
    control('temporal');settle('new chronological originals rebuild temporal support','supported_rise',2)
    control('dropout');v=settle('missing commissioned sensor withholds the join','unsupported',4.1,lambda v:all(x['result']['temporal']['state']=='unavailable' for x in v.values()))
    assert all(x['result']['temporal']['state']=='unavailable' for x in v.values())
    control('dropout');settle('fresh captures rebuild after commissioned sensor dropout','supported_rise',2)
    control('displace');v=settle('displaced actual image fixture cannot become a joint rising incident','unsupported',1.3,lambda v:all(x['result']['spatial']['state']=='conflicting' for x in v.values()))
    assert all(x['result']['spatial']['state']=='conflicting' for x in v.values())
    if app:d.qa('capture')
    control('displace');settle('matching spatial evidence restores context','supported_rise',1.3)
    control('partition');settle('partition expires remote support without inventing a rise','unavailable',4.1);assert d.network['blocked']>0
    if app:d.qa('capture')
    control('partition');settle('repair with fresh original sequence restores context','supported_rise',2)
    control('duplicates');settle('repeated originals cannot extend joined support','unavailable',4.1)
    if app:d.qa('capture')
    control('duplicates');settle('fresh captures recover after duplicate-only delivery','supported_rise',2)
    control('restart');assert all(x['temporal']['window']['state'] in ('warming','unknown') for x in d.restart_warmup.values());record(dict(label='restarted temporal estimators do not reconstruct dwell'))
    settle('fresh post-restart history restores context','supported_rise',2)
    if app:d.qa('capture')
    for name,mutate in [('frame',lambda q:q['samples'][0]['frame'].update(revision='wrong')),('run',lambda q:q['samples'][0].update(run_id='wrong')),('calibration',lambda q:q['samples'][0].update(calibration_revision='wrong'))]:
        q=copy.deepcopy(d.last_visual);mutate(q);reply=d.visual.call(q,True);assert reply.get('state')=='unavailable' or 'error' in reply,reply;record(dict(label='actual visual adapter rejects '+name,response=reply));settle('fresh originals recover after '+name,'supported_rise',1.3)
    return dict(passed=True,checks=checks,captures=d.sequence,blocked_repairs=d.network['blocked'],run_id=d.run,simulation='MuJoCo rendered cameras/LiDAR and analytic gas; no motion',limits=['Phased original-batch delivery, not continuous streaming capacity.','Exact original-hash conjunction; not leak causality, independent sensor authenticity or physical gas accuracy.'])
def main():
    p=argparse.ArgumentParser();p.add_argument('--out',required=True);p.add_argument('--executable',required=True);p.add_argument('--incident-executable',required=True);p.add_argument('--trend-executable',required=True);p.add_argument('--worldd',required=True);p.add_argument('--linux-worldd');p.add_argument('--linux-image');p.add_argument('--app');p.add_argument('--base-port',type=int,default=29880);p.add_argument('--seed',type=int,default=73);p.add_argument('--seconds',type=int,default=300);p.add_argument('--check',action='store_true');a=p.parse_args()
    if bool(a.linux_worldd)!=bool(a.linux_image):raise ValueError('both Linux binary and locally cached image required')
    if not 1<=a.seconds<=1800:raise ValueError('bounded duration required')
    d=None;result={};stopped=[False]
    def stop(*_):
        stopped[0]=True
        if d is not None:d.deadline=min(d.deadline,time.monotonic())
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    try:
        d=ChangeIncidentDemo(a.out,a.executable,a.worldd,a.base_port,a.incident_executable,a.trend_executable,a.seed,a.linux_worldd,a.linux_image);d.deadline=time.monotonic()+a.seconds
        if a.check:result=check(d,a.app)
        else:
            if a.app:d.launch_native(a.app)
            while not stopped[0] and time.monotonic()<d.deadline:d.step();time.sleep(.08)
    except Exception as e:
        result=dict(passed=False,error=repr(e));raise
    finally:
        if d:
            d.close();atomic(d.out/'acceptance.json',result);private=[]
            for path in d.runtime.glob('*.json'):
                if path.name!='public.json':private.append(path.name);path.unlink()
            atomic(d.out/'cleanup.json',dict(owned_processes_stopped=True,private_runtime_configs_removed=private))
if __name__=='__main__':main()

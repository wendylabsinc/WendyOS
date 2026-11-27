"""Three independent Go fusers, delivered-result selection and a native twin.

Pixels and LiDAR rays are MuJoCo measurements; gas and audio are generated.
Delivery switches model partitions. They do not configure a physical network.
"""
from __future__ import annotations
import argparse
import base64
from datetime import datetime, timedelta, timezone
import hashlib
import json
import math
import os
from pathlib import Path
import select
import signal
import subprocess
import time
from types import SimpleNamespace
import uuid

from sim.sensors import calibration, png_rgb, audio_wav
from sim.office_fusion import display_result
from sim.macos_demo_bridge import SessionFiles, geometry, _transform, COMMAND_SCHEMA

GATEWAYS = ('hub', 'atlas', 'iris')
ANCHOR = datetime(2026, 1, 1, tzinfo=timezone.utc)
XML = '''<mujoco model="Fusion handoff lab"><option timestep=".01"/><visual><global offwidth="320" offheight="240"/><headlight ambient=".5 .5 .5" diffuse=".6 .6 .6"/></visual><worldbody>
<light pos="0 -2 5"/><geom name="floor" type="plane" size="4 4 .1" rgba=".22 .27 .30 1"/>
<body name="fixture" mocap="true" pos="0 0 1"><geom name="red-fixture" type="box" size=".22 .15 .24" rgba="1 .02 .02 1"/></body>
<body name="focus" pos=".3 0 1"/>
<camera name="camera-a" pos="-1.7 -3 1.7" mode="targetbody" target="focus" fovy="50"/>
<camera name="camera-b" pos="1.7 -3 1.7" mode="targetbody" target="focus" fovy="50"/>
<geom name="gateway-hub" type="box" pos="0 -3 .12" size=".12 .12 .12" rgba=".05 .7 .6 1"/>
<geom name="gateway-atlas" type="box" pos="-.4 -3 .12" size=".12 .12 .12" rgba=".1 .4 .85 1"/>
<geom name="gateway-iris" type="box" pos=".4 -3 .12" size=".12 .12 .12" rgba=".65 .4 .8 1"/>
</worldbody></mujoco>'''


def timestamp(seconds):
    nanoseconds=round(seconds*1_000_000_000)
    whole,fraction=divmod(nanoseconds,1_000_000_000)
    return (ANCHOR+timedelta(seconds=whole)).strftime('%Y-%m-%dT%H:%M:%S')+f'.{fraction:09d}Z'


def elapsed(value):
    return (datetime.fromisoformat(value.replace('Z', '+00:00'))-ANCHOR).total_seconds()


class Process:
    def __init__(self, command, log):
        self.log=Path(log).open('w')
        self.process=subprocess.Popen([str(x) for x in command], stdin=subprocess.PIPE,
            stdout=subprocess.PIPE, stderr=self.log, text=True, bufsize=1)

    def call(self, value):
        self.process.stdin.write(json.dumps(value, allow_nan=False, separators=(',', ':'))+'\n')
        self.process.stdin.flush()
        if not select.select([self.process.stdout], [], [], 10)[0]:
            raise TimeoutError('simulation worker response timed out')
        raw=self.process.stdout.readline((16<<20)+1)
        if not raw.endswith('\n') or len(raw)>16<<20:raise RuntimeError('invalid worker response')
        value=json.loads(raw)
        if 'error' in value:raise RuntimeError(value['error'])
        return value

    def close(self):
        if self.process.poll() is None:
            self.process.stdin.close()
            try:self.process.wait(timeout=2)
            except subprocess.TimeoutExpired:
                self.process.kill();self.process.wait(timeout=2)
        self.log.close()


class Demo:
    def __init__(self, out, fusion_binary, view_binary, run_id):
        import mujoco
        import numpy as np
        if mujoco.__version__!='4.0.0':raise ValueError('MuJoCo 4.0.0 required')
        self.out,self.run_id=Path(out),run_id
        self.mj,self.np=mujoco,np
        self.model=mujoco.MjModel.from_xml_string(XML);self.data=mujoco.MjData(self.model)
        self.renderer=mujoco.Renderer(self.model,height=240,width=320)
        self.digest=hashlib.sha256(XML.encode()).hexdigest()
        self.frame={'id':'handoff-lab','revision':self.digest[:16]}
        self.sequence=self.command_sequence=self.capture_sequence=0
        self.seconds=.25;self.target_x=0.;self.last_capture=-1.
        self.input_links={g:True for g in GATEWAYS};self.result_links={g:True for g in GATEWAYS}
        self.uncertainty={'hub':.3,'atlas':.55,'iris':.65}
        self.delivered={};self.delivered_outputs={};self.duplicate_pending=None
        self.workers={};self.selector=None
        self.catalog=[{'sensor_id':sid,'kind':kind,'owner':'hub'} for sid,kind in
            [('camera-a','camera'),('camera-b','camera'),('gas-1','gas'),('audio-1','audio')]] + [
            {'sensor_id':'lidar-'+g,'kind':'lidar','owner':g} for g in GATEWAYS]
        self.timeline=(self.out/'timeline.jsonl').open('w')
        self.requests=(self.out/'delivered-reports.jsonl').open('w')
        try:
            for g in GATEWAYS:
                path=self.out/(g+'-config.json')
                path.write_text(json.dumps({'version':1,'participant_id':g,'run_id':run_id,'frame':self.frame,'sensors':self.catalog}))
                self.workers[g]=Process([fusion_binary,'--config',path,'--state-dir',self.out/g],self.out/(g+'.log'))
                (self.out/'fusion'/g).mkdir(parents=True,mode=0o700)
            cfg={'tenant':'simulation','site':'studio-01','run_id':run_id,'clock_domain':'simulation:'+run_id,
                'frame':self.frame,'gateways':{g:{'device':g,'app':'world-fusion-demo','service':'hazard','stream':'claims'} for g in GATEWAYS},
                'hold_ns':1_000_000_000,'improvement_fraction':.2}
            path=self.out/'selector-config.json';path.write_text(json.dumps(cfg))
            self.selector=Process([view_binary,'--config',path],self.out/'selector.log')
        except BaseException:self.close();raise
        self.mj.mj_forward(self.model,self.data)
        self.runtime=SimpleNamespace(model=self.model,data=self.data,mj=self.mj,bindings={},run_id=run_id,
            config={'model':{'sha256':self.digest},'presentation':{'name':'Fusion / handoff lab',
                'subtitle':'One hazard view across three gateways','labels':[]}})
        self.geometry,self.dynamic_ids=geometry(self.runtime)
        self.dynamic_ids=[]
        for geom in self.geometry['geoms']:
            if geom['name']=='red-fixture':geom['dynamic']=True;self.dynamic_ids.append(geom['id'])
        self.geometry['semantics']='MuJoCo reference geometry. Hazard position comes only from camera and LiDAR inference.'
        self.display_origin_ns=time.monotonic_ns()-250_000_000
        self.view=None
        self.recording=self.out/'recording';self.recording.mkdir(mode=0o700)
        (self.recording/'assets').mkdir(mode=0o700)
        (self.recording/'geometry.json').write_text(json.dumps(self.geometry))
        self.frames=(self.recording/'frames.jsonl').open('w');self.last_record=-1.

    def samples(self):
        self.capture_sequence+=1
        self.data.time=self.seconds;self.data.mocap_pos[0][0]=self.target_x
        self.mj.mj_forward(self.model,self.data)
        samples=[];media={}
        for entry in self.catalog:
            name,kind=entry['sensor_id'],entry['kind']
            item={'schema_version':'wendy.sensor.sample.v1','id':f'{self.run_id}/{name}/{self.capture_sequence}',
                'site_id':'studio-01','frame':self.frame,'sensor_id':name,'kind':kind,'sequence':self.capture_sequence,
                'sample_ns':round(self.seconds*1e9),'run_id':self.run_id,'scenario_version':'fusion-handoff-v1','seed':7,
                'package_sha256':self.digest,'sensor_model':'simulated-handoff-fixture','position_m':[0,1,3],
                'uncertainty_m':.02,'calibration_revision':'handoff-camera-v1'}
            blob=None
            if kind=='camera':
                cid=self.mj.mj_name2id(self.model,self.mj.mjtObj.mjOBJ_CAMERA,name)
                item['position_m'],item['camera']=calibration(self.model,self.data,cid)
                self.renderer.update_scene(self.data,camera=name)
                blob=png_rgb(self.renderer.render());mime='image/png'
            elif kind=='audio':
                item['audio']={'sample_rate_hz':8000,'channels':1,'duration_ns':250_000_000}
                blob=audio_wav(7,self.capture_sequence,True);mime='audio/wav'
            elif kind=='gas':item['gas']={'value_ppm':90.,'uncertainty_ppm':1.,'species':'synthetic-test-gas'}
            else:
                gateway=entry['owner'];x={'hub':0.,'atlas':-.4,'iris':.4}[gateway]
                origin=self.np.array([x,-3.,1.1]);points=[]
                for elevation in range(-8,9,2):
                    for azimuth in range(-35,36,2):
                        a,e=math.radians(azimuth),math.radians(elevation)
                        direction=self.np.array([math.sin(a)*math.cos(e),math.cos(a)*math.cos(e),math.sin(e)])
                        geom=self.np.array([-1],dtype=self.np.int32)
                        distance=self.mj.mj_ray(self.model,self.data,origin,direction,None,1,-1,geom)
                        if 0<distance<=6:
                            p=origin+direction*distance;points.append([float(p[0]),float(p[2]),float(-p[1])])
                item.update(position_m=[x,1.1,3],uncertainty_m=self.uncertainty[gateway],sensor_model='mujoco-raycast-with-declared-position-error')
                item['lidar']={'points_m':points,'max_range_m':6.}
            if blob is not None:
                digest=hashlib.sha256(blob).hexdigest();item['evidence']={'sha256':digest,'content_type':mime,'size':len(blob)}
                media[digest]=base64.b64encode(blob).decode()
            samples.append(item)
        return samples,media

    def capture(self):
        samples,media=self.samples();outputs={};reports=[]
        for g in GATEWAYS:
            local=[s for s in samples if next(x['owner'] for x in self.catalog if x['sensor_id']==s['sensor_id'])==g]
            if g=='hub' and not self.input_links[g]:local=[s for s in local if s['kind']=='lidar']
            received=[]
            if g!='hub' and self.input_links[g]:
                received=[c for c in outputs['hub']['replica_records'] if c['key']['source']['service'] in ('camera-a','camera-b','gas-1','audio-1')]
            delivered_media=(media if g=='hub' else outputs['hub']['media']) if self.input_links[g] else {}
            output=self.workers[g].call({'now_ns':round(self.seconds*1e9),'local_samples':local,'received':received,'media':delivered_media})
            outputs[g]=output
            if self.result_links[g]:
                report={'gateway':g,'version':output['version'],'evaluated_at':timestamp(self.seconds),'state':output['result']['state']}
                if output.get('claim'):report['claim']=output['claim']
                reports.append(report);self.delivered[g]=report;self.delivered_outputs[g]=output
                # These are the images actually delivered to this gateway and
                # retained with this delivered result, never newest global RGB.
                for item in output['observations']:
                    if item['kind']=='camera' and item['evidence']['sha256'] in output['media']:
                        path=self.out/'fusion'/g/(item['sensor_id']+'.png')
                        tmp=path.with_suffix('.tmp');tmp.write_bytes(base64.b64decode(output['media'][item['evidence']['sha256']]));tmp.replace(path)
        self.last_outputs=outputs
        return reports

    def tick(self, seconds):
        if seconds<self.seconds:raise ValueError('simulation clock regressed')
        self.seconds=seconds
        reports=[]
        if seconds-self.last_capture>=.24999:
            self.last_capture=seconds;reports=self.capture()
        if self.duplicate_pending:
            reports.append(self.delivered[self.duplicate_pending]);self.duplicate_pending=None
        request={'now':timestamp(seconds),'reports':reports}
        self.view=self.selector.call(request)
        self.requests.write(json.dumps(request)+'\n');self.requests.flush()
        selected=self.view.get('selected')
        if selected:
            output=self.delivered_outputs[selected]
            fusion=display_result({'now_ns':round(seconds*1e9),'samples':output['observations']},output['result'],node=selected,catalog=self.catalog,records=output['replica_records'])
            fusion['sample_time_s']=elapsed(self.delivered[selected]['claim']['timing']['sample_time'])
        else:
            fusion={'state':self.view['state'],'label':'Conflicting locations' if self.view['state']=='disputed' else 'No current hazard estimate',
                'reason':self.view['reason'],'sample_time_s':seconds,'observations':[]}
        candidates=[]
        for c in self.view['candidates']:
            c=dict(c);g=c['gateway'];c.update(input_enabled=self.input_links[g],result_enabled=self.result_links[g],
                position_error_m=self.uncertainty[g])
            if c.get('valid_until'):c['remaining_s']=max(0,elapsed(c['valid_until'])-seconds)
            candidates.append(c)
        selection={k:v for k,v in self.view.items() if k not in ('claim','candidates','events')}
        selection['candidates']=candidates
        selection['events']=[dict(e,time_s=elapsed(e['time'])) for e in self.view['events']]
        selection['fixture_moved']=self.target_x!=0
        self.sequence+=1
        edge={'id':'internet','a':'hub','b':'internet','enabled':False,'up':False,'packets':0}
        snapshot={'run_id':self.run_id,'time_s':seconds,'phase':'fusion-handoff','authorized':True,'stopped':False,
            'robots':[],'jobs':[],'completed':0,'total':0,'events':[],'voxels':[],'voxel_size':.2,
            'map_node':selected or 'hub','network':{'edges':[],'internet':edge,'gossip_rounds':0,'peer_voxels':{}},
            'fusion':fusion,'fusion_selection':selection}
        state={'schema':'wendy.macos-demo.state.v1','run_id':self.run_id,'sequence':self.sequence,
            'mode':'fusion-handoff','display_clock':'mach_absolute_time_ns',
            'observation_deadline_monotonic_ns':self.display_origin_ns+round((seconds+2)*1e9),
            'display_deadline_monotonic_ns':self.display_origin_ns+round(fusion.get('valid_until_s',seconds)*1e9),
            'worker_status':'running','last_command_sequence':self.command_sequence,'snapshot':snapshot,
            'dynamic_geoms':[_transform(self.runtime,i) for i in self.dynamic_ids]}
        self.timeline.write(json.dumps({'seconds':seconds,'selection':selection,'fusion':fusion})+'\n');self.timeline.flush()
        if seconds-self.last_record>=.24999:
            self.last_record=seconds;assets={}
            for observation in fusion['observations']:
                name=observation.get('evidence_path')
                if name:
                    raw=(self.out/name).read_bytes();digest=hashlib.sha256(raw).hexdigest()
                    asset='assets/'+digest+'.png';path=self.recording/asset
                    if not path.exists():path.write_bytes(raw)
                    assets[name]=asset
            self.frames.write(json.dumps({'at_s':seconds-.25,'state':state,'assets':assets})+'\n');self.frames.flush()
        return state

    def command(self,action,target):
        if action=='handoff_input' and target in GATEWAYS:self.input_links[target]=not self.input_links[target]
        elif action=='handoff_result' and target in GATEWAYS:self.result_links[target]=not self.result_links[target]
        elif action=='handoff_quality' and target in GATEWAYS:self.uncertainty[target]=.03 if self.uncertainty[target]>.1 else {'hub':.3,'atlas':.55,'iris':.65}[target]
        elif action=='handoff_duplicate' and target in self.delivered:self.duplicate_pending=target
        elif action=='handoff_fixture' and target=='fixture':self.target_x=1.1 if self.target_x==0 else 0.
        else:raise ValueError('unknown handoff control')
        self.command_sequence+=1
        return {'schema':'wendy.macos-demo.command-result.v1','run_id':self.run_id,'sequence':self.command_sequence,
            'action':action,'target':target,'accepted':True,'time_s':self.seconds}

    def close(self):
        for p in self.workers.values():p.close()
        if self.selector:self.selector.close()
        self.renderer.close();self.timeline.close();self.requests.close()
        if hasattr(self,"frames"):self.frames.close()


def check(demo):
    records=[]
    def step(count=1):
        for _ in range(count):records.append(demo.tick(demo.seconds+.25))
        return records[-1]['snapshot']['fusion_selection']
    assert step()['selected']=='hub'
    demo.command('handoff_quality','atlas');v=step();assert v['selected']=='hub' and v['challenger']=='atlas',v
    assert step(4)['selected']=='atlas'
    demo.command('handoff_result','atlas');assert step(8)['selected']=='hub'
    demo.command('handoff_duplicate','atlas');assert step()['selected']=='hub'
    demo.command('handoff_result','atlas');assert step(5)['selected']=='atlas'
    demo.command('handoff_input','atlas');assert step(8)['selected']=='hub'
    demo.command('handoff_input','atlas');assert step(5)['selected']=='atlas'
    # All outputs partitioned. Repeated old delivery must not renew the claim.
    for g in GATEWAYS:demo.command('handoff_result',g)
    demo.command('handoff_duplicate','atlas');v=step(8);assert v['state']=='unavailable',v
    for g in GATEWAYS:demo.command('handoff_result',g)
    assert step()['selected']=='atlas'
    # Old and new actual captures disagree while both remain within their TTL.
    demo.command('handoff_result','atlas');demo.command('handoff_fixture','fixture')
    v=step();assert v['state']=='disputed',v
    v=step(8);assert v['state']=='selected',v
    result={'passed':True,'captures':demo.capture_sequence,'states':len(records),
        'worker_pids':{g:p.process.pid for g,p in demo.workers.items()},
        'handoffs':demo.view['events'],'source':'actual MuJoCo RGB/rays and three independent Go fusion processes'}
    (demo.out/'checks.json').write_text(json.dumps(result,indent=2)+'\n')
    (demo.out/'check-states.json').write_text(json.dumps(records))
    print(json.dumps(result))


def main():
    p=argparse.ArgumentParser(description=__doc__);p.add_argument('--out',type=Path,required=True)
    p.add_argument('--fusion',type=Path,required=True);p.add_argument('--selector',type=Path,required=True)
    p.add_argument('--duration',type=float,default=120);p.add_argument('--check',action='store_true');args=p.parse_args()
    if not 1<=args.duration<=1800:p.error('duration must be1..1800 seconds')
    os.umask(0o077);args.out.mkdir(parents=True,exist_ok=False,mode=0o700)
    run='handoff-'+uuid.uuid4().hex[:12];files=SessionFiles(args.out,run);demo=None
    stopping=False
    def stop(_signal,_frame):
        nonlocal stopping
        stopping=True
    signal.signal(signal.SIGTERM,stop);signal.signal(signal.SIGINT,stop)
    try:
        demo=Demo(args.out,args.fusion.resolve(),args.selector.resolve(),run)
        files.write('geometry.json',demo.geometry)
        files.write('bridge-ready.json',{'schema':'wendy.macos-demo.ready.v1','run_id':run,'scope':'simulated-fusion-handoff'})
        if args.check:check(demo);return
        start=time.monotonic();demo.display_origin_ns=time.monotonic_ns()-250_000_000;state=None
        while time.monotonic()-start<args.duration and not stopping and not files.stop_requested():
            sequence=demo.command_sequence+1;command=files.command(sequence)
            if command:
                if command.get('schema')!=COMMAND_SCHEMA or command.get('run_id')!=run or command.get('sequence')!=sequence:raise ValueError('command belongs to another session')
                receipt=demo.command(command['action'],command['target']);files.write(f'{sequence:06d}.json',receipt,receipt=True)
            state=demo.tick(.25+time.monotonic()-start)
            files.write('state.json',state,replace=demo.sequence>1);time.sleep(.025)
        if state:
            state['sequence']+=1;state['worker_status']='closed';files.write('state.json',state,replace=True)
    finally:
        if demo:demo.close()
        files.close()

if __name__=='__main__':main()
